package base

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

// exprRegexpIndex maps expression pattern strings to their compiled form.
// Patterns are compiled once at plugin initialization; request-time evaluation
// only reads the index.
type exprRegexpIndex struct {
	mu sync.RWMutex
	re map[string]*regexp.Regexp
}

func (idx *exprRegexpIndex) Store(pattern string, compiled *regexp.Regexp) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.re == nil {
		idx.re = make(map[string]*regexp.Regexp)
	}
	idx.re[pattern] = compiled
}

func (idx *exprRegexpIndex) Load(pattern string) (*regexp.Regexp, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	compiled, ok := idx.re[pattern]
	return compiled, ok
}

var preparedExprRegexps exprRegexpIndex

func NestedLogMap(fields map[string]any, key string) map[string]any {
	if value, ok := fields[key].(map[string]any); ok {
		return value
	}
	value := map[string]any{}
	fields[key] = value
	return value
}

func ExprMatched(r *http.Request, expressions any, status int) bool {
	conditions, nested, ok := expressionConditions(expressions)
	if !ok {
		return false
	}
	if len(conditions) == 0 {
		return true
	}

	result := false
	hasOperand := false
	pendingOp := "AND"
	for _, condition := range conditions {
		op, isOperator, valid := expressionOperator(condition, nested)
		if !valid {
			return false
		}
		if isOperator {
			pendingOp = op
			continue
		}
		matched := matchCondition(r, condition, status)
		if !hasOperand {
			result = matched
			hasOperand = true
			continue
		}
		if pendingOp == "OR" {
			result = result || matched
		} else {
			result = result && matched
		}
		pendingOp = "AND"
	}
	return hasOperand && result
}

// expressionOperator classifies condition as an AND/OR operator, returning
// the upper-cased operator when recognized. An unrecognized operator string
// marks the whole expression invalid; other values are ordinary conditions.
func expressionOperator(condition any, nested bool) (op string, isOperator, valid bool) {
	if text, ok := condition.(string); ok {
		switch strings.ToUpper(text) {
		case "AND", "OR":
			return strings.ToUpper(text), true, true
		default:
			return "", false, false
		}
	}
	if nested {
		if parts, ok := condition.([]any); ok && len(parts) == 1 {
			if text, ok := parts[0].(string); ok {
				switch strings.ToUpper(text) {
				case "AND", "OR":
					return strings.ToUpper(text), true, true
				default:
					return "", false, false
				}
			}
		}
	}
	return "", false, true
}

// PrepareExprRegexps compiles configured logger expression patterns before
// they enter the request path. An invalid pattern fails plugin initialization
// so a malformed expression never reaches request handling.
func PrepareExprRegexps(expressionSets ...any) error {
	for _, expressions := range expressionSets {
		conditions, _, ok := expressionConditions(expressions)
		if !ok {
			continue
		}
		for _, condition := range conditions {
			parts, ok := condition.([]any)
			if !ok || len(parts) != 3 {
				continue
			}
			op := exprOperandString(parts[1])
			if op != "~" && op != "!~" {
				continue
			}
			pattern := exprOperandString(parts[2])
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("invalid expression pattern %q: %w", pattern, err)
			}
			preparedExprRegexps.Store(pattern, compiled)
		}
	}
	return nil
}

func expressionConditions(expressions any) ([]any, bool, bool) {
	switch value := expressions.(type) {
	case nil:
		return nil, false, true
	case []any:
		return value, false, true
	case [][]any:
		conditions := make([]any, len(value))
		for i, condition := range value {
			conditions[i] = condition
		}
		return conditions, true, true
	default:
		return nil, false, false
	}
}

func matchCondition(r *http.Request, condition any, status int) bool {
	parts, ok := condition.([]any)
	if !ok || len(parts) != 3 {
		return false
	}

	left := exprOperandString(parts[0])
	op := exprOperandString(parts[1])
	right := exprOperandString(parts[2])
	actual := RequestVar(r, left, status)

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
		pattern, ok := preparedExprRegexps.Load(right)
		return ok && pattern.MatchString(actual)
	case "!~":
		pattern, ok := preparedExprRegexps.Load(right)
		return !ok || !pattern.MatchString(actual)
	default:
		return false
	}
}

func exprOperandString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
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

// requestHeaderValue returns the first value of header name without
// allocating: the name is canonicalized into a stack buffer mirroring
// textproto.CanonicalMIMEHeaderKey, so expression evaluation never boxes or
// copies header names. Names longer than 127 bytes fall back to the standard
// library path.
func requestHeaderValue(header http.Header, name string) string {
	if name == "" || len(name) > 127 {
		return header.Get(name)
	}
	buf := [128]byte{}
	copy(buf[:], name)
	canonicalHeaderKey(buf[:len(name)])
	values, ok := header[unsafe.String(&buf[0], len(name))]
	if !ok || len(values) == 0 {
		return ""
	}
	return values[0]
}

// canonicalHeaderKey canonicalizes name in place, mirroring
// textproto.CanonicalMIMEHeaderKey: the first letter and the letter after each
// dash are uppercased and other letters lowercased, with underscores folded
// into dashes (the APISIX http_ variable convention); names containing bytes
// that must not be canonicalized are left unchanged.
func canonicalHeaderKey(name []byte) {
	upper := true
	for i, c := range name {
		if !validHeaderFieldByte(c) {
			return
		}
		if c == '_' {
			name[i] = '-'
			upper = true
			continue
		}
		if upper && 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		} else if !upper && 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		name[i] = c
		upper = c == '-'
	}
}

// validHeaderFieldByte mirrors the RFC 7230 token characters accepted by
// textproto.CanonicalMIMEHeaderKey.
func validHeaderFieldByte(c byte) bool {
	if '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' {
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

func RequestVar(r *http.Request, name string, status int) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "status", name == "status_code":
		if status > 0 {
			return strconv.Itoa(status)
		}
		return fmt.Sprint(apisixctx.GetRequestVar(r, "$status"))
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return apisixlog.RedactedRequestURI(r)
	case name == "args", name == "query_string":
		return apisixlog.RedactedRequestQueryString(r)
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
		return apisixctx.EffectiveRemoteIP(r)
	case strings.HasPrefix(name, "arg_"):
		return fmt.Sprint(apisixlog.GetField(r, "$"+name))
	case strings.HasPrefix(name, "http_"):
		header := strings.TrimPrefix(name, "http_")
		return requestHeaderValue(r.Header, header)
	default:
		key := "$" + name
		if value, ok := apisixctx.GetApisixVars(r)[key]; ok {
			return fmt.Sprint(value)
		}
		if value, ok := apisixctx.GetRequestVars(r)[key]; ok {
			return fmt.Sprint(value)
		}
		return ""
	}
}
