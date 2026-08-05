package base

import (
	"bytes"
	"context"
	"net/http"
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
	header      http.Header
	body        bytes.Buffer
	bodyPtr     *bytes.Buffer // shared buffer in pipeline mode; nil otherwise
	statusCode  int
	wroteHeader bool
}

func NewBufferedResponseWriter() *BufferedResponseWriter {
	return &BufferedResponseWriter{
		header:     make(http.Header),
		statusCode: http.StatusOK,
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
		return NewBufferedResponseWriter()
	}
	return &BufferedResponseWriter{
		header:     make(http.Header),
		bodyPtr:    &pipeline.buf,
		statusCode: http.StatusOK,
	}
}

func (w *BufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *BufferedResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.statusCode = statusCode
	w.wroteHeader = true
}

func (w *BufferedResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
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

// Commit writes the buffered headers, status and body to dst.
// When dst is another BufferedResponseWriter that shares the same pipeline
// buffer, the body copy is skipped (the buffer is already shared).
func (w *BufferedResponseWriter) Commit(dst http.ResponseWriter) {
	sameBuffer := false
	if dstBrw, ok := dst.(*BufferedResponseWriter); ok {
		sameBuffer = dstBrw.bodyPtr != nil && dstBrw.bodyPtr == w.bodyPtr
	}

	for field, values := range w.header {
		for _, value := range values {
			dst.Header().Add(field, value)
		}
	}
	dst.WriteHeader(w.statusCode)

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
	if brw, ok := dst.(*BufferedResponseWriter); ok && brw.bodyPtr != nil && brw.bodyPtr == w.bodyPtr {
		return
	}
	_, _ = dst.Write(w.Body())
}
