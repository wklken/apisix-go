package base

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

// ResponseRecorder forwards responses while retaining a bounded response body
// and the status code for logger plugins.
type ResponseRecorder struct {
	http.ResponseWriter
	body   bytes.Buffer
	limit  int
	status int
}

func NewResponseRecorder(w http.ResponseWriter, limit int) *ResponseRecorder {
	return &ResponseRecorder{
		ResponseWriter: w,
		limit:          limit,
	}
}

func (w *ResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *ResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *ResponseRecorder) capture(body []byte) {
	if w.limit <= 0 || w.body.Len() >= w.limit {
		return
	}
	remaining := w.limit - w.body.Len()
	if len(body) > remaining {
		body = body[:remaining]
	}
	_, _ = w.body.Write(body)
}

func (w *ResponseRecorder) Body() string {
	return w.body.String()
}

func (w *ResponseRecorder) HasBody() bool {
	return w.body.Len() > 0
}

func (w *ResponseRecorder) StatusCode() int {
	return w.status
}

func ReadAndRestoreRequestBody(r *http.Request, limit int) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	return string(body), nil
}

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

	pendingOp := "AND"
	hasResult := false
	result := true
	for _, condition := range conditions {
		if op, ok := condition.(string); ok {
			switch strings.ToUpper(op) {
			case "AND", "OR":
				pendingOp = strings.ToUpper(op)
			default:
				return false
			}
			continue
		}
		if nested {
			if parts, ok := condition.([]any); ok && len(parts) == 1 {
				if op, ok := parts[0].(string); ok {
					switch strings.ToUpper(op) {
					case "AND", "OR":
						pendingOp = strings.ToUpper(op)
					default:
						return false
					}
					continue
				}
			}
		}

		matched := matchCondition(r, condition, status)
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

	left := fmt.Sprint(parts[0])
	op := fmt.Sprint(parts[1])
	right := fmt.Sprint(parts[2])
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

// ReadSharedRequestBody returns the request body up to limit bytes, using the
// already-cached body from ctx.ReadRequestBody to avoid repeated io.ReadAll
// calls across multiple logger plugins in a single request.
func ReadSharedRequestBody(r *http.Request, limit int) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := apisixctx.ReadRequestBody(r)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		return "", nil
	}
	if limit > 0 && limit < len(body) {
		body = body[:limit]
	}
	return string(body), nil
}

// SharedResponseRecorder captures response body once per request and is shared
// across multiple logger plugins to avoid O(logger × body) buffer duplication.
type SharedResponseRecorder struct {
	http.ResponseWriter
	buf      bytes.Buffer
	status   int
	maxBytes int
}

// NewSharedResponseRecorder creates a new shared response recorder wrapping w.
func NewSharedResponseRecorder(w http.ResponseWriter) *SharedResponseRecorder {
	return &SharedResponseRecorder{ResponseWriter: w}
}

func (w *SharedResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *SharedResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	capture := body
	if w.maxBytes > 0 {
		remaining := w.maxBytes - w.buf.Len()
		if remaining <= 0 {
			capture = nil
		} else if len(capture) > remaining {
			capture = capture[:remaining]
		}
	}
	_, _ = w.buf.Write(capture)
	return w.ResponseWriter.Write(body)
}

func (w *SharedResponseRecorder) BodyBytes() []byte {
	return w.buf.Bytes()
}

// Body returns the full captured response body as a string.
func (w *SharedResponseRecorder) Body() string {
	return string(w.buf.Bytes())
}

// BodyTruncated returns the response body as a string, truncated to limit bytes.
func (w *SharedResponseRecorder) BodyTruncated(limit int) string {
	b := w.buf.Bytes()
	if limit > 0 && len(b) > limit {
		b = b[:limit]
	}
	return string(b)
}

func (w *SharedResponseRecorder) StatusCode() int {
	return w.status
}

func (w *SharedResponseRecorder) HasBody() bool {
	return w.buf.Len() > 0
}

type sharedResponseRecorderContextKey struct{}

// GetOrCreateSharedResponseRecorder returns the current shared recorder for
// adjacent logger middleware. If another response middleware sits between two
// loggers, a new recorder is required so that the current writer is never
// bypassed and each logger preserves its response-view semantics.
func GetOrCreateSharedResponseRecorder(w http.ResponseWriter, r *http.Request) *SharedResponseRecorder {
	return GetOrCreateSharedResponseRecorderWithLimit(w, r, 0)
}

func GetOrCreateSharedResponseRecorderWithLimit(
	w http.ResponseWriter,
	r *http.Request,
	limit int,
) *SharedResponseRecorder {
	if existing, _ := r.Context().Value(sharedResponseRecorderContextKey{}).(*SharedResponseRecorder); existing != nil {
		if w == existing {
			if limit <= 0 || existing.maxBytes <= 0 {
				existing.maxBytes = 0
			} else if limit > existing.maxBytes {
				existing.maxBytes = limit
			}
			return existing
		}
		recorder := NewSharedResponseRecorder(w)
		recorder.maxBytes = limit
		return recorder
	}
	recorder := NewSharedResponseRecorder(w)
	recorder.maxBytes = limit
	*r = *r.WithContext(context.WithValue(r.Context(), sharedResponseRecorderContextKey{}, recorder))
	return recorder
}
