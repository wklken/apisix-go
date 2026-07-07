package http_dubbo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 504
	name     = "http-dubbo"
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
	applyDefaults(&cfg)
	frame, err := buildDubboRequest(r, cfg)
	if err != nil {
		writeJSONMessage(w, http.StatusBadRequest, "failed to build Dubbo request: "+err.Error())
		return
	}

	conn, err := net.DialTimeout("tcp", target, time.Duration(cfg.ConnectTimeout)*time.Millisecond)
	if err != nil {
		writeJSONMessage(w, http.StatusBadGateway, "failed to connect to upstream: "+err.Error())
		return
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(time.Duration(cfg.SendTimeout) * time.Millisecond)); err != nil {
		writeJSONMessage(w, http.StatusInternalServerError, "failed to set upstream write deadline: "+err.Error())
		return
	}
	if _, err := conn.Write(frame); err != nil {
		writeJSONMessage(w, http.StatusInternalServerError, "failed to send Dubbo request: "+err.Error())
		return
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Duration(cfg.ReadTimeout) * time.Millisecond)); err != nil {
		writeJSONMessage(w, http.StatusInternalServerError, "failed to set upstream read deadline: "+err.Error())
		return
	}
	status, body, err := readDubboResponse(conn)
	if err != nil {
		writeJSONMessage(w, http.StatusInternalServerError, "failed to read Dubbo response: "+err.Error())
		return
	}
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
	encoded, err := json.Marshal(param)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func appendDubboLine(buf *bytes.Buffer, value string) {
	encoded, _ := json.Marshal(value)
	buf.Write(encoded)
	buf.WriteByte('\n')
}

func readDubboResponse(conn net.Conn) (int, string, error) {
	reader := bufio.NewReader(conn)
	header := make([]byte, 16)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, "", err
	}
	if header[3] != 20 {
		return 0, "", fmt.Errorf("unexpected Dubbo response status %d", header[3])
	}

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
