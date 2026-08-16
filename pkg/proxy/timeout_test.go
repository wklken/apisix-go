package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingBody struct {
	closed chan struct{}
	once   sync.Once
}

type blockingDuplexWriteBody struct {
	closed chan struct{}
	once   sync.Once
}

type immediateDuplexBody struct{}

func (immediateDuplexBody) Read([]byte) (int, error) {
	return 0, nil
}

func (immediateDuplexBody) Write(payload []byte) (int, error) {
	return len(payload), nil
}

func (immediateDuplexBody) Close() error {
	return nil
}

func newBlockingDuplexWriteBody() *blockingDuplexWriteBody {
	return &blockingDuplexWriteBody{closed: make(chan struct{})}
}

func (b *blockingDuplexWriteBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingDuplexWriteBody) Write([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockingDuplexWriteBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type closeIdleRoundTripper struct {
	closed int
}

func (transport *closeIdleRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}

func (transport *closeIdleRoundTripper) CloseIdleConnections() {
	transport.closed++
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

func TestProgressTimeoutTransportForwardsCloseIdleConnections(t *testing.T) {
	base := &closeIdleRoundTripper{}
	transport := NewProgressTimeoutTransport(base, time.Second, time.Second)
	closer, ok := transport.(interface{ CloseIdleConnections() })
	if !ok {
		t.Fatal("progress timeout transport does not expose CloseIdleConnections")
	}

	closer.CloseIdleConnections()
	if base.closed != 1 {
		t.Fatalf("base CloseIdleConnections calls = %d, want 1", base.closed)
	}
}

func TestProgressTimeoutTransportPreservesDuplexBodyAndWriteTimeout(t *testing.T) {
	body := newControlledDuplexBody()
	transport := NewProgressTimeoutTransport(
		newResponseHeaderTimeoutTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: body, Request: request}, nil
		}), time.Second),
		0,
		20*time.Millisecond,
	)
	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://upstream.test/socket", nil))
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	duplex, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("response body type = %T, want io.ReadWriteCloser", response.Body)
	}
	if _, err := duplex.Write([]byte("frame")); err != nil {
		t.Fatalf("duplex Write() error = %v", err)
	}
	_, err = duplex.Read(make([]byte, 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duplex Read() error = %v, want context deadline exceeded", err)
	}
	_ = duplex.Close()
}

func TestProgressTimeoutTransportAppliesSendTimeoutToDuplexWrite(t *testing.T) {
	body := newBlockingDuplexWriteBody()
	transport := NewProgressTimeoutTransport(roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: body, Request: request}, nil
	}), 20*time.Millisecond, 0)
	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://upstream.test/socket", nil))
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	duplex, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("response body type = %T, want io.ReadWriteCloser", response.Body)
	}
	_, err = duplex.Write([]byte("stalled"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("duplex Write() error = %v, want context deadline exceeded", err)
	}
	_ = duplex.Close()
}

func TestProgressTimeoutDuplexBodyConcurrentReadWriteClose(t *testing.T) {
	const rounds = 64
	for range rounds {
		wrapped := wrapProgressTimeoutBody(immediateDuplexBody{}, time.Hour, time.Hour, func() {})
		duplex, ok := wrapped.(io.ReadWriteCloser)
		if !ok {
			t.Fatalf("wrapped body type = %T, want io.ReadWriteCloser", wrapped)
		}

		start := make(chan struct{})
		var group sync.WaitGroup
		group.Add(6)
		for range 2 {
			go func() {
				defer group.Done()
				<-start
				_, _ = duplex.Read(make([]byte, 1))
			}()
		}
		for range 2 {
			go func() {
				defer group.Done()
				<-start
				_, _ = duplex.Write([]byte("frame"))
			}()
		}
		for range 2 {
			go func() {
				defer group.Done()
				<-start
				_ = duplex.Close()
			}()
		}
		close(start)
		group.Wait()
		_ = duplex.Close()
	}
}

func TestProgressTimeoutDuplexBodyDoesNotRearmAfterClose(t *testing.T) {
	var timeoutCalls atomic.Int32
	wrapped := wrapProgressTimeoutBody(immediateDuplexBody{}, time.Hour, 10*time.Millisecond, func() {
		timeoutCalls.Add(1)
	})
	duplex, ok := wrapped.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("wrapped body type = %T, want io.ReadWriteCloser", wrapped)
	}
	if err := duplex.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := duplex.Write([]byte("after-close")); err != nil {
		t.Fatalf("Write() after Close() error = %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if calls := timeoutCalls.Load(); calls != 0 {
		t.Fatalf("timeout callback calls after Close() = %d, want 0", calls)
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
