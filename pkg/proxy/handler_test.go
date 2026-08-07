package proxy

import (
	"context"
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

func TestRetryTransportRestoresPOSTBodyForEveryAttempt(t *testing.T) {
	var bodies []string
	attempts := 0
	transport := NewRetryTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies = append(bodies, string(body))
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection reset")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: request}, nil
	}))
	request, err := http.NewRequest(http.MethodPost, "http://upstream.example/submit", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request = WithRetries(request, 2, func(*http.Request) bool { return true })

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	_ = response.Body.Close()
	if got, want := strings.Join(bodies, ","), "payload,payload,payload"; got != want {
		t.Fatalf("attempt bodies = %q, want %q", got, want)
	}
}

func TestRetryTransportStopsWhenRequestCannotBeReplayed(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*http.Request)
	}{
		{name: "missing GetBody", prepare: func(request *http.Request) { request.GetBody = nil }},
		{name: "failing GetBody", prepare: func(request *http.Request) {
			request.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("cannot replay") }
		}},
		{name: "canceled context", prepare: func(request *http.Request) {
			ctx, cancel := context.WithCancel(request.Context())
			cancel()
			*request = *request.WithContext(ctx)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			nextCalls := 0
			transport := NewRetryTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return nil, errors.New("connection reset")
			}))
			request := httptest.NewRequest(
				http.MethodPost,
				"http://upstream.example/submit",
				strings.NewReader("payload"),
			)
			test.prepare(request)
			request = WithRetries(request, 2, func(*http.Request) bool {
				nextCalls++
				return true
			})

			if _, err := transport.RoundTrip(request); err == nil {
				t.Fatal("RoundTrip() error = nil, want transport error")
			}
			if attempts != 1 || nextCalls != 0 {
				t.Fatalf("attempts/next calls = %d/%d, want 1/0", attempts, nextCalls)
			}
		})
	}
}

func TestRetryTransportStopsWhenTargetSelectionFails(t *testing.T) {
	attempts := 0
	nextCalls := 0
	transport := NewRetryTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("connection reset")
	}))
	request := httptest.NewRequest(http.MethodGet, "http://upstream.example/", nil)
	request = WithRetries(request, 3, func(*http.Request) bool {
		nextCalls++
		return false
	})

	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("RoundTrip() error = nil, want transport error")
	}
	if attempts != 1 || nextCalls != 1 {
		t.Fatalf("attempts/next calls = %d/%d, want 1/1", attempts, nextCalls)
	}
}

func TestRetryTransportReturnsSuccessfulResponseWithoutSelectingAnotherTarget(t *testing.T) {
	nextCalls := 0
	transport := NewRetryTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusAccepted, Body: http.NoBody, Request: request}, nil
	}))
	request := httptest.NewRequest(http.MethodGet, "http://upstream.example/", nil)
	request = WithRetries(request, 3, func(*http.Request) bool {
		nextCalls++
		return true
	})

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || nextCalls != 0 {
		t.Fatalf("status/next calls = %d/%d, want %d/0", response.StatusCode, nextCalls, http.StatusAccepted)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
