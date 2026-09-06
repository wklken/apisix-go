package limit_count

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
)

func TestParityCompoundDefaultKey(t *testing.T) {
	r := httptest.NewRequest("GET", "http://example.com/", nil)
	got := resolveLimitCountKey(r, "var_combination", "tenant:${http_x_tenant ?? anonymous}")
	if got != "tenant:anonymous" {
		t.Fatalf("key=%q, want tenant:anonymous", got)
	}
}

func TestParityDynamicCountUsesConsumedTotal(t *testing.T) {
	p := newTestPlugin(t, Config{Count: "$http_x_count", TimeWindow: 60, Key: "remote_addr"})
	p.SetRateLimitState(limitbase.NewState())
	h := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for i, v := range []string{"1", "3"} {
		r := httptest.NewRequest("GET", "http://example.com/", nil)
		r.Header.Set("X-Count", v)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf(
				"request%d with count%s status=%d remaining=%q, want204",
				i+1,
				v,
				w.Code,
				w.Header().Get("X-RateLimit-Remaining"),
			)
		}
	}
}
