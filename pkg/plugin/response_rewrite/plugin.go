package response_rewrite

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	brotlidec "github.com/andybalholm/brotli"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
)

type Plugin struct {
	base.BasePlugin
	config Config
	expr   *pluginexpr.Expression
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
      "minProperties": 1,
      "anyOf": [
        {
          "patternProperties": {
            "^[^:]+$": {
              "oneOf": [
                {"type": "string"},
                {"type": "number"}
              ]
            }
          },
          "additionalProperties": false
        },
        {
          "properties": {
            "add": {
              "type": "array",
              "minItems": 1,
              "items": {
                "type": "string",
                "pattern": "^[^:]+:[^:]*[^/]$"
              }
            },
            "set": {
              "type": "object",
              "minProperties": 1,
              "patternProperties": {
                "^[^:]+$": {
                  "oneOf": [
                    {"type": "string"},
                    {"type": "number"}
                  ]
                }
              },
              "additionalProperties": false
            },
            "remove": {
              "type": "array",
              "minItems": 1,
              "items": {
                "type": "string",
                "pattern": "^[^:]+$"
              }
            }
          },
          "additionalProperties": false
        }
      ]
    },
	    "body": {
	      "type": "string"
	    },
	    "body_secret": {
	      "type": "string",
	      "minLength": 1,
	      "description": "Go extension: explicitly opted-in APISIX data-encryption ciphertext"
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
	Headers    Headers  `json:"headers"`
	Body       *string  `json:"body,omitempty"`
	BodySecret *string  `json:"body_secret,omitempty"`
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
	var raw map[string]any
	if err := jsonUnmarshal(data, &raw); err != nil {
		return err
	}
	_, addConfigured := raw["add"].([]any)
	_, setConfigured := raw["set"].(map[string]any)
	_, removeConfigured := raw["remove"].([]any)
	if addConfigured || setConfigured || removeConfigured {
		var err error
		h.Add, err = stringValues(raw["add"], "headers.add")
		if err != nil {
			return err
		}
		h.Set, err = headerValues(raw["set"], "headers.set")
		if err != nil {
			return err
		}
		h.Remove, err = stringValues(raw["remove"], "headers.remove")
		if err != nil {
			return err
		}
		return nil
	}

	legacy, err := headerValues(raw, "headers")
	if err != nil {
		return err
	}
	h.LegacySet = legacy
	return nil
}

func stringValues(value any, name string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	values := make([]string, len(items))
	for i, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s item %d must be a string", name, i)
		}
		values[i] = text
	}
	return values, nil
}

func headerValues(value any, name string) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	values := make(map[string]string, len(items))
	for field, value := range items {
		switch typed := value.(type) {
		case string:
			values[field] = typed
		case float64:
			values[field] = strconv.FormatFloat(typed, 'f', -1, 64)
		default:
			return nil, fmt.Errorf("%s.%s must be a string or number", name, field)
		}
	}
	return values, nil
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
	if p.config.Body != nil && p.config.BodySecret != nil {
		return fmt.Errorf("response-rewrite body and body_secret cannot be configured together")
	}
	if p.config.BodySecret != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body_secret and filters cannot be configured together")
	}
	if p.config.Body != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body and filters cannot be configured together")
	}
	if p.config.BodySecret != nil {
		if *p.config.BodySecret == "" {
			return fmt.Errorf("response-rewrite body_secret must not be empty")
		}
		keyring, enabled := data_encryption.Keyring()
		resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(*p.config.BodySecret)
		if err != nil {
			return fmt.Errorf("response-rewrite body_secret: %w", err)
		}
		p.config.Body = &resolved
	}
	if *p.config.BodyBase64 {
		if p.config.Body == nil || *p.config.Body == "" {
			return fmt.Errorf("response-rewrite body_base64 requires a non-empty body")
		}
		if _, err := base64.StdEncoding.DecodeString(*p.config.Body); err != nil {
			return fmt.Errorf("response-rewrite body is not valid base64: %w", err)
		}
	}
	if len(p.config.Vars) > 0 {
		expr, err := pluginexpr.Compile(p.config.Vars)
		if err != nil {
			return fmt.Errorf("response-rewrite vars validation failed: %w", err)
		}
		p.expr = expr
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

func (p *Plugin) Config() any {
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
		body := resp.body
		canFilter := true
		if resp.header.Get("Content-Encoding") != "" {
			decoded, ok := decodeFilterBody(resp)
			if !ok {
				canFilter = false
			} else {
				body = decoded
				resp.header.Del("Content-Encoding")
			}
		}
		if canFilter {
			for _, filter := range p.config.Filters {
				if filter.Scope == "global" {
					body = []byte(filter.pattern.ReplaceAllString(string(body), filter.Replace))
					continue
				}
				body = []byte(replaceFirstString(filter.pattern, string(body), filter.Replace))
			}
			resp.body = body
			resp.header.Del("Content-Length")
		}
	}

	p.config.Headers.apply(r, resp)
}

func (p *Plugin) varsMatched(r *http.Request, resp *responseRecorder) bool {
	if p.expr == nil {
		return true
	}
	return p.expr.Eval(func(name string) any {
		return responseValue(r, resp, name)
	})
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

func decodeFilterBody(resp *responseRecorder) ([]byte, bool) {
	switch strings.ToLower(strings.TrimSpace(resp.header.Get("Content-Encoding"))) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(resp.body))
		if err != nil {
			return nil, false
		}
		defer reader.Close()
		decoded, err := io.ReadAll(reader)
		if err != nil {
			return nil, false
		}
		return decoded, true
	case "br":
		decoded, err := io.ReadAll(brotlidec.NewReader(bytes.NewReader(resp.body)))
		if err != nil {
			return nil, false
		}
		return decoded, true
	default:
		return nil, false
	}
}

func expressionString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(typed, ",")
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(value)
	}
}

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

func resolveValue(r *http.Request, resp *responseRecorder, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return responseVar(r, resp, strings.TrimPrefix(variable, "$"))
	})
}

func responseVar(r *http.Request, resp *responseRecorder, name string) string {
	return expressionString(responseValue(r, resp, name))
}

func responseValue(r *http.Request, resp *responseRecorder, name string) any {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "status", name == "status_code", name == "upstream_status":
		return resp.statusCode
	case strings.HasPrefix(name, "sent_http_"), strings.HasPrefix(name, "upstream_http_"):
		prefix := "sent_http_"
		if strings.HasPrefix(name, "upstream_http_") {
			prefix = "upstream_http_"
		}
		header := strings.ReplaceAll(strings.TrimPrefix(name, prefix), "_", "-")
		return headerValue(resp.header, header)
	case name == "body_bytes_sent" || name == "bytes_sent":
		return len(resp.body)
	}
	return pluginexpr.RequestValue(r, name)
}

func headerValue(header http.Header, name string) any {
	values := header.Values(name)
	if len(values) == 0 {
		return ""
	}
	if len(values) == 1 {
		return values[0]
	}
	return values
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
