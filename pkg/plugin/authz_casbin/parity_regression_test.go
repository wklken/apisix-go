package authz_casbin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityMetadataSuppliesIncompleteRoutePair(t *testing.T) {
	for _, field := range []string{"model", "policy", "model_path", "policy_path"} {
		t.Run(field, func(t *testing.T) {
			config := map[string]any{"username": "X-User", field: "unused-partial-value"}
			p := &Plugin{}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := util.Validate(config, p.GetSchema()); err != nil {
				t.Fatal(err)
			}
			var cfg Config
			if err := util.Parse(config, &cfg); err != nil {
				t.Fatal(err)
			}
			p = newTestPluginWithMetadata(t, cfg, testModel, "p, alice, /data1, GET")
			request := httptest.NewRequest("GET", "/data1", nil)
			request.Header.Set("X-User", "alice")
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).
				ServeHTTP(response, request)
			if response.Code != 204 {
				t.Fatalf("status=%d", response.Code)
			}
		})
	}
}
