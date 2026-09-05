package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type RetryTarget func(*http.Request) bool

type retryState struct {
	count    int
	next     RetryTarget
	attempts int
	deadline time.Time
}

type retryTransport struct {
	base    http.RoundTripper
	observe func(string)
}

type retryContextKey struct{}

type upstreamStatusState struct {
	statuses []int
}

type upstreamStatusContextKey struct{}

// WithRetries attaches the number of transport-error retries and the target
// selector that must run before each subsequent attempt. Requests that cannot
// be replayed are returned unchanged so they are never retried.
func WithRetries(request *http.Request, count int, next RetryTarget) *http.Request {
	return WithRetriesTimeout(request, count, 0, next)
}

// WithRetriesTimeout attaches retries with APISIX retry_timeout semantics. The
// timeout bounds when another attempt may start; it does not cancel an attempt
// that is already in flight.
func WithRetriesTimeout(
	request *http.Request,
	count int,
	timeout time.Duration,
	next RetryTarget,
) *http.Request {
	if request == nil || count <= 0 || next == nil || !retryRequestReplayable(request) {
		return request
	}
	state := &retryState{count: count, next: next}
	if timeout > 0 {
		state.deadline = time.Now().Add(timeout)
	}
	return request.WithContext(context.WithValue(request.Context(), retryContextKey{}, state))
}

// retryRequestReplayable only checks whether another attempt can obtain the
// body. Non-idempotent methods additionally depend on per-attempt send progress.
func retryRequestReplayable(request *http.Request) bool {
	return request != nil && request.Method != http.MethodConnect &&
		(request.Body == nil || request.Body == http.NoBody || request.GetBody != nil)
}

func nonIdempotentUpstreamMethod(method string) bool {
	// NGINX's default proxy_next_upstream excludes these methods once sent.
	return method == http.MethodPost || method == http.MethodPatch || method == "LOCK"
}

// roundTripAttempt tracks entering the write phase, including partial writes.
// Dial/TLS failures have not entered this phase and may fail over for any method.
func (transport *retryTransport) roundTripAttempt(request *http.Request) (*http.Response, error, bool) {
	if !nonIdempotentUpstreamMethod(request.Method) {
		response, err := transport.base.RoundTrip(request)
		return response, err, false
	}
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() { sent.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
	}
	attempt := request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	// net/http recognizes these exact canonical map keys as permission to
	// replay a sent request on a pooled connection. Preserve the wire headers
	// using legal lowercase field names, without granting that permission.
	// The pooled-connection regression guards this Go transport assumption.
	for _, key := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		if _, ok := attempt.Header[key]; !ok {
			continue
		}
		attempt.Header = attempt.Header.Clone()
		lower := strings.ToLower(key)
		attempt.Header[lower] = append(attempt.Header[lower], attempt.Header[key]...)
		delete(attempt.Header, key)
	}
	response, err := transport.base.RoundTrip(attempt)
	return response, err, sent.Load()
}

// NewRetryTransport wraps a transport with bounded retries for connection and
// other RoundTrip errors. HTTP responses are returned without retrying.
func NewRetryTransport(base http.RoundTripper) http.RoundTripper {
	return NewRetryTransportWithObserver(base, nil)
}

// NewRetryTransportWithObserver wraps a transport with bounded retries and
// reports every terminal outcome as one of the bounded labels success, error,
// or stopped. Raw error text is never used as a label value. A nil observer
// makes this identical to NewRetryTransport.
func NewRetryTransportWithObserver(base http.RoundTripper, observe func(string)) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base, observe: observe}
}

func (transport *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	state, _ := request.Context().Value(retryContextKey{}).(*retryState)
	if state == nil {
		response, err, _ := transport.roundTripAttempt(request)
		recordUpstreamTransportFailure(request, err)
		transport.observeResult(err, false)
		return response, err
	}

	response, err, sent := transport.roundTripAttempt(request)
	recordUpstreamTransportFailure(request, err)
	stopped := false
	for remaining := state.count; err != nil && remaining > 0; remaining-- {
		if sent {
			stopped = true
			break
		}
		if !state.deadline.IsZero() && !time.Now().Before(state.deadline) {
			stopped = true
			break
		}
		if request.Context().Err() != nil || !resetRequestBody(request) {
			stopped = true
			break
		}
		reportRetryFailure(request, err)
		if !state.next(request) {
			stopped = true
			break
		}
		state.attempts++
		apisixctx.RegisterRequestVar(request, "$retry_count", state.attempts)
		response, err, sent = transport.roundTripAttempt(request)
		recordUpstreamTransportFailure(request, err)
	}
	transport.observeResult(err, stopped)
	return response, err
}

// RecordUpstreamStatus appends one upstream attempt status to the request-local
// status chain used by APISIX variables and response headers.
func RecordUpstreamStatus(request *http.Request, status int) {
	if request == nil || status <= 0 {
		return
	}
	state, ok := request.Context().Value(upstreamStatusContextKey{}).(*upstreamStatusState)
	if !ok {
		state = &upstreamStatusState{}
		*request = *request.WithContext(context.WithValue(request.Context(), upstreamStatusContextKey{}, state))
	}
	state.statuses = append(state.statuses, status)
}

// UpstreamStatusChain returns the NGINX-compatible comma-separated statuses
// from every upstream attempt made for this request.
func UpstreamStatusChain(request *http.Request) string {
	if request == nil {
		return ""
	}
	state, ok := request.Context().Value(upstreamStatusContextKey{}).(*upstreamStatusState)
	if !ok || len(state.statuses) == 0 {
		return ""
	}
	values := make([]string, 0, len(state.statuses))
	for _, status := range state.statuses {
		values = append(values, strconv.Itoa(status))
	}
	return strings.Join(values, ", ")
}

func recordUpstreamTransportFailure(request *http.Request, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		RecordUpstreamStatus(request, http.StatusGatewayTimeout)
		return
	}
	RecordUpstreamStatus(request, http.StatusBadGateway)
}

func (transport *retryTransport) observeResult(err error, stopped bool) {
	if transport.observe == nil {
		return
	}
	switch {
	case stopped:
		transport.observe("stopped")
	case err != nil:
		transport.observe("error")
	default:
		transport.observe("success")
	}
}

func resetRequestBody(request *http.Request) bool {
	if request.Body == nil || request.Body == http.NoBody {
		return true
	}
	if request.GetBody == nil {
		return false
	}
	body, err := request.GetBody()
	if err != nil {
		return false
	}
	request.Body = body
	return true
}

func reportRetryFailure(request *http.Request, err error) {
	if err == nil || err == context.Canceled {
		return
	}
	if netErr, ok := err.(net.Error); ok {
		ReportTCPFailureOutcome(request, netErr.Timeout())
		return
	}
	ReportTCPFailureOutcome(request, false)
}
