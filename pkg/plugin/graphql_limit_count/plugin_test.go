package graphql_limit_count

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitRequiresEffectiveConfig(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || err.Error() != "effective config is required" {
		t.Fatalf("PostInit() error = %v, want stable missing-config error", err)
	}
}

func TestRequestVarUsesEffectiveRemoteIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r = r.WithContext(context.WithValue(r.Context(), apisixctx.RemoteAddrKey, "198.51.100.2"))

	if got := requestVar(r, "remote_addr"); got != "198.51.100.2" {
		t.Fatalf("requestVar(remote_addr) = %q, want effective remote address", got)
	}
}

func TestPostInitAcceptsRedisPolicyDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      5,
		TimeWindow: 60,
		Key:        "remote_addr",
		Policy:     "redis",
		RedisHost:  "127.0.0.1",
	})

	if p.config.Policy != "redis" {
		t.Fatalf("Policy = %q, want redis", p.config.Policy)
	}
	if p.config.RedisPort != 6379 {
		t.Fatalf("RedisPort = %d, want 6379", p.config.RedisPort)
	}
	if p.config.RedisTimeout != 1000 {
		t.Fatalf("RedisTimeout = %d, want 1000", p.config.RedisTimeout)
	}
	if p.config.RedisKeepaliveTimeout != 10000 {
		t.Fatalf("RedisKeepaliveTimeout = %d, want 10000", p.config.RedisKeepaliveTimeout)
	}
	if p.config.RedisSSL == nil || *p.config.RedisSSL {
		t.Fatalf("RedisSSL = %v, want false", p.config.RedisSSL)
	}
	if p.config.RedisSSLVerify == nil || *p.config.RedisSSLVerify {
		t.Fatalf("RedisSSLVerify = %v, want false", p.config.RedisSSLVerify)
	}
	if p.redisLimiter == nil {
		t.Fatal("redisLimiter = nil, want initialized limiter")
	}
}

func TestPostInitRejectsRedisPolicyWithoutHost(t *testing.T) {
	p := &Plugin{config: Config{
		Count:      5,
		TimeWindow: 60,
		Key:        "remote_addr",
		Policy:     "redis",
	}}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "redis_host is required") {
		t.Fatalf("PostInit() error = %v, want redis_host required", err)
	}
}

func TestSchemaAcceptsRedisPolicyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"count":                   5,
		"time_window":             60,
		"key":                     "remote_addr",
		"policy":                  "redis",
		"redis_host":              "127.0.0.1",
		"redis_port":              6379,
		"redis_username":          "default",
		"redis_password":          "",
		"redis_database":          0,
		"redis_timeout":           1000,
		"redis_ssl":               false,
		"redis_ssl_verify":        false,
		"redis_keepalive_timeout": 10000,
		"redis_keepalive_pool":    100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected redis policy fields: %v", err)
	}
}

func TestSchemaAndPostInitAcceptRedisClusterPolicy(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	clusterConfig := map[string]any{
		"count":                    5,
		"time_window":              60,
		"key":                      "remote_addr",
		"policy":                   "redis-cluster",
		"redis_cluster_nodes":      []any{"127.0.0.1:5000", "127.0.0.1:5001"},
		"redis_password":           "secret",
		"redis_timeout":            1500,
		"redis_cluster_name":       "cluster-1",
		"redis_cluster_ssl":        true,
		"redis_cluster_ssl_verify": false,
		"redis_keepalive_timeout":  12000,
		"redis_keepalive_pool":     80,
	}
	if err := util.Validate(clusterConfig, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected redis-cluster policy fields: %v", err)
	}
	delete(clusterConfig, "redis_cluster_nodes")
	if err := util.Validate(clusterConfig, p.GetSchema()); err == nil {
		t.Fatal("schema accepted redis-cluster policy without redis_cluster_nodes")
	}

	ssl := true
	sslVerify := false
	initialized := newTestPlugin(t, Config{
		Count:                 5,
		TimeWindow:            60,
		Policy:                "redis-cluster",
		RedisClusterNodes:     []string{"127.0.0.1:5000", "127.0.0.1:5001"},
		RedisPassword:         "secret",
		RedisTimeout:          1500,
		RedisClusterName:      "cluster-1",
		RedisClusterSSL:       &ssl,
		RedisClusterSSLVerify: &sslVerify,
		RedisKeepaliveTimeout: 12000,
		RedisKeepalivePool:    80,
	})
	if initialized.redisLimiter == nil {
		t.Fatal("redisLimiter = nil, want initialized cluster limiter")
	}
	if initialized.config.RedisKeepaliveTimeout != 12000 || initialized.config.RedisKeepalivePool != 80 {
		t.Fatalf(
			"cluster keepalive = %d/%d, want 12000/80",
			initialized.config.RedisKeepaliveTimeout,
			initialized.config.RedisKeepalivePool,
		)
	}
}

func TestSchemaAcceptsRulesAndStringLimitValues(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(map[string]any{
		"count":       "$http_x_limit",
		"time_window": "$http_x_window",
	}, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected string limit values: %v", err)
	}
	if err := util.Validate(map[string]any{
		"rules": []any{
			map[string]any{
				"count":         10,
				"time_window":   60,
				"key":           "$http_x_tenant",
				"header_prefix": "Tenant",
			},
		},
	}, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected rules: %v", err)
	}
}

func TestHandlerUsesRedisLimiterDepthCost(t *testing.T) {
	redisLimiter := &fakeRedisLimiter{
		remaining: 1,
		reset:     60,
		allowed:   true,
	}
	p := newTestPlugin(t, Config{
		Count:      5,
		TimeWindow: 60,
		Key:        "remote_addr",
		Policy:     "redis",
		RedisHost:  "127.0.0.1",
	})
	p.redisLimiter = redisLimiter

	req := httptest.NewRequest(
		http.MethodPost,
		"/graphql",
		strings.NewReader(`{"query":"query { foo { bar { baz { id } } } }"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if redisLimiter.key != "192.0.2.10" {
		t.Fatalf("redis key = %q, want 192.0.2.10", redisLimiter.key)
	}
	if redisLimiter.cost != 4 {
		t.Fatalf("redis cost = %d, want query depth 4", redisLimiter.cost)
	}
	if got := rr.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1", got)
	}
}

func TestHandlerAllowsDegradationWhenRedisLimiterFails(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:            5,
		TimeWindow:       60,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
		AllowDegradation: new(true),
	})
	p.redisLimiter = &fakeRedisLimiter{err: errors.New("redis down")}

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ foo { id } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.20:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 degradation pass; body=%s", rr.Code, rr.Body.String())
	}
}

func TestGraphQLDepthCountsNestedSelections(t *testing.T) {
	depth, err := queryDepth(`query { foo { bar { baz { id } } } }`)
	if err != nil {
		t.Fatalf("queryDepth() error = %v", err)
	}
	if depth != 4 {
		t.Fatalf("depth = %d, want 4", depth)
	}

	depth, err = queryDepth(`query { foo { ...Fields } } fragment Fields on Foo { bar { id } }`)
	if err != nil {
		t.Fatalf("queryDepth() with fragment error = %v", err)
	}
	if depth != 3 {
		t.Fatalf("fragment depth = %d, want 3", depth)
	}
}

func TestGraphQLDepthHandlesAliasesArgumentsAndDirectives(t *testing.T) {
	depth, err := queryDepth(`query ($id: ID!) {
  user: viewer(id: $id) @include(if: true) {
    profile { name }
    posts { id }
  }
}`)
	if err != nil {
		t.Fatalf("queryDepth() with alias/arguments/directive error = %v", err)
	}
	if depth != 3 {
		t.Fatalf("depth = %d, want 3", depth)
	}
}

func TestGraphQLDepthRejectsUndefinedFragment(t *testing.T) {
	if _, err := queryDepth(`query { viewer { ...MissingFields } }`); err == nil {
		t.Fatal("queryDepth() error = nil, want undefined fragment rejection")
	}
}

func TestGraphQLDepthBoundsCyclicFragments(t *testing.T) {
	query := `query { viewer { ...First } }
fragment First on Viewer { ...Second }
fragment Second on Viewer { ...First }`
	depth, err := queryDepth(query)
	if err != nil {
		t.Fatalf("queryDepth() error = %v", err)
	}
	if depth != 1 {
		t.Fatalf("depth = %d, want 1", depth)
	}
}

func TestGraphQLDepthRejectsUnknownOperationKeyword(t *testing.T) {
	query := `test {
  persons(first: 1, after: "xxx") {
    name
  }
}`
	if _, err := queryDepth(query); err == nil {
		t.Fatal("queryDepth() error = nil, want unknown operation keyword rejection")
	}
}

func TestGraphQLDepthRejectsArgumentWithoutValue(t *testing.T) {
	if _, err := queryDepth(`query{persons(filter){id}}`); err == nil {
		t.Fatal("queryDepth() error = nil, want argument without value rejection")
	}
}

func TestHandlerLimitsJSONGraphQLByDepthCost(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:                5,
		TimeWindow:           60,
		Key:                  "remote_addr",
		RejectedCode:         http.StatusTooManyRequests,
		ShowLimitQuotaHeader: new(true),
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/graphql",
		strings.NewReader(`{"query":"query { foo { bar { baz { id } } } }"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("X-RateLimit-Limit"); got != "5" {
		t.Fatalf("X-RateLimit-Limit = %q, want 5", got)
	}
	if got := rr.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ foo { bar } }"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.RemoteAddr = "192.0.2.10:1234"
	rr = httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when quota is exhausted")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want 429", rr.Code)
	}
	if got := rr.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("rejected X-RateLimit-Remaining = %q, want 0", got)
	}
}

func TestHandlerAcceptsApplicationGraphQLBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      3,
		TimeWindow: 60,
		Key:        "remote_addr",
	})

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`query { foo { id } }`))
	req.Header.Set("Content-Type", "application/graphql")
	req.RemoteAddr = "192.0.2.11:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
	if got := rr.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1", got)
	}
}

func TestHandlerRejectsInvalidGraphQLRequests(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      3,
		TimeWindow: 60,
		Key:        "remote_addr",
	})

	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "get method", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed},
		{
			name:        "unsupported content type",
			method:      http.MethodPost,
			contentType: "text/plain",
			body:        `query { foo }`,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "missing query field",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        `{"variables":{}}`,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "empty query",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        `{"query":""}`,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "invalid query",
			method:      http.MethodPost,
			contentType: "application/graphql",
			body:        `query { foo { `,
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/graphql", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called")
			})).ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.wantStatus)
			}
		})
	}
}

func TestHandlerReportsEmptyGraphQLQuery(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      3,
		TimeWindow: 60,
		Key:        "remote_addr",
	})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":""}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rr.Body.String(), "empty graphql query") {
		t.Fatalf("response body = %q, want empty graphql query error", rr.Body.String())
	}
}

func TestHandlerEnforcesGlobalGraphQLMaxSize(t *testing.T) {
	p := &Plugin{config: Config{Count: 100, TimeWindow: 60}}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{
		Config: config.Config{GraphQL: config.GraphQL{MaxSize: 50}},
	}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/graphql",
		strings.NewReader(`{"query":"query { viewer { id name email address phone } }"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for oversized GraphQL bodies")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
}

func TestHandlerAppliesGraphQLDepthToMultipleRules(t *testing.T) {
	p := newTestPlugin(t, Config{
		RejectedCode: http.StatusTooManyRequests,
		Rules: []Rule{
			{Count: 10, TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Tenant"},
			{Count: 3, TimeWindow: 60, Key: "$http_x_user", HeaderPrefix: "User"},
		},
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := func() *http.Request {
		req := httptest.NewRequest(
			http.MethodPost,
			"/graphql",
			strings.NewReader(`{"query":"query { viewer { id } }"}`),
		)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant", "tenant-1")
		req.Header.Set("X-User", "alice")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusNoContent)
	}
	if got := first.Header().Get("X-Tenant-RateLimit-Remaining"); got != "8" {
		t.Fatalf("tenant remaining = %q, want 8", got)
	}
	if got := first.Header().Get("X-User-RateLimit-Remaining"); got != "1" {
		t.Fatalf("user remaining = %q, want 1", got)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request())
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
	if got := second.Header().Get("X-User-RateLimit-Remaining"); got != "0" {
		t.Fatalf("rejected user remaining = %q, want 0", got)
	}
}

func TestHandlerResolvesStringLimitValues(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        "$http_x_limit",
		TimeWindow:   "$http_x_window",
		Key:          "http_x_user",
		RejectedCode: http.StatusTooManyRequests,
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := func() *http.Request {
		req := httptest.NewRequest(
			http.MethodPost,
			"/graphql",
			strings.NewReader(`{"query":"query { viewer { id } }"}`),
		)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Limit", "2")
		req.Header.Set("X-Window", "60")
		req.Header.Set("X-User", "alice")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusNoContent)
	}
	if got := first.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("remaining = %q, want 0", got)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request())
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
}

func TestHandlerRejectsWhenNoGraphQLRuleCanBeResolved(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{Count: "$http_x_limit", TimeWindow: 60, Key: "$http_x_tenant"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ viewer }"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusInternalServerError)
	}
}

func TestPostInitRejectsDuplicateGraphQLRuleKeys(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{
		{Count: 3, TimeWindow: 60, Key: "$http_x_user"},
		{Count: 5, TimeWindow: 60, Key: "$http_x_user"},
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want duplicate rule key rejected")
	}
}

func TestGroupSharesLocalQuotaAcrossPluginInstances(t *testing.T) {
	resetGroupCountersForTest()
	t.Cleanup(resetGroupCountersForTest)

	config := Config{Count: 2, TimeWindow: 60, Group: "shared-group", RejectedCode: http.StatusTooManyRequests}
	firstPlugin := newTestPlugin(t, config)
	secondPlugin := newTestPlugin(t, config)
	handler := func(plugin *Plugin) http.Handler {
		return plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ viewer }"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.50:1234"
		return req
	}

	for i, plugin := range []*Plugin{firstPlugin, secondPlugin} {
		res := httptest.NewRecorder()
		handler(plugin).ServeHTTP(res, request())
		if res.Code != http.StatusNoContent {
			t.Fatalf("request %d response code = %d, want %d", i+1, res.Code, http.StatusNoContent)
		}
	}
	res := httptest.NewRecorder()
	handler(firstPlugin).ServeHTTP(res, request())
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("third response code = %d, want shared group rejection", res.Code)
	}
}

func TestGroupRegistryReleasesLastOwner(t *testing.T) {
	resetGroupCountersForTest()
	t.Cleanup(resetGroupCountersForTest)

	config := Config{Count: 2, TimeWindow: 60, Policy: "local", Group: "shared"}
	first := newTestPlugin(t, config)
	second := newTestPlugin(t, config)

	graphqlLimitCountGroups.Lock()
	entry, ok := graphqlLimitCountGroups.entries[config.Group]
	graphqlLimitCountGroups.Unlock()
	if !ok || entry.refs != 2 {
		t.Fatalf("group entry = %#v/%t, want refs=2", entry, ok)
	}

	first.Stop()
	graphqlLimitCountGroups.Lock()
	entry, ok = graphqlLimitCountGroups.entries[config.Group]
	graphqlLimitCountGroups.Unlock()
	if !ok || entry.refs != 1 {
		t.Fatalf("group entry after first Stop = %#v/%t, want refs=1", entry, ok)
	}

	second.Stop()
	graphqlLimitCountGroups.Lock()
	_, ok = graphqlLimitCountGroups.entries[config.Group]
	graphqlLimitCountGroups.Unlock()
	if ok {
		t.Fatal("group entry remains after final owner Stop")
	}

	second.Stop()
	graphqlLimitCountGroups.Lock()
	_, ok = graphqlLimitCountGroups.entries[config.Group]
	graphqlLimitCountGroups.Unlock()
	if ok {
		t.Fatal("group entry recreated by idempotent Stop")
	}
}

func TestPostInitRejectsMismatchedGroupConfiguration(t *testing.T) {
	resetGroupCountersForTest()
	t.Cleanup(resetGroupCountersForTest)

	newTestPlugin(t, Config{Count: 2, TimeWindow: 60, Group: "shared-group"})
	p := &Plugin{config: Config{Count: 3, TimeWindow: 60, Group: "shared-group"}}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || err.Error() != "group conf mismatched" {
		t.Fatalf("PostInit() error = %v, want group conf mismatched", err)
	}
}

func TestCounterNamespaceUsesRouteUnlessGrouped(t *testing.T) {
	p := newTestPlugin(t, Config{Count: 2, TimeWindow: 60})
	p.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})
	if got := p.counterNamespace(); !strings.Contains(got, "route-1") {
		t.Fatalf("counter namespace = %q, want route-1", got)
	}

	p.config.Group = "shared"
	if got := p.counterNamespace(); got != "group:shared" {
		t.Fatalf("group counter namespace = %q, want group:shared", got)
	}
}

func TestHandlerUsesLimitCountMetadataHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{Count: 2, TimeWindow: 60})
	p.metadata = Metadata{
		LimitHeader:     "X-Custom-Limit",
		RemainingHeader: "X-Custom-Remaining",
		ResetHeader:     "X-Custom-Reset",
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ viewer }"}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Header().Get("X-Custom-Limit") != "2" ||
		res.Header().Get("X-Custom-Remaining") != "1" ||
		res.Header().Get("X-Custom-Reset") == "" {
		t.Fatalf("custom quota headers = %#v", res.Header())
	}
}

func TestWindowResetsAfterTimeWindow(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      2,
		TimeWindow: 1,
		Key:        "remote_addr",
	})
	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	p.now = func() time.Time { return base }

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ foo { id } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.12:1234"
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want 204", rr.Code)
	}

	p.now = func() time.Time { return base.Add(2 * time.Second) }
	req = httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ foo { id } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.12:1234"
	rr = httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second response code = %d, want 204 after reset", rr.Code)
	}
}

func resetGroupCountersForTest() {
	groupCounters.Lock()
	groupCounters.entries = nil
	groupCounters.Unlock()
	graphqlLimitCountGroups.Lock()
	graphqlLimitCountGroups.entries = map[string]graphqlLimitCountGroup{}
	graphqlLimitCountGroups.Unlock()
}

type fakeRedisLimiter struct {
	key       string
	cost      int64
	remaining int64
	reset     int64
	allowed   bool
	err       error
}

type scriptedRedisClient struct {
	redis.UniversalClient
	result any
	err    error
	keys   []string
	args   []any
}

func (f *scriptedRedisClient) Eval(_ context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	f.keys = append([]string(nil), keys...)
	f.args = append([]any(nil), args...)
	return redis.NewCmdResult(f.result, f.err)
}

func TestRedisCountLimiterDecodesAtomicAdmissionResponse(t *testing.T) {
	client := &scriptedRedisClient{result: []any{int64(1), "7", uint64(30)}}
	limiter := &redisCountLimiter{client: client, namespace: "route-a"}
	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)

	remaining, reset, allowed, err := limiter.incoming(req, "client-a", 3, 10, 60)
	if err != nil {
		t.Fatalf("incoming() error = %v", err)
	}
	if remaining != 7 || reset != 30 || !allowed {
		t.Fatalf("incoming() = remaining %d, reset %d, allowed %t", remaining, reset, allowed)
	}
	if len(client.keys) != 1 || client.keys[0] != "plugin-graphql-limit-count:route-a:client-a" {
		t.Fatalf("Eval keys = %#v", client.keys)
	}
	wantArgs := []any{int64(3), int64(10), int64(60)}
	if len(client.args) != len(wantArgs) {
		t.Fatalf("Eval args = %#v, want %#v", client.args, wantArgs)
	}
	for i := range wantArgs {
		if client.args[i] != wantArgs[i] {
			t.Fatalf("Eval arg %d = %#v, want %#v", i, client.args[i], wantArgs[i])
		}
	}
}

func TestRedisCountLimiterFailsClosedOnBackendAndProtocolErrors(t *testing.T) {
	tests := []struct {
		name   string
		result any
		err    error
	}{
		{name: "backend error", err: errors.New("redis unavailable")},
		{name: "wrong result type", result: "invalid"},
		{name: "wrong result length", result: []any{1, 2}},
		{name: "invalid allowed", result: []any{[]byte("1"), 2, 3}},
		{name: "invalid remaining", result: []any{1, "invalid", 3}},
		{name: "invalid reset", result: []any{1, 2, nil}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limiter := &redisCountLimiter{client: &scriptedRedisClient{result: test.result, err: test.err}}
			_, _, _, err := limiter.incoming(httptest.NewRequest(http.MethodPost, "/graphql", nil), "key", 1, 2, 3)
			if err == nil {
				t.Fatal("incoming() error = nil")
			}
		})
	}
}

func TestRedisIntRejectsOverflowAndInvalidWireValues(t *testing.T) {
	for _, test := range []struct {
		value any
		want  int64
		ok    bool
	}{
		{value: int(1), want: 1, ok: true},
		{value: int64(2), want: 2, ok: true},
		{value: uint64(3), want: 3, ok: true},
		{value: "4", want: 4, ok: true},
		{value: "invalid"},
		{value: []byte("5")},
		{value: ^uint64(0)},
	} {
		got, ok := limitbase.RedisInt(test.value)
		if got != test.want || ok != test.ok {
			t.Fatalf("limitbase.RedisInt(%#v) = %d, %t; want %d, %t", test.value, got, ok, test.want, test.ok)
		}
	}
}

func (f *fakeRedisLimiter) incoming(
	_ *http.Request,
	key string,
	cost int64,
	_ int64,
	_ int64,
) (int64, int64, bool, error) {
	f.key = key
	f.cost = cost
	return f.remaining, f.reset, f.allowed, f.err
}

func TestGraphqlLimitCountLocalCountersEvictOldestAndExpired(t *testing.T) {
	original := defaultLocalCountersCapacity
	defaultLocalCountersCapacity = 4
	t.Cleanup(func() { defaultLocalCountersCapacity = original })

	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	p := newTestPlugin(t, Config{Count: 100, TimeWindow: 60, Policy: "local"})
	p.now = func() time.Time { return base }

	for i := range 6 {
		remaining, _, allowed, err := p.incoming(nil, "user-"+strconv.Itoa(i), 1, 100, 60)
		if err != nil {
			t.Fatalf("incoming user-%d: %v", i, err)
		}
		if !allowed || remaining != 99 {
			t.Fatalf("incoming user-%d remaining = %d allowed = %t, want 99/true", i, remaining, allowed)
		}
	}

	// Active keys preserve their counters: user-2 now consumed twice. Checked
	// before touching user-0, whose re-insertion would evict the oldest
	// remaining live entry (user-2) at capacity.
	remaining, _, allowed, err := p.incoming(nil, "user-2", 1, 100, 60)
	if err != nil {
		t.Fatalf("incoming user-2: %v", err)
	}
	if !allowed || remaining != 98 {
		t.Fatalf("active key user-2 remaining = %d allowed = %t, want 98/true", remaining, allowed)
	}

	// Capacity 4 was exceeded, so the two oldest counters were evicted and
	// user-0 restarts from a fresh counter.
	remaining, _, allowed, err = p.incoming(nil, "user-0", 1, 100, 60)
	if err != nil {
		t.Fatalf("incoming user-0 after eviction: %v", err)
	}
	if !allowed || remaining != 99 {
		t.Fatalf("evicted key user-0 remaining = %d allowed = %t, want 99/true", remaining, allowed)
	}

	// Advancing past the time window expires user-5 and resets its counter.
	p.now = func() time.Time { return base.Add(2 * time.Minute) }
	remaining, _, allowed, err = p.incoming(nil, "user-5", 1, 100, 60)
	if err != nil {
		t.Fatalf("incoming user-5 after expiry: %v", err)
	}
	if !allowed || remaining != 99 {
		t.Fatalf("expired key user-5 remaining = %d allowed = %t, want 99/true", remaining, allowed)
	}
}
