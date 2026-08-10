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

func (w *minimalResponseWriter) Header() http.Header            { return w.header }
func (w *minimalResponseWriter) WriteHeader(int)                {}
func (w *minimalResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

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
