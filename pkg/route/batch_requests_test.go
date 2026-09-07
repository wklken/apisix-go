package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
)

func TestRegisterExtraRoutesAddsBatchRequestsWhenEnabled(t *testing.T) {
	mux := chi.NewRouter()
	registerExtraRoutes(mux, &config.Config{Plugins: []string{"batch-requests"}})

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{
		"pipeline": [{"method": "GET", "path": "/hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want 404 without a public-api route; body=%s", res.Code, res.Body.String())
	}
}

func TestRegisterExtraRoutesRegistersBatchRequestsOnPublicAPI(t *testing.T) {
	staticConfig := &config.Config{Plugins: []string{"batch-requests"}}
	registry := public_api.NewRegistry()

	mux := chi.NewRouter()
	mux.Get("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Global", r.Header.Get("X-Global"))
		w.Header().Set("X-Seen-Item", r.Header.Get("X-Item"))
		_, _ = w.Write([]byte("hello " + r.URL.Query().Get("name") + " " + r.URL.Query().Get("token")))
	})
	mux.Post("/submit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})
	registerExtraRoutes(mux, staticConfig, registry)

	if handler := registry.Lookup(http.MethodPost, "/apisix/batch-requests"); handler == nil {
		t.Fatal("batch-requests handler is missing from public API registry")
	}

	p := newPublicAPITestPlugin(t, map[string]any{}, registry)
	mux.Method(http.MethodPost, "/apisix/batch-requests", p.Handler(http.NotFoundHandler()))

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{
		"query": {"token": "global"},
		"headers": {"X-Global": "yes"},
		"pipeline": [
			{"method": "GET", "path": "/hello", "query": {"name": "alice"}, "headers": {"X-Item": "one"}},
			{"method": "POST", "path": "/submit", "body": "payload"}
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
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 2 {
		t.Fatalf("response len = %d, want 2: %#v", len(body), body)
	}
	if body[0]["status"] != float64(http.StatusOK) {
		t.Fatalf("first status = %v, want 200", body[0]["status"])
	}
	if body[0]["body"] != "hello alice global" {
		t.Fatalf("first body = %q, want merged query response", body[0]["body"])
	}
	headers, ok := body[0]["headers"].(map[string]any)
	if !ok {
		t.Fatalf("first headers = %#v, want object", body[0]["headers"])
	}
	if headers["X-Seen-Global"] != "yes" {
		t.Fatalf("X-Seen-Global = %v, want yes", headers["X-Seen-Global"])
	}
	if headers["X-Seen-Item"] != "one" {
		t.Fatalf("X-Seen-Item = %v, want one", headers["X-Seen-Item"])
	}
	if body[1]["status"] != float64(http.StatusCreated) {
		t.Fatalf("second status = %v, want 201", body[1]["status"])
	}
	if body[1]["body"] != "created" {
		t.Fatalf("second body = %q, want created", body[1]["body"])
	}
}

func TestRegisterExtraRoutesRegistersCustomBatchURIOnly(t *testing.T) {
	registry := public_api.NewRegistry()
	mux := chi.NewRouter()
	registerExtraRoutes(mux, &config.Config{
		Plugins:    []string{"batch-requests"},
		PluginAttr: map[string]map[string]any{"batch-requests": {"uri": "/foo/bar"}},
	}, registry)

	req := httptest.NewRequest(http.MethodPost, "/foo/bar", strings.NewReader(`{"pipeline":[{"path":"/missing"}]}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("custom URI data-plane response = %d, want 404 without a public-api route", res.Code)
	}

	if registry.Lookup(http.MethodPost, "/apisix/batch-requests") != nil {
		t.Fatal("default URI must not stay registered when plugin_attr.uri is customized")
	}
	if registry.Lookup(http.MethodPost, "/foo/bar") == nil {
		t.Fatal("custom URI handler is missing from public API registry")
	}
}

func TestRegisterExtraRoutesSkipsBatchRequestsWhenDisabled(t *testing.T) {
	mux := chi.NewRouter()
	registerExtraRoutes(mux, &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{"pipeline":[]}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want 404", res.Code)
	}
}

func TestBatchRequestsRejectsInvalidBody(t *testing.T) {
	mux := chi.NewRouter()
	mountBatchRequestsPublicAPI(t, mux, &config.Config{Plugins: []string{"batch-requests"}})

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{"pipeline":[]}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", res.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body["error_msg"], "pipeline") {
		t.Fatalf("error_msg = %q, want pipeline validation error", body["error_msg"])
	}
}

func TestBatchRequestsParentCancellationStopsPipeline(t *testing.T) {
	var started atomic.Int32
	mux := chi.NewRouter()
	mux.Get("/slow", func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-r.Context().Done()
	})
	mountBatchRequestsPublicAPI(t, mux, &config.Config{Plugins: []string{"batch-requests"}})

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{
		"pipeline": [
			{"path": "/slow"},
			{"path": "/slow"}
		]
	}`))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	res := httptest.NewRecorder()

	served := make(chan struct{})
	go func() {
		mux.ServeHTTP(res, req)
		close(served)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for started.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("first pipeline request never reached the route")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("batch endpoint did not return after parent cancellation")
	}

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 1 || body[0]["status"] != float64(http.StatusGatewayTimeout) {
		t.Fatalf("pipeline responses = %#v, want one 504 for the canceled request", body)
	}
}

func mountBatchRequestsPublicAPI(t *testing.T, mux *chi.Mux, staticConfig *config.Config) *public_api.Registry {
	t.Helper()
	registry := public_api.NewRegistry()
	registerExtraRoutes(mux, staticConfig, registry)
	uri := batchRequestsURI(staticConfig)
	p := newPublicAPITestPlugin(t, map[string]any{}, registry)
	mux.Method(http.MethodPost, uri, p.Handler(http.NotFoundHandler()))
	return registry
}
