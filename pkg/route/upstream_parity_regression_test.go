package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestParityUnselectableUpstreamReturns503(t *testing.T) {
	for _, nodes := range []string{`[]`} {
		t.Run(nodes, func(t *testing.T) {
			route := testRouteFromJSON(t, `{"id":"no-live-nodes","uri":"/","upstream":{"nodes":`+nodes+`}}`)
			handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
			if response.Code != 503 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("X-APISIX-Upstream-Status"); got != "" {
				t.Fatalf("unattempted upstream status=%q", got)
			}
		})
	}
}

func TestSingleZeroWeightUpstreamIsStillAttempted(t *testing.T) {
	upstream := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	)
	defer upstream.Close()
	route := testRouteFromJSON(
		t,
		fmt.Sprintf(`{"id":"zero-weight","uri":"/","upstream":{"nodes":{%q:0}}}`, upstream.Listener.Addr().String()),
	)
	handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
