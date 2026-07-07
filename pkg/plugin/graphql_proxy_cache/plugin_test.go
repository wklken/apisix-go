package graphql_proxy_cache

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
