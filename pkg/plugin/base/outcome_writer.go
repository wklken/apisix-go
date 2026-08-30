package base

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

// ResponseCaptureSnapshot is the detached bounded response view used by log
// snapshots. Body capture is disabled until EnableBodyCapture is called.
type ResponseCaptureSnapshot struct {
	Header        http.Header
	Trailer       http.Header
	Body          []byte
	BodyTruncated bool
}

// ResponseCapture is the sole outer response observer. It records outcome
// metadata for every response while retaining a bounded body only when a log
// binding explicitly enables it.
type ResponseCapture struct {
	mu             sync.Mutex
	root           http.ResponseWriter
	outcome        ctx.ResponseOutcome
	body           []byte
	bodyLimit      int
	bodyTruncated  bool
	wireBodyPrefix []byte
	hijackedConn   io.Closer
	closeOnce      sync.Once
	closeErr       error
}

// CaptureResponseOutcomeController installs one response capture controller
// around w and returns the controller used to observe and finalize it.
func CaptureResponseOutcomeController(w http.ResponseWriter) (http.ResponseWriter, *ResponseCapture) {
	capture := &ResponseCapture{
		root: w,
		outcome: ctx.ResponseOutcome{
			Kind:   ctx.RequestOutcomeCompleted,
			Status: http.StatusOK,
		},
	}
	wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				if status < http.StatusContinue || status >= http.StatusOK || status == http.StatusSwitchingProtocols {
					capture.commit(status)
				}
				writeHeader(status)
			}
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				capture.commit(http.StatusOK)
				n, err := write(body)
				capture.recordWrite(body, int64(n))
				return n, err
			}
		},
		WriteString: func(writeString httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) {
				capture.commit(http.StatusOK)
				n, err := writeString(value)
				capture.recordWrite([]byte(value), int64(n))
				return n, err
			}
		},
		ReadFrom: func(readFrom httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				capture.commit(http.StatusOK)
				tracked := &captureReader{reader: reader, limit: capture.readCaptureLimit()}
				n, err := readFrom(tracked)
				capture.recordWrite(tracked.body, n)
				return n, err
			}
		},
		Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				capture.markFlushed()
				flush()
			}
		},
		FlushError: func(flushError httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error {
				capture.markFlushed()
				return flushError()
			}
		},
		Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) {
				conn, rw, err := hijack()
				if err == nil {
					capture.markHijacked(conn)
				}
				return conn, rw, err
			}
		},
	})
	return wrapped, capture
}

// HTTPWireLength returns the exact buffered HTTP/1.1 response length when the
// outer response capture retained enough framing information to reconstruct
// it. Unsupported framing returns known=false.
func (c *ResponseCapture) HTTPWireLength(r *http.Request) (size int64, known bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	outcome := c.outcome
	bodyPrefix := append([]byte(nil), c.wireBodyPrefix...)
	root := c.root
	c.mu.Unlock()
	var header http.Header
	if root != nil {
		header = root.Header().Clone()
	}
	return apisixlog.EstimateHTTP1ResponseLength(r, header, outcome, bodyPrefix)
}

type captureContextKey struct{}

func WithResponseCapture(r *http.Request, capture *ResponseCapture) *http.Request {
	if r == nil {
		return nil
	}
	return r.WithContext(contextWithResponseCapture(r, capture))
}

func contextWithResponseCapture(r *http.Request, capture *ResponseCapture) context.Context {
	return context.WithValue(r.Context(), captureContextKey{}, capture)
}

func ResponseCaptureFromRequest(r *http.Request) (*ResponseCapture, bool) {
	if r == nil {
		return nil, false
	}
	capture, ok := r.Context().Value(captureContextKey{}).(*ResponseCapture)
	return capture, ok && capture != nil
}

func (c *ResponseCapture) EnableBodyCapture(limit int) error {
	if c == nil {
		return fmt.Errorf("response capture is nil")
	}
	if limit < 0 || limit > MAX_RESP_BODY {
		return fmt.Errorf("response body capture must be between 0 and %d bytes: %d", MAX_RESP_BODY, limit)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit == 0 {
		c.bodyLimit = 0
		c.body = nil
		c.bodyTruncated = false
		return nil
	}
	if limit > c.bodyLimit {
		c.bodyLimit = limit
		if len(c.body) > limit {
			c.body = c.body[:limit]
			c.bodyTruncated = true
		}
	}
	return nil
}

func (c *ResponseCapture) Outcome() ctx.ResponseOutcome {
	if c == nil {
		return ctx.ResponseOutcome{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outcome
}

// RecordFailure attaches the first bounded transport/application failure to
// the final response outcome. Raw errors are deliberately excluded.
func (c *ResponseCapture) RecordFailure(reason ctx.ResponseFailureReason) bool {
	if c == nil || !ctx.ValidResponseFailureReason(reason) {
		return false
	}
	c.mu.Lock()
	if c.outcome.FailureReason == "" {
		c.outcome.FailureReason = reason
	}
	c.mu.Unlock()
	return true
}

func (c *ResponseCapture) Snapshot() ResponseCaptureSnapshot {
	if c == nil {
		return ResponseCaptureSnapshot{}
	}
	c.mu.Lock()
	body := append([]byte(nil), c.body...)
	truncated := c.bodyTruncated
	root := c.root
	c.mu.Unlock()
	var header, trailer http.Header
	if root != nil {
		header = root.Header().Clone()
		trailer = make(http.Header)
		for _, declaration := range header.Values("Trailer") {
			for name := range strings.SplitSeq(declaration, ",") {
				name = http.CanonicalHeaderKey(strings.TrimSpace(name))
				if name == "" {
					continue
				}
				if values := header.Values(name); len(values) > 0 {
					trailer[name] = append([]string(nil), values...)
					header.Del(name)
				}
			}
		}
		for name, values := range header {
			const prefix = http.TrailerPrefix
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			trailerName := strings.TrimPrefix(name, prefix)
			trailer[trailerName] = append([]string(nil), values...)
			header.Del(name)
		}
		if len(trailer) == 0 {
			trailer = nil
		}
	}
	return ResponseCaptureSnapshot{Header: header, Trailer: trailer, Body: body, BodyTruncated: truncated}
}

func (c *ResponseCapture) CloseHijacked() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		conn := c.hijackedConn
		c.mu.Unlock()
		if conn != nil {
			c.closeErr = conn.Close()
		}
	})
	return c.closeErr
}

func (c *ResponseCapture) commit(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outcome.Committed {
		return
	}
	c.outcome.Status = status
	c.outcome.Committed = true
}

func (c *ResponseCapture) markFlushed() {
	c.mu.Lock()
	if !c.outcome.Committed {
		c.outcome.Status = http.StatusOK
		c.outcome.Committed = true
	}
	c.outcome.Flushed = true
	c.mu.Unlock()
}

func (c *ResponseCapture) markHijacked(conn net.Conn) {
	c.mu.Lock()
	if !c.outcome.Committed {
		c.outcome.Status = http.StatusOK
		c.outcome.Committed = true
	}
	c.outcome.Hijacked = true
	if c.hijackedConn == nil {
		c.hijackedConn = conn
	}
	c.mu.Unlock()
}

func (c *ResponseCapture) recordWrite(body []byte, written int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcome.Bytes += written
	if written > 0 && len(c.wireBodyPrefix) < 512 {
		confirmed := min(int64(len(body)), written)
		keep := min(int64(512-len(c.wireBodyPrefix)), confirmed)
		c.wireBodyPrefix = append(c.wireBodyPrefix, body[:keep]...)
	}
	if c.bodyLimit <= 0 || written <= 0 {
		return
	}
	remaining := c.bodyLimit - len(c.body)
	if remaining <= 0 {
		c.bodyTruncated = true
		return
	}
	confirmed := min(int64(len(body)), written)
	captured := min(int64(remaining), confirmed)
	if captured > 0 {
		c.body = append(c.body, body[:captured]...)
	}
	if captured < written {
		c.bodyTruncated = true
	}
}

func (c *ResponseCapture) readCaptureLimit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return max(max(c.bodyLimit-len(c.body), 0), 512-len(c.wireBodyPrefix))
}

type captureReader struct {
	reader io.Reader
	limit  int
	body   []byte
}

func (r *captureReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && len(r.body) < r.limit {
		keep := min(n, r.limit-len(r.body))
		r.body = append(r.body, p[:keep]...)
	}
	return n, err
}
