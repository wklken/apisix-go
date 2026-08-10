package base

import (
	"bytes"
	"context"
	"net/http"
	"strings"
)

// pipelineBuffer holds the shared bytes.Buffer for a transform pipeline.
// Multiple BufferedResponseWriters can share the same pipelineBuffer so that
// the response body is stored only once when multiple transform plugins are
// present in the handler chain.
type transformPipelineContextKey struct{}

type pipelineBuffer struct {
	buf   bytes.Buffer
	count int
}

type informationalResponse struct {
	status int
	header http.Header
}

// WithTransformPipeline marks a handler chain containing response-transform
// plugins. Chains with zero or one transform preserve standalone buffering;
// chains with multiple transforms share one response body buffer.
func WithTransformPipeline(count int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if count < 2 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if existing, _ := r.Context().Value(transformPipelineContextKey{}).(*pipelineBuffer); existing != nil {
				existing.count += count
				defer func() { existing.count -= count }()
				next.ServeHTTP(w, r)
				return
			}
			pipeline := &pipelineBuffer{count: count}
			*r = *r.WithContext(context.WithValue(r.Context(), transformPipelineContextKey{}, pipeline))
			next.ServeHTTP(w, r)
		})
	}
}

// BufferedResponseWriter delays header and body commitment until Commit,
// allowing the response to be inspected, rewritten and replayed. Distinct
// from ResponseRecorder, which forwards writes immediately for observers.
//
// In pipeline mode (bodyPtr != nil) multiple BufferedResponseWriter instances
// share the same underlying bytes.Buffer; Commit detects this and skips the
// body copy when committing between pipeline writers.
type BufferedResponseWriter struct {
	header         http.Header
	body           bytes.Buffer
	bodyPtr        *bytes.Buffer // shared buffer in pipeline mode; nil otherwise
	statusCode     int
	wroteHeader    bool
	requestMethod  string
	informationals []informationalResponse
}

func NewBufferedResponseWriter() *BufferedResponseWriter {
	return &BufferedResponseWriter{
		header:        make(http.Header),
		statusCode:    http.StatusOK,
		requestMethod: "",
	}
}

// GetOrCreateTransformResponseWriter returns a BufferedResponseWriter for
// transform plugins. When multiple transform plugins are present in the
// handler chain, all returned writers share a single underlying bytes.Buffer
// via a pipelineBuffer stored in request context. This eliminates O(N) copies
// of the response body.
//
// Each writer still has its own header map and status code; Commit copies
// headers between writers and only the final commit writes to the real
// http.ResponseWriter.
func GetOrCreateTransformResponseWriter(r *http.Request) *BufferedResponseWriter {
	pipeline, _ := r.Context().Value(transformPipelineContextKey{}).(*pipelineBuffer)
	if pipeline == nil || pipeline.count < 2 {
		writer := NewBufferedResponseWriter()
		writer.requestMethod = r.Method
		return writer
	}
	return &BufferedResponseWriter{
		header:        make(http.Header),
		bodyPtr:       &pipeline.buf,
		statusCode:    http.StatusOK,
		requestMethod: r.Method,
	}
}

func (w *BufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *BufferedResponseWriter) WriteHeader(statusCode int) {
	if statusCode >= 100 && statusCode <= 199 && statusCode != http.StatusSwitchingProtocols {
		if w.wroteHeader {
			return
		}
		w.informationals = append(w.informationals, informationalResponse{
			status: statusCode,
			header: w.header.Clone(),
		})
		return
	}
	if w.wroteHeader {
		return
	}
	w.statusCode = statusCode
	w.wroteHeader = true
	if !ResponseAllowsBody(w.requestMethod, statusCode) {
		w.discardBodyForStatus()
	}
}

func (w *BufferedResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !ResponseAllowsBody(w.requestMethod, w.statusCode) {
		if strings.EqualFold(w.requestMethod, http.MethodHead) {
			return len(body), nil
		}
		return 0, http.ErrBodyNotAllowed
	}
	if w.bodyPtr != nil {
		return w.bodyPtr.Write(body)
	}
	return w.body.Write(body)
}

func (w *BufferedResponseWriter) StatusCode() int {
	return w.statusCode
}

// SetStatusCode overrides the buffered status, used by response-transform
// plugins that rewrite the status after capture.
func (w *BufferedResponseWriter) SetStatusCode(statusCode int) {
	w.statusCode = statusCode
	if !ResponseAllowsBody(w.requestMethod, statusCode) {
		w.discardBodyForStatus()
	}
}

func (w *BufferedResponseWriter) Body() []byte {
	if w.bodyPtr != nil {
		return w.bodyPtr.Bytes()
	}
	return w.body.Bytes()
}

// Reset discards all buffered response state so an error response can replace
// a failed transformation cleanly, including in a shared transform pipeline.
func (w *BufferedResponseWriter) Reset() {
	clear(w.header)
	if w.bodyPtr != nil {
		w.bodyPtr.Reset()
	} else {
		w.body.Reset()
	}
	w.statusCode = http.StatusOK
	w.wroteHeader = false
	w.informationals = nil
}

// SetBody replaces the buffered body content.
func (w *BufferedResponseWriter) SetBody(body []byte) {
	if w.bodyPtr != nil {
		w.bodyPtr.Reset()
		_, _ = w.bodyPtr.Write(body)
		return
	}
	w.body.Reset()
	_, _ = w.body.Write(body)
}

// ReplaceBody replaces the buffered body and invalidates metadata derived from
// the previous representation. SetBody intentionally remains a raw operation
// for callers that already own representation metadata.
func (w *BufferedResponseWriter) ReplaceBody(body []byte) {
	w.SetBody(body)
	InvalidateBodyDerivedHeaders(w.header)
}

// Commit writes the buffered headers, status and body to dst.
// When dst is another BufferedResponseWriter that shares the same pipeline
// buffer, the body copy is skipped (the buffer is already shared).
func (w *BufferedResponseWriter) Commit(dst http.ResponseWriter) {
	w.discardBodyForStatus()
	sameBuffer := false
	if dstBrw, ok := dst.(*BufferedResponseWriter); ok {
		sameBuffer = dstBrw.bodyPtr != nil && dstBrw.bodyPtr == w.bodyPtr
		for _, informational := range w.informationals {
			dstBrw.informationals = append(dstBrw.informationals, informationalResponse{
				status: informational.status,
				header: informational.header.Clone(),
			})
		}
	}

	for field, values := range w.header {
		for _, value := range values {
			dst.Header().Add(field, value)
		}
	}
	if dstBrw, ok := dst.(*BufferedResponseWriter); ok {
		dstBrw.WriteHeader(w.statusCode)
	} else {
		w.commitToResponseWriter(dst)
		return
	}

	if !sameBuffer {
		_, _ = dst.Write(w.Body())
	}
}

// WriteBodyTo writes the buffered body to dst. When dst is another
// BufferedResponseWriter that shares the same pipeline buffer, the
// body write is a no-op (the data is already shared).
//
// This is useful for plugins that write headers/status/body manually
// instead of using Commit.
func (w *BufferedResponseWriter) WriteBodyTo(dst http.ResponseWriter) {
	if !ResponseAllowsBody(w.requestMethod, w.statusCode) {
		return
	}
	if brw, ok := dst.(*BufferedResponseWriter); ok && brw.bodyPtr != nil && brw.bodyPtr == w.bodyPtr {
		return
	}
	_, _ = dst.Write(w.Body())
}

func (w *BufferedResponseWriter) discardBodyForStatus() {
	if ResponseAllowsBody(w.requestMethod, w.statusCode) {
		return
	}
	if w.bodyPtr != nil {
		w.bodyPtr.Reset()
	} else {
		w.body.Reset()
	}
	for actual := range w.header {
		if equalHeaderName(actual, "Content-Length") {
			delete(w.header, actual)
		}
	}
}

func (w *BufferedResponseWriter) commitToResponseWriter(dst http.ResponseWriter) {
	finalHeader := dst.Header().Clone()
	for _, informational := range w.informationals {
		replaceHeader(dst.Header(), informational.header)
		dst.WriteHeader(informational.status)
		replaceHeader(dst.Header(), finalHeader)
	}
	dst.WriteHeader(w.statusCode)
	if ResponseAllowsBody(w.requestMethod, w.statusCode) {
		_, _ = dst.Write(w.Body())
	}
}

func replaceHeader(dst, src http.Header) {
	for field := range dst {
		delete(dst, field)
	}
	for field, values := range src {
		dst[field] = append([]string(nil), values...)
	}
}

func equalHeaderName(actual, wanted string) bool {
	return strings.EqualFold(actual, wanted)
}
