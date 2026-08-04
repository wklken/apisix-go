package base

import (
	"bytes"
	"net/http"
)

// BufferedResponseWriter delays header and body commitment until Commit,
// allowing the response to be inspected, rewritten and replayed. Distinct
// from ResponseRecorder, which forwards writes immediately for observers.
type BufferedResponseWriter struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func NewBufferedResponseWriter() *BufferedResponseWriter {
	return &BufferedResponseWriter{
		header:     make(http.Header),
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
	return w.body.Bytes()
}

// SetBody replaces the buffered body content.
func (w *BufferedResponseWriter) SetBody(body []byte) {
	w.body.Reset()
	_, _ = w.body.Write(body)
}

// Commit writes the buffered headers, status and body to dst.
func (w *BufferedResponseWriter) Commit(dst http.ResponseWriter) {
	for field, values := range w.header {
		for _, value := range values {
			dst.Header().Add(field, value)
		}
	}
	dst.WriteHeader(w.statusCode)
	_, _ = dst.Write(w.body.Bytes())
}
