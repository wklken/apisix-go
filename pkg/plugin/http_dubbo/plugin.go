package http_dubbo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority                = 504
	name                    = "http-dubbo"
	maxDubboResponsePayload = 8 * 1024 * 1024
	maxDubboRetries         = 10
)

const schema = `
{
  "type": "object",
  "properties": {
    "service_name": {
      "type": "string",
      "minLength": 1
    },
    "service_version": {
      "type": "string",
      "pattern": "^\\d+\\.\\d+\\.\\d+",
      "default": "0.0.0"
    },
    "method": {
      "type": "string",
      "minLength": 1
    },
    "params_type_desc": {
      "type": "string",
      "default": ""
    },
    "serialization_header_key": {
      "type": "string"
    },
    "serialized": {
      "type": "boolean",
      "default": false
    },
    "connect_timeout": {
      "type": "number",
      "default": 6000
    },
    "read_timeout": {
      "type": "number",
      "default": 6000
    },
    "send_timeout": {
      "type": "number",
      "default": 6000
    }
  },
  "required": ["service_name", "method"]
}
`

type Config struct {
	ServiceName            string `json:"service_name"`
	ServiceVersion         string `json:"service_version,omitempty"`
	Method                 string `json:"method"`
	ParamsTypeDesc         string `json:"params_type_desc,omitempty"`
	SerializationHeaderKey string `json:"serialization_header_key,omitempty"`
	Serialized             bool   `json:"serialized,omitempty"`
	ConnectTimeout         int    `json:"connect_timeout,omitempty"`
	ReadTimeout            int    `json:"read_timeout,omitempty"`
	SendTimeout            int    `json:"send_timeout,omitempty"`
}

type configKey struct{}

func WithConfig(r *http.Request, cfg Config) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), configKey{}, cfg))
}

func GetConfig(r *http.Request) (Config, bool) {
	cfg, ok := r.Context().Value(configKey{}).(Config)
	return cfg, ok
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	applyDefaults(&p.config)
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, WithConfig(r, p.config))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) ServeDubbo(w http.ResponseWriter, r *http.Request, target string) {
	ServeDubbo(w, r, target, p.config)
}

func ServeDubbo(w http.ResponseWriter, r *http.Request, target string, cfg Config) {
	result := serveDubboAttempt(r, target, cfg)
	reportDubboOutcome(r, result)
	if result.err != nil {
		writeJSONMessage(w, dubboErrorStatus(r.Context(), result.err), result.err.Error())
		return
	}
	writeDubboResponse(w, result.status, result.body)
}

// ServeDubboWithRetries retries only failures that happen before any request
// bytes are written. A Dubbo invocation may be non-idempotent, so a timeout or
// malformed response after a successful write must not issue it again.
func ServeDubboWithRetries(
	w http.ResponseWriter,
	r *http.Request,
	nextTarget func() (string, error),
	cfg Config,
	retries int,
) {
	attempts := retries + 1
	if attempts < 1 {
		attempts = 1
	}
	if attempts > maxDubboRetries+1 {
		attempts = maxDubboRetries + 1
	}

	var result dubboAttemptResult
	for attempt := 0; attempt < attempts; attempt++ {
		target, err := nextTarget()
		if err != nil {
			result.err = fmt.Errorf("failed to select upstream target: %w", err)
			break
		}
		result = serveDubboAttempt(r, target, cfg)
		reportDubboOutcome(r, result)
		if result.err == nil || !result.retryable {
			break
		}
		if r.Context().Err() != nil {
			break
		}
	}

	if result.err != nil {
		writeJSONMessage(w, dubboErrorStatus(r.Context(), result.err), result.err.Error())
		return
	}
	writeDubboResponse(w, result.status, result.body)
}

type dubboAttemptResult struct {
	status    int
	body      string
	err       error
	retryable bool
}

func reportDubboOutcome(r *http.Request, result dubboAttemptResult) {
	if result.err == nil {
		pxy.ReportHTTPOutcome(r, result.status)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	var netErr net.Error
	pxy.ReportTCPFailureOutcome(r, errors.Is(result.err, context.DeadlineExceeded) ||
		(errors.As(result.err, &netErr) && netErr.Timeout()))
}

func serveDubboAttempt(r *http.Request, target string, cfg Config) dubboAttemptResult {
	applyDefaults(&cfg)
	frame, err := buildDubboRequest(r, cfg)
	if err != nil {
		return dubboAttemptResult{err: fmt.Errorf("failed to build Dubbo request: %w", err)}
	}

	conn, err := (&net.Dialer{Timeout: time.Duration(cfg.ConnectTimeout) * time.Millisecond}).DialContext(
		r.Context(),
		"tcp",
		target,
	)
	if err != nil {
		return dubboAttemptResult{
			err:       fmt.Errorf("failed to connect to upstream: %w", err),
			retryable: true,
		}
	}
	defer conn.Close()
	stopClose := context.AfterFunc(r.Context(), func() { _ = conn.Close() })
	defer stopClose()

	if err := conn.SetWriteDeadline(dubboDeadline(r.Context(), time.Duration(cfg.SendTimeout)*time.Millisecond)); err != nil {
		return dubboAttemptResult{
			err:       fmt.Errorf("failed to set upstream write deadline: %w", err),
			retryable: true,
		}
	}
	written, err := conn.Write(frame)
	if err != nil {
		return dubboAttemptResult{
			err:       fmt.Errorf("failed to send Dubbo request: %w", err),
			retryable: written == 0,
		}
	}
	if written != len(frame) {
		return dubboAttemptResult{err: io.ErrShortWrite}
	}

	if err := conn.SetReadDeadline(dubboDeadline(r.Context(), time.Duration(cfg.ReadTimeout)*time.Millisecond)); err != nil {
		return dubboAttemptResult{err: fmt.Errorf("failed to set upstream read deadline: %w", err)}
	}
	status, body, err := readDubboResponse(conn)
	if err != nil {
		return dubboAttemptResult{err: fmt.Errorf("failed to read Dubbo response: %w", err)}
	}
	return dubboAttemptResult{status: status, body: body}
}

func writeDubboResponse(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	if body != "" {
		_, _ = w.Write([]byte(body))
	}
}

func applyDefaults(cfg *Config) {
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "0.0.0"
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 6000
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 6000
	}
	if cfg.SendTimeout == 0 {
		cfg.SendTimeout = 6000
	}
}

func buildDubboRequest(r *http.Request, cfg Config) ([]byte, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}

	params, err := dubboParams(r, cfg, body)
	if err != nil {
		return nil, err
	}

	payload := bytes.NewBuffer(nil)
	appendDubboLine(payload, "2.0.2")
	appendDubboLine(payload, cfg.ServiceName)
	appendDubboLine(payload, cfg.ServiceVersion)
	appendDubboLine(payload, cfg.Method)
	appendDubboLine(payload, cfg.ParamsTypeDesc)
	payload.WriteString(params)
	payload.WriteString("{}\n")

	frame := make([]byte, 16+payload.Len())
	frame[0], frame[1], frame[2], frame[3] = 0xda, 0xbb, 0xc6, 0x00
	binary.BigEndian.PutUint64(frame[4:12], 1)
	binary.BigEndian.PutUint32(frame[12:16], uint32(payload.Len()))
	copy(frame[16:], payload.Bytes())
	return frame, nil
}

func dubboParams(r *http.Request, cfg Config, body []byte) (string, error) {
	if requestBodyIsSerialized(r, cfg) {
		params := string(body)
		if params != "" && !strings.HasSuffix(params, "\n") {
			params += "\n"
		}
		return params, nil
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return "", nil
	}

	var arrayParams []any
	if err := json.Unmarshal(body, &arrayParams); err == nil {
		return encodeParamList(arrayParams)
	}

	var objectParams map[string]any
	if err := json.Unmarshal(body, &objectParams); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(objectParams))
	for key := range objectParams {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	params := make([]any, 0, len(keys))
	for _, key := range keys {
		params = append(params, objectParams[key])
	}
	return encodeParamList(params)
}

func requestBodyIsSerialized(r *http.Request, cfg Config) bool {
	if cfg.SerializationHeaderKey == "" {
		return cfg.Serialized
	}
	return r.Header.Get(cfg.SerializationHeaderKey) == "true"
}

func encodeParamList(params []any) (string, error) {
	var out strings.Builder
	for _, param := range params {
		encoded, err := encodeDubboParam(param)
		if err != nil {
			return "", err
		}
		out.WriteString(encoded)
		out.WriteByte('\n')
	}
	return out.String(), nil
}

func encodeDubboParam(param any) (string, error) {
	if param == nil {
		return "null", nil
	}
	if stringValue, ok := param.(string); ok {
		return encodeFastJSONString(stringValue), nil
	}
	encoded, err := json.Marshal(param)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func encodeFastJSONString(value string) string {
	var out strings.Builder
	out.Grow(len(value) + 2)
	out.WriteByte('"')
	for _, char := range value {
		switch char {
		case '\\':
			out.WriteString(`\\`)
		case '"':
			out.WriteString(`\"`)
		case '\n':
			out.WriteString(`\n`)
		case '\t':
			out.WriteString(`\t`)
		case '\r':
			out.WriteString(`\r`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		default:
			out.WriteRune(char)
		}
	}
	out.WriteByte('"')
	return out.String()
}

func appendDubboLine(buf *bytes.Buffer, value string) {
	encoded, _ := json.Marshal(value)
	buf.Write(encoded)
	buf.WriteByte('\n')
}

func readDubboResponse(conn net.Conn) (int, string, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, "", err
	}
	if header[0] != 0xda || header[1] != 0xbb {
		return 0, "", fmt.Errorf("unexpected Dubbo response magic %x%02x", header[0], header[1])
	}
	if header[3] != 20 {
		return 0, "", fmt.Errorf("unexpected Dubbo response status %d", header[3])
	}
	payloadLength := binary.BigEndian.Uint32(header[12:16])
	if payloadLength == 0 {
		return 0, "", fmt.Errorf("empty Dubbo response payload")
	}
	if payloadLength > maxDubboResponsePayload {
		return 0, "", fmt.Errorf("Dubbo response payload exceeds %d bytes", maxDubboResponsePayload)
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, "", err
	}
	reader := bufio.NewReader(bytes.NewReader(payload))

	bodyStatus, err := reader.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	switch strings.TrimSuffix(strings.TrimSuffix(bodyStatus, "\n"), "\r") {
	case "2", "5":
		return http.StatusOK, "", nil
	case "1", "4":
		body, err := reader.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		return http.StatusOK, strings.TrimSuffix(strings.TrimSuffix(body, "\n"), "\r"), nil
	default:
		return 0, "", fmt.Errorf("unexpected Dubbo body status %q", bodyStatus)
	}
}

func dubboDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

func dubboErrorStatus(ctx context.Context, err error) int {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
