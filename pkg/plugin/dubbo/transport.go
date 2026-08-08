// Package dubbo shares the Dubbo transport skeleton used by the dubbo-proxy
// and http-dubbo plugins. Framing and response decoding remain caller-owned
// adapters because the two plugins speak different Dubbo dialects.
package dubbo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/util"
)

// MaxRetries caps retry attempts beyond the first request.
const MaxRetries = 10

// Config carries the transport timeouts and dialect adapters.
type Config struct {
	ConnectTimeout time.Duration
	SendTimeout    time.Duration
	ReadTimeout    time.Duration
	// AcquireSlot is an optional pre-attempt gate, such as a per-target
	// concurrency slot.
	AcquireSlot func(ctx context.Context, target string) (bool, func())
	// DecodeResponse reads and decodes one upstream response.
	DecodeResponse func(conn net.Conn) (Response, error)
}

// Response is the decoded upstream result written back to the client.
type Response struct {
	Status  int
	Body    []byte
	Headers http.Header
}

// Result is the outcome of one transport attempt.
type Result struct {
	Response  Response
	Err       error
	Retryable bool
	// ConnectFailed marks an error from the connect step, before any request
	// bytes were written.
	ConnectFailed bool
}

// Attempt performs one transport attempt: dial, write the frame, and decode
// the response.
func Attempt(ctx context.Context, target string, cfg Config, frame []byte) Result {
	release := func() {}
	if cfg.AcquireSlot != nil {
		acquired, rel := cfg.AcquireSlot(ctx, target)
		if !acquired {
			return Result{Err: fmt.Errorf("dubbo upstream concurrency limit was canceled")}
		}
		release = rel
	}
	defer release()

	conn, err := (&net.Dialer{Timeout: cfg.ConnectTimeout}).DialContext(ctx, "tcp", target)
	if err != nil {
		return Result{
			Err:           fmt.Errorf("failed to connect to upstream: %w", err),
			Retryable:     true,
			ConnectFailed: true,
		}
	}
	defer func() { _ = conn.Close() }()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	if err := conn.SetWriteDeadline(Deadline(ctx, cfg.SendTimeout)); err != nil {
		return Result{Err: fmt.Errorf("failed to set upstream write deadline: %w", err), Retryable: true}
	}
	written, err := conn.Write(frame)
	if err != nil {
		return Result{Err: fmt.Errorf("failed to send Dubbo request: %w", err), Retryable: written == 0}
	}
	if written != len(frame) {
		return Result{Err: io.ErrShortWrite}
	}
	if err := conn.SetReadDeadline(Deadline(ctx, cfg.ReadTimeout)); err != nil {
		return Result{Err: fmt.Errorf("failed to set upstream read deadline: %w", err)}
	}
	response, err := cfg.DecodeResponse(conn)
	if err != nil {
		return Result{Err: fmt.Errorf("failed to read Dubbo response: %w", err)}
	}
	return Result{Response: response}
}

// ServeWithRetries retries only failures that happen before any request bytes
// are written. A Dubbo invocation may be non-idempotent, so a timeout or
// malformed response after a successful write must not issue it again.
func ServeWithRetries(r *http.Request, attempts int, attempt func() Result) Result {
	var result Result
	for range attempts {
		result = attempt()
		ReportOutcome(r, result.Response.Status, result.Err)
		if result.Err == nil || !result.Retryable {
			break
		}
		if r.Context().Err() != nil {
			break
		}
	}
	return result
}

// ReportOutcome reports the attempt result to the request metrics pipeline.
func ReportOutcome(r *http.Request, status int, err error) {
	if err == nil {
		pxy.ReportHTTPOutcome(r, status)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	var netErr net.Error
	pxy.ReportTCPFailureOutcome(r, errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()))
}

// ErrorStatus classifies a transport error into the HTTP status to return.
func ErrorStatus(ctx context.Context, err error) int {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// Deadline combines the transport timeout with an earlier request deadline.
func Deadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

type targetSlot struct {
	semaphore chan struct{}
}

var targetSlots sync.Map

// AcquireTargetSlot gates attempts per upstream target with a bounded
// semaphore shared across requests.
func AcquireTargetSlot(ctx context.Context, target string, limit int) (bool, func()) {
	if limit < 1 {
		limit = 32
	}
	key := target + "\x00" + strconv.Itoa(limit)
	value, _ := targetSlots.LoadOrStore(key, &targetSlot{semaphore: make(chan struct{}, limit)})
	slot := value.(*targetSlot)
	select {
	case slot.semaphore <- struct{}{}:
		return true, func() { <-slot.semaphore }
	case <-ctx.Done():
		return false, func() {}
	}
}

// WriteError writes a JSON error response.
func WriteError(w http.ResponseWriter, status int, message string) {
	_ = util.WriteJSONMessage(w, status, message)
}

// WriteResponse writes the decoded upstream response.
func WriteResponse(w http.ResponseWriter, response Response) {
	for field, values := range response.Headers {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(response.Status)
	if response.Body != nil {
		_, _ = w.Write(response.Body)
	}
}
