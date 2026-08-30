package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	graphql_proxy_cache "github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestCompileHTTPDoesNotObserveInputMutation(t *testing.T) {
	t.Parallel()

	input := CompileInput{
		Revision: 7,
		Routes: []PreparedRoute{{
			Route: resource.Route{ID: "r1", Uri: "/before"}, Hosts: []string{"service.example"},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}),
		}},
	}
	snapshot, err := CompileHTTP(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	input.Routes[0].Route.Uri = "/after"
	input.Routes[0].Hosts[0] = "mutated.example"
	request := httptest.NewRequest(http.MethodGet, "http://service.example/before", nil)
	response := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("service-inherited host status = %d, want %d", response.Code, http.StatusNoContent)
	}
	assertCompiledHTTPStatus(t, snapshot.Handler(), http.MethodGet, "/before", http.StatusNotFound)
	assertCompiledHTTPStatus(t, snapshot.Handler(), http.MethodGet, "/after", http.StatusNotFound)
}

func TestCompileHTTPRejectsIncompleteInput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		ctx   context.Context
		input CompileInput
	}{
		{name: "nil context", input: CompileInput{Revision: 1}},
		{name: "zero revision", ctx: context.Background()},
		{
			name: "missing route id", ctx: context.Background(),
			input: CompileInput{Revision: 1, Routes: []PreparedRoute{{
				Route: resource.Route{Uri: "/missing-id"}, Handler: http.NotFoundHandler(),
			}}},
		},
		{
			name: "nil handler", ctx: context.Background(),
			input: CompileInput{Revision: 1, Routes: []PreparedRoute{{
				Route: resource.Route{ID: "r1", Uri: "/nil"},
			}}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := CompileHTTP(test.ctx, test.input); err == nil {
				t.Fatal("CompileHTTP() error = nil")
			}
		})
	}
}

func TestCompileHTTPBindsGenerationGraphQLPurgeRegistry(t *testing.T) {
	registry := graphql_proxy_cache.NewRegistry()
	plugin := &graphql_proxy_cache.Plugin{}
	plugin.Config().(*graphql_proxy_cache.Config).CacheStrategy = "memory"
	plugin.SetConfiguredZones(nil)
	plugin.SetDependencies(base.Dependencies{Config: &appconfig.EffectiveConfig{}})
	plugin.SetPurgeRegistry(registry)
	if err := plugin.Init(); err != nil {
		t.Fatal(err)
	}
	plugin.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})
	if err := plugin.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plugin.Stop)

	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision:                  1,
		StaticConfig:              &appconfig.Config{Plugins: []string{"graphql-proxy-cache"}},
		PublicAPIRegistry:         public_api.NewRegistry(),
		GraphQLProxyCacheRegistry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		"PURGE",
		"/apisix/plugin/graphql-proxy-cache/memory/route-1/cache-key",
		nil,
	)
	response := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL purge status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestCompileHTTPBindsBatchRequestsGenerationMetadata(t *testing.T) {
	metadata, err := runtime.NewMetadataView(map[string][]byte{
		"batch-requests": []byte(`{"max_pipeline_items":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision:                  1,
		StaticConfig:              &appconfig.Config{Plugins: []string{"batch-requests"}},
		Metadata:                  metadata,
		PublicAPIRegistry:         public_api.NewRegistry(),
		GraphQLProxyCacheRegistry: graphql_proxy_cache.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"/apisix/batch-requests",
		strings.NewReader(`{"pipeline":[{"path":"/one"},{"path":"/two"}]}`),
	)
	response := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want 400; body=%q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "exceeds the maximum of 1") {
		t.Fatalf("response body = %q, want generation metadata limit", response.Body.String())
	}
}

func TestCompileHTTPBindsSharedServerInfoView(t *testing.T) {
	view := server_info.NewView("node-test")
	registry := public_api.NewRegistry()
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision:                  1,
		StaticConfig:              &appconfig.Config{Plugins: []string{"server-info"}},
		PublicAPIRegistry:         registry,
		GraphQLProxyCacheRegistry: graphql_proxy_cache.NewRegistry(),
		ServerInfo:                view,
	})
	if err != nil {
		t.Fatal(err)
	}
	view.SetEtcdVersion("3.6.13")

	request := httptest.NewRequest(http.MethodGet, "/v1/server_info", nil)
	response := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("data-plane server-info response = %d, want 404", response.Code)
	}
	handler := registry.Lookup(http.MethodGet, "/v1/server_info")
	if handler == nil {
		t.Fatal("compiled server-info handler is missing from public API registry")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"etcd_version":"3.6.13"`) {
		t.Fatalf("server-info response = %d %s, want resolved etcd version", response.Code, response.Body.String())
	}
}

func assertCompiledHTTPStatus(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	want int,
) {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s status = %d, want %d", method, path, response.Code, want)
	}
}
