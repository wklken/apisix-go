package proxy

import (
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
)

// TODO: 1. websocket
// TODO: 2. streaming for file download/upload
// TODO: 3. nopCloser for response https://github.com/TykTechnologies/tyk/blob/master/reverse_proxy.go

type (
	ErrorHandler   func(http.ResponseWriter, *http.Request, error)
	ModifyResponse func(*http.Response) error
	Director       func(req *http.Request)
)

const proxyBufferSize = 32 * 1024

type proxyBuffer [proxyBufferSize]byte

type proxyBufferPool struct {
	pool sync.Pool
}

func newProxyBufferPool() *proxyBufferPool {
	pool := &proxyBufferPool{}
	pool.pool.New = func() any { return new(proxyBuffer) }
	return pool
}

func (p *proxyBufferPool) Get() []byte {
	return p.pool.Get().(*proxyBuffer)[:]
}

func (p *proxyBufferPool) Put(buffer []byte) {
	if cap(buffer) != proxyBufferSize {
		return
	}
	p.pool.Put((*proxyBuffer)(buffer[:proxyBufferSize]))
}

var _ httputil.BufferPool = (*proxyBufferPool)(nil)

var bufferPool = newProxyBufferPool()

func NewProxyHandler(transport http.RoundTripper, director Director,
	modifyResponse ModifyResponse, errorHandler ErrorHandler,
) http.Handler {
	return NewProxyHandlerWithFlushInterval(transport, director, modifyResponse, errorHandler, 0)
}

func NewProxyHandlerWithFlushInterval(
	transport http.RoundTripper,
	director Director,
	modifyResponse ModifyResponse,
	errorHandler ErrorHandler,
	flushInterval time.Duration,
) http.Handler {
	return &proxyHandler{ReverseProxy: &httputil.ReverseProxy{
		Director:       director,
		Transport:      httpTrailerTransport{next: transport},
		ModifyResponse: modifyResponse,
		BufferPool:     bufferPool,
		ErrorHandler:   errorHandler,
		FlushInterval:  flushInterval,
		// ErrorLog
	}}
}

// The upstream status must remain observable when copying a committed body
// fails. Flush its pending headers before net/http aborts the incomplete body.
type proxyHandler struct{ *httputil.ReverseProxy }

func (handler *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r, balanceScope := withAlgorithmRequestScope(r)
	defer balanceScope.finish()
	committed := false
	writer := httpsnoop.Wrap(
		w,
		httpsnoop.Hooks{WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				if status >= 200 {
					committed = true
				}
				next(status)
			}
		}},
	)
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered == http.ErrAbortHandler && committed {
				_ = http.NewResponseController(w).Flush()
			}
			panic(recovered)
		}
	}()
	handler.ReverseProxy.ServeHTTP(writer, r)
}

// net/http's body reader populates Trailer on its original response at EOF.
// Return a detached HTTP/1 response so announcements survive but those later
// values are not forwarded. HTTP/2 gRPC retains its status trailers.
type httpTrailerTransport struct{ next http.RoundTripper }

func (transport httpTrailerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	next := transport.next
	if next == nil {
		next = http.DefaultTransport
	}
	response, err := next.RoundTrip(r)
	if err != nil || response == nil || response.ProtoMajor == 2 {
		return response, err
	}
	detached := *response
	detached.Trailer = response.Trailer.Clone()
	return &detached, nil
}
