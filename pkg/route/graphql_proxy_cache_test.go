package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
)

func TestRegisterExtraRoutesExposesGraphQLProxyCachePurge(t *testing.T) {
	oldConfig := config.GlobalConfig
	config.GlobalConfig = &config.Config{Plugins: []string{"graphql-proxy-cache"}}
	t.Cleanup(func() { config.GlobalConfig = oldConfig })

	mux := chi.NewRouter()
	registerExtraRoutes(mux)
	req := httptest.NewRequest(
		"PURGE",
		"/apisix/plugin/graphql-proxy-cache/invalid/route-1/cache-key",
		nil,
	)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
}
