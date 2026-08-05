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

type sharedRequestBodyContextKey struct{}

type sharedRequestBodyCapture struct {
	body []byte
	err  error
}

// ReadSharedRequestBody returns the current request body up to limit bytes.
// The first logger captures and restores r.Body, then adjacent logger plugins
// reuse that capture. This cache is separate from the request-variable body
// cache because higher-priority plugins may rewrite r.Body after validation.
func ReadSharedRequestBody(r *http.Request, limit int) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	capture, _ := r.Context().Value(sharedRequestBodyContextKey{}).(*sharedRequestBodyCapture)
	if capture == nil {
		body, err := ReadRequestBody(r)
		capture = &sharedRequestBodyCapture{body: body, err: err}
		*r = *r.WithContext(context.WithValue(r.Context(), sharedRequestBodyContextKey{}, capture))
	}
	if capture.err != nil {
		return "", capture.err
	}
	body := capture.body
	if len(body) == 0 {
		return "", nil
	}
	if limit > 0 && limit < len(body) {
		body = body[:limit]
	}
	return string(body), nil
}

type sharedResponseCapture struct {
	buf      bytes.Buffer
	status   int
	maxBytes int
}

// SharedResponseRecorder captures response body once per request and is shared
// across multiple logger plugins to avoid O(logger × body) buffer duplication.
type SharedResponseRecorder struct {
	http.ResponseWriter
	capture     *sharedResponseCapture
	forwardOnly bool
}

// NewSharedResponseRecorder creates a new shared response recorder wrapping w.
func NewSharedResponseRecorder(w http.ResponseWriter) *SharedResponseRecorder {
	return &SharedResponseRecorder{
		ResponseWriter: w,
		capture:        &sharedResponseCapture{},
	}
}

func (w *SharedResponseRecorder) WriteHeader(status int) {
	if !w.forwardOnly {
		w.sharedCapture().status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *SharedResponseRecorder) Write(body []byte) (int, error) {
	if !w.forwardOnly {
		capture := w.sharedCapture()
		if capture.status == 0 {
			capture.status = http.StatusOK
		}
		capturedBody := body
		if capture.maxBytes > 0 {
			remaining := capture.maxBytes - capture.buf.Len()
			if remaining <= 0 {
				capturedBody = nil
			} else if len(capturedBody) > remaining {
				capturedBody = capturedBody[:remaining]
			}
		}
		_, _ = capture.buf.Write(capturedBody)
	}
	return w.ResponseWriter.Write(body)
}

func (w *SharedResponseRecorder) BodyBytes() []byte {
	return w.sharedCapture().buf.Bytes()
}

// Body returns the full captured response body as a string.
func (w *SharedResponseRecorder) Body() string {
	return w.sharedCapture().buf.String()
}

// BodyTruncated returns the response body as a string, truncated to limit bytes.
func (w *SharedResponseRecorder) BodyTruncated(limit int) string {
	b := w.sharedCapture().buf.Bytes()
	if limit > 0 && len(b) > limit {
		b = b[:limit]
	}
	return string(b)
}

func (w *SharedResponseRecorder) StatusCode() int {
	return w.sharedCapture().status
}

func (w *SharedResponseRecorder) HasBody() bool {
	return w.sharedCapture().buf.Len() > 0
}

func (w *SharedResponseRecorder) sharedCapture() *sharedResponseCapture {
	if w.capture == nil {
		w.capture = &sharedResponseCapture{}
	}
	return w.capture
}

type sharedResponseRecorderContextKey struct{}

func GetOrCreateSharedResponseRecorderWithLimit(
	w http.ResponseWriter,
	r *http.Request,
	limit int,
) *SharedResponseRecorder {
	if existing, _ := r.Context().Value(sharedResponseRecorderContextKey{}).(*SharedResponseRecorder); existing != nil {
		existingCapture := existing.sharedCapture()
		if w == existing {
			updateSharedResponseCaptureLimit(existingCapture, limit)
			return existing
		}
		if responseWriterWrapsSharedCapture(w, existingCapture) {
			updateSharedResponseCaptureLimit(existingCapture, limit)
			return &SharedResponseRecorder{
				ResponseWriter: w,
				capture:        existingCapture,
				forwardOnly:    true,
			}
		}
		recorder := NewSharedResponseRecorder(w)
		recorder.capture.maxBytes = limit
		return recorder
	}
	recorder := NewSharedResponseRecorder(w)
	recorder.capture.maxBytes = limit
	*r = *r.WithContext(context.WithValue(r.Context(), sharedResponseRecorderContextKey{}, recorder))
	return recorder
}

func updateSharedResponseCaptureLimit(capture *sharedResponseCapture, limit int) {
	if limit <= 0 || capture.maxBytes <= 0 {
		capture.maxBytes = 0
	} else if limit > capture.maxBytes {
		capture.maxBytes = limit
	}
}

func responseWriterWrapsSharedCapture(w http.ResponseWriter, capture *sharedResponseCapture) bool {
	for w != nil {
		if recorder, ok := w.(*SharedResponseRecorder); ok {
			return recorder.sharedCapture() == capture
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		next := unwrapper.Unwrap()
		if next == w {
			return false
		}
		w = next
	}
	return false
}
