package graphql_proxy_cache

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerCachesGraphQLPOSTResponses(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Origin", "upstream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"persons":[]}}`))
	}))

	first := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/json",
		`{"query":"query { persons { name } }"}`,
	)
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get(cacheStatusHeader); got != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", got)
	}
	cacheKey := first.Header().Get(cacheKeyHeader)
	if cacheKey == "" {
		t.Fatal("first APISIX-Cache-Key is empty")
	}

	second := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/json",
		`{"query":"query { persons { name } }"}`,
	)
	if got := second.Header().Get(cacheStatusHeader); got != "HIT" {
		t.Fatalf("second cache status = %q, want HIT", got)
	}
	if got := second.Header().Get(cacheKeyHeader); got != cacheKey {
		t.Fatalf("second cache key = %q, want %q", got, cacheKey)
	}
	if got := second.Body.String(); got != `{"data":{"persons":[]}}` {
		t.Fatalf("second body = %q, want cached body", got)
	}
	if got := second.Header().Get("X-Origin"); got != "upstream" {
		t.Fatalf("cached X-Origin = %q, want upstream", got)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestHandlerCachesGraphQLGETResponses(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte("get-response"))
	}))

	first := performGraphQLRequest(
		t,
		handler,
		http.MethodGet,
		"/graphql?query=query%20%7B%20viewer%20%7B%20id%20%7D%20%7D",
		"",
		"",
	)
	second := performGraphQLRequest(
		t,
		handler,
		http.MethodGet,
		"/graphql?query=query%20%7B%20viewer%20%7B%20id%20%7D%20%7D",
		"",
		"",
	)

	if got := first.Header().Get(cacheStatusHeader); got != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", got)
	}
	if got := second.Header().Get(cacheStatusHeader); got != "HIT" {
		t.Fatalf("second cache status = %q, want HIT", got)
	}
	if second.Body.String() != "get-response" {
		t.Fatalf("second body = %q, want get-response", second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestHandlerBypassesMutationOperations(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte("mutation-response"))
	}))

	first := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/json",
		`{"query":"mutation { addPerson(name:\"Alice\") { id } }"}`,
	)
	second := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/json",
		`{"query":"mutation { addPerson(name:\"Alice\") { id } }"}`,
	)

	if got := first.Header().Get(cacheStatusHeader); got != "BYPASS" {
		t.Fatalf("first cache status = %q, want BYPASS", got)
	}
	if got := second.Header().Get(cacheStatusHeader); got != "BYPASS" {
		t.Fatalf("second cache status = %q, want BYPASS", got)
	}
	if first.Header().Get(cacheKeyHeader) != "" || second.Header().Get(cacheKeyHeader) != "" {
		t.Fatalf(
			"mutation cache keys = %q/%q, want empty",
			first.Header().Get(cacheKeyHeader),
			second.Header().Get(cacheKeyHeader),
		)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerRejectsInvalidGraphQLCacheRequests(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})

	tests := []struct {
		name        string
		method      string
		target      string
		contentType string
		body        string
		wantStatus  int
	}{
		{
			name:       "unsupported method",
			method:     http.MethodPut,
			target:     "/graphql",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{name: "get missing query", method: http.MethodGet, target: "/graphql", wantStatus: http.StatusBadRequest},
		{
			name:        "post missing query field",
			method:      http.MethodPost,
			target:      "/graphql",
			contentType: "application/json",
			body:        `{"variables":{}}`,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "bad content type",
			method:      http.MethodPost,
			target:      "/graphql",
			contentType: "text/plain",
			body:        "query { viewer { id } }",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "invalid query",
			method:      http.MethodPost,
			target:      "/graphql",
			contentType: "application/graphql",
			body:        "query { viewer {",
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := performGraphQLRequest(t, p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called")
			})), tt.method, tt.target, tt.contentType, tt.body)
			if res.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", res.Code, tt.wantStatus)
			}
		})
	}
}

func TestHandlerRefreshesExpiredGraphQLCacheEntries(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 1})
	calls := 0
	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	p.now = func() time.Time { return base }
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte("response"))
	}))

	_ = performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/json",
		`{"query":"{ viewer { id } }"}`,
	)
	p.now = func() time.Time { return base.Add(2 * time.Second) }
	res := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/json",
		`{"query":"{ viewer { id } }"}`,
	)

	if got := res.Header().Get(cacheStatusHeader); got != "EXPIRED" {
		t.Fatalf("cache status = %q, want EXPIRED", got)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestHandlerEnforcesGlobalGraphQLMaxSize(t *testing.T) {
	oldConfig := config.GlobalConfig
	config.GlobalConfig = &config.Config{GraphQL: config.GraphQL{MaxSize: 32}}
	t.Cleanup(func() { config.GlobalConfig = oldConfig })

	p := newTestPlugin(t, Config{CacheTTL: 60})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for oversized requests")
	}))

	post := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/graphql",
		"query { viewer { id name email } }",
	)
	if post.Code != http.StatusBadRequest {
		t.Fatalf("oversized POST response code = %d, want %d", post.Code, http.StatusBadRequest)
	}

	get := performGraphQLRequest(
		t,
		handler,
		http.MethodGet,
		"/graphql?query=query%20%7B%20viewer%20%7B%20id%20name%20email%20%7D%20%7D",
		"",
		"",
	)
	if get.Code != http.StatusBadRequest {
		t.Fatalf("oversized GET response code = %d, want %d", get.Code, http.StatusBadRequest)
	}
}

func TestCacheKeyIncludesRouteServiceAndConsumerIdentity(t *testing.T) {
	p := newTestPlugin(t, Config{CacheTTL: 60})
	p.SetResourceContext(
		resource.Route{ID: "route-1", ServiceID: "service-1"},
		resource.Service{ID: "service-1"},
	)

	request := httptest.NewRequest(http.MethodPost, "http://example.com/graphql", nil)
	request = apisixctx.WithApisixVars(request, map[string]string{
		"$route_id":      "route-1",
		"$service_id":    "service-1",
		"$consumer_name": "alice",
	})
	aliceKey := p.cacheKey(request, []byte(`{"query":"{ viewer { id } }"}`))

	apisixctx.RegisterApisixVar(request, "$consumer_name", "bob")
	bobKey := p.cacheKey(request, []byte(`{"query":"{ viewer { id } }"}`))
	if aliceKey == bobKey {
		t.Fatalf("consumer cache keys are equal: %q", aliceKey)
	}

	apisixctx.RegisterApisixVar(request, "$consumer_name", "alice")
	apisixctx.RegisterApisixVar(request, "$route_id", "route-2")
	routeKey := p.cacheKey(request, []byte(`{"query":"{ viewer { id } }"}`))
	if aliceKey == routeKey {
		t.Fatalf("route cache keys are equal: %q", aliceKey)
	}
}

func TestPurgeHandlerRemovesRouteCacheEntry(t *testing.T) {
	p := &Plugin{config: Config{CacheStrategy: "memory", CacheTTL: 60}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	calls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte("response"))
	}))
	first := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/graphql",
		"query { viewer { id } }",
	)
	cacheKey := first.Header().Get(cacheKeyHeader)
	if cacheKey == "" {
		t.Fatal("cache key is empty")
	}

	purge := httptest.NewRequest(
		"PURGE",
		"/apisix/plugin/graphql-proxy-cache/memory/route-1/"+cacheKey,
		nil,
	)
	purgeResponse := httptest.NewRecorder()
	PurgeHandler(purgeResponse, purge)
	if purgeResponse.Code != http.StatusOK {
		t.Fatalf("purge response code = %d, want %d", purgeResponse.Code, http.StatusOK)
	}

	second := performGraphQLRequest(
		t,
		handler,
		http.MethodPost,
		"/graphql",
		"application/graphql",
		"query { viewer { id } }",
	)
	if got := second.Header().Get(cacheStatusHeader); got != "MISS" {
		t.Fatalf("cache status after purge = %q, want MISS", got)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func performGraphQLRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	contentType string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Host = "example.com"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
