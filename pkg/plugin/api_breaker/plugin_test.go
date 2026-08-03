package api_breaker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerResolvesBreakResponseHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusTooManyRequests,
		BreakResponseBody: new("blocked"),
		BreakResponseHeaders: []Header{
			{Key: "X-Break-Method", Value: "$request_method"},
			{Key: "X-Break-URI", Value: "$request_uri"},
			{Key: "X-Break-IP", Value: "$remote_addr"},
		},
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})

	nextCalls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/first", nil)
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusInternalServerError)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/blocked?x=1", nil)
	secondReq.RemoteAddr = "192.0.2.10:12345"
	handler.ServeHTTP(second, secondReq)
	result := second.Result()
	defer func() { _ = result.Body.Close() }()

	if result.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked response code = %d, want %d", result.StatusCode, http.StatusTooManyRequests)
	}
	if nextCalls != 1 {
		t.Fatalf("next calls = %d, want only the first request to reach upstream", nextCalls)
	}
	if got := result.Header.Get("X-Break-Method"); got != http.MethodPost {
		t.Fatalf("X-Break-Method = %q, want %q", got, http.MethodPost)
	}
	if got := result.Header.Get("X-Break-URI"); got != "/blocked?x=1" {
		t.Fatalf("X-Break-URI = %q, want /blocked?x=1", got)
	}
	if got := result.Header.Get("X-Break-IP"); got != "192.0.2.10" {
		t.Fatalf("X-Break-IP = %q, want 192.0.2.10", got)
	}
}

func TestHandlerHealthySuccessesClearAccumulatedFailures(t *testing.T) {
	p := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		MaxBreakerSec:     10,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(3),
		},
		Healthy: HealthCheck{
			HTTPStatuses: []int{http.StatusNoContent},
			Successes:    new(3),
		},
	})

	statuses := []int{
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusNoContent,
	}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := statuses[0]
		statuses = statuses[1:]
		w.WriteHeader(status)
	}))

	wants := []int{500, 500, 204, 204, 204, 500, 500, 204}
	for i, want := range wants {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api", nil))
		if response.Code != want {
			t.Fatalf("response %d code = %d, want %d", i+1, response.Code, want)
		}
	}
}

func TestHandlerUsesExponentialBreakerWindowCappedByConfiguration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusBadGateway,
		MaxBreakerSec:     10,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})
	p.now = func() time.Time { return now }
	upstreamCalls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))

	request := func(want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api", nil))
		if response.Code != want {
			t.Fatalf("response code = %d, want %d at %s", response.Code, want, now)
		}
	}
	request(500)
	now = now.Add(time.Second)
	request(502)
	now = now.Add(1100 * time.Millisecond)
	request(500)
	now = now.Add(3 * time.Second)
	request(502)
	now = now.Add(1100 * time.Millisecond)
	request(500)
	now = now.Add(8100 * time.Millisecond)
	request(500)
	if upstreamCalls != 4 {
		t.Fatalf("upstream calls = %d, want 4", upstreamCalls)
	}
}
