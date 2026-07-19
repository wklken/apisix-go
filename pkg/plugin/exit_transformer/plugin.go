package exit_transformer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
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

var (
	statusRemapPattern = regexp.MustCompile(
		`if\s+code\s*==\s*(\d+)\s+then\s+return\s+(\d+)`,
	)
	statusBodyRemapPattern = regexp.MustCompile(
		`if\s+code\s*==\s*(\d+)\s+then\s+return\s+(\d+)\s*,\s*"([^"]*)"`,
	)
	requestContentTypePattern = regexp.MustCompile(
		`(?s)ct\s*==\s*"([^"]+)"\s+and\s+code\s*==\s*(\d+)\s+then.*?return\s+(\d+)`,
	)
	errorTablePattern = regexp.MustCompile(
		`(?s)if\s+code\s*==\s*(\d+)\s+and\s+body\.message\s*==\s*"([^"]+)"\s+then\s+return\s+(\d+)\s*,\s*\{message\s*=\s*"([^"]+)"\}\s*,\s*\{\["content-type"\]\s*=\s*"([^"]+)"\}`,
	)
	invalidEqualityPattern = regexp.MustCompile(`code\s*==\s*then\b`)
	invalidCallPattern     = regexp.MustCompile(`if\s+code\s*==\s*(\d+)\s+then\s+return\s+code\s*\(\s*\)`)
)

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if slices.ContainsFunc(p.config.Functions, invalidEqualityPattern.MatchString) {
		return fmt.Errorf("unexpected symbol near 'then'")
	}
	return nil
}

func (p *Plugin) Config() any {
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
			resp = applyFunction(resp, fn, r)
		}
		writeResponse(w, resp)
	})
}

func applyFunction(resp exitResponse, fn string, r *http.Request) exitResponse {
	if strings.Contains(fn, `core.log.warn("exit transformer running outside if check")`) {
		logger.Warn("exit transformer running outside if check")
	}
	if matches := invalidCallPattern.FindStringSubmatch(fn); len(matches) == 2 {
		if from, ok := parseNumber(matches[1]); ok && resp.status == from {
			logger.Errorf("attempt to call local 'code' (a number value)")
			return resp
		}
	}
	if transformed, ok := transformErrorTable(resp, fn); ok {
		resp = transformed
	} else if matches := requestContentTypePattern.FindStringSubmatch(fn); len(matches) == 4 {
		from, fromOK := parseNumber(matches[2])
		to, toOK := parseNumber(matches[3])
		if fromOK && toOK && resp.status == from && r.Header.Get("Content-Type") == matches[1] {
			if strings.Contains(fn, `core.log.warn("exit transformer running inside if check")`) {
				logger.Warn("exit transformer running inside if check")
			}
			resp.status = to
		}
	} else if from, to, body, ok := parseStatusBodyRemap(fn); ok && resp.status == from {
		resp.status = to
		resp.body = []byte(body)
		resp.header.Set("Content-Length", fmt.Sprint(len(resp.body)))
	} else if from, to, ok := parseStatusRemap(fn); ok && resp.status == from {
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

func parseStatusBodyRemap(fn string) (int, int, string, bool) {
	matches := statusBodyRemapPattern.FindStringSubmatch(fn)
	if len(matches) != 4 {
		return 0, 0, "", false
	}
	from, fromOK := parseNumber(matches[1])
	to, toOK := parseNumber(matches[2])
	return from, to, matches[3], fromOK && toOK
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

func parseNumber(value string) (int, bool) {
	var number int
	if _, err := fmt.Sscanf(value, "%d", &number); err != nil {
		return 0, false
	}
	return number, true
}

func transformErrorTable(resp exitResponse, fn string) (exitResponse, bool) {
	matches := errorTablePattern.FindStringSubmatch(fn)
	if len(matches) != 6 {
		return resp, false
	}
	from, fromOK := parseNumber(matches[1])
	to, toOK := parseNumber(matches[3])
	if !fromOK || !toOK || resp.status != from {
		return resp, false
	}
	var body map[string]any
	if err := json.Unmarshal(resp.body, &body); err != nil || body["message"] != matches[2] {
		return resp, false
	}
	encoded, err := json.Marshal(map[string]string{"message": matches[4]})
	if err != nil {
		return resp, false
	}
	resp.status = to
	resp.body = encoded
	resp.header = make(http.Header)
	resp.header.Set("Content-Type", matches[5])
	resp.header.Set("Content-Length", fmt.Sprint(len(resp.body)))
	return resp, true
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
	_, _ = w.Write(resp.body)
}
