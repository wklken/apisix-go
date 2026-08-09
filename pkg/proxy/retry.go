package proxy

import (
	"context"
	"net"
	"net/http"
)

type RetryTarget func(*http.Request) bool

type retryState struct {
	count int
	next  RetryTarget
}

type retryTransport struct {
	base http.RoundTripper
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
		keyed := request.Header.Get("Idempotency-Key") != "" || request.Header.Get("X-Idempotency-Key") != ""
		return keyed && (request.Body == nil || request.Body == http.NoBody || request.GetBody != nil)
	default:
		return false
	}
}

// NewRetryTransport wraps a transport with bounded retries for connection and
// other RoundTrip errors. HTTP responses are returned without retrying.
func NewRetryTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base}
}

func (transport *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	state, _ := request.Context().Value(retryContextKey{}).(*retryState)
	if state == nil {
		return transport.base.RoundTrip(request)
	}

	response, err := transport.base.RoundTrip(request)
	for remaining := state.count; err != nil && remaining > 0; remaining-- {
		if request.Context().Err() != nil || !resetRequestBody(request) {
			return response, err
		}
		reportRetryFailure(request, err)
		if !state.next(request) {
			return response, err
		}
		response, err = transport.base.RoundTrip(request)
	}
	return response, err
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
