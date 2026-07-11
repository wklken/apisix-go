package limit_count

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/resource"
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

func TestPostInitAcceptsRootRedisPolicyFields(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:         "$http_x_limit",
		TimeWindow:    60,
		Policy:        "redis",
		RedisHost:     "127.0.0.1",
		RedisPort:     6380,
		RedisUsername: "default",
		RedisPassword: "secret",
		RedisDatabase: 2,
		RedisTimeout:  1500,
	})

	if p.config.Redis.RedisHost != "127.0.0.1" {
		t.Fatalf("Redis.RedisHost = %q, want 127.0.0.1", p.config.Redis.RedisHost)
	}
	if p.config.Redis.RedisPort != 6380 {
		t.Fatalf("Redis.RedisPort = %d, want 6380", p.config.Redis.RedisPort)
	}
	if p.config.Redis.RedisUsername != "default" {
		t.Fatalf("Redis.RedisUsername = %q, want default", p.config.Redis.RedisUsername)
	}
	if p.config.Redis.RedisPassword != "secret" {
		t.Fatalf("Redis.RedisPassword = %q, want secret", p.config.Redis.RedisPassword)
	}
	if p.config.Redis.RedisDatabase != 2 {
		t.Fatalf("Redis.RedisDatabase = %d, want 2", p.config.Redis.RedisDatabase)
	}
	if p.config.Redis.RedisTimeout != 1500 {
		t.Fatalf("Redis.RedisTimeout = %d, want 1500", p.config.Redis.RedisTimeout)
	}
}

func TestSchemaAcceptsRootRedisPolicyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"count":                   1,
		"time_window":             60,
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
		t.Fatalf("schema rejected root redis policy fields: %v", err)
	}
}

func TestSchemaAcceptsRootRedisClusterPolicyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"count":                    1,
		"time_window":              60,
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
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected root redis-cluster policy fields: %v", err)
	}

	delete(config, "redis_cluster_nodes")
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("schema accepted redis-cluster policy without redis_cluster_nodes")
	}
}

func TestPostInitBuildsRedisClusterOptionsFromRootFields(t *testing.T) {
	ssl := true
	verify := false
	p := newTestPlugin(t, Config{
		Count:                 "$http_x_limit",
		TimeWindow:            60,
		Policy:                "redis-cluster",
		RedisClusterNodes:     []string{"127.0.0.1:5000", "127.0.0.1:5001"},
		RedisPassword:         "secret",
		RedisTimeout:          1500,
		RedisClusterName:      "cluster-1",
		RedisClusterSSL:       &ssl,
		RedisClusterSSLVerify: &verify,
		RedisKeepaliveTimeout: 12000,
		RedisKeepalivePool:    80,
	})

	options := p.redisClusterOptions()
	if len(options.Addrs) != 2 || options.Addrs[0] != "127.0.0.1:5000" {
		t.Fatalf("cluster addresses = %#v", options.Addrs)
	}
	if options.Password != "secret" {
		t.Fatalf("cluster password = %q, want secret", options.Password)
	}
	if options.DialTimeout != 1500*time.Millisecond ||
		options.ReadTimeout != 1500*time.Millisecond ||
		options.WriteTimeout != 1500*time.Millisecond {
		t.Fatalf("cluster timeouts = %s/%s/%s", options.DialTimeout, options.ReadTimeout, options.WriteTimeout)
	}
	if options.PoolSize != 80 || options.ConnMaxIdleTime != 12*time.Second {
		t.Fatalf("cluster pool = %d, idle timeout = %s", options.PoolSize, options.ConnMaxIdleTime)
	}
	if options.TLSConfig == nil || !options.TLSConfig.InsecureSkipVerify {
		t.Fatalf("cluster TLS config = %#v, want TLS with verification disabled", options.TLSConfig)
	}
}

func TestHandlerUsesHTTPVariableKey(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		Key:          "http_x_user",
		KeyType:      "var",
		RejectedCode: http.StatusTooManyRequests,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.Header.Set("X-User", "alice")
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.Header.Set("X-User", "bob")
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusNoContent {
		t.Fatalf("second status = %d, want separate quota bucket for different X-User", secondRecorder.Code)
	}
}

func TestHandlerUsesVariableCombinationKey(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		Key:          "$http_x_tenant:$http_x_user",
		KeyType:      "var_combination",
		RejectedCode: http.StatusTooManyRequests,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	requests := []struct {
		tenant string
		user   string
	}{
		{tenant: "t1", user: "alice"},
		{tenant: "t1", user: "bob"},
	}
	for _, req := range requests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Tenant", req.tenant)
		r.Header.Set("X-User", req.user)
		r.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, r)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status for %s/%s = %d, want separate quota bucket", req.tenant, req.user, rr.Code)
		}
	}
}

func TestHandlerAppliesResolvedRules(t *testing.T) {
	p := newTestPlugin(t, Config{
		RejectedCode: http.StatusTooManyRequests,
		Rules: []Rule{
			{
				Count:        3,
				TimeWindow:   60,
				Key:          "$http_x_tenant",
				HeaderPrefix: "Tenant",
			},
			{
				Count:        1,
				TimeWindow:   60,
				Key:          "$http_x_user",
				HeaderPrefix: "User",
			},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.Header.Set("X-Tenant", "t1")
	first.Header.Set("X-User", "alice")
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}
	if got := firstRecorder.Header().Get("X-User-RateLimit-Limit"); got != "1" {
		t.Fatalf("user limit header = %q, want 1", got)
	}
	if got := firstRecorder.Header().Get("X-Tenant-RateLimit-Limit"); got != "3" {
		t.Fatalf("tenant limit header = %q, want 3", got)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.Header.Set("X-Tenant", "t1")
	second.Header.Set("X-User", "alice")
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want user rule rejection", secondRecorder.Code)
	}
	if got := secondRecorder.Header().Get("X-User-RateLimit-Remaining"); got != "0" {
		t.Fatalf("user remaining header = %q, want 0", got)
	}

	third := httptest.NewRequest(http.MethodGet, "/", nil)
	third.Header.Set("X-Tenant", "t1")
	third.Header.Set("X-User", "bob")
	third.RemoteAddr = "192.0.2.1:1234"
	thirdRecorder := httptest.NewRecorder()
	handler.ServeHTTP(thirdRecorder, third)
	if thirdRecorder.Code != http.StatusNoContent {
		t.Fatalf("third status = %d, want tenant rule still allows second tenant request", thirdRecorder.Code)
	}
}

func TestHandlerUsesMetadataQuotaHeaderNames(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		RejectedCode: http.StatusTooManyRequests,
	})
	p.metadata = Metadata{
		LimitHeader:     "X-Custom-Limit",
		RemainingHeader: "X-Custom-Remaining",
		ResetHeader:     "X-Custom-Reset",
	}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}
	if got := firstRecorder.Header().Get("X-Custom-Limit"); got != "1" {
		t.Fatalf("custom limit header = %q, want 1", got)
	}
	if got := firstRecorder.Header().Get("X-RateLimit-Limit"); got != "" {
		t.Fatalf("default limit header = %q, want empty", got)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want rejection", secondRecorder.Code)
	}
	if got := secondRecorder.Header().Get("X-Custom-Remaining"); got != "0" {
		t.Fatalf("custom remaining header = %q, want 0", got)
	}
	if got := secondRecorder.Header().Get("X-Custom-Reset"); got == "" {
		t.Fatal("custom reset header is empty, want reset timestamp")
	}
}

func TestHandlerUsesRejectedMessage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		RejectedCode: http.StatusTooManyRequests,
		RejectedMsg:  "quota exceeded",
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
	if got := secondRecorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := secondRecorder.Body.String(); got != `{"error_msg":"quota exceeded"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"quota exceeded"}`)
	}
}

func TestHandlerResolvesStringCountAndTimeWindow(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"count":         "$http_x_limit",
		"time_window":   "$http_x_window",
		"key":           "remote_addr",
		"rejected_code": http.StatusTooManyRequests,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("string count/time_window config should validate: %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.Header.Set("X-Limit", "1")
	first.Header.Set("X-Window", "60")
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.Header.Set("X-Limit", "1")
	second.Header.Set("X-Window", "60")
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestHandlerResolvesStringRuleCountAndTimeWindow(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"rejected_code": http.StatusTooManyRequests,
		"rules": []any{
			map[string]any{
				"count":         "$http_x_limit",
				"time_window":   "$http_x_window",
				"key":           "$http_x_user",
				"header_prefix": "User",
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("string rule count/time_window config should validate: %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.Header.Set("X-Limit", "1")
	first.Header.Set("X-Window", "60")
	first.Header.Set("X-User", "alice")
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.Header.Set("X-Limit", "1")
	second.Header.Set("X-Window", "60")
	second.Header.Set("X-User", "alice")
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
	if got := secondRecorder.Header().Get("X-User-RateLimit-Remaining"); got != "0" {
		t.Fatalf("user remaining header = %q, want 0", got)
	}
}

func TestPostInitRejectsDuplicateRuleKeys(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Count: 1, TimeWindow: 60, Key: "$http_x_user"},
			{Count: 2, TimeWindow: 60, Key: "$http_x_user"},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want duplicate rule key error")
	}
}

func TestGroupSharesLocalQuotaAcrossPluginInstances(t *testing.T) {
	resetLimitCountGroupsForTest()
	t.Cleanup(resetLimitCountGroupsForTest)

	config := Config{
		Count:        2,
		TimeWindow:   60,
		Group:        "shared-group",
		RejectedCode: http.StatusTooManyRequests,
	}
	firstPlugin := newTestPlugin(t, config)
	secondPlugin := newTestPlugin(t, config)
	handler := func(plugin *Plugin) http.Handler {
		return plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
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

func TestPostInitRejectsMismatchedGroupConfiguration(t *testing.T) {
	resetLimitCountGroupsForTest()
	t.Cleanup(resetLimitCountGroupsForTest)

	newTestPlugin(t, Config{Count: 2, TimeWindow: 60, Group: "shared-group"})
	p := &Plugin{config: Config{Count: 3, TimeWindow: 60, Group: "shared-group"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || err.Error() != "group conf mismatched" {
		t.Fatalf("PostInit() error = %v, want group conf mismatched", err)
	}
}

func TestScopedKeyUsesRouteUnlessGrouped(t *testing.T) {
	p := newTestPlugin(t, Config{Count: 2, TimeWindow: 60})
	p.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})
	if got := p.scopedKey("alice"); got != "route:route-1:alice" {
		t.Fatalf("scoped key = %q, want route-scoped key", got)
	}

	p.config.Group = "shared"
	if got := p.scopedKey("alice"); got != "group:shared:alice" {
		t.Fatalf("group key = %q, want group-scoped key", got)
	}
}

func TestHandlerRejectsWhenNoRuleCanBeResolved(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{Count: "$http_x_limit", TimeWindow: 60, Key: "$http_x_user"},
		},
	})
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusInternalServerError)
	}
}

func TestHandlerAllowsDegradationWhenNoRuleCanBeResolved(t *testing.T) {
	allowDegradation := true
	p := newTestPlugin(t, Config{
		AllowDegradation: &allowDegradation,
		Rules: []Rule{
			{Count: "$http_x_limit", TimeWindow: 60, Key: "$http_x_user"},
		},
	})
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want degradation pass", res.Code)
	}
}

func resetLimitCountGroupsForTest() {
	limitCountGroups.Lock()
	limitCountGroups.entries = map[string]limitCountGroup{}
	limitCountGroups.Unlock()
}
