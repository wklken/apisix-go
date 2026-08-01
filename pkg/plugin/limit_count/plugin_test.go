package limit_count

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type failingLimiterStore struct {
	err error
}

func (s failingLimiterStore) Get(context.Context, string, limiter.Rate) (limiter.Context, error) {
	return limiter.Context{}, s.err
}

func (s failingLimiterStore) Peek(context.Context, string, limiter.Rate) (limiter.Context, error) {
	return limiter.Context{}, s.err
}

func (s failingLimiterStore) Reset(context.Context, string, limiter.Rate) (limiter.Context, error) {
	return limiter.Context{}, s.err
}

func (s failingLimiterStore) Increment(
	context.Context,
	string,
	int64,
	limiter.Rate,
) (limiter.Context, error) {
	return limiter.Context{}, s.err
}

type countingLimiterStore struct {
	failingLimiterStore
	hits *atomic.Uint32
}

func (s countingLimiterStore) Get(
	context.Context,
	string,
	limiter.Rate,
) (limiter.Context, error) {
	s.hits.Add(1)
	return limiter.Context{}, nil
}

type fakeRedisPoolStatsProvider struct {
	hits *atomic.Uint32
}

func (p fakeRedisPoolStatsProvider) PoolStats() *redis.PoolStats {
	return &redis.PoolStats{Hits: p.hits.Load()}
}

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
		Count:                 "$http_x_limit",
		TimeWindow:            60,
		Policy:                "redis",
		RedisHost:             "127.0.0.1",
		RedisPort:             6380,
		RedisUsername:         "default",
		RedisPassword:         "secret",
		RedisDatabase:         2,
		RedisTimeout:          1500,
		RedisKeepaliveTimeout: 12000,
		RedisKeepalivePool:    80,
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
	options := p.redisOptions()
	if options.PoolSize != 80 || options.ConnMaxIdleTime != 12*time.Second {
		t.Fatalf("Redis pool = %d, idle timeout = %s; want 80 and 12s", options.PoolSize, options.ConnMaxIdleTime)
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

func TestSchemaRequiresRedisHostForRedisPolicy(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"count":       1,
		"time_window": 60,
		"policy":      "redis",
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("schema accepted redis policy without redis_host")
	}
}

func TestSchemaAcceptsNestedRedisConfigHost(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"count":       1,
		"time_window": 60,
		"policy":      "redis",
		"redis_config": map[string]any{
			"redis_host": "127.0.0.1",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected nested redis_config.redis_host: %v", err)
	}
}

func TestSchemaAcceptsRedisSentinelPolicy(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"count":             1,
		"time_window":       60,
		"policy":            "redis-sentinel",
		"redis_sentinels":   []any{map[string]any{"host": "127.0.0.1", "port": 26379}},
		"redis_master_name": "mymaster",
		"redis_role":        "master",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected redis-sentinel policy: %v", err)
	}

	delete(config, "redis_master_name")
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("schema accepted redis-sentinel policy without redis_master_name")
	}
}

func TestRedisSentinelSlaveRoleBuildsReplicaClientWithoutPanic(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:           1,
		TimeWindow:      60,
		Policy:          "redis-sentinel",
		RedisSentinels:  []RedisSentinel{{Host: "127.0.0.1", Port: 26379}},
		RedisMasterName: "mymaster",
		RedisRole:       "slave",
	})

	options := p.redisSentinelOptions()
	if !options.ReplicaOnly {
		t.Fatal("ReplicaOnly = false, want slave role routed only to replicas")
	}
	if options.RouteByLatency {
		t.Fatal("RouteByLatency = true, which panics with NewFailoverClient")
	}

	client := redis.NewFailoverClient(options)
	t.Cleanup(func() {
		_ = client.Close()
	})
}

func TestPostInitAllowsUnavailableRedisWhenDegradationEnabled(t *testing.T) {
	allowDegradation := true
	p := &Plugin{config: Config{
		Count:            1,
		TimeWindow:       60,
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
		RedisPort:        1,
		RedisTimeout:     1,
		AllowDegradation: &allowDegradation,
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want lazy Redis initialization for degradation", err)
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

func TestHandlerScopesConsumerPluginQuotaByConsumerName(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		Key:          "remote_addr",
		RejectedCode: http.StatusTooManyRequests,
	})
	p.SetResourceContext(resource.Route{ID: "consumer-route"}, resource.Service{})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := func(consumer string) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r = apisixctx.WithApisixVars(r, map[string]string{"$consumer_name": consumer})
		r = apisixctx.WithConsumerPluginOverrides(r, map[string]struct{}{name: {}})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Code
	}

	if status := request("jack1"); status != http.StatusNoContent {
		t.Fatalf("jack1 first status = %d, want %d", status, http.StatusNoContent)
	}
	if status := request("jack1"); status != http.StatusTooManyRequests {
		t.Fatalf("jack1 second status = %d, want %d", status, http.StatusTooManyRequests)
	}
	if status := request("jack2"); status != http.StatusNoContent {
		t.Fatalf("jack2 first status = %d, want isolated quota status %d", status, http.StatusNoContent)
	}
}

func TestHandlerRouteQuotaRemainsSharedAcrossAuthenticatedConsumers(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		Key:          "remote_addr",
		RejectedCode: http.StatusTooManyRequests,
	})
	p.SetResourceContext(resource.Route{ID: "shared-route"}, resource.Service{})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := func(consumer string) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r = apisixctx.WithApisixVars(r, map[string]string{"$consumer_name": consumer})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Code
	}

	if status := request("jack1"); status != http.StatusNoContent {
		t.Fatalf("jack1 route status = %d, want %d", status, http.StatusNoContent)
	}
	if status := request("jack2"); status != http.StatusTooManyRequests {
		t.Fatalf("jack2 route status = %d, want shared quota status %d", status, http.StatusTooManyRequests)
	}
}

func TestPostInitResolvesEnvironmentVariableKey(t *testing.T) {
	t.Setenv("LIMIT_COUNT_KEY", "remote_addr")

	p := newTestPlugin(t, Config{
		Count:      2,
		TimeWindow: 60,
		Key:        "$ENV://LIMIT_COUNT_KEY",
	})
	if p.config.Key != "remote_addr" {
		t.Fatalf("resolved key = %q, want remote_addr", p.config.Key)
	}
}

func TestPostInitResolvesRedisHostEnvironmentReference(t *testing.T) {
	t.Setenv("LIMIT_COUNT_REDIS_HOST", "127.0.0.2")

	p := newTestPlugin(t, Config{
		Count:      2,
		TimeWindow: 60,
		Policy:     "redis",
		RedisHost:  "$ENV://LIMIT_COUNT_REDIS_HOST",
	})
	if p.config.Redis.RedisHost != "127.0.0.2" {
		t.Fatalf("resolved Redis host = %q, want 127.0.0.2", p.config.Redis.RedisHost)
	}
	if options := p.redisOptions(); options.Addr != "127.0.0.2:6379" {
		t.Fatalf("Redis address = %q, want 127.0.0.2:6379", options.Addr)
	}
}

func TestPostInitResolvesRedisClusterNodeEnvironmentReferences(t *testing.T) {
	t.Setenv("LIMIT_COUNT_REDIS_NODE_0", "127.0.0.1:5000")
	t.Setenv("LIMIT_COUNT_REDIS_NODE_1", "127.0.0.1:5001")

	p := newTestPlugin(t, Config{
		Count:             2,
		TimeWindow:        60,
		Policy:            "redis-cluster",
		RedisClusterNodes: []string{"$ENV://LIMIT_COUNT_REDIS_NODE_0", "$ENV://LIMIT_COUNT_REDIS_NODE_1"},
		RedisClusterName:  "redis-cluster-1",
	})
	want := []string{"127.0.0.1:5000", "127.0.0.1:5001"}
	if !slices.Equal(p.config.RedisCluster.RedisClusterNodes, want) {
		t.Fatalf("resolved Redis cluster nodes = %#v, want %#v", p.config.RedisCluster.RedisClusterNodes, want)
	}
	if options := p.redisClusterOptions(); !slices.Equal(options.Addrs, want) {
		t.Fatalf("Redis cluster addresses = %#v, want %#v", options.Addrs, want)
	}
}

func TestRedisDiagnosticStoreLogsConnectionReuseFromInitializationBaseline(t *testing.T) {
	var hits atomic.Uint32
	store := newRedisDiagnosticStore(
		countingLimiterStore{
			failingLimiterStore: failingLimiterStore{err: errors.New("unexpected operation")},
			hits:                &hits,
		},
		fakeRedisPoolStatsProvider{hits: &hits},
	)

	logged := make([]string, 0, 2)
	stop := logger.ReplaceObserver("limit-count-redis-reuse-test", func(entry logger.Entry) {
		if strings.HasPrefix(entry.Message, "redis connection reused times:") {
			logged = append(logged, entry.Message)
		}
	})
	t.Cleanup(stop)

	rate := limiter.Rate{Period: time.Minute, Limit: 20}
	if _, err := store.Get(context.Background(), "test", rate); err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	if _, err := store.Get(context.Background(), "test", rate); err != nil {
		t.Fatalf("second Get() error = %v", err)
	}

	want := []string{
		"redis connection reused times: 0",
		"redis connection reused times: 1",
	}
	if !reflect.DeepEqual(logged, want) {
		t.Fatalf("reuse logs = %v, want %v", logged, want)
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

func TestHandlerConfiguredCostConsumesMultipleQuota(t *testing.T) {
	cost := 2
	p := newTestPlugin(t, Config{
		Count:        3,
		TimeWindow:   60,
		Cost:         &cost,
		RejectedCode: http.StatusTooManyRequests,
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusNoContent)
	}
	if got := first.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("first remaining quota = %q, want 1 after cost 2", got)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want %d", second.Code, http.StatusTooManyRequests)
	}
}

func TestHandlerResetHeaderReportsSecondsRemaining(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		RejectedCode: http.StatusTooManyRequests,
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	reset, err := strconv.ParseInt(response.Header().Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		t.Fatalf("parse reset header: %v", err)
	}
	if reset <= 0 || reset > 60 {
		t.Fatalf("reset header = %d, want remaining seconds in (0, 60]", reset)
	}
}

func TestFixedWindowResetSeconds(t *testing.T) {
	now := time.Unix(100, 0)
	tests := []struct {
		name       string
		expiration int64
		want       int64
	}{
		{name: "future expiry", expiration: 160, want: 60},
		{name: "already expired", expiration: 99, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := fixedWindowResetSeconds(test.expiration, now); got != test.want {
				t.Fatalf("fixedWindowResetSeconds(%d, %s) = %d, want %d", test.expiration, now, got, test.want)
			}
		})
	}
}

func TestHandlerZeroCostPeeksWithoutConsumingQuota(t *testing.T) {
	cost := 0
	p := newTestPlugin(t, Config{
		Count:        2,
		TimeWindow:   60,
		Cost:         &cost,
		RejectedCode: http.StatusTooManyRequests,
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for i := range 3 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("peek %d response code = %d, want %d", i+1, response.Code, http.StatusNoContent)
		}
		if got := response.Header().Get("X-RateLimit-Remaining"); got != "2" {
			t.Fatalf("peek %d remaining quota = %q, want 2", i+1, got)
		}
	}
}

func TestSlidingWindowCommitFlushesPermittedDeltaAfterLimitReached(t *testing.T) {
	store := newMemorySlidingWindowStore()
	limiter := newSlidingWindowLimiter(store, "plugin-limit-count", 2, 5)
	now := time.Unix(102, 0)

	remaining, _, err := limiter.incoming(context.Background(), "commit-regression", 2, now)
	if err != nil || remaining != 0 {
		t.Fatalf("consume quota = remaining %d, error %v; want remaining 0", remaining, err)
	}
	if _, _, err := limiter.incoming(context.Background(), "commit-regression", 3, now); !errors.Is(err, errSlidingWindowRejected) {
		t.Fatalf("over-limit incoming error = %v, want %v", err, errSlidingWindowRejected)
	}
	remaining, _, err = limiter.commit(context.Background(), "commit-regression", 3, now)
	if err != nil {
		t.Fatalf("commit() error = %v", err)
	}
	if remaining != -3 {
		t.Fatalf("commit() remaining = %d, want -3", remaining)
	}
}

func TestSlidingWindowCountersAreIsolatedByPluginName(t *testing.T) {
	store := newMemorySlidingWindowStore()
	limitCount := newSlidingWindowLimiter(store, "plugin-limit-count", 2, 5)
	graphqlLimitCount := newSlidingWindowLimiter(store, "plugin-graphql-limit-count", 2, 5)
	now := time.Unix(102, 0)

	if _, _, err := limitCount.incoming(context.Background(), "same-key", 2, now); err != nil {
		t.Fatalf("limit-count consume quota error = %v", err)
	}
	if _, _, err := limitCount.incoming(context.Background(), "same-key", 1, now); !errors.Is(err, errSlidingWindowRejected) {
		t.Fatalf("limit-count over-limit error = %v, want %v", err, errSlidingWindowRejected)
	}
	remaining, _, err := graphqlLimitCount.incoming(context.Background(), "same-key", 1, now)
	if err != nil {
		t.Fatalf("graphql-limit-count first request error = %v", err)
	}
	if remaining != 1 {
		t.Fatalf("graphql-limit-count remaining = %d, want independent quota 1", remaining)
	}
}

func TestSlidingWindowCheckAndIncrementIsAtomicAndDoesNotIncrementOnReject(t *testing.T) {
	store := newMemorySlidingWindowStore()
	limiter := newSlidingWindowLimiter(store, "plugin-limit-count", 2, 5)
	now := time.Unix(102, 0)

	var accepted atomic.Int64
	var rejected atomic.Int64
	var unexpected atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			_, _, err := limiter.incoming(context.Background(), "atomic-regression", 1, now)
			switch {
			case err == nil:
				accepted.Add(1)
			case errors.Is(err, errSlidingWindowRejected):
				rejected.Add(1)
			default:
				unexpected.Add(1)
			}
		})
	}
	wg.Wait()

	if got := accepted.Load(); got != 2 {
		t.Fatalf("accepted requests = %d, want exactly 2", got)
	}
	if got := rejected.Load(); got != 30 {
		t.Fatalf("rejected requests = %d, want 30", got)
	}
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("unexpected errors = %d, want 0", got)
	}
	currentKey, _ := limiter.counterKeys("atomic-regression", now)
	if got := store.count(currentKey, now); got != 2 {
		t.Fatalf("stored current-window count = %d, want 2", got)
	}
}

func TestSlidingWindowRejectsCostThatWouldExceedLimitWithoutIncrement(t *testing.T) {
	store := newMemorySlidingWindowStore()
	limiter := newSlidingWindowLimiter(store, "plugin-limit-count", 2, 5)
	now := time.Unix(102, 0)

	remaining, _, err := limiter.incoming(context.Background(), "cost-overflow", 1, now)
	if err != nil || remaining != 1 {
		t.Fatalf("first request = remaining %d, error %v; want remaining 1", remaining, err)
	}
	remaining, _, err = limiter.incoming(context.Background(), "cost-overflow", 2, now)
	if !errors.Is(err, errSlidingWindowRejected) {
		t.Fatalf("cost-overflow request error = %v, want %v", err, errSlidingWindowRejected)
	}
	if remaining != 0 {
		t.Fatalf("cost-overflow remaining = %d, want rejection header value 0", remaining)
	}
	currentKey, _ := limiter.counterKeys("cost-overflow", now)
	if got := store.count(currentKey, now); got != 1 {
		t.Fatalf("stored current-window count = %d, want rejected cost not incremented", got)
	}
}

func TestSlidingWindowResponseHeadersStayRoundedAcrossAcceptedAndRejectedRequests(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        2,
		TimeWindow:   5,
		WindowType:   "sliding",
		RejectedCode: http.StatusServiceUnavailable,
	})
	limiter := newSlidingWindowLimiter(newMemorySlidingWindowStore(), "plugin-limit-count", 2, 5)
	headers := defaultHeaders(Metadata{})
	start := time.Unix(100, 0)
	times := []time.Time{
		start,
		start.Add(time.Second),
		start.Add(2 * time.Second),
		start.Add(3500 * time.Millisecond),
		start.Add(5 * time.Second),
	}
	statuses := []int{
		http.StatusOK,
		http.StatusOK,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
	}

	for i, now := range times {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/hello", nil)
		allowed := p.runSlidingLimit(response, request, limiter, 2, "127.0.0.1", headers, now)
		if allowed {
			response.WriteHeader(http.StatusOK)
		}
		if response.Code != statuses[i] {
			t.Fatalf("request %d response status = %d, want %d", i+1, response.Code, statuses[i])
		}
		if remaining := response.Header().Get(headers.remaining); !regexp.MustCompile(`^[0-9]+$`).MatchString(remaining) {
			t.Fatalf("request %d remaining header = %q, want an integer", i+1, remaining)
		}
		if reset := response.Header().Get(headers.reset); !regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`).MatchString(reset) {
			t.Fatalf("request %d reset header = %q, want at most two decimal places", i+1, reset)
		}
	}
}

func TestMemorySlidingWindowStoreEvictsExpiredCountersGlobally(t *testing.T) {
	store := newMemorySlidingWindowStore()
	now := time.Unix(100, 0)
	for i := range 64 {
		key := fmt.Sprintf("expired-%d", i)
		if _, err := store.increment(context.Background(), key, 1, 2*time.Second, now); err != nil {
			t.Fatalf("increment(%q) error = %v", key, err)
		}
	}

	later := now.Add(3 * time.Second)
	if _, err := store.increment(context.Background(), "live", 1, 2*time.Second, later); err != nil {
		t.Fatalf("increment(live) error = %v", err)
	}

	if got := len(store.counters); got != 1 {
		t.Fatalf("counter count after global expiry = %d, want only the live counter", got)
	}
	if got := store.count("live", later); got != 1 {
		t.Fatalf("live counter = %d, want 1", got)
	}
}

func TestDelayedSyncRemainingDecreasesLocallyBeforeRemoteFlush(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 7, reset: 10 * time.Second}
	syncer := newDelayedSyncer(backend, 7, 10*time.Second, time.Hour, 10000)
	t.Cleanup(syncer.Stop)
	now := time.Unix(100, 0)

	for request, expected := range []int64{6, 5, 4, 3} {
		remaining, _, err := syncer.incoming(context.Background(), "example-1.com", 1, now)
		if err != nil {
			t.Fatalf("request %d error = %v", request+1, err)
		}
		if remaining != expected {
			t.Fatalf("request %d remaining = %d, want %d", request+1, remaining, expected)
		}
	}
	if got := backend.deltas(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("remote deltas before flush = %v, want initial quota read [0]", got)
	}
	if err := syncer.flushNow(context.Background(), now); err != nil {
		t.Fatalf("flushNow() error = %v", err)
	}
	if got := backend.deltas(); len(got) != 2 || got[1] != 4 {
		t.Fatalf("remote deltas after flush = %v, want final delta 4", got)
	}
}

func TestDelayedSyncRejectsAfterLocallyReservedQuotaIsExhausted(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 7, reset: 10 * time.Second}
	syncer := newDelayedSyncer(backend, 7, 10*time.Second, time.Hour, 10000)
	t.Cleanup(syncer.Stop)
	now := time.Unix(100, 0)

	for request := range 7 {
		remaining, _, err := syncer.incoming(context.Background(), "example-1.com", 1, now)
		if err != nil {
			t.Fatalf("request %d error = %v", request+1, err)
		}
		if remaining != int64(6-request) {
			t.Fatalf("request %d remaining = %d, want %d", request+1, remaining, 6-request)
		}
	}
	if _, _, err := syncer.incoming(context.Background(), "example-1.com", 1, now); !errors.Is(err, errDelayedSyncRejected) {
		t.Fatalf("eighth request error = %v, want %v", err, errDelayedSyncRejected)
	}
}

func TestDelayedSyncQueueRemainsBufferedUntilFlush(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 7, reset: 10 * time.Second}
	syncer := newDelayedSyncer(backend, 7, 10*time.Second, time.Hour, 10000)
	t.Cleanup(syncer.Stop)

	if !syncer.enqueue("buffered-key") {
		t.Fatal("enqueue() = false, want key buffered")
	}
	time.Sleep(10 * time.Millisecond)
	if got := len(syncer.queue); got != 1 {
		t.Fatalf("buffered queue length = %d, want 1 until the sync interval", got)
	}
	if got := cap(syncer.queue); got != 10000 {
		t.Fatalf("queue capacity = %d, want upstream cap 10000", got)
	}
}

func TestDelayedSyncQueueOverflowDoesNotFailAlreadyAllowedRequestAndWarnsOnce(t *testing.T) {
	warnings := 0
	syncer := &delayedSyncer{
		queue: make(chan string, 10000),
		warnQueueFull: func() {
			warnings++
		},
	}
	for i := range 10000 {
		if enqueued := syncer.enqueue(fmt.Sprintf("dummy-key-%d", i)); !enqueued {
			t.Fatalf("enqueue distinct key %d = false before upstream cap 10000", i)
		}
	}

	if enqueued := syncer.enqueue("new-key"); enqueued {
		t.Fatal("enqueue() = true, want saturated 10000-entry queue to drop the new key")
	}
	if enqueued := syncer.enqueue("another-key"); enqueued {
		t.Fatal("second enqueue() = true, want saturated queue to keep dropping new keys")
	}
	if warnings != 1 {
		t.Fatalf("queue saturation warnings = %d, want one throttled warning", warnings)
	}
	if got := len(syncer.queue); got != 10000 {
		t.Fatalf("queue length after overflow = %d, want cap 10000 unchanged", got)
	}
}

func TestDelayedSyncFlushRetriesDroppedStateWithoutAnotherRequest(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 10, reset: 10 * time.Second}
	syncer := newDelayedSyncer(backend, 10, 10*time.Second, time.Hour, 2)
	t.Cleanup(syncer.Stop)
	now := time.Unix(100, 0)

	for _, key := range []string{"queued-1", "queued-2", "overflow"} {
		if _, _, err := syncer.incoming(context.Background(), key, 1, now); err != nil {
			t.Fatalf("incoming(%q) error = %v", key, err)
		}
	}
	backend.resetCalls()

	if err := syncer.flushNow(context.Background(), now); err != nil {
		t.Fatalf("first flushNow() error = %v", err)
	}
	if got := backend.keyDeltas(); fmt.Sprint(got) != "[queued-1:1 queued-2:1]" {
		t.Fatalf("first flush calls = %v, want only the two queued keys", got)
	}
	syncer.mu.Lock()
	overflowDelta := syncer.states["overflow"].localDelta
	syncer.mu.Unlock()
	if overflowDelta != 1 {
		t.Fatalf("overflow local delta after first flush = %d, want unsynced 1", overflowDelta)
	}

	backend.resetCalls()
	if err := syncer.flushNow(context.Background(), now); err != nil {
		t.Fatalf("retry flushNow() error = %v", err)
	}
	if got := backend.keyDeltas(); fmt.Sprint(got) != "[overflow:1]" {
		t.Fatalf("retry flush calls = %v, want preserved overflow delta", got)
	}
}

func TestDelayedSyncFlushRequeuesBackendErrorsWithoutLosingDelta(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 7, reset: 10 * time.Second}
	syncer := newDelayedSyncer(backend, 7, 10*time.Second, time.Hour, 2)
	t.Cleanup(syncer.Stop)
	now := time.Unix(100, 0)

	if _, _, err := syncer.incoming(context.Background(), "retry-key", 1, now); err != nil {
		t.Fatalf("incoming() error = %v", err)
	}
	backend.resetCalls()
	backend.failNext(1)
	if err := syncer.flushNow(context.Background(), now); err == nil {
		t.Fatal("first flushNow() error = nil, want backend failure")
	}
	syncer.mu.Lock()
	deltaAfterFailure := syncer.states["retry-key"].localDelta
	_, retryScheduled := syncer.retry["retry-key"]
	syncer.mu.Unlock()
	if deltaAfterFailure != 1 {
		t.Fatalf("local delta after backend failure = %d, want preserved 1", deltaAfterFailure)
	}
	if !retryScheduled {
		t.Fatal("backend failure did not schedule retry-key for the next flush")
	}

	if err := syncer.flushNow(context.Background(), now); err != nil {
		t.Fatalf("retry flushNow() error = %v", err)
	}
	if got := backend.keyDeltas(); fmt.Sprint(got) != "[retry-key:1 retry-key:1]" {
		t.Fatalf("backend calls = %v, want failed attempt followed by retry", got)
	}
}

func TestDelayedSyncStopFlushesAllDirtyStatesIncludingDroppedQueueEntries(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 7, reset: 10 * time.Second}
	syncer := newDelayedSyncer(backend, 7, 10*time.Second, time.Hour, 1)
	now := time.Unix(100, 0)

	for _, key := range []string{"queued", "overflow"} {
		if _, _, err := syncer.incoming(context.Background(), key, 1, now); err != nil {
			t.Fatalf("incoming(%q) error = %v", key, err)
		}
	}
	backend.resetCalls()
	syncer.Stop()

	got := backend.keyDeltas()
	slices.SortFunc(got, func(left, right delayedSyncCall) int {
		return strings.Compare(left.key, right.key)
	})
	if fmt.Sprint(got) != "[overflow:1 queued:1]" {
		t.Fatalf("shutdown flush calls = %v, want every dirty state", got)
	}
}

func TestDelayedSyncStopFlushesPendingDelta(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 7, reset: 10 * time.Second}
	syncer := newDelayedSyncer(backend, 7, 10*time.Second, time.Hour, 10000)
	now := time.Unix(100, 0)

	if _, _, err := syncer.incoming(context.Background(), "shutdown-key", 1, now); err != nil {
		t.Fatalf("incoming() error = %v", err)
	}
	syncer.Stop()

	if got := backend.deltas(); len(got) != 2 || got[1] != 1 {
		t.Fatalf("remote deltas after Stop = %v, want pending delta 1 flushed", got)
	}
}

func TestDelayedSlidingFlushKeepsTheReservationWindowAcrossRollover(t *testing.T) {
	backend := &recordingDelayedSyncBackend{limit: 2, reset: 5 * time.Second}
	syncer := newDelayedSyncer(backend, 2, 5*time.Second, time.Hour, 10000)
	t.Cleanup(syncer.Stop)
	reservedAt := time.Unix(104, 900_000_000)
	flushedAt := time.Unix(105, 100_000_000)

	if _, _, err := syncer.incoming(context.Background(), "rollover-key", 1, reservedAt); err != nil {
		t.Fatalf("incoming() error = %v", err)
	}
	if err := syncer.flushNow(context.Background(), flushedAt); err != nil {
		t.Fatalf("flushNow() error = %v", err)
	}

	times := backend.syncTimes()
	if len(times) != 2 {
		t.Fatalf("backend sync times = %v, want initial read and one flush", times)
	}
	if !times[1].Equal(reservedAt) {
		t.Fatalf("flush counter time = %s, want reservation time %s", times[1], reservedAt)
	}
}

func TestDelayedSlidingFlushStoresTheReservedDeltaUnderTheRuntimeWindowKey(t *testing.T) {
	store := newMemorySlidingWindowStore()
	limiter := newSlidingWindowLimiter(store, "plugin-limit-count", 2, 60)
	syncer := newDelayedSyncer(
		slidingWindowDelayedBackend{limiter: limiter},
		2,
		60*time.Second,
		time.Hour,
		10000,
	)
	t.Cleanup(syncer.Stop)
	reservedAt := time.Unix(1_750_000_001, 0)
	key := "route:delayed-sliding:redis-user"

	for request, expected := range []int64{1, 0} {
		remaining, _, err := syncer.incoming(context.Background(), key, 1, reservedAt)
		if err != nil {
			t.Fatalf("request %d error = %v", request+1, err)
		}
		if remaining != expected {
			t.Fatalf("request %d remaining = %d, want %d", request+1, remaining, expected)
		}
	}
	if _, _, err := syncer.incoming(context.Background(), key, 1, reservedAt); !errors.Is(err, errDelayedSyncRejected) {
		t.Fatalf("third request error = %v, want %v", err, errDelayedSyncRejected)
	}
	if err := syncer.flushNow(context.Background(), reservedAt.Add(time.Second)); err != nil {
		t.Fatalf("flushNow() error = %v", err)
	}

	runtimeKey, _ := limiter.counterKeys(key, reservedAt)
	if !strings.HasPrefix(runtimeKey, "plugin-limit-count:route:delayed-sliding:redis-user.") ||
		!strings.HasSuffix(runtimeKey, ".counter") {
		t.Fatalf("runtime counter key = %q, want plugin, route, resolved key, and window id", runtimeKey)
	}
	if got := store.count(runtimeKey, reservedAt); got != 2 {
		t.Fatalf("runtime counter %q = %d, want exact flushed delta 2", runtimeKey, got)
	}
}

func TestRequestResolvedLimitsDisableDelayedSyncWorkers(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        "$http_x_count",
		TimeWindow:   "$http_x_window",
		Policy:       "redis",
		RedisHost:    "127.0.0.1",
		SyncInterval: 1,
	})

	if p.delayedSyncEnabledFor(60) {
		t.Fatal("request-resolved limits enabled delayed-sync worker creation")
	}
	if p.delayedByKey != nil {
		t.Fatalf("delayed syncer map = %#v, want no permanent per-combination state", p.delayedByKey)
	}
}

func TestRequestResolvedLimitsUseSharedQuotaWithoutCachingCombinations(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      "$http_x_count",
		TimeWindow: "$http_x_window",
	})

	first, err := p.limiterFor(2, 60)
	if err != nil {
		t.Fatalf("limiterFor(2, 60) error = %v", err)
	}
	firstQuota, err := first.Get(context.Background(), "shared-key")
	if err != nil {
		t.Fatalf("first Get() error = %v", err)
	}
	if firstQuota.Remaining != 1 {
		t.Fatalf("first remaining = %d, want 1", firstQuota.Remaining)
	}

	second, err := p.limiterFor(1, 60)
	if err != nil {
		t.Fatalf("limiterFor(1, 60) error = %v", err)
	}
	secondQuota, err := second.Get(context.Background(), "shared-key")
	if err != nil {
		t.Fatalf("second Get() error = %v", err)
	}
	if !secondQuota.Reached {
		t.Fatal("stricter request-resolved limit bypassed the shared counter")
	}

	for count := int64(3); count <= 128; count++ {
		if _, err := p.limiterFor(count, 60); err != nil {
			t.Fatalf("limiterFor(%d, 60) error = %v", count, err)
		}
	}
	if got := len(p.limiters); got != 0 {
		t.Fatalf("cached request-resolved limiter combinations = %d, want 0", got)
	}
}

type recordingDelayedSyncBackend struct {
	mu       sync.Mutex
	limit    int64
	reset    time.Duration
	calls    []int64
	keys     []string
	times    []time.Time
	failures int
}

func (b *recordingDelayedSyncBackend) sync(
	_ context.Context,
	key string,
	delta int64,
	now time.Time,
) (int64, time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, delta)
	b.keys = append(b.keys, key)
	b.times = append(b.times, now)
	if b.failures > 0 {
		b.failures--
		return 0, 0, errors.New("recording delayed backend failure")
	}
	b.limit -= delta
	return b.limit, b.reset, nil
}

func (b *recordingDelayedSyncBackend) deltas() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.calls...)
}

func (b *recordingDelayedSyncBackend) syncTimes() []time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]time.Time(nil), b.times...)
}

type delayedSyncCall struct {
	key   string
	delta int64
}

func (c delayedSyncCall) String() string {
	return fmt.Sprintf("%s:%d", c.key, c.delta)
}

func (b *recordingDelayedSyncBackend) keyDeltas() []delayedSyncCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	calls := make([]delayedSyncCall, len(b.calls))
	for i := range b.calls {
		calls[i] = delayedSyncCall{key: b.keys[i], delta: b.calls[i]}
	}
	return calls
}

func (b *recordingDelayedSyncBackend) resetCalls() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = nil
	b.keys = nil
	b.times = nil
}

func (b *recordingDelayedSyncBackend) failNext(count int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = count
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

func TestResolveLimitValueSupportsDefaultExpressions(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)

	count, err := resolveLimitValue(request, "${http_count ?? 2}", "count")
	if err != nil {
		t.Fatalf("resolve default count: %v", err)
	}
	if count != 2 {
		t.Fatalf("default count = %d, want 2", count)
	}
	timeWindow, err := resolveLimitValue(request, "${http_time_window ?? 5}", "time_window")
	if err != nil {
		t.Fatalf("resolve default time_window: %v", err)
	}
	if timeWindow != 5 {
		t.Fatalf("default time_window = %d, want 5", timeWindow)
	}

	request.Header.Set("Count", "5")
	request.Header.Set("Time-Window", "2")
	count, err = resolveLimitValue(request, "${http_count ?? 2}", "count")
	if err != nil {
		t.Fatalf("resolve header count: %v", err)
	}
	if count != 5 {
		t.Fatalf("header count = %d, want 5", count)
	}
	timeWindow, err = resolveLimitValue(request, "${http_time_window ?? 5}", "time_window")
	if err != nil {
		t.Fatalf("resolve header time_window: %v", err)
	}
	if timeWindow != 2 {
		t.Fatalf("header time_window = %d, want 2", timeWindow)
	}
}

func TestResolveLimitValueRejectsInvalidDynamicValues(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantError string
	}{
		{name: "zero", value: "0", wantError: "resolved value must be a positive number"},
		{name: "negative", value: "-1", wantError: "resolved value must be a positive number"},
		{name: "fractional", value: "1.5", wantError: "resolved value must be an integer"},
		{name: "unsafe integer", value: "9999999999999999", wantError: "resolved value exceeds safe integer range"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Count", test.value)
			_, err := resolveLimitValue(request, "${http_count ?? 2}", "count")
			if err == nil {
				t.Fatal("resolveLimitValue() error = nil")
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("resolveLimitValue() error = %q, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestHandlerDynamicCountUpdatesShareOneCounter(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        "${http_count ?? 2}",
		TimeWindow:   10,
		RejectedCode: http.StatusServiceUnavailable,
		Key:          "remote_addr",
		KeyType:      "var",
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i, test := range []struct {
		count     string
		remaining string
	}{
		{count: "3", remaining: "2"},
		{count: "2", remaining: "0"},
		{count: "5", remaining: "2"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Count", test.count)
		request.RemoteAddr = "192.0.2.50:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, response.Code)
		}
		if remaining := response.Header().Get("X-RateLimit-Remaining"); remaining != test.remaining {
			t.Fatalf("request %d remaining = %q, want %q", i+1, remaining, test.remaining)
		}
	}
}

func TestHandlerLogsInvalidResolvedRuleCount(t *testing.T) {
	p := newTestPlugin(t, Config{
		RejectedCode: http.StatusServiceUnavailable,
		Rules: []Rule{{
			Key:        "${http_user}",
			Count:      "${http_count ?? 2}",
			TimeWindow: 60,
		}},
	})
	logged := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("limit-count-invalid-rule-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "resolved value") {
			logged <- entry
		}
	})
	t.Cleanup(stop)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("User", "jack")
	request.Header.Set("Count", "0")
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("invalid rule reached the upstream")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", response.Code)
	}
	select {
	case entry := <-logged:
		if entry.Level != "ERROR" ||
			!strings.Contains(entry.Message, "resolved value must be a positive number") {
			t.Fatalf("log entry = %#v, want invalid resolved value error", entry)
		}
	default:
		t.Fatal("invalid resolved rule count was not logged")
	}
}

func TestHandlerLogsLimiterStoreFailure(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      1,
		TimeWindow: 60,
		Key:        "remote_addr",
		Policy:     "local",
	})
	p.limiter = limiter.New(
		failingLimiterStore{err: errors.New("redis auth failed")},
		limiter.Rate{Period: time.Minute, Limit: 1},
	)

	logged := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("limit-count-store-failure-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "failed to limit count") {
			logged <- entry
		}
	})
	t.Cleanup(stop)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("limiter store failure reached the upstream")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", response.Code)
	}
	select {
	case entry := <-logged:
		if entry.Level != "ERROR" || !strings.Contains(entry.Message, "redis auth failed") {
			t.Fatalf("log entry = %#v, want limiter store error", entry)
		}
	default:
		t.Fatal("limiter store failure was not logged")
	}
}

func TestHandlerPublishesRateLimitingInfoForAccessLogs(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:        2,
		TimeWindow:   10,
		Key:          "http_host",
		KeyType:      "var",
		RejectedCode: http.StatusServiceUnavailable,
	})
	p.SetResourceContext(resource.Route{ID: "1"}, resource.Service{})
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "/hello", nil))
	request.Host = "test.com"
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, request)

	info, ok := apisixctx.GetRequestVar(request, "$rate_limiting_info").(map[string]any)
	if !ok {
		t.Fatalf("$rate_limiting_info = %#v, want an object", apisixctx.GetRequestVar(request, "$rate_limiting_info"))
	}
	want := map[string]any{
		"rate_limiting_key":       "route:1:test.com",
		"rate_limiting_limit":     int64(2),
		"rate_limiting_remaining": int64(1),
		"rate_limiting_reset":     int64(10),
	}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("$rate_limiting_info = %#v, want %#v", info, want)
	}
}

func TestSlidingWindowPublishesRateLimitingInfoForAccessLogs(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      2,
		TimeWindow: 5,
		WindowType: "sliding",
		Key:        "remote_addr",
	})
	p.SetResourceContext(resource.Route{ID: "sliding-info"}, resource.Service{})
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "/", nil))
	request.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	lim := newSlidingWindowLimiter(newMemorySlidingWindowStore(), "plugin-limit-count", 2, 5)

	if allowed := p.runSlidingLimit(
		response,
		request,
		lim,
		2,
		"192.0.2.1",
		defaultHeaders(Metadata{}),
		time.Unix(102, 0),
	); !allowed {
		t.Fatal("first sliding-window request was rejected")
	}

	want := map[string]any{
		"rate_limiting_key":       "route:sliding-info:192.0.2.1",
		"rate_limiting_limit":     int64(2),
		"rate_limiting_remaining": int64(1),
		"rate_limiting_reset":     float64(3),
	}
	if got := apisixctx.GetRequestVar(request, "$rate_limiting_info"); !reflect.DeepEqual(got, want) {
		t.Fatalf("$rate_limiting_info = %#v, want %#v", got, want)
	}
}

func TestDelayedSyncPublishesRateLimitingInfoForAccessLogs(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      2,
		TimeWindow: 10,
		Key:        "remote_addr",
	})
	p.SetResourceContext(resource.Route{ID: "delayed-info"}, resource.Service{})
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "/", nil))
	request.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	syncer := newDelayedSyncer(
		&recordingDelayedSyncBackend{limit: 2, reset: 10 * time.Second},
		2,
		10*time.Second,
		time.Hour,
		10,
	)
	t.Cleanup(syncer.Stop)

	if allowed := p.runDelayedLimit(
		response,
		request,
		syncer,
		2,
		"192.0.2.1",
		defaultHeaders(Metadata{}),
	); !allowed {
		t.Fatal("first delayed-sync request was rejected")
	}

	want := map[string]any{
		"rate_limiting_key":       "route:delayed-info:192.0.2.1",
		"rate_limiting_limit":     int64(2),
		"rate_limiting_remaining": int64(1),
		"rate_limiting_reset":     int64(10),
	}
	if got := apisixctx.GetRequestVar(request, "$rate_limiting_info"); !reflect.DeepEqual(got, want) {
		t.Fatalf("$rate_limiting_info = %#v, want %#v", got, want)
	}
}

func TestResolveKeySupportsAPISIXArgumentAndHTTPHostVariables(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/hello?key=redis-user", nil)
	request.Host = "example-1.com"

	argumentPlugin := &Plugin{config: Config{KeyType: "var", Key: "arg_key"}}
	if got := argumentPlugin.resolveKey(request); got != "redis-user" {
		t.Fatalf("arg_key = %q, want redis-user", got)
	}

	hostPlugin := &Plugin{config: Config{KeyType: "var", Key: "http_host"}}
	if got := hostPlugin.resolveKey(request); got != "example-1.com" {
		t.Fatalf("http_host = %q, want example-1.com", got)
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
