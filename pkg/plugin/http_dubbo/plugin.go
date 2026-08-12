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

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo"
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

func (p *Plugin) Config() any {
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
		if apisixctx.GetRequestLifecycle(r) != nil {
			base.AdaptRequestPhase(p, next).ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, p.prepareRequest(r))
	}
	return http.HandlerFunc(fn)
}

// RunRequestPhase prepares the request-local HTTP-Dubbo configuration. The
// route-owned terminal performs framing and response ownership later.
func (p *Plugin) RunRequestPhase(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	return base.ContinueRequest(p.prepareRequest(r))
}

func (p *Plugin) prepareRequest(r *http.Request) *http.Request {
	return WithConfig(r, p.config)
}

func (p *Plugin) ServeDubbo(w http.ResponseWriter, r *http.Request, target string) {
	ServeDubbo(w, r, target, p.config)
}

func ServeDubbo(w http.ResponseWriter, r *http.Request, target string, cfg Config) {
	applyDefaults(&cfg)
	frame, err := buildDubboRequest(r, cfg)
	if err != nil {
		dubbo.WriteError(w, http.StatusBadRequest, "failed to build Dubbo request: "+err.Error())
		return
	}
	result := dubbo.Attempt(r.Context(), target, transportConfig(cfg), frame)
	dubbo.ReportOutcome(r, result.Response.Status, result.Err)
	writeDubboResult(w, r, result)
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
	applyDefaults(&cfg)
	attempts := max(retries+1, 1)
	attempts = min(attempts, dubbo.MaxRetries+1)

	tcfg := transportConfig(cfg)
	result := dubbo.ServeWithRetries(r, attempts, func() dubbo.Result {
		target, err := nextTarget()
		if err != nil {
			return dubbo.Result{Err: fmt.Errorf("failed to select upstream target: %w", err)}
		}
		frame, frameErr := buildDubboRequest(r, cfg)
		if frameErr != nil {
			return dubbo.Result{Err: fmt.Errorf("failed to build Dubbo request: %w", frameErr)}
		}
		return dubbo.Attempt(r.Context(), target, tcfg, frame)
	})
	writeDubboResult(w, r, result)
}

func transportConfig(cfg Config) dubbo.Config {
	return dubbo.Config{
		ConnectTimeout: time.Duration(cfg.ConnectTimeout) * time.Millisecond,
		SendTimeout:    time.Duration(cfg.SendTimeout) * time.Millisecond,
		ReadTimeout:    time.Duration(cfg.ReadTimeout) * time.Millisecond,
		DecodeResponse: decodeTextResponse,
	}
}

func decodeTextResponse(conn net.Conn) (dubbo.Response, error) {
	status, body, err := readDubboResponse(conn)
	if err != nil {
		return dubbo.Response{}, err
	}
	return dubbo.Response{Status: status, Body: []byte(body)}, nil
}

func writeDubboResult(w http.ResponseWriter, r *http.Request, result dubbo.Result) {
	if result.Err == nil {
		dubbo.WriteResponse(w, result.Response)
		return
	}
	if result.ConnectFailed && r.Context().Err() == nil {
		var netErr net.Error
		if !errors.As(result.Err, &netErr) || !netErr.Timeout() {
			logger.Errorf("%s", result.Err)
		}
	}
	dubbo.WriteError(w, dubbo.ErrorStatus(r.Context(), result.Err), result.Err.Error())
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
	body, err := base.ReadRequestBody(r)
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
		return 0, "", fmt.Errorf("dubbo response payload exceeds %d bytes", maxDubboResponsePayload)
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
		return http.StatusInternalServerError, "", nil
	}
}
