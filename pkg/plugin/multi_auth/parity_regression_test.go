package multi_auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/batch_requests"
)

func TestRegressionChallengeSurvivesNonChallengeFailure(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{
			AuthPlugins: []AuthPluginConfig{
				{"basic-auth": {"realm": "first"}},
				{"jwe-decrypt": {"header": "Authorization", "forward_header": "Authorization"}},
			},
		},
	)
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, newMultiAuthRequest())
	if rr.Code != 401 {
		t.Fatalf("status=%d", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Basic realm="first"` {
		t.Fatalf("challenge=%q; APISIX keeps Basic realm=first after later jwe-decrypt failure", got)
	}
}

func TestRegressionBatchAuthorizationReachesAuthenticator(t *testing.T) {
	addAuthConsumer(t, "item-user", map[string]any{"key-auth": map[string]any{"key": "item-key"}})
	p := newTestPlugin(
		t,
		Config{AuthPlugins: []AuthPluginConfig{{"key-auth": {"header": "Authorization"}}, {"basic-auth": {}}}},
	)
	handler := batch_requests.NewHandlerWithLimits(
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })),
		batch_requests.Limits{},
	)
	req := httptest.NewRequest(
		"POST",
		batch_requests.DefaultURI,
		strings.NewReader(`{"pipeline":[{"path":"/inner","headers":{"Authorization":"item-key"}}]}`),
	)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `"status":204`) {
		t.Fatalf("batch status=%d body=%s; want child authenticated by item Authorization", rr.Code, rr.Body.String())
	}
}

func TestRegressionChallengeSurvivesSuccessfulFallback(t *testing.T) {
	addAuthConsumer(t, "fallback-user", map[string]any{"key-auth": map[string]any{"key": "fallback-key"}})
	p := newTestPlugin(t, Config{AuthPlugins: []AuthPluginConfig{{"basic-auth": {"realm": "first"}}, {"key-auth": {}}}})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "fallback-key")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).
		ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || rr.Header().Get("WWW-Authenticate") != `Basic realm="first"` {
		t.Fatalf("response = %d %v", rr.Code, rr.Header())
	}
}
