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
