package proxy

import (
	"net/http"
	"net/http/httputil"

	"github.com/oxtoacart/bpool"
)

// TODO: 1. websocket
// TODO: 2. streaming for file download/upload
// TODO: 3. nopCloser for response https://github.com/TykTechnologies/tyk/blob/master/reverse_proxy.go

type (
	ErrorHandler   func(http.ResponseWriter, *http.Request, error)
	ModifyResponse func(*http.Response) error
	Director       func(req *http.Request)
)

var bufferPool *bpool.BytePool

func init() {
	// use byte pool to prevent gc
	// will take 320MB memory
	bufferPool = bpool.NewBytePool(10000, 32*1024)
}

func NewProxyHandler(transport http.RoundTripper, director Director,
	modifyResponse ModifyResponse, errorHandler ErrorHandler,
) http.Handler {
	return &httputil.ReverseProxy{
		Director:       director,
		Transport:      transport,
		ModifyResponse: modifyResponse,
		BufferPool:     bufferPool,
		ErrorHandler:   errorHandler,
		// FlushInterval
		// ErrorLog
	}
}
