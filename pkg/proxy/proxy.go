package proxy

import (
	"net/http"
	"net/http/httputil"
	"sync"
	"time"
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
	return &httputil.ReverseProxy{
		Director:       director,
		Transport:      transport,
		ModifyResponse: modifyResponse,
		BufferPool:     bufferPool,
		ErrorHandler:   errorHandler,
		FlushInterval:  flushInterval,
		// ErrorLog
	}
}
