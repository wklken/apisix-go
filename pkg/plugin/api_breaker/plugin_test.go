package api_breaker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sony/gobreaker"
)

//go:fix inline
func intPtr(value int) *int {
	return new(value)
}

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
	defer result.Body.Close()

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

func TestHandlerUsesConfiguredHealthyStatusesForRecovery(t *testing.T) {
	p := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		MaxBreakerSec:     1,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
		Healthy: HealthCheck{
			HTTPStatuses: []int{http.StatusNoContent},
			Successes:    new(1),
		},
	})

	statuses := []int{
		http.StatusInternalServerError,
		http.StatusCreated,
		http.StatusNoContent,
	}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := statuses[0]
		statuses = statuses[1:]
		w.WriteHeader(status)
	}))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api", nil))
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusInternalServerError)
	}
	if state := p.cb.State(); state != gobreaker.StateOpen {
		t.Fatalf("state after unhealthy response = %s, want open", state)
	}

	time.Sleep(1100 * time.Millisecond)
	neutral := httptest.NewRecorder()
	handler.ServeHTTP(neutral, httptest.NewRequest(http.MethodGet, "/api", nil))
	if neutral.Code != http.StatusCreated {
		t.Fatalf("neutral response code = %d, want %d", neutral.Code, http.StatusCreated)
	}
	if state := p.cb.State(); state != gobreaker.StateHalfOpen {
		t.Fatalf("state after neutral response = %s, want half-open", state)
	}

	healthy := httptest.NewRecorder()
	handler.ServeHTTP(healthy, httptest.NewRequest(http.MethodGet, "/api", nil))
	if healthy.Code != http.StatusNoContent {
		t.Fatalf("healthy response code = %d, want %d", healthy.Code, http.StatusNoContent)
	}
	if state := p.cb.State(); state != gobreaker.StateClosed {
		t.Fatalf("state after configured healthy response = %s, want closed", state)
	}
}
