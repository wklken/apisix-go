package dubbo

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeadlinePrefersEarlierContextDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancel()

	if got := Deadline(ctx, 30*time.Second); !got.Before(time.Now().Add(2 * time.Second)) {
		t.Fatalf("Deadline() = %v, want the context deadline to win", got)
	}
	if got := Deadline(context.Background(), 30*time.Second); !got.After(time.Now().Add(29 * time.Second)) {
		t.Fatalf("Deadline() = %v, want the transport timeout to win", got)
	}
}

func TestErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		ctx  context.Context
		want int
	}{
		{name: "canceled context", ctx: context.Background(), err: context.Canceled, want: http.StatusGatewayTimeout},
		{
			name: "deadline exceeded",
			ctx:  context.Background(),
			err:  context.DeadlineExceeded,
			want: http.StatusGatewayTimeout,
		},
		{
			name: "net timeout",
			ctx:  context.Background(),
			err:  &net.DNSError{IsTimeout: true},
			want: http.StatusGatewayTimeout,
		},
		{name: "plain error", ctx: context.Background(), err: errors.New("boom"), want: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ErrorStatus(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("ErrorStatus() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestServeWithRetriesTargetOrder(t *testing.T) {
	var targets []string
	var calls atomic.Int32
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	result := ServeWithRetries(r, 3, func() Result {
		calls.Add(1)
		return Result{Err: errors.New("connect"), Retryable: true, ConnectFailed: true}
	})
	if result.Err == nil || !result.Retryable {
		t.Fatal("expected retryable failure result")
	}
	if calls.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", calls.Load())
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %v", targets)
	}

	ServeWithRetries(r, 3, func() Result {
		targets = append(targets, "t1")
		calls.Add(1)
		return Result{Err: errors.New("connect"), Retryable: true, ConnectFailed: true}
	})
	_ = targets
}

func TestServeWithRetriesStopsAfterSuccessfulAttempt(t *testing.T) {
	var calls atomic.Int32
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	result := ServeWithRetries(r, 5, func() Result {
		calls.Add(1)
		if calls.Load() == 2 {
			return Result{Response: Response{Status: http.StatusOK, Body: []byte("ok")}}
		}
		return Result{Err: errors.New("connect"), Retryable: true, ConnectFailed: true}
	})
	if result.Err != nil || result.Response.Status != http.StatusOK {
		t.Fatalf("result = %+v, want success", result)
	}
	if calls.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", calls.Load())
	}
}

func TestServeWithRetriesDoesNotRetryAfterRequestWrite(t *testing.T) {
	var calls atomic.Int32
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	result := ServeWithRetries(r, 3, func() Result {
		calls.Add(1)
		return Result{Err: errors.New("read failed"), Retryable: false}
	})
	if result.Err == nil || result.Retryable {
		t.Fatal("expected non-retryable failure result")
	}
	if calls.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", calls.Load())
	}
}

func TestServeWithRetriesStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	var calls atomic.Int32
	ServeWithRetries(r, 3, func() Result {
		calls.Add(1)
		return Result{Err: errors.New("connect"), Retryable: true, ConnectFailed: true}
	})
	if calls.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", calls.Load())
	}
}

func TestAcquireTargetSlotBoundedAndReleased(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	acquired, release := AcquireTargetSlot(ctx, "upstream:8080", 1)
	if !acquired {
		t.Fatal("first slot not acquired")
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer secondCancel()
	second, _ := AcquireTargetSlot(secondCtx, "upstream:8080", 1)
	if second {
		t.Fatal("second slot acquired with limit 1")
	}
	release()
	third, thirdRelease := AcquireTargetSlot(ctx, "upstream:8080", 1)
	if !third {
		t.Fatal("slot not reusable after release")
	}
	thirdRelease()
}

func TestAcquireTargetSlotSeparateTargets(t *testing.T) {
	ctx := context.Background()
	first, releaseFirst := AcquireTargetSlot(ctx, "a:8080", 1)
	if !first {
		t.Fatal("first target slot not acquired")
	}
	second, releaseSecond := AcquireTargetSlot(ctx, "b:8080", 1)
	if !second {
		t.Fatal("second target slot not acquired")
	}
	releaseFirst()
	releaseSecond()
}

func TestAcquireTargetSlotCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, release := AcquireTargetSlot(ctx, "upstream:8080", 1)
	cancel()
	acquired, _ := AcquireTargetSlot(ctx, "upstream:8080", 1)
	if acquired {
		t.Fatal("slot acquired on canceled context")
	}
	release()
}

func TestWriteErrorWritesJSONMessage(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusBadGateway, "boom")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"message":"boom"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteResponseWritesHeadersStatusAndBody(t *testing.T) {
	w := httptest.NewRecorder()
	WriteResponse(w, Response{
		Status:  http.StatusCreated,
		Body:    []byte("payload"),
		Headers: http.Header{"X-Upstream": {"dubbo"}},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("X-Upstream"); got != "dubbo" {
		t.Fatalf("X-Upstream = %q", got)
	}
	if got := w.Body.String(); got != "payload" {
		t.Fatalf("body = %q", got)
	}
}

func TestAttemptRoundTripOverTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	var wg sync.WaitGroup
	wg.Go(func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		frame := make([]byte, 16)
		if _, err := io.ReadFull(conn, frame); err != nil {
			return
		}
		_, _ = conn.Write([]byte("response-bytes"))
	})

	cfg := Config{
		ConnectTimeout: time.Second,
		SendTimeout:    time.Second,
		ReadTimeout:    time.Second,
		DecodeResponse: func(conn net.Conn) (Response, error) {
			body, err := io.ReadAll(conn)
			return Response{Status: http.StatusOK, Body: body}, err
		},
	}
	result := Attempt(context.Background(), listener.Addr().String(), cfg, make([]byte, 16))
	wg.Wait()
	if result.Err != nil {
		t.Fatalf("Attempt() error = %v", result.Err)
	}
	if got := string(result.Response.Body); got != "response-bytes" {
		t.Fatalf("response body = %q", got)
	}
}

func TestAttemptConnectFailureIsRetryableAndMarked(t *testing.T) {
	result := Attempt(context.Background(), "127.0.0.1:1", Config{
		ConnectTimeout: 100 * time.Millisecond,
		SendTimeout:    100 * time.Millisecond,
		ReadTimeout:    100 * time.Millisecond,
	}, []byte{})
	if result.Err == nil {
		t.Fatal("Attempt() error = nil")
	}
	if !result.Retryable || !result.ConnectFailed {
		t.Fatalf("result = %+v, want retryable connect failure", result)
	}
}

func TestAttemptHonorsConcurrencySlot(t *testing.T) {
	cfg := Config{
		ConnectTimeout: time.Second,
		SendTimeout:    time.Second,
		ReadTimeout:    time.Second,
		AcquireSlot: func(ctx context.Context, target string) (bool, func()) {
			return false, func() {}
		},
	}
	result := Attempt(context.Background(), "upstream:8080", cfg, []byte{})
	if result.Err == nil || result.Err.Error() != "dubbo upstream concurrency limit was canceled" {
		t.Fatalf("result = %+v, want canceled concurrency error", result)
	}
}

func TestWriteResponseRejectsNonTerminalStatus(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	WriteResponse(rr, Response{Status: 600, Body: []byte("should not write")})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

func TestWriteResponseWritesTerminalStatus(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	WriteResponse(rr, Response{Status: http.StatusCreated, Body: []byte("ok")})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if got := rr.Body.String(); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
}
