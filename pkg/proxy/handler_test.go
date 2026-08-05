package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"
)

func TestNewProxyHandlerWithFlushInterval(t *testing.T) {
	handler := NewProxyHandlerWithFlushInterval(
		http.DefaultTransport,
		func(req *http.Request) {},
		nil,
		nil,
		-1*time.Second,
	)

	rp, ok := handler.(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("handler type = %T, want *httputil.ReverseProxy", handler)
	}
	if rp.FlushInterval != -1*time.Second {
		t.Fatalf("FlushInterval = %s, want -1s", rp.FlushInterval)
	}
	if _, ok := rp.BufferPool.(*proxyBufferPool); !ok {
		t.Fatalf("BufferPool type = %T, want *proxyBufferPool", rp.BufferPool)
	}
}

func TestProxyBufferPoolUsesFixedSizeBuffers(t *testing.T) {
	pool := newProxyBufferPool()
	buffer := pool.Get()
	if len(buffer) != proxyBufferSize || cap(buffer) != proxyBufferSize {
		t.Fatalf("buffer len/cap = %d/%d, want %d/%d", len(buffer), cap(buffer), proxyBufferSize, proxyBufferSize)
	}
	pool.Put(buffer)
	pool.Put(make([]byte, proxyBufferSize+1))
	pool.Put(nil)
}

func TestRetryTransportRetriesTransportErrorsWithNextTargets(t *testing.T) {
	attempts := 0
	nextTargets := 0
	transport := NewRetryTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection refused")
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	}))
	request := httptest.NewRequest(http.MethodGet, "http://upstream.example/hello", nil)
	request = WithRetries(request, 2, func(*http.Request) bool {
		nextTargets++
		return true
	})

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	if attempts != 3 {
		t.Fatalf("transport attempts = %d, want 3", attempts)
	}
	if nextTargets != 2 {
		t.Fatalf("next-target selections = %d, want 2", nextTargets)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
