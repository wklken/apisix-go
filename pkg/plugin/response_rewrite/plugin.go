package response_rewrite

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 899
	name     = "response-rewrite"
)

const schema = `
{
  "type": "object",
  "properties": {
    "headers": {
      "type": "object",
      "minProperties": 1
    },
    "body": {
      "type": "string"
    },
    "body_base64": {
      "type": "boolean",
      "default": false
    },
    "status_code": {
      "type": "integer",
      "minimum": 200,
      "maximum": 598
    }
  }
}
`

type Config struct {
	Headers    Headers `json:"headers,omitempty"`
	Body       *string `json:"body,omitempty"`
	BodyBase64 *bool   `json:"body_base64,omitempty"`
	StatusCode int     `json:"status_code,omitempty"`
}

type Headers struct {
	Add       []string          `json:"add,omitempty"`
	Set       map[string]string `json:"set,omitempty"`
	Remove    []string          `json:"remove,omitempty"`
	LegacySet map[string]string `json:"-"`
}

func (h *Headers) UnmarshalJSON(data []byte) error {
	type headerOperations Headers
	var operations headerOperations
	if err := jsonUnmarshal(data, &operations); err != nil {
		return err
	}
	if len(operations.Add) > 0 || len(operations.Set) > 0 || len(operations.Remove) > 0 {
		*h = Headers(operations)
		return nil
	}

	var legacy map[string]string
	if err := jsonUnmarshal(data, &legacy); err != nil {
		return err
	}
	h.LegacySet = legacy
	return nil
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.BodyBase64 == nil {
		b := false
		p.config.BodyBase64 = &b
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)

		p.rewrite(recorder)
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewrite(resp *responseRecorder) {
	if p.config.StatusCode != 0 {
		resp.statusCode = p.config.StatusCode
	}

	if p.config.Body != nil {
		if p.config.BodyBase64 != nil && *p.config.BodyBase64 {
			body, err := base64.StdEncoding.DecodeString(*p.config.Body)
			if err == nil {
				resp.body = body
			}
		} else {
			resp.body = []byte(*p.config.Body)
		}
		resp.header.Del("Content-Length")
	}

	p.config.Headers.apply(resp.header)
}

func (h Headers) apply(header http.Header) {
	for _, field := range h.Remove {
		header.Del(field)
	}
	for field, value := range h.LegacySet {
		header.Set(field, value)
	}
	for field, value := range h.Set {
		header.Set(field, value)
	}
	for _, entry := range h.Add {
		field, value, ok := strings.Cut(entry, ":")
		if !ok {
			continue
		}
		header.Add(strings.TrimSpace(field), strings.TrimSpace(value))
	}
}

type responseRecorder struct {
	header      http.Header
	body        []byte
	statusCode  int
	wroteHeader bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.body = append(r.body, body...)
	return len(body), nil
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	w.Write(r.body)
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
