package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestPublicAPIExposesBatchRequestsAtCustomRoute(t *testing.T) {
	registry := public_api.NewRegistry()

	mux := chi.NewRouter()
	mux.Get("/hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello " + r.URL.Query().Get("name")))
	})
	registerExtraRoutes(mux, &config.Config{Plugins: []string{"batch-requests"}}, registry)

	p := newPublicAPITestPlugin(t, map[string]any{"uri": "/apisix/batch-requests"}, registry)
	mux.Method(http.MethodPost, "/batch", p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("public-api should dispatch to internal public API without calling next")
	})))

	req := httptest.NewRequest(http.MethodPost, "/batch", strings.NewReader(`{
		"pipeline": [
			{"method": "GET", "path": "/hello", "query": {"name": "alice"}}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()

	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("response len = %d, want 1", len(body))
	}
	if body[0]["status"] != float64(http.StatusOK) {
		t.Fatalf("subresponse status = %v, want 200", body[0]["status"])
	}
	if body[0]["body"] != "hello alice" {
		t.Fatalf("subresponse body = %q, want hello alice", body[0]["body"])
	}
}

func TestPublicAPIUsesRouteURIWhenConfigURIEmpty(t *testing.T) {
	registry := public_api.NewRegistry()

	mux := chi.NewRouter()
	registerExtraRoutes(mux, &config.Config{Plugins: []string{"node-status"}}, registry)
	p := newPublicAPITestPlugin(t, map[string]any{}, registry)
	mux.Method(
		http.MethodGet,
		"/apisix/status",
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("public-api should dispatch to internal public API without calling next")
		})),
	)

	req := httptest.NewRequest(http.MethodGet, "/apisix/status", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["id"] == "" {
		t.Fatalf("id = %v, want non-empty", body["id"])
	}
}

func TestPublicAPIReturnsNotFoundForUnknownInternalURI(t *testing.T) {
	registry := public_api.NewRegistry()

	p := newPublicAPITestPlugin(t, map[string]any{"uri": "/missing"}, registry)
	req := httptest.NewRequest(http.MethodGet, "/expose", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("public-api should not call next when no internal API matches")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want 404", res.Code)
	}
}

func TestPublicAPIExplicitlyExposesServerInfo(t *testing.T) {
	registry := public_api.NewRegistry()
	mux := chi.NewRouter()
	registerExtraRoutes(mux, &config.Config{
		Apisix: config.Apisix{ID: "server-info-public-id"}, Plugins: []string{"server-info"},
	}, registry)
	p := newPublicAPITestPlugin(t, map[string]any{"uri": "/v1/server_info"}, registry)
	mux.Method(http.MethodGet, "/expose-server-info", p.Handler(http.NotFoundHandler()))

	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/expose-server-info", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["id"] != "server-info-public-id" {
		t.Fatalf("id = %v, want server-info-public-id", body["id"])
	}
}

func newPublicAPITestPlugin(
	t *testing.T,
	cfg map[string]any,
	registries ...*public_api.Registry,
) *public_api.Plugin {
	t.Helper()

	p := &public_api.Plugin{}
	registry := public_api.NewRegistry()
	if len(registries) > 0 && registries[0] != nil {
		registry = registries[0]
	}
	p.SetPublicAPIRegistry(registry)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(cfg, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
