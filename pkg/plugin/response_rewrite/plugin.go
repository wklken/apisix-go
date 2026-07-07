package response_rewrite

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
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
    },
    "vars": {
      "type": "array"
    },
    "filters": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["regex", "replace"],
        "properties": {
          "regex": {
            "type": "string",
            "minLength": 1
          },
          "scope": {
            "type": "string",
            "enum": ["once", "global"],
            "default": "once"
          },
          "replace": {
            "type": "string"
          },
          "options": {
            "type": "string",
            "default": "jo"
          }
        }
      }
    }
  },
  "dependencies": {
    "body": {
      "not": { "required": ["filters"] }
    },
    "filters": {
      "not": { "required": ["body"] }
    }
  }
}
`

type Config struct {
	Headers    Headers  `json:"headers,omitempty"`
	Body       *string  `json:"body,omitempty"`
	BodyBase64 *bool    `json:"body_base64,omitempty"`
	StatusCode int      `json:"status_code,omitempty"`
	Vars       []any    `json:"vars,omitempty"`
	Filters    []Filter `json:"filters,omitempty"`
}

type Filter struct {
	Regex   string `json:"regex,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Replace string `json:"replace,omitempty"`
	Options string `json:"options,omitempty"`

	pattern *regexp.Regexp
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
	if p.config.Body != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body and filters cannot be configured together")
	}
	for i := range p.config.Filters {
		if p.config.Filters[i].Scope == "" {
			p.config.Filters[i].Scope = "once"
		}
		if p.config.Filters[i].Scope != "once" && p.config.Filters[i].Scope != "global" {
			return fmt.Errorf("response-rewrite filter scope %q is not supported", p.config.Filters[i].Scope)
		}
		if p.config.Filters[i].Options == "" {
			p.config.Filters[i].Options = "jo"
		}
		pattern, err := compileFilterPattern(p.config.Filters[i].Regex, p.config.Filters[i].Options)
		if err != nil {
			return fmt.Errorf("response-rewrite filter regex %q validation failed: %w", p.config.Filters[i].Regex, err)
		}
		p.config.Filters[i].pattern = pattern
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

		if p.varsMatched(r, recorder) {
			p.rewrite(r, recorder)
		}
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewrite(r *http.Request, resp *responseRecorder) {
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

	if len(p.config.Filters) > 0 {
		for _, filter := range p.config.Filters {
			if filter.Scope == "global" {
				resp.body = []byte(filter.pattern.ReplaceAllString(string(resp.body), filter.Replace))
				continue
			}
			resp.body = []byte(replaceFirstString(filter.pattern, string(resp.body), filter.Replace))
		}
		resp.header.Del("Content-Length")
	}

	p.config.Headers.apply(r, resp)
}

func (p *Plugin) varsMatched(r *http.Request, resp *responseRecorder) bool {
	if len(p.config.Vars) == 0 {
		return true
	}

	pendingOp := "AND"
	hasResult := false
	result := true
	for _, condition := range p.config.Vars {
		if op, ok := condition.(string); ok {
			switch strings.ToUpper(op) {
			case "AND", "OR":
				pendingOp = strings.ToUpper(op)
			default:
				return false
			}
			continue
		}

		matched := matchCondition(r, resp, condition)
		if !hasResult {
			result = matched
			hasResult = true
			continue
		}
		if pendingOp == "OR" {
			result = result || matched
		} else {
			result = result && matched
		}
		pendingOp = "AND"
	}
	return hasResult && result
}

func (h Headers) apply(r *http.Request, resp *responseRecorder) {
	header := resp.header
	for _, field := range h.Remove {
		header.Del(field)
	}
	for field, value := range h.LegacySet {
		header.Set(field, resolveValue(r, resp, value))
	}
	for field, value := range h.Set {
		header.Set(field, resolveValue(r, resp, value))
	}
	for _, entry := range h.Add {
		field, value, ok := strings.Cut(entry, ":")
		if !ok {
			continue
		}
		header.Add(strings.TrimSpace(field), resolveValue(r, resp, strings.TrimSpace(value)))
	}
}

func compileFilterPattern(pattern string, options string) (*regexp.Regexp, error) {
	prefix := ""
	if strings.Contains(options, "i") {
		prefix += "(?i)"
	}
	if strings.Contains(options, "m") {
		prefix += "(?m)"
	}
	if strings.Contains(options, "s") {
		prefix += "(?s)"
	}
	return regexp.Compile(prefix + pattern)
}

func replaceFirstString(pattern *regexp.Regexp, body string, replacement string) string {
	replaced := false
	return pattern.ReplaceAllStringFunc(body, func(match string) string {
		if replaced {
			return match
		}
		replaced = true
		return pattern.ReplaceAllString(match, replacement)
	})
}

func matchCondition(r *http.Request, resp *responseRecorder, condition any) bool {
	parts, ok := condition.([]any)
	if !ok || len(parts) != 3 {
		return false
	}

	left := fmt.Sprint(parts[0])
	op := fmt.Sprint(parts[1])
	right := fmt.Sprint(parts[2])
	actual := responseVar(r, resp, left)

	switch op {
	case "==":
		return actual == right
	case "!=":
		return actual != right
	case ">":
		return compareNumber(actual, right, func(a, b float64) bool { return a > b })
	case ">=":
		return compareNumber(actual, right, func(a, b float64) bool { return a >= b })
	case "<":
		return compareNumber(actual, right, func(a, b float64) bool { return a < b })
	case "<=":
		return compareNumber(actual, right, func(a, b float64) bool { return a <= b })
	case "~":
		matched, _ := regexp.MatchString(right, actual)
		return matched
	case "!~":
		matched, _ := regexp.MatchString(right, actual)
		return !matched
	default:
		return false
	}
}

func compareNumber(left string, right string, compare func(float64, float64) bool) bool {
	l, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return false
	}
	r, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return false
	}
	return compare(l, r)
}

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

func resolveValue(r *http.Request, resp *responseRecorder, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return responseVar(r, resp, strings.TrimPrefix(variable, "$"))
	})
}

func responseVar(r *http.Request, resp *responseRecorder, name string) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "status", name == "status_code":
		return strconv.Itoa(resp.statusCode)
	case strings.HasPrefix(name, "sent_http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "sent_http_"), "_", "-")
		return resp.header.Get(header)
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method", name == "request_method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
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
