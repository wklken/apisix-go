package proxy

import (
	"context"
	"net"
	"net/http"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type RetryTarget func(*http.Request) bool

type retryState struct {
	count    int
	next     RetryTarget
	attempts int
}

type retryTransport struct {
	base    http.RoundTripper
	observe func(string)
}

type retryContextKey struct{}

// WithRetries attaches the number of transport-error retries and the target
// selector that must run before each subsequent attempt. Requests that cannot
// be replayed are returned unchanged so they are never retried.
func WithRetries(request *http.Request, count int, next RetryTarget) *http.Request {
	if request == nil || count <= 0 || next == nil || !retryRequestAllowed(request) {
		return request
	}
	state := &retryState{count: count, next: next}
	return request.WithContext(context.WithValue(request.Context(), retryContextKey{}, state))
}

// retryRequestAllowed reports whether a request may be retried after a
// transport error. Safe methods retry when the body can be replayed or there
// is no body. POST and PATCH additionally require an idempotency key because
// replaying them without one can duplicate side effects.
func retryRequestAllowed(request *http.Request) bool {
	if request == nil {
		return false
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace,
		http.MethodPut, http.MethodDelete:
		return request.Body == nil || request.Body == http.NoBody || request.GetBody != nil
	case http.MethodPost, http.MethodPatch:
		if request.Method == http.MethodPost && request.GetBody != nil &&
			isGRPCContentType(request.Header.Get("Content-Type")) {
			return true
		}
		keyed := request.Header.Get("Idempotency-Key") != "" || request.Header.Get("X-Idempotency-Key") != ""
		return keyed && (request.Body == nil || request.Body == http.NoBody || request.GetBody != nil)
	default:
		return false
	}
}

func isGRPCContentType(value string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
	return mediaType == "application/grpc" || strings.HasPrefix(mediaType, "application/grpc+")
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
		response, err := transport.base.RoundTrip(request)
		transport.observeResult(err, false)
		return response, err
	}

	response, err := transport.base.RoundTrip(request)
	stopped := false
	for remaining := state.count; err != nil && remaining > 0; remaining-- {
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
		response, err = transport.base.RoundTrip(request)
	}
	transport.observeResult(err, stopped)
	return response, err
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
