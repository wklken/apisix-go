package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewWeightedRRLoadBalanceFirstPickIsDeterministic(t *testing.T) {
	servers := map[string]int{
		"traffic-split-0-1": 2,
		"traffic-split-0-0": 2,
		"traffic-split-0-2": 1,
	}

	first := ""
	for iteration := range 50 {
		lb := NewWeightedRRLoadBalance(servers)
		got := lb.Next()
		if first == "" {
			first = got
		}
		if got != first {
			t.Fatalf("iteration %d: Next() = %q, want stable first pick %q", iteration, got, first)
		}
	}
	if first != "traffic-split-0-0" {
		t.Fatalf("first pick = %q, want traffic-split-0-0 (sorted key order)", first)
	}
}

func TestNewUpstreamLoadBalanceRejectsEmptyPool(t *testing.T) {
	if _, err := NewUpstreamLoadBalance(map[string]int{}, nil); err == nil {
		t.Fatal("NewUpstreamLoadBalance() error = nil for an empty node pool")
	}
}

func TestEmptyRoundRobinPickerReturnsEmptyTarget(t *testing.T) {
	lb := NewWeightedRRLoadBalance(map[string]int{})
	if got := lb.Next(); got != "" {
		t.Fatalf("empty picker Next() = %q, want empty target without panic", got)
	}
}

var errDirectorTargetSelection = errors.New("target selection failed")

type directorErrorContextKey struct{}

func withTestDirectorError(request *http.Request, err error) *http.Request {
	return request.WithContext(context.WithValue(request.Context(), directorErrorContextKey{}, err))
}

type writeRecordingResponseWriter struct {
	header http.Header
	status int
	writes int
}

func (w *writeRecordingResponseWriter) Header() http.Header { return w.header }
func (w *writeRecordingResponseWriter) WriteHeader(status int) {
	w.writes++
	if w.status == 0 {
		w.status = status
	}
}

func (w *writeRecordingResponseWriter) Write(body []byte) (int, error) {
	w.writes++
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(body), nil
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("partial")),
		Request:    request,
	}, errDirectorTargetSelection
}

func TestDirectorErrorAfterWrite(t *testing.T) {
	// A director error surfaces after the first attempt already started a
	// response (RoundTrip returned both a response and an error) and the
	// retry selector then fails. The proxy must classify the director error
	// exactly once: no panic and no second write.
	director := func(request *http.Request) {
		*request = *withTestDirectorError(request, errDirectorTargetSelection)
		request.URL.Scheme = ""
		request.URL.Host = ""
	}
	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		if !errors.Is(err, errDirectorTargetSelection) {
			t.Errorf("error handler received %v, want the director error", err)
		}
		w.WriteHeader(http.StatusBadGateway)
	}
	handler := NewProxyHandler(NewRetryTransport(failingRoundTripper{}), director, nil, errorHandler)

	request := httptest.NewRequest(http.MethodGet, "http://origin.example/", nil)
	request = WithRetries(request, 2, func(retry *http.Request) bool { return false })
	writer := &writeRecordingResponseWriter{header: http.Header{}}
	handler.ServeHTTP(writer, request)

	if writer.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", writer.status, http.StatusBadGateway)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d, want exactly one response write", writer.writes)
	}
}
