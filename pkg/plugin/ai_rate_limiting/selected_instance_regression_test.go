package ai_rate_limiting

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestWithoutPickedAIInstanceDoesNotLimitOrCharge(t *testing.T) {
	for _, cfg := range []Config{
		{Limit: 1, TimeWindow: 60},
		{Rules: []Rule{{Count: 1, TimeWindow: 60, Key: "$http_user"}}},
	} {
		p := newTestPlugin(t, cfg, time.Now)
		for index := range 3 {
			request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/ai", nil))
			request.Header.Set("User", "one")
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"usage":{"total_tokens":3}}`))
			})).ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("request %d status=%d body=%q; want no quota", index, response.Code, response.Body.String())
			}
			for key := range response.Header() {
				if strings.HasPrefix(key, "X-Ai-") {
					t.Fatalf("unexpected quota header %s", key)
				}
			}
		}
		if got := localCounterUsed(t, p, "global", 1); got != 0 {
			t.Fatalf("charged without instance = %d", got)
		}
	}
}

func TestDefaultAIQuotaRejectionIsStatusOnly(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 1, TimeWindow: 60}, time.Now)
	response := httptest.NewRecorder()
	p.reject(response)
	if response.Code != 503 || response.Body.Len() != 0 || response.Header().Get("Content-Type") != "" {
		t.Fatalf(
			"rejection=%d %q %q; want status-only 503",
			response.Code,
			response.Header().Get("Content-Type"),
			response.Body.String(),
		)
	}
}

// Quota behavior tests model the selected AI instance published before access.
func newSelectedAIRequest(method, target string, body io.Reader) *http.Request {
	return WithPickedAIInstanceName(httptest.NewRequest(method, target, body), "global")
}
