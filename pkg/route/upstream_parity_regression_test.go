package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestParityUnselectableUpstreamReturns503(t *testing.T) {
	for _, nodes := range []string{`[]`, `{"127.0.0.1:1":0}`, `[{"host":"127.0.0.1","port":1,"weight":0}]`} {
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
