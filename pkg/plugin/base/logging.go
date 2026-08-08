package base

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	brotli "github.com/andybalholm/brotli"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

var preparedExprRegexps sync.Map

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

// PrepareExprRegexps compiles configured logger expression patterns before
// they enter the request path. Invalid patterns retain the existing no-match
// behavior.
func PrepareExprRegexps(expressionSets ...any) {
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
			if err == nil {
				preparedExprRegexps.Store(pattern, compiled)
			}
		}
	}
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
		return ok && pattern.(*regexp.Regexp).MatchString(actual)
	case "!~":
		pattern, ok := preparedExprRegexps.Load(right)
		return !ok || !pattern.(*regexp.Regexp).MatchString(actual)
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
	body        []byte
	err         error
	bodyText    string
	bodyTextLen int
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
	return capture.bodyString(limit), nil
}

type sharedResponseCapture struct {
	buf             bytes.Buffer
	status          int
	maxBytes        int
	bodyText        string
	bodyTextLen     int
	decodedBody     string
	decodedEncoding string
	decodedReady    bool
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
		if len(capturedBody) > 0 {
			_, _ = capture.buf.Write(capturedBody)
			capture.bodyText = ""
			capture.bodyTextLen = 0
			capture.decodedBody = ""
			capture.decodedEncoding = ""
			capture.decodedReady = false
		}
	}
	return w.ResponseWriter.Write(body)
}

func (w *SharedResponseRecorder) BodyBytes() []byte {
	return w.sharedCapture().buf.Bytes()
}

// Body returns the full captured response body as a string.
func (w *SharedResponseRecorder) Body() string {
	return w.sharedCapture().bodyString(0)
}

// BodyTruncated returns the response body as a string, truncated to limit bytes.
func (w *SharedResponseRecorder) BodyTruncated(limit int) string {
	return w.sharedCapture().bodyString(limit)
}

// BodyDecoded returns the captured response body after decoding gzip or Brotli.
// The decoded representation is cached per response so adjacent logger plugins
// do not repeat the decompression or byte-to-string conversion.
func (w *SharedResponseRecorder) BodyDecoded(limit int, encoding string) string {
	capture := w.sharedCapture()
	encoding = strings.ToLower(strings.TrimSpace(encoding))
	if encoding == "" || encoding == "identity" {
		return capture.bodyString(limit)
	}
	if !capture.decodedReady || capture.decodedEncoding != encoding {
		capture.decodedBody = decodeResponseBody(capture.buf.Bytes(), encoding)
		capture.decodedEncoding = encoding
		capture.decodedReady = true
	}
	return truncateString(capture.decodedBody, limit)
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

func (capture *sharedRequestBodyCapture) bodyString(limit int) string {
	requested := len(capture.body)
	if limit > 0 && limit < requested {
		requested = limit
	}
	if capture.bodyTextLen < requested {
		capture.bodyText = string(capture.body[:requested])
		capture.bodyTextLen = requested
	}
	return capture.bodyText[:requested]
}

func (capture *sharedResponseCapture) bodyString(limit int) string {
	body := capture.buf.Bytes()
	requested := len(body)
	if limit > 0 && limit < requested {
		requested = limit
	}
	if capture.bodyTextLen < requested {
		capture.bodyText = string(body[:requested])
		capture.bodyTextLen = requested
	}
	return capture.bodyText[:requested]
}

func truncateString(value string, limit int) string {
	if limit > 0 && len(value) > limit {
		return value[:limit]
	}
	return value
}

func decodeResponseBody(body []byte, encoding string) string {
	var reader io.Reader
	switch {
	case strings.Contains(encoding, "gzip"):
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return string(body)
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	case encoding == "br":
		reader = brotli.NewReader(bytes.NewReader(body))
	default:
		return string(body)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return string(body)
	}
	return string(decoded)
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
