package api_breaker

import (
	"net/http/httptest"
	"testing"
)

func TestParityRepeatedBreakHeaders(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{
			BreakResponseCode:    503,
			BreakResponseBody:    new("blocked"),
			BreakResponseHeaders: []Header{{Key: "Set-Cookie", Value: "a=1"}, {Key: "Set-Cookie", Value: "b=2"}},
			Unhealthy:            UnHealthCheck{Failures: new(1)},
		},
	)
	r := httptest.NewRequest("GET", "http://example.com/", nil)
	p.observeStatus(breakerKeyForRequest(r), 500)
	w := httptest.NewRecorder()
	p.RunRequestPhase(w, r)
	if got := w.Header().Values("Set-Cookie"); len(got) != 2 {
		t.Fatalf("cookies=%v, want [a=1 b=2]", got)
	}
}

func TestParityRepeatedBreakContentType(t *testing.T) {
	for _, requestPhase := range []bool{false, true} {
		p := newTestPlugin(t, Config{
			BreakResponseCode: 503,
			BreakResponseBody: new("blocked"),
			BreakResponseHeaders: []Header{
				{Key: "Content-Type", Value: "application/json"},
				{Key: "content-type", Value: "application/json+v1"},
			},
			Unhealthy: UnHealthCheck{Failures: new(1)},
		})
		r := httptest.NewRequest("GET", "http://example.com/", nil)
		p.observeStatus(breakerKeyForRequest(r), 500)
		w := httptest.NewRecorder()
		if requestPhase {
			p.RunRequestPhase(w, r)
		} else {
			p.Handler(nil).ServeHTTP(w, r)
		}
		if got := w.Header().Values("Content-Type"); len(got) != 1 || got[0] != "application/json+v1" {
			t.Fatalf("requestPhase=%v content-type=%v, want [application/json+v1]", requestPhase, got)
		}
	}
}
