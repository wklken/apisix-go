package response_rewrite

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	brotlidec "github.com/andybalholm/brotli"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixvar "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	expr   *responseExpression
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
	if p.config.Body != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body and filters cannot be configured together")
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
		expr, err := compileResponseExpression(p.config.Vars)
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
	return p.expr.eval(r, resp)
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

type responseExpression struct {
	logic     string
	children  []*responseExpression
	condition *responseCondition
}

type responseCondition struct {
	variable string
	operator string
	right    any
	reverse  bool
	pattern  *regexp.Regexp
	prefixes []netip.Prefix
}

func compileResponseExpression(value any) (*responseExpression, error) {
	rules, ok := value.([]any)
	if !ok || len(rules) == 0 {
		return nil, fmt.Errorf("expression must be a non-empty array")
	}

	if logic, ok := rules[0].(string); ok && isLogicOperator(logic) {
		if len(rules) < 3 {
			return nil, fmt.Errorf("logical expression %s requires at least two operands", logic)
		}
		children := make([]*responseExpression, 0, len(rules)-1)
		for _, child := range rules[1:] {
			compiled, err := compileResponseExpression(child)
			if err != nil {
				return nil, err
			}
			children = append(children, compiled)
		}
		return &responseExpression{logic: strings.ToUpper(logic), children: children}, nil
	}

	if len(rules) == 3 || len(rules) == 4 {
		if _, ok := rules[0].(string); ok {
			return compileResponseCondition(rules)
		}
	}

	if _, ok := rules[0].([]any); !ok {
		return nil, fmt.Errorf("expression must contain conditions or a logical operator")
	}
	current, err := compileResponseExpression(rules[0])
	if err != nil {
		return nil, err
	}
	pending := "AND"
	wantsCondition := false
	for _, item := range rules[1:] {
		if logic, ok := item.(string); ok {
			logic = strings.ToUpper(logic)
			if logic != "AND" && logic != "OR" {
				return nil, fmt.Errorf("invalid infix logical operator %q", logic)
			}
			if wantsCondition {
				return nil, fmt.Errorf("logical operator %q requires a following condition", pending)
			}
			pending = logic
			wantsCondition = true
			continue
		}
		child, err := compileResponseExpression(item)
		if err != nil {
			return nil, err
		}
		current = &responseExpression{logic: pending, children: []*responseExpression{current, child}}
		pending = "AND"
		wantsCondition = false
	}
	if wantsCondition {
		return nil, fmt.Errorf("logical operator %q requires a following condition", pending)
	}
	return current, nil
}

func isLogicOperator(operator string) bool {
	switch strings.ToUpper(operator) {
	case "AND", "OR", "!AND", "!OR":
		return true
	default:
		return false
	}
}

func compileResponseCondition(parts []any) (*responseExpression, error) {
	condition := &responseCondition{variable: fmt.Sprint(parts[0])}
	if len(parts) == 4 {
		if fmt.Sprint(parts[1]) != "!" {
			return nil, fmt.Errorf("invalid negated condition")
		}
		condition.reverse = true
		condition.operator = strings.ToLower(fmt.Sprint(parts[2]))
		condition.right = parts[3]
	} else {
		condition.operator = strings.ToLower(fmt.Sprint(parts[1]))
		condition.right = parts[2]
	}
	switch condition.operator {
	case "!=":
		condition.operator = "~="
	case "~":
		condition.operator = "~~"
	case "!~":
		condition.operator = "~~"
		condition.reverse = !condition.reverse
	}

	switch condition.operator {
	case "==", "~=", ">", ">=", "<", "<=", "has":
	case "in":
		if _, ok := expressionValues(condition.right); !ok {
			return nil, fmt.Errorf("in operator requires an array")
		}
	case "~~", "~*":
		options := ""
		if condition.operator == "~*" {
			options = "(?i)"
		}
		pattern, err := regexp.Compile(options + fmt.Sprint(condition.right))
		if err != nil {
			return nil, fmt.Errorf("invalid expression regex: %w", err)
		}
		condition.pattern = pattern
	case "ipmatch":
		values, ok := expressionValues(condition.right)
		if !ok {
			values = []any{condition.right}
		}
		for _, value := range values {
			prefix, err := parseIPPrefix(fmt.Sprint(value))
			if err != nil {
				return nil, err
			}
			condition.prefixes = append(condition.prefixes, prefix)
		}
	default:
		return nil, fmt.Errorf("invalid operator %q", condition.operator)
	}
	return &responseExpression{condition: condition}, nil
}

func parseIPPrefix(value string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix, nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid ipmatch value %q", value)
	}
	return netip.PrefixFrom(address, address.BitLen()), nil
}

func (e *responseExpression) eval(r *http.Request, resp *responseRecorder) bool {
	if e.condition != nil {
		matched := e.condition.eval(r, resp)
		if e.condition.reverse {
			return !matched
		}
		return matched
	}

	matched := e.logic == "AND" || e.logic == "!AND"
	if e.logic == "OR" || e.logic == "!OR" {
		matched = false
	}
	for _, child := range e.children {
		if e.logic == "AND" || e.logic == "!AND" {
			matched = matched && child.eval(r, resp)
			if !matched {
				break
			}
		} else {
			matched = matched || child.eval(r, resp)
			if matched {
				break
			}
		}
	}
	if e.logic == "!AND" || e.logic == "!OR" {
		return !matched
	}
	return matched
}

func (c *responseCondition) eval(r *http.Request, resp *responseRecorder) bool {
	actual := responseValue(r, resp, c.variable)
	switch c.operator {
	case "==":
		return expressionEqual(actual, c.right)
	case "~=":
		return !expressionEqual(actual, c.right)
	case ">", ">=", "<", "<=":
		left, leftErr := strconv.ParseFloat(expressionString(actual), 64)
		right, rightErr := strconv.ParseFloat(expressionString(c.right), 64)
		if leftErr != nil || rightErr != nil {
			return false
		}
		switch c.operator {
		case ">":
			return left > right
		case ">=":
			return left >= right
		case "<":
			return left < right
		default:
			return left <= right
		}
	case "~~", "~*":
		return c.pattern.MatchString(expressionString(actual))
	case "in":
		values, _ := expressionValues(c.right)
		for _, value := range values {
			if expressionEqual(actual, value) {
				return true
			}
		}
		return false
	case "has":
		values, ok := expressionValues(actual)
		if !ok {
			return false
		}
		for _, value := range values {
			if expressionEqual(value, c.right) {
				return true
			}
		}
		return false
	case "ipmatch":
		address, err := netip.ParseAddr(expressionString(actual))
		if err != nil {
			return false
		}
		for _, prefix := range c.prefixes {
			if prefix.Contains(address) {
				return true
			}
		}
	}
	return false
}

func expressionEqual(left any, right any) bool {
	switch right.(type) {
	case float64, float32, int, int64, json.Number:
		leftNumber, leftErr := strconv.ParseFloat(expressionString(left), 64)
		rightNumber, rightErr := strconv.ParseFloat(expressionString(right), 64)
		return leftErr == nil && rightErr == nil && leftNumber == rightNumber
	default:
		return expressionString(left) == expressionString(right)
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

func expressionValues(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []string:
		values := make([]any, len(typed))
		for i, value := range typed {
			values[i] = value
		}
		return values, true
	default:
		return nil, false
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
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "query_string" || name == "args":
		return r.URL.RawQuery
	case name == "is_args":
		if r.URL.RawQuery != "" {
			return "?"
		}
		return ""
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
		if value := apisixctx.GetString(r.Context(), "remote_addr"); value != "" {
			return value
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case name == "remote_port":
		if value := apisixctx.GetString(r.Context(), "remote_port"); value != "" {
			return value
		}
		_, port, _ := net.SplitHostPort(r.RemoteAddr)
		return port
	case name == "body_bytes_sent" || name == "bytes_sent":
		return len(resp.body)
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "cookie_"):
		cookie, err := r.Cookie(strings.TrimPrefix(name, "cookie_"))
		if err == nil {
			return cookie.Value
		}
		return ""
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return headerValue(r.Header, header)
	}

	key := "$" + name
	if value := apisixvar.GetNginxVar(r, key); value != "" {
		return value
	}
	if value := apisixctx.GetApisixVar(r, key); value != nil {
		return value
	}
	if value := apisixctx.GetRequestVar(r, key); value != nil {
		return value
	}
	return ""
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
