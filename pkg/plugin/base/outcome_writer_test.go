package base

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type optionalResponseWriter struct {
	header      http.Header
	status      int
	body        strings.Builder
	flushErr    error
	hijackConn  net.Conn
	hijackErr   error
	closeNotify chan bool
}

type minimalResponseWriter struct {
	header http.Header
}

type shortResponseWriter struct {
	header http.Header
	body   strings.Builder
	limit  int
}

func (w *minimalResponseWriter) Header() http.Header            { return w.header }
func (w *minimalResponseWriter) WriteHeader(int)                {}
func (w *minimalResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

func (w *shortResponseWriter) Header() http.Header { return w.header }
func (w *shortResponseWriter) WriteHeader(int)     {}
func (w *shortResponseWriter) Write(body []byte) (int, error) {
	n := min(w.limit, len(body))
	_, _ = w.body.Write(body[:n])
	return n, io.ErrShortWrite
}

func (w *shortResponseWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func (w *shortResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	body, err := io.ReadAll(reader)
	if err != nil {
		return 0, err
	}
	n := min(w.limit, len(body))
	_, _ = w.body.Write(body[:n])
	return int64(n), io.ErrShortWrite
}

func newOptionalResponseWriter() *optionalResponseWriter {
	return &optionalResponseWriter{header: make(http.Header), closeNotify: make(chan bool)}
}

func (w *optionalResponseWriter) Header() http.Header { return w.header }
func (w *optionalResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *optionalResponseWriter) Write(body []byte) (int, error) { return w.body.Write(body) }
func (w *optionalResponseWriter) Flush()                         {}
func (w *optionalResponseWriter) FlushError() error              { return w.flushErr }
func (w *optionalResponseWriter) CloseNotify() <-chan bool       { return w.closeNotify }
func (w *optionalResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.hijackConn, nil, w.hijackErr
}

func (w *optionalResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(&w.body, reader)
}

func (w *optionalResponseWriter) SetReadDeadline(time.Time) error  { return nil }
func (w *optionalResponseWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *optionalResponseWriter) EnableFullDuplex() error          { return nil }
func (w *optionalResponseWriter) Push(string, *http.PushOptions) error {
	return nil
}

func (w *optionalResponseWriter) WriteString(value string) (int, error) {
	return w.body.WriteString(value)
}

type closeCountingConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func TestCaptureResponseOutcomePreservesOptionalInterfaces(t *testing.T) {
	minimal := &minimalResponseWriter{header: make(http.Header)}
	wrappedMinimal, _, _ := CaptureResponseOutcome(minimal)
	assertOptionalInterfaces(t, minimal, wrappedMinimal)

	all := newOptionalResponseWriter()
	wrappedAll, _, _ := CaptureResponseOutcome(all)
	assertOptionalInterfaces(t, all, wrappedAll)
	if httpsnoop.Unwrap(wrappedAll) != all {
		t.Fatal("httpsnoop.Unwrap(wrapped) did not return original writer")
	}
}

type panicResponseWriter struct {
	*optionalResponseWriter
	panicWrite bool
	panicFlush bool
}

func (w *panicResponseWriter) Write([]byte) (int, error) {
	if w.panicWrite {
		panic("write panic")
	}
	return 0, nil
}

func (w *panicResponseWriter) Flush() {
	if w.panicFlush {
		panic("flush panic")
	}
}

func TestCaptureResponseOutcomeCommitsBeforeUnderlyingPanic(t *testing.T) {
	tests := []struct {
		name        string
		call        func(http.ResponseWriter)
		new         func() *panicResponseWriter
		wantFlushed bool
	}{
		{
			name: "write",
			call: func(w http.ResponseWriter) { _, _ = w.Write([]byte("x")) },
			new: func() *panicResponseWriter {
				return &panicResponseWriter{optionalResponseWriter: newOptionalResponseWriter(), panicWrite: true}
			},
		},
		{
			name: "flush",
			call: func(w http.ResponseWriter) { w.(http.Flusher).Flush() },
			new: func() *panicResponseWriter {
				return &panicResponseWriter{optionalResponseWriter: newOptionalResponseWriter(), panicFlush: true}
			},
			wantFlushed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wrapped, snapshot, _ := CaptureResponseOutcome(test.new())
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("underlying call did not panic")
					}
				}()
				test.call(wrapped)
			}()
			got := snapshot()
			if !got.Committed || got.Flushed != test.wantFlushed {
				t.Fatalf("snapshot after panic = %#v", got)
			}
		})
	}
}

func assertOptionalInterfaces(t *testing.T, original, wrapped http.ResponseWriter) {
	t.Helper()
	checks := []struct {
		name string
		has  func(http.ResponseWriter) bool
	}{
		{"Flusher", func(w http.ResponseWriter) bool { _, ok := w.(http.Flusher); return ok }},
		{"FlushError", func(w http.ResponseWriter) bool { _, ok := w.(interface{ FlushError() error }); return ok }},
		{"CloseNotifier", func(w http.ResponseWriter) bool {
			_, ok := w.(interface{ CloseNotify() <-chan bool })
			return ok
		}},
		{"Hijacker", func(w http.ResponseWriter) bool { _, ok := w.(http.Hijacker); return ok }},
		{"ReaderFrom", func(w http.ResponseWriter) bool { _, ok := w.(io.ReaderFrom); return ok }},
		{"Pusher", func(w http.ResponseWriter) bool { _, ok := w.(http.Pusher); return ok }},
		{"StringWriter", func(w http.ResponseWriter) bool { _, ok := w.(io.StringWriter); return ok }},
		{"deadliner", func(w http.ResponseWriter) bool {
			_, ok := w.(interface {
				SetReadDeadline(time.Time) error
				SetWriteDeadline(time.Time) error
			})
			return ok
		}},
		{"fullDuplex", func(w http.ResponseWriter) bool {
			_, ok := w.(interface{ EnableFullDuplex() error })
			return ok
		}},
	}
	for _, check := range checks {
		if check.has(original) != check.has(wrapped) {
			t.Errorf("%s parity: original=%v wrapped=%v", check.name, check.has(original), check.has(wrapped))
		}
	}
}

func TestCaptureResponseOutcomeTracksInformationalAndFinalStatus(t *testing.T) {
	wrapped, snapshot, _ := CaptureResponseOutcome(httptest.NewRecorder())
	wrapped.WriteHeader(http.StatusEarlyHints)
	if got := snapshot(); got.Committed || got.Status != http.StatusOK {
		t.Fatalf("after 103: %#v", got)
	}
	wrapped.WriteHeader(http.StatusCreated)
	if got := snapshot(); !got.Committed || got.Status != http.StatusCreated {
		t.Fatalf("after final status: %#v", got)
	}
}

func TestCaptureResponseOutcomeDefaultsCompletedNoWriteToOK(t *testing.T) {
	_, snapshot, _ := CaptureResponseOutcome(httptest.NewRecorder())
	if got := snapshot(); got.Kind != "completed" || got.Status != http.StatusOK || got.Committed || got.Bytes != 0 {
		t.Fatalf("snapshot() = %#v", got)
	}
}

func TestCaptureResponseOutcomeTracksImplicitOKAndBytes(t *testing.T) {
	wrapped, snapshot, _ := CaptureResponseOutcome(httptest.NewRecorder())
	n, err := wrapped.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := snapshot(); got.Status != http.StatusOK || !got.Committed || got.Bytes != 5 {
		t.Fatalf("snapshot() = %#v", got)
	}
}

func TestCaptureResponseOutcomeTracksFlushCommit(t *testing.T) {
	underlying := newOptionalResponseWriter()
	wrapped, snapshot, _ := CaptureResponseOutcome(underlying)
	wrapped.(http.Flusher).Flush()
	if got := snapshot(); !got.Committed || !got.Flushed || got.Status != http.StatusOK {
		t.Fatalf("snapshot() = %#v", got)
	}
}

func TestCaptureResponseOutcomeTracksOnlySuccessfulHijack(t *testing.T) {
	failed := newOptionalResponseWriter()
	failed.hijackErr = errors.New("hijack failed")
	wrappedFailed, failedSnapshot, _ := CaptureResponseOutcome(failed)
	_, _, _ = wrappedFailed.(http.Hijacker).Hijack()
	if got := failedSnapshot(); got.Hijacked || got.Committed {
		t.Fatalf("failed hijack snapshot = %#v", got)
	}

	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	counting := &closeCountingConn{Conn: left}
	success := newOptionalResponseWriter()
	success.hijackConn = counting
	wrappedSuccess, successSnapshot, closeHijacked := CaptureResponseOutcome(success)
	_, _, err := wrappedSuccess.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatalf("Hijack() error = %v", err)
	}
	if got := successSnapshot(); !got.Hijacked || !got.Committed {
		t.Fatalf("successful hijack snapshot = %#v", got)
	}
	if err := closeHijacked(); err != nil {
		t.Fatalf("closeHijacked() error = %v", err)
	}
	if err := closeHijacked(); err != nil {
		t.Fatalf("second closeHijacked() error = %v", err)
	}
	if counting.closes.Load() != 1 {
		t.Fatalf("close calls = %d, want 1", counting.closes.Load())
	}
}

func TestCaptureResponseOutcomeTracksReadFromBytes(t *testing.T) {
	wrapped, snapshot, _ := CaptureResponseOutcome(newOptionalResponseWriter())
	n, err := wrapped.(io.ReaderFrom).ReadFrom(strings.NewReader("reader"))
	if err != nil || n != 6 {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if got := snapshot(); got.Bytes != 6 || !got.Committed {
		t.Fatalf("snapshot() = %#v", got)
	}
}

func TestCaptureResponseOutcomeTracksWriteStringBytes(t *testing.T) {
	wrapped, snapshot, _ := CaptureResponseOutcome(newOptionalResponseWriter())
	n, err := wrapped.(io.StringWriter).WriteString("string")
	if err != nil || n != 6 {
		t.Fatalf("WriteString = %d, %v", n, err)
	}
	if got := snapshot(); got.Bytes != 6 || !got.Committed {
		t.Fatalf("snapshot() = %#v", got)
	}
}

func TestCaptureResponseOutcomeTracksFlushErrorCommit(t *testing.T) {
	wantErr := errors.New("flush failed")
	underlying := newOptionalResponseWriter()
	underlying.flushErr = wantErr
	wrapped, snapshot, _ := CaptureResponseOutcome(underlying)
	err := wrapped.(interface{ FlushError() error }).FlushError()
	if !errors.Is(err, wantErr) {
		t.Fatalf("FlushError() = %v", err)
	}
	if got := snapshot(); !got.Committed || !got.Flushed {
		t.Fatalf("snapshot() = %#v", got)
	}
}

func TestResponseCaptureBodyIsDisabledUntilEnabled(t *testing.T) {
	underlying := httptest.NewRecorder()
	wrapped, capture := CaptureResponseOutcomeController(underlying)
	_, _ = wrapped.Write([]byte("hidden"))
	if got := capture.Snapshot(); len(got.Body) != 0 || got.BodyTruncated {
		t.Fatalf("disabled capture snapshot = %#v", got)
	}
	if err := capture.EnableBodyCapture(3); err != nil {
		t.Fatalf("EnableBodyCapture() error = %v", err)
	}
	_, _ = wrapped.Write([]byte("visible"))
	got := capture.Snapshot()
	if string(got.Body) != "vis" || !got.BodyTruncated {
		t.Fatalf("enabled capture snapshot = %#v, want vis/truncated", got)
	}
}

func TestResponseCaptureIncludesOnlyBytesConfirmedWritten(t *testing.T) {
	tests := []struct {
		name string
		call func(http.ResponseWriter) (int64, error)
	}{
		{
			name: "write",
			call: func(w http.ResponseWriter) (int64, error) {
				n, err := w.Write([]byte("sensitive"))
				return int64(n), err
			},
		},
		{
			name: "write string",
			call: func(w http.ResponseWriter) (int64, error) {
				n, err := w.(io.StringWriter).WriteString("sensitive")
				return int64(n), err
			},
		},
		{
			name: "read from",
			call: func(w http.ResponseWriter) (int64, error) {
				return w.(io.ReaderFrom).ReadFrom(strings.NewReader("sensitive"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			underlying := &shortResponseWriter{header: make(http.Header), limit: 3}
			wrapped, capture := CaptureResponseOutcomeController(underlying)
			if err := capture.EnableBodyCapture(16); err != nil {
				t.Fatalf("EnableBodyCapture() error = %v", err)
			}
			n, err := test.call(wrapped)
			if n != 3 || !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("write result = %d/%v, want 3/%v", n, err, io.ErrShortWrite)
			}
			if got := capture.Outcome().Bytes; got != 3 {
				t.Fatalf("outcome bytes = %d, want 3", got)
			}
			if got := string(capture.Snapshot().Body); got != "sen" {
				t.Fatalf("captured body = %q, want %q", got, "sen")
			}
		})
	}
}

func TestResponseCaptureRecordsBytesAndBodyTogether(t *testing.T) {
	capture := &ResponseCapture{
		outcome: apisixctx.ResponseOutcome{
			Kind:   apisixctx.RequestOutcomeCompleted,
			Status: http.StatusOK,
		},
	}
	if err := capture.EnableBodyCapture(4); err != nil {
		t.Fatalf("EnableBodyCapture() error = %v", err)
	}

	capture.recordWrite([]byte("abcdef"), 3)
	capture.recordWrite([]byte("wxyz"), 4)

	if got := capture.Outcome().Bytes; got != 7 {
		t.Fatalf("outcome bytes = %d, want 7", got)
	}
	got := capture.Snapshot()
	if string(got.Body) != "abcw" || !got.BodyTruncated {
		t.Fatalf("capture snapshot = %#v, want body abcw and truncation", got)
	}
}

func TestResponseCaptureOmitsUnconfirmedBodyWhenUnderlyingWriterPanics(t *testing.T) {
	underlying := &panicResponseWriter{optionalResponseWriter: newOptionalResponseWriter(), panicWrite: true}
	wrapped, capture := CaptureResponseOutcomeController(underlying)
	if err := capture.EnableBodyCapture(8); err != nil {
		t.Fatalf("EnableBodyCapture() error = %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("underlying write did not panic")
			}
		}()
		_, _ = wrapped.Write([]byte("panic-body"))
	}()
	got := capture.Snapshot()
	if len(got.Body) != 0 || got.BodyTruncated {
		t.Fatalf("panic capture snapshot = %#v, want no unconfirmed body", got)
	}
}

func TestResponseCaptureSnapshotSeparatesDeclaredTrailers(t *testing.T) {
	recorder := httptest.NewRecorder()
	w, capture := CaptureResponseOutcomeController(recorder)
	w.Header().Add("Trailer", "X-Checksum")
	w.Header().Set("X-Visible", "header")
	w.WriteHeader(http.StatusOK)
	w.Header().Set("X-Checksum", "done")

	snapshot := capture.Snapshot()
	if got := snapshot.Trailer.Get("X-Checksum"); got != "done" {
		t.Fatalf("trailer X-Checksum = %q, want done", got)
	}
	if got := snapshot.Header.Get("X-Checksum"); got != "" {
		t.Fatalf("final header leaked trailer value %q", got)
	}
	if got := snapshot.Header.Get("X-Visible"); got != "header" {
		t.Fatalf("final header X-Visible = %q", got)
	}
}

func TestResponseCaptureRequestContextAndNilBoundaries(t *testing.T) {
	if WithResponseCapture(nil, nil) != nil {
		t.Fatal("WithResponseCapture(nil) returned a request")
	}
	if capture, ok := ResponseCaptureFromRequest(nil); capture != nil || ok {
		t.Fatalf("ResponseCaptureFromRequest(nil) = %#v/%v", capture, ok)
	}

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	if capture, ok := ResponseCaptureFromRequest(request); capture != nil || ok {
		t.Fatalf("unexpected capture on plain request = %#v/%v", capture, ok)
	}
	_, capture := CaptureResponseOutcomeController(httptest.NewRecorder())
	request = WithResponseCapture(request, capture)
	if got, ok := ResponseCaptureFromRequest(request); !ok || got != capture {
		t.Fatalf("request capture = %#v/%v, want %#v/true", got, ok, capture)
	}
	request = WithResponseCapture(request, nil)
	if got, ok := ResponseCaptureFromRequest(request); got != nil || ok {
		t.Fatalf("nil request capture = %#v/%v", got, ok)
	}

	var nilCapture *ResponseCapture
	if got := nilCapture.Outcome(); got.Status != 0 || got.Committed {
		t.Fatalf("nil Outcome() = %#v", got)
	}
	if got := nilCapture.Snapshot(); got.Header != nil || got.Body != nil {
		t.Fatalf("nil Snapshot() = %#v", got)
	}
	if err := nilCapture.CloseHijacked(); err != nil {
		t.Fatalf("nil CloseHijacked() error = %v", err)
	}
	if err := nilCapture.EnableBodyCapture(1); err == nil {
		t.Fatal("nil EnableBodyCapture() unexpectedly succeeded")
	}
}

func TestResponseCaptureBodyLimitCanExpandAndDisable(t *testing.T) {
	underlying := httptest.NewRecorder()
	w, capture := CaptureResponseOutcomeController(underlying)
	for _, invalid := range []int{-1, MAX_RESP_BODY + 1} {
		if err := capture.EnableBodyCapture(invalid); err == nil {
			t.Fatalf("EnableBodyCapture(%d) unexpectedly succeeded", invalid)
		}
	}
	if err := capture.EnableBodyCapture(2); err != nil {
		t.Fatalf("EnableBodyCapture(2) error = %v", err)
	}
	_, _ = w.Write([]byte("abcd"))
	if got := capture.Snapshot(); string(got.Body) != "ab" || !got.BodyTruncated {
		t.Fatalf("bounded capture = %#v", got)
	}
	_, _ = w.Write([]byte("z"))
	if err := capture.EnableBodyCapture(4); err != nil {
		t.Fatalf("EnableBodyCapture(4) error = %v", err)
	}
	_, _ = w.Write([]byte("ef"))
	if got := capture.Snapshot(); string(got.Body) != "abef" || !got.BodyTruncated {
		t.Fatalf("expanded capture = %#v", got)
	}
	if err := capture.EnableBodyCapture(0); err != nil {
		t.Fatalf("EnableBodyCapture(0) error = %v", err)
	}
	if got := capture.Snapshot(); len(got.Body) != 0 || got.BodyTruncated {
		t.Fatalf("disabled capture = %#v", got)
	}
	if err := capture.CloseHijacked(); err != nil {
		t.Fatalf("CloseHijacked() without connection error = %v", err)
	}
}

func TestResponseCaptureSnapshotSeparatesTrailerPrefix(t *testing.T) {
	recorder := httptest.NewRecorder()
	_, capture := CaptureResponseOutcomeController(recorder)
	recorder.Header().Set("X-Visible", "header")
	recorder.Header().Add("Trailer", " , ")
	recorder.Header()[http.TrailerPrefix+"X-Late"] = []string{"done"}

	snapshot := capture.Snapshot()
	if snapshot.Header.Get("X-Visible") != "header" || snapshot.Header.Get(http.TrailerPrefix+"X-Late") != "" {
		t.Fatalf("snapshot header = %#v", snapshot.Header)
	}
	if snapshot.Trailer.Get("X-Late") != "done" {
		t.Fatalf("snapshot trailer = %#v", snapshot.Trailer)
	}
}

func TestResponseCaptureRecordsOnlyBoundedFailureReasons(t *testing.T) {
	_, capture := CaptureResponseOutcomeController(httptest.NewRecorder())
	if !capture.RecordFailure(apisixctx.ResponseFailureUpstreamIdleTimeout) {
		t.Fatal("RecordFailure(upstream_idle_timeout) = false")
	}
	if capture.RecordFailure(apisixctx.ResponseFailureReason("raw-error")) {
		t.Fatal("RecordFailure(raw-error) = true, want bounded rejection")
	}
	if got := capture.Outcome().FailureReason; got != apisixctx.ResponseFailureUpstreamIdleTimeout {
		t.Fatalf("failure reason = %q, want upstream_idle_timeout", got)
	}
}
