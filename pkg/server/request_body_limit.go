package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/util"
)

const requestBodyTooLargeMessage = "request body too large"

type requestBodyLimitState struct {
	mu                sync.Mutex
	responseWriter    http.ResponseWriter
	rejected          bool
	committed         bool
	canonicalizing    bool
	canonicalDisabled bool
}

type requestBodyLimitContextKey struct{}

func (s *requestBodyLimitState) canonicalResponsePending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected && !s.committed && !s.canonicalizing && !s.canonicalDisabled
}

func (s *requestBodyLimitState) disableCanonicalResponse() {
	s.mu.Lock()
	s.canonicalDisabled = true
	s.mu.Unlock()
}

func (s *requestBodyLimitState) reject() {
	s.mu.Lock()
	s.rejected = true
	s.mu.Unlock()
}

func (s *requestBodyLimitState) suppress() bool {
	return s.rejected && !s.committed && !s.canonicalizing
}

func (s *requestBodyLimitState) writeHeader(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
	return func(code int) {
		s.mu.Lock()
		if code < 200 && code != http.StatusSwitchingProtocols {
			s.mu.Unlock()
			next(code)
			return
		}
		if s.suppress() {
			s.mu.Unlock()
			return
		}
		s.committed = true
		s.mu.Unlock()
		next(code)
	}
}

func (s *requestBodyLimitState) write(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
	return func(body []byte) (int, error) {
		s.mu.Lock()
		if s.suppress() {
			s.mu.Unlock()
			return len(body), nil
		}
		s.committed = true
		s.mu.Unlock()
		return next(body)
	}
}

func (s *requestBodyLimitState) writeString(next httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
	return func(body string) (int, error) {
		s.mu.Lock()
		if s.suppress() {
			s.mu.Unlock()
			return len(body), nil
		}
		s.committed = true
		s.mu.Unlock()
		return next(body)
	}
}

func (s *requestBodyLimitState) readFrom(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
	return func(reader io.Reader) (int64, error) {
		s.mu.Lock()
		if s.suppress() {
			s.mu.Unlock()
			return 0, nil
		}
		s.committed = true
		s.mu.Unlock()
		return next(reader)
	}
}

func (s *requestBodyLimitState) flush(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
	return func() {
		s.mu.Lock()
		if s.suppress() {
			s.mu.Unlock()
			return
		}
		s.committed = true
		s.mu.Unlock()
		next()
	}
}

func (s *requestBodyLimitState) flushError(next httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
	return func() error {
		s.mu.Lock()
		if s.suppress() {
			s.mu.Unlock()
			return nil
		}
		s.committed = true
		s.mu.Unlock()
		return next()
	}
}

func (s *requestBodyLimitState) hijack(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
	return func() (net.Conn, *bufio.ReadWriter, error) {
		s.mu.Lock()
		if s.suppress() {
			s.mu.Unlock()
			return nil, nil, http.ErrNotSupported
		}
		s.mu.Unlock()
		conn, rw, err := next()
		if err == nil {
			s.mu.Lock()
			s.committed = true
			s.mu.Unlock()
		}
		return conn, rw, err
	}
}

func (s *requestBodyLimitState) wrapResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: s.writeHeader,
		Write:       s.write,
		WriteString: s.writeString,
		ReadFrom:    s.readFrom,
		Flush:       s.flush,
		FlushError:  s.flushError,
		Hijack:      s.hijack,
	})
}

func (s *requestBodyLimitState) writeCanonicalResponse(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	if !s.rejected || s.committed || s.canonicalizing || s.canonicalDisabled {
		s.mu.Unlock()
		return false
	}
	s.canonicalizing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.canonicalizing = false
		s.mu.Unlock()
	}()

	clearResponseHeaders(w)
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
	_ = util.WriteJSONMessage(w, http.StatusRequestEntityTooLarge, requestBodyTooLargeMessage)
	return true
}

func withRequestBodyLimitState(r *http.Request, state *requestBodyLimitState) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestBodyLimitContextKey{}, state))
}

func requestBodyLimitStateFromRequest(r *http.Request) *requestBodyLimitState {
	state, _ := r.Context().Value(requestBodyLimitContextKey{}).(*requestBodyLimitState)
	return state
}

type requestBodyLimitBody struct {
	io.ReadCloser
	state *requestBodyLimitState
}

func (b *requestBodyLimitBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		b.state.reject()
		_ = b.Close()
	}
	return n, err
}

func limitRequestBody(next http.Handler, limit int64) http.Handler {
	if limit <= 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > limit {
			_ = r.Body.Close()
			clearResponseHeaders(w)
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
			_ = util.WriteJSONMessage(w, http.StatusRequestEntityTooLarge, requestBodyTooLargeMessage)
			return
		}

		state := &requestBodyLimitState{responseWriter: w}
		r.Body = &requestBodyLimitBody{
			ReadCloser: http.MaxBytesReader(rootResponseWriter(w), r.Body, limit),
			state:      state,
		}
		wrapped := state.wrapResponseWriter(w)
		request := withRequestBodyLimitState(r, state)
		defer state.writeCanonicalResponse(wrapped, request)
		next.ServeHTTP(wrapped, request)
	})
}

func rootResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	for {
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		next := unwrapper.Unwrap()
		if next == nil || next == w {
			return w
		}
		w = next
	}
}

func clearResponseHeaders(w http.ResponseWriter) {
	for key := range w.Header() {
		w.Header().Del(key)
	}
}
