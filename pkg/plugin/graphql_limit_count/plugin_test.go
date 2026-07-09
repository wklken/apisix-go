package graphql_limit_count

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
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
		AllowDegradation: boolPtr(true),
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

func TestHandlerLimitsJSONGraphQLByDepthCost(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:                5,
		TimeWindow:           60,
		Key:                  "remote_addr",
		RejectedCode:         http.StatusTooManyRequests,
		ShowLimitQuotaHeader: boolPtr(true),
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

func boolPtr(value bool) *bool {
	return &value
}

type fakeRedisLimiter struct {
	key       string
	cost      int64
	remaining int64
	reset     int64
	allowed   bool
	err       error
}

func (f *fakeRedisLimiter) incoming(_ *http.Request, key string, cost int64) (int64, int64, bool, error) {
	f.key = key
	f.cost = cost
	return f.remaining, f.reset, f.allowed, f.err
}
