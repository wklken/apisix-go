package base

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type responseOutcomeState struct {
	mu           sync.Mutex
	outcome      ctx.ResponseOutcome
	hijackedConn interface{ Close() error }
	closeOnce    sync.Once
	closeErr     error
}

func CaptureResponseOutcome(w http.ResponseWriter) (
	wrapped http.ResponseWriter,
	snapshot func() ctx.ResponseOutcome,
	closeHijacked func() error,
) {
	state := &responseOutcomeState{
		outcome: ctx.ResponseOutcome{
			Kind:   ctx.RequestOutcomeCompleted,
			Status: http.StatusOK,
		},
	}

	wrapped = httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				if status < http.StatusContinue || status >= http.StatusOK || status == http.StatusSwitchingProtocols {
					state.commit(status)
				}
				writeHeader(status)
			}
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				state.commit(http.StatusOK)
				n, err := write(body)
				state.addBytes(int64(n))
				return n, err
			}
		},
		WriteString: func(writeString httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
			return func(value string) (int, error) {
				state.commit(http.StatusOK)
				n, err := writeString(value)
				state.addBytes(int64(n))
				return n, err
			}
		},
		ReadFrom: func(readFrom httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				state.commit(http.StatusOK)
				n, err := readFrom(reader)
				state.addBytes(n)
				return n, err
			}
		},
		Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() {
				state.markFlushed()
				flush()
			}
		},
		FlushError: func(flushError httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
			return func() error {
				state.markFlushed()
				return flushError()
			}
		},
		Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) {
				conn, rw, err := hijack()
				if err == nil {
					state.markHijacked(conn)
				}
				return conn, rw, err
			}
		},
	})

	snapshot = state.snapshot
	closeHijacked = state.closeHijacked
	return wrapped, snapshot, closeHijacked
}

func (s *responseOutcomeState) commit(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome.Committed {
		return
	}
	s.outcome.Status = status
	s.outcome.Committed = true
}

func (s *responseOutcomeState) addBytes(written int64) {
	s.mu.Lock()
	s.outcome.Bytes += written
	s.mu.Unlock()
}

func (s *responseOutcomeState) markFlushed() {
	s.mu.Lock()
	if !s.outcome.Committed {
		s.outcome.Status = http.StatusOK
		s.outcome.Committed = true
	}
	s.outcome.Flushed = true
	s.mu.Unlock()
}

func (s *responseOutcomeState) markHijacked(conn net.Conn) {
	s.mu.Lock()
	if !s.outcome.Committed {
		s.outcome.Status = http.StatusOK
		s.outcome.Committed = true
	}
	s.outcome.Hijacked = true
	if s.hijackedConn == nil {
		s.hijackedConn = conn
	}
	s.mu.Unlock()
}

func (s *responseOutcomeState) snapshot() ctx.ResponseOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome
}

func (s *responseOutcomeState) closeHijacked() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		conn := s.hijackedConn
		s.mu.Unlock()
		if conn != nil {
			s.closeErr = conn.Close()
		}
	})
	return s.closeErr
}
