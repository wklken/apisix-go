package exit_transformer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 22950
	name     = "exit-transformer"
)

const schema = `
{
  "type": "object",
  "properties": {
    "functions": {
      "type": "array",
      "items": {
        "type": "string"
      }
    }
  },
  "required": ["functions"]
}
`

type Config struct {
	Functions []string `json:"functions"`
}

type exitResponse struct {
	status int
	body   []byte
	header http.Header
}

var statusRemapPattern = regexp.MustCompile(`if\s+code\s*==\s*(\d+)\s+then\s+return\s+(\d+)`)

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)

		resp := exitResponse{
			status: recorder.statusCode,
			body:   recorder.body.Bytes(),
			header: recorder.header.Clone(),
		}
		if source, _ := apisixctx.GetRequestVar(r, "$response_source").(string); source == "upstream" {
			writeResponse(w, resp)
			return
		}
		for _, fn := range p.config.Functions {
			resp = applyFunction(resp, fn)
		}
		writeResponse(w, resp)
	})
}

func applyFunction(resp exitResponse, fn string) exitResponse {
	if from, to, ok := parseStatusRemap(fn); ok && resp.status == from {
		resp.status = to
	}
	if isNormalizedErrorFunction(fn) && resp.status >= http.StatusBadRequest {
		resp.header.Set("X-Error-Code", fmt.Sprint(resp.status))
		resp.header.Set("Content-Type", "application/json")
		resp.body = normalizeBody(resp.status, resp.body)
		resp.header.Set("Content-Length", fmt.Sprint(len(resp.body)))
	}
	return resp
}

func parseStatusRemap(fn string) (int, int, bool) {
	matches := statusRemapPattern.FindStringSubmatch(fn)
	if len(matches) != 3 {
		return 0, 0, false
	}
	var from, to int
	if _, err := fmt.Sscanf(matches[1], "%d", &from); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(matches[2], "%d", &to); err != nil {
		return 0, 0, false
	}
	return from, to, true
}

func isNormalizedErrorFunction(fn string) bool {
	return strings.Contains(fn, `header["X-Error-Code"]`) &&
		strings.Contains(fn, "tostring(code)") &&
		strings.Contains(fn, "body = {error = true")
}

func normalizeBody(status int, body []byte) []byte {
	message := "request failed"
	var original map[string]any
	if err := json.Unmarshal(body, &original); err == nil {
		if value, ok := original["message"].(string); ok && value != "" {
			message = value
		}
	}
	payload := map[string]any{
		"error":   true,
		"status":  status,
		"message": message,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
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
	return r.body.Write(body)
}

func writeResponse(w http.ResponseWriter, resp exitResponse) {
	for field, values := range resp.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(resp.status)
	w.Write(resp.body)
}
