package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingBody() *blockingBody { return &blockingBody{closed: make(chan struct{})} }

func (b *blockingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, context.Canceled
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestProgressTimeoutBodyClosesStalledRead(t *testing.T) {
	body := newBlockingBody()
	timed := newProgressTimeoutBody(body, 20*time.Millisecond, func() {})
	started := time.Now()
	_, err := timed.Read(make([]byte, 1))
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("Read() error/elapsed = %v/%s", err, time.Since(started))
	}
}

func TestProgressTimeoutBodyCloseDoesNotCancelContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := newBlockingBody()
	timed := newProgressTimeoutBody(body, time.Second, cancel)
	if err := timed.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("Close() canceled the request context")
	default:
	}
}

func TestResponseHeaderTimeoutRejectsResponseReturnedAfterDeadline(t *testing.T) {
	body := io.NopCloser(strings.NewReader("late"))
	transport := newResponseHeaderTimeoutTransport(roundTripperFunc(
		func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return &http.Response{StatusCode: http.StatusOK, Body: body, Request: request}, nil
		},
	), 10*time.Millisecond)

	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://upstream.test", nil))
	if !errors.Is(err, context.DeadlineExceeded) || response != nil {
		t.Fatalf("RoundTrip() = response:%v error:%v, want nil/deadline exceeded", response, err)
	}
}
