package limit_conn

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
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

func TestPostInitDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
	})

	if p.GetName() != "limit-conn" {
		t.Fatalf("GetName() = %q, want limit-conn", p.GetName())
	}
	if p.GetPriority() != 1003 {
		t.Fatalf("GetPriority() = %d, want 1003", p.GetPriority())
	}
	if p.config.Policy != "local" {
		t.Fatalf("Policy = %q, want local", p.config.Policy)
	}
	if p.config.KeyType != "var" {
		t.Fatalf("KeyType = %q, want var", p.config.KeyType)
	}
	if p.config.RejectedCode != http.StatusServiceUnavailable {
		t.Fatalf("RejectedCode = %d, want %d", p.config.RejectedCode, http.StatusServiceUnavailable)
	}
}

func TestPostInitAcceptsRedisPolicyDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
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
	if p.config.RedisKeyTTL != 3600 {
		t.Fatalf("RedisKeyTTL = %d, want 3600", p.config.RedisKeyTTL)
	}
	if p.config.RedisSSL == nil || *p.config.RedisSSL {
		t.Fatalf("RedisSSL = %v, want false", p.config.RedisSSL)
	}
	if p.config.RedisSSLVerify == nil || *p.config.RedisSSLVerify {
		t.Fatalf("RedisSSLVerify = %v, want false", p.config.RedisSSLVerify)
	}
	options := p.redisConnConfig().Options()
	if options.PoolSize != 100 || options.ConnMaxIdleTime != 10*time.Second {
		t.Fatalf("redis pool = %d, idle timeout = %s", options.PoolSize, options.ConnMaxIdleTime)
	}
}

func TestSchemaAcceptsRedisPolicyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"conn":                    1,
		"burst":                   0,
		"default_conn_delay":      0.1,
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
		"key_ttl":                 3600,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected redis policy fields: %v", err)
	}
}

func TestSchemaAcceptsRedisClusterPolicyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"conn":                     1,
		"burst":                    0,
		"default_conn_delay":       0.1,
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
		"key_ttl":                  7200,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected redis-cluster policy fields: %v", err)
	}

	delete(config, "redis_cluster_nodes")
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("schema accepted redis-cluster policy without redis_cluster_nodes")
	}
}

func TestPostInitBuildsRedisClusterOptions(t *testing.T) {
	ssl := true
	verify := false
	p := newTestPlugin(t, Config{
		Conn:                  1,
		Burst:                 0,
		DefaultConnDelay:      0.1,
		Key:                   "remote_addr",
		Policy:                "redis-cluster",
		RedisClusterNodes:     []string{"127.0.0.1:5000", "127.0.0.1:5001"},
		RedisPassword:         "secret",
		RedisTimeout:          1500,
		RedisClusterName:      "cluster-1",
		RedisClusterSSL:       &ssl,
		RedisClusterSSLVerify: &verify,
		RedisKeepaliveTimeout: 12000,
		RedisKeepalivePool:    80,
		RedisKeyTTL:           7200,
		OnlyUseDefaultDelay:   true,
	})

	options := p.redisClusterConnConfig().ClusterOptions()
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
	limiter, ok := p.redisLimiter.(*redisConnLimiter)
	if !ok || !limiter.onlyUseDefaultDelay {
		t.Fatalf("redis limiter = %#v, want only_use_default_delay enabled", p.redisLimiter)
	}
}

func TestHandlerScopesRedisClusterAdmissionAndReleaseKeyByRoute(t *testing.T) {
	redisLimiter := &fakeRedisConnLimiter{allowed: true}
	p := newTestPlugin(t, Config{
		Conn:              1,
		Burst:             0,
		DefaultConnDelay:  0.1,
		Key:               "remote_addr",
		Policy:            "redis-cluster",
		RedisClusterNodes: []string{"127.0.0.1:5000"},
		RedisClusterName:  "cluster-1",
	})
	p.redisLimiter = redisLimiter
	p.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), "192.0.2.70:12345")
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	wantPrefix := "route:route-1:192.0.2.70:config:"
	if redisLimiter.key != redisLimiter.leavingKey || !strings.HasPrefix(redisLimiter.key, wantPrefix) {
		t.Fatalf(
			"admission/release keys = %q/%q, want matching keys with prefix %q",
			redisLimiter.key,
			redisLimiter.leavingKey,
			wantPrefix,
		)
	}
	if redisLimiter.left != 1 {
		t.Fatalf("redis leaving calls = %d, want 1", redisLimiter.left)
	}
}

func TestRedisScopesDistinctLimitConfigsOnSameRoute(t *testing.T) {
	routeLimiter := &fakeRedisConnLimiter{allowed: true}
	routePlugin := newTestPlugin(t, Config{
		Conn:             4,
		Burst:            1,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
	})
	routePlugin.redisLimiter = routeLimiter
	routePlugin.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})

	globalLimiter := &fakeRedisConnLimiter{allowed: true}
	globalPlugin := newTestPlugin(t, Config{
		Conn:             2,
		Burst:            1,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
	})
	globalPlugin.redisLimiter = globalLimiter
	globalPlugin.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})

	if _, _, err := routePlugin.increase("192.0.2.70", 4, 1); err != nil {
		t.Fatalf("route increase error = %v", err)
	}
	if _, _, err := globalPlugin.increase("192.0.2.70", 2, 1); err != nil {
		t.Fatalf("global increase error = %v", err)
	}
	if routeLimiter.key == globalLimiter.key {
		t.Fatalf("route/global redis keys both = %q, want distinct config scopes", routeLimiter.key)
	}
}

func TestHandlerRejectsConcurrentRequestsAboveConnAndBurst(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Go(func() {
		performRequest(handler, "192.0.2.10:12345")
	})
	<-started

	second := performRequest(handler, "192.0.2.10:23456")
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second response code = %d, want %d", second.Code, http.StatusServiceUnavailable)
	}

	close(block)
	wg.Wait()

	afterRelease := performRequest(handler, "192.0.2.10:34567")
	if afterRelease.Code != http.StatusNoContent {
		t.Fatalf("after release response code = %d, want %d", afterRelease.Code, http.StatusNoContent)
	}
}

func TestHandlerUsesRejectedMessage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		RejectedMsg:      "too many connections",
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Go(func() {
		performRequest(handler, "192.0.2.20:12345")
	})
	<-started

	rejected := performRequest(handler, "192.0.2.20:23456")
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", rejected.Code, http.StatusServiceUnavailable)
	}
	if got := rejected.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := rejected.Body.String(); got != `{"error_msg":"too many connections"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"too many connections"}`)
	}

	close(block)
	wg.Wait()
}

func TestHandlerUsesRedisLimiter(t *testing.T) {
	redisLimiter := &fakeRedisConnLimiter{allowed: true}
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
	})
	p.redisLimiter = redisLimiter

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), "192.0.2.70:12345")

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if !strings.HasPrefix(redisLimiter.key, "192.0.2.70:config:") {
		t.Fatalf("redis key = %q, want config-scoped 192.0.2.70 key", redisLimiter.key)
	}
	if redisLimiter.left != 1 {
		t.Fatalf("redis leaving calls = %d, want 1", redisLimiter.left)
	}
}

func TestHandlerRejectsWhenRedisLimiterRejects(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
		RejectedMsg:      "too many connections",
	})
	p.redisLimiter = &fakeRedisConnLimiter{allowed: false}

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), "192.0.2.80:12345")

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := res.Body.String(); got != `{"error_msg":"too many connections"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"too many connections"}`)
	}
}

func TestHandlerLogsRedisLimiterError(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
	})
	p.redisLimiter = &fakeRedisConnLimiter{
		err: errors.New("WRONGPASS invalid username-password pair or user is disabled"),
	}

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("limit-conn-redis-error-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "WRONGPASS") {
			entries <- entry
		}
	})
	defer stop()

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), "192.0.2.81:12345")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusInternalServerError)
	}

	select {
	case entry := <-entries:
		want := "failed to limit conn: WRONGPASS invalid username-password pair or user is disabled"
		if entry.Message != want {
			t.Fatalf("log message = %q, want %q", entry.Message, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Redis limiter error was not logged")
	}
}

func TestLogRedisConnectionReuseReportsPoolHits(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatalf("enable debug logging: %v", err)
	}
	entries := make(chan logger.Entry, 2)
	stop := logger.ReplaceObserver("limit-conn-redis-reuse-test", func(entry logger.Entry) {
		if strings.HasPrefix(entry.Message, "redis connection reused times:") {
			entries <- entry
		}
	})
	defer stop()

	logRedisConnectionReuse(fakeRedisPoolStatsProvider{hits: 0})
	logRedisConnectionReuse(fakeRedisPoolStatsProvider{hits: 1})

	for _, want := range []string{
		"redis connection reused times: 0",
		"redis connection reused times: 1",
	} {
		select {
		case entry := <-entries:
			if entry.Message != want {
				t.Fatalf("log message = %q, want %q", entry.Message, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("did not observe %q", want)
		}
	}
}

func TestHandlerTracksSeparateKeys(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RemoteAddr == "192.0.2.30:12345" {
			startedOnce.Do(func() {
				close(started)
			})
			<-block
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Go(func() {
		performRequest(handler, "192.0.2.30:12345")
	})
	<-started

	secondKey := performRequest(handler, "192.0.2.31:12345")
	if secondKey.Code != http.StatusNoContent {
		t.Fatalf("second key response code = %d, want %d", secondKey.Code, http.StatusNoContent)
	}

	close(block)
	wg.Wait()
}

func TestIncreaseUsesDefaultDelayWhenConfigured(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:                1,
		Burst:               2,
		DefaultConnDelay:    0.2,
		Key:                 "remote_addr",
		OnlyUseDefaultDelay: true,
	})

	firstDelay, allowed, err := p.increase("client", 1, 2)
	if err != nil {
		t.Fatalf("increase() error = %v", err)
	}
	if !allowed {
		t.Fatal("first request rejected, want allowed")
	}
	if firstDelay != 0 {
		t.Fatalf("first delay = %s, want 0", firstDelay)
	}

	secondDelay, allowed, err := p.increase("client", 1, 2)
	if err != nil {
		t.Fatalf("increase() error = %v", err)
	}
	if !allowed {
		t.Fatal("second request rejected, want allowed")
	}
	if secondDelay != 200*time.Millisecond {
		t.Fatalf("second delay = %s, want 200ms", secondDelay)
	}

	thirdDelay, allowed, err := p.increase("client", 1, 2)
	if err != nil {
		t.Fatalf("increase() error = %v", err)
	}
	if !allowed {
		t.Fatal("third request rejected, want allowed")
	}
	if thirdDelay != 400*time.Millisecond {
		t.Fatalf("third delay = %s, want 400ms", thirdDelay)
	}
}

func TestDecreaseAdaptsUnitDelayFromDownstreamLatency(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            1,
		DefaultConnDelay: 0.2,
		Key:              "remote_addr",
	})

	if _, allowed, err := p.increase("client", 1, 1); err != nil || !allowed {
		t.Fatalf("initial increase = allowed %v, error %v", allowed, err)
	}
	latency := 600 * time.Millisecond
	p.decrease("client", &latency)

	if delay, allowed, err := p.increase("client", 1, 1); err != nil || !allowed || delay != 0 {
		t.Fatalf("first adapted increase = delay %s, allowed %v, error %v", delay, allowed, err)
	}
	delay, allowed, err := p.increase("client", 1, 1)
	if err != nil || !allowed {
		t.Fatalf("second adapted increase = allowed %v, error %v", allowed, err)
	}
	if delay != 400*time.Millisecond {
		t.Fatalf("adapted delay = %s, want 400ms", delay)
	}
}

func TestDecreaseKeepsDefaultUnitDelayWhenConfigured(t *testing.T) {
	p := newTestPlugin(t, Config{
		Conn:                1,
		Burst:               1,
		DefaultConnDelay:    0.2,
		Key:                 "remote_addr",
		OnlyUseDefaultDelay: true,
	})

	if _, allowed, err := p.increase("client", 1, 1); err != nil || !allowed {
		t.Fatalf("initial increase = allowed %v, error %v", allowed, err)
	}
	latency := 600 * time.Millisecond
	p.decrease("client", &latency)
	if _, allowed, err := p.increase("client", 1, 1); err != nil || !allowed {
		t.Fatalf("first fixed increase = allowed %v, error %v", allowed, err)
	}
	delay, allowed, err := p.increase("client", 1, 1)
	if err != nil || !allowed {
		t.Fatalf("second fixed increase = allowed %v, error %v", allowed, err)
	}
	if delay != 200*time.Millisecond {
		t.Fatalf("fixed delay = %s, want 200ms", delay)
	}
}

func TestHandlerAppliesResolvedRules(t *testing.T) {
	p := newTestPlugin(t, Config{
		DefaultConnDelay: 0.1,
		RejectedCode:     http.StatusTooManyRequests,
		Rules: []Rule{
			{Conn: 2, Burst: 0, Key: "$http_x_tenant"},
			{Conn: 1, Burst: 0, Key: "$http_x_user"},
		},
	})

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User") == "alice" {
			startedOnce.Do(func() {
				close(started)
			})
			<-block
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Go(func() {
		performRequestWithHeaders(handler, "192.0.2.40:12345", map[string]string{
			"X-Tenant": "t1",
			"X-User":   "alice",
		})
	})
	<-started

	rejected := performRequestWithHeaders(handler, "192.0.2.40:23456", map[string]string{
		"X-Tenant": "t1",
		"X-User":   "alice",
	})
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected response code = %d, want %d", rejected.Code, http.StatusTooManyRequests)
	}

	differentUser := performRequestWithHeaders(handler, "192.0.2.40:34567", map[string]string{
		"X-Tenant": "t1",
		"X-User":   "bob",
	})
	if differentUser.Code != http.StatusNoContent {
		t.Fatalf("different user response code = %d, want %d", differentUser.Code, http.StatusNoContent)
	}

	close(block)
	wg.Wait()

	afterRelease := performRequestWithHeaders(handler, "192.0.2.40:45678", map[string]string{
		"X-Tenant": "t1",
		"X-User":   "alice",
	})
	if afterRelease.Code != http.StatusNoContent {
		t.Fatalf("after release response code = %d, want %d", afterRelease.Code, http.StatusNoContent)
	}
}

func TestHandlerReturnsInternalServerErrorWhenAllRulesAreUnresolved(t *testing.T) {
	p := newTestPlugin(t, Config{
		DefaultConnDelay: 0.1,
		Rules: []Rule{
			{Conn: 1, Burst: 0, Key: "tenant"},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := performRequest(handler, "192.0.2.50:12345")
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusInternalServerError)
	}
}

func TestHandlerAllowsDegradationWhenAllRulesAreUnresolved(t *testing.T) {
	allowDegradation := true
	p := newTestPlugin(t, Config{
		DefaultConnDelay: 0.1,
		AllowDegradation: &allowDegradation,
		Rules: []Rule{
			{Conn: 1, Burst: 0, Key: "tenant"},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := performRequest(handler, "192.0.2.60:12345")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusNoContent)
	}
}

func TestHandlerResolvesStringConnAndBurst(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"conn":               "$http_x_conn",
		"burst":              "$http_x_burst",
		"default_conn_delay": 0.1,
		"key":                "remote_addr",
		"rejected_code":      http.StatusTooManyRequests,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("string conn/burst config should validate: %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Go(func() {
		performRequestWithHeaders(handler, "192.0.2.70:12345", map[string]string{
			"X-Conn":  "1",
			"X-Burst": "0",
		})
	})
	<-started

	rejected := performRequestWithHeaders(handler, "192.0.2.70:23456", map[string]string{
		"X-Conn":  "1",
		"X-Burst": "0",
	})
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected response code = %d, want %d", rejected.Code, http.StatusTooManyRequests)
	}

	close(block)
	wg.Wait()
}

func TestHandlerResolvesStringRuleConnAndBurst(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"default_conn_delay": 0.1,
		"rejected_code":      http.StatusTooManyRequests,
		"rules": []any{
			map[string]any{
				"conn":  "$http_x_conn",
				"burst": "$http_x_burst",
				"key":   "$http_x_user",
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("string rule conn/burst config should validate: %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	var wg sync.WaitGroup
	wg.Go(func() {
		performRequestWithHeaders(handler, "192.0.2.80:12345", map[string]string{
			"X-Conn":  "1",
			"X-Burst": "0",
			"X-User":  "alice",
		})
	})
	<-started

	rejected := performRequestWithHeaders(handler, "192.0.2.80:23456", map[string]string{
		"X-Conn":  "1",
		"X-Burst": "0",
		"X-User":  "alice",
	})
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected response code = %d, want %d", rejected.Code, http.StatusTooManyRequests)
	}

	close(block)
	wg.Wait()
}

func TestResolveLimitValueSupportsDefaultExpressions(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)

	conn, err := resolveLimitValue(req, "${http_conn ?? 5}", "conn", false)
	if err != nil {
		t.Fatalf("resolve default conn: %v", err)
	}
	if conn != 5 {
		t.Fatalf("default conn = %d, want 5", conn)
	}

	burst, err := resolveLimitValue(req, "${http_burst ?? 2}", "burst", true)
	if err != nil {
		t.Fatalf("resolve default burst: %v", err)
	}
	if burst != 2 {
		t.Fatalf("default burst = %d, want 2", burst)
	}

	req.Header.Set("Conn", "3")
	req.Header.Set("Burst", "4")
	conn, err = resolveLimitValue(req, "${http_conn ?? 5}", "conn", false)
	if err != nil {
		t.Fatalf("resolve header conn: %v", err)
	}
	if conn != 3 {
		t.Fatalf("header conn = %d, want 3", conn)
	}
	burst, err = resolveLimitValue(req, "${http_burst ?? 2}", "burst", true)
	if err != nil {
		t.Fatalf("resolve header burst: %v", err)
	}
	if burst != 4 {
		t.Fatalf("header burst = %d, want 4", burst)
	}
}

func TestResolveRuleKeySkipsMissingVariable(t *testing.T) {
	p := &Plugin{}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rule := Rule{Key: "${http_project}"}

	if key, ok := p.resolveRuleKey(req, 1, rule); ok {
		t.Fatalf("resolveRuleKey() = %q, true; want missing variable to skip the rule", key)
	}

	req.Header.Set("Project", "apisix")
	key, ok := p.resolveRuleKey(req, 1, rule)
	if !ok {
		t.Fatal("resolveRuleKey() skipped a present project variable")
	}
	if key != "rule:1:apisix" {
		t.Fatalf("resolveRuleKey() = %q, want rule:1:apisix", key)
	}
}

func TestResolveLimitValueRejectsInvalidDynamicValues(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		value      string
		expression string
		allowZero  bool
		wantError  string
	}{
		{
			name:       "zero conn",
			header:     "Conn",
			value:      "0",
			expression: "${http_conn ?? 5}",
			wantError:  "resolved value must be a positive number",
		},
		{
			name:       "negative conn",
			header:     "Conn",
			value:      "-1",
			expression: "${http_conn ?? 5}",
			wantError:  "resolved value must be a positive number",
		},
		{
			name:       "fractional conn",
			header:     "Conn",
			value:      "1.5",
			expression: "${http_conn ?? 5}",
			wantError:  "resolved value must be an integer",
		},
		{
			name:       "conn above safe integer range",
			header:     "Conn",
			value:      "99007199254740993",
			expression: "${http_conn ?? 5}",
			wantError:  "resolved value exceeds safe integer range",
		},
		{
			name:       "negative burst",
			header:     "Burst",
			value:      "-1",
			expression: "${http_burst ?? 2}",
			allowZero:  true,
			wantError:  "resolved value must be a non-negative number",
		},
		{
			name:       "fractional burst",
			header:     "Burst",
			value:      "1.5",
			expression: "${http_burst ?? 2}",
			allowZero:  true,
			wantError:  "resolved value must be an integer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
			req.Header.Set(test.header, test.value)

			_, err := resolveLimitValue(req, test.expression, strings.ToLower(test.header), test.allowZero)
			if err == nil {
				t.Fatal("resolveLimitValue() error = nil")
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("resolveLimitValue() error = %q, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestResolveLimitValueAcceptsZeroDynamicBurst(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Burst", "0")

	burst, err := resolveLimitValue(req, "${http_burst ?? 2}", "burst", true)
	if err != nil {
		t.Fatalf("resolveLimitValue() error = %v", err)
	}
	if burst != 0 {
		t.Fatalf("resolveLimitValue() = %d, want 0", burst)
	}
}

func TestResolveKeyUsesConsumerName(t *testing.T) {
	p := &Plugin{config: Config{Key: "consumer_name"}}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{"$consumer_name": "consumer_jack"})

	if key := p.resolveKey(req); key != "consumer_jack" {
		t.Fatalf("resolveKey() = %q, want consumer_jack", key)
	}
}

func TestResolveKeyUsesServerAddr(t *testing.T) {
	p := &Plugin{config: Config{Key: "server_addr"}}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = req.WithContext(context.WithValue(
		req.Context(),
		http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 8080},
	))

	if key := p.resolveKey(req); key != "127.0.0.2" {
		t.Fatalf("resolveKey() = %q, want 127.0.0.2", key)
	}
}

func TestRequestLimitKeyPreservesNginxVariables(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?a=1", nil)
	req.Header.Set("Content-Length", "12")

	tests := []struct {
		key  string
		want string
	}{
		{key: "query_string", want: "a=1"},
		{key: "content_length", want: "12"},
		{key: "request_line", want: "GET /get?a=1 HTTP/1.1"},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			if got := requestLimitKey(req, test.key); got != test.want {
				t.Fatalf("requestLimitKey(%q) = %q, want %q", test.key, got, test.want)
			}
		})
	}
}

func TestResolveKeyLogsFallbackToClientIP(t *testing.T) {
	p := &Plugin{config: Config{
		Key:     "$http_a $http_b",
		KeyType: "var_combination",
	}}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = "192.0.2.90:12345"

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("limit-conn-fallback-key-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "configured key is empty") {
			entries <- entry
		}
	})
	defer stop()

	if key := p.resolveKey(req); key != "192.0.2.90" {
		t.Fatalf("resolveKey() = %q, want 192.0.2.90", key)
	}
	select {
	case entry := <-entries:
		want := "The value of the configured key is empty, use client IP instead"
		if entry.Message != want {
			t.Fatalf("log message = %q, want %q", entry.Message, want)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback to client IP was not logged")
	}
}

func TestDecreaseLogsMeasuredAndDefaultRequestLatency(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatalf("enable debug logging: %v", err)
	}
	tests := []struct {
		name                string
		onlyUseDefaultDelay bool
		want                string
	}{
		{name: "measured latency", want: "request latency is 0.1"},
		{name: "default latency", onlyUseDefaultDelay: true, want: "request latency is nil"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Conn:                1,
				Burst:               0,
				DefaultConnDelay:    0.3,
				Key:                 "remote_addr",
				OnlyUseDefaultDelay: test.onlyUseDefaultDelay,
			})
			p.conns["client"] = 1

			entries := make(chan logger.Entry, 1)
			stop := logger.ReplaceObserver("limit-conn-latency-test", func(entry logger.Entry) {
				if strings.HasPrefix(entry.Message, "request latency is") {
					entries <- entry
				}
			})
			defer stop()

			latency := 100 * time.Millisecond
			p.decrease("client", &latency)
			select {
			case entry := <-entries:
				if entry.Message != test.want {
					t.Fatalf("log message = %q, want %q", entry.Message, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("request latency was not logged")
			}
		})
	}
}

func performRequest(handler http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = remoteAddr

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func performRequestWithHeaders(
	handler http.Handler,
	remoteAddr string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = remoteAddr
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

type fakeRedisConnLimiter struct {
	key        string
	leavingKey string
	delay      time.Duration
	allowed    bool
	err        error
	left       int
}

type scriptedConnRedisClient struct {
	redis.UniversalClient
	result any
	err    error
	keys   []string
	args   []any
}

func (f *scriptedConnRedisClient) Eval(_ context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	f.keys = append([]string(nil), keys...)
	f.args = append([]any(nil), args...)
	return redis.NewCmdResult(f.result, f.err)
}

func (f *scriptedConnRedisClient) PoolStats() *redis.PoolStats {
	return &redis.PoolStats{}
}

func TestRedisConnLimiterDecodesAdmissionAndUpdatesMeasuredDelay(t *testing.T) {
	client := &scriptedConnRedisClient{result: []any{int64(1), "250"}}
	limiter := &redisConnLimiter{
		client:    client,
		unitDelay: 2,
		keyTTL:    90 * time.Second,
	}

	delay, allowed, err := limiter.incoming("route-a:client-a", 3, 4)
	if err != nil {
		t.Fatalf("incoming() error = %v", err)
	}
	if delay != 250*time.Millisecond || !allowed {
		t.Fatalf("incoming() = delay %s, allowed %t", delay, allowed)
	}
	if len(client.keys) != 1 || client.keys[0] != "plugin-limit-conn:route-a:client-a" {
		t.Fatalf("Eval keys = %#v", client.keys)
	}
	wantArgs := []any{3, 4, float64(2), int64(90_000)}
	if len(client.args) != len(wantArgs) {
		t.Fatalf("Eval args = %#v, want %#v", client.args, wantArgs)
	}
	for i := range wantArgs {
		if client.args[i] != wantArgs[i] {
			t.Fatalf("Eval arg %d = %#v, want %#v", i, client.args[i], wantArgs[i])
		}
	}

	latency := 4 * time.Second
	client.result = nil
	if err := limiter.leaving("route-a:client-a", &latency); err != nil {
		t.Fatalf("leaving() error = %v", err)
	}
	if limiter.unitDelay != 3 {
		t.Fatalf("unitDelay = %v, want averaged 3 seconds", limiter.unitDelay)
	}
}

func TestRedisConnLimiterFailsClosedOnBackendAndProtocolErrors(t *testing.T) {
	tests := []struct {
		name   string
		result any
		err    error
	}{
		{name: "backend error", err: errors.New("redis unavailable")},
		{name: "wrong result type", result: "invalid"},
		{name: "wrong result length", result: []any{1}},
		{name: "invalid allowed", result: []any{[]byte("1"), 2}},
		{name: "invalid delay", result: []any{1, "invalid"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limiter := &redisConnLimiter{
				client: &scriptedConnRedisClient{result: test.result, err: test.err},
				keyTTL: time.Minute,
			}
			_, _, err := limiter.incoming("key", 1, 2)
			if err == nil {
				t.Fatal("incoming() error = nil")
			}
		})
	}
}

func TestRedisConnLimiterLeavingPreservesConfiguredDelayAndErrors(t *testing.T) {
	backendErr := errors.New("redis unavailable")
	client := &scriptedConnRedisClient{err: backendErr}
	limiter := &redisConnLimiter{client: client, unitDelay: 2}
	latency := 4 * time.Second
	if err := limiter.leaving("key", &latency); !errors.Is(err, backendErr) {
		t.Fatalf("leaving() error = %v, want %v", err, backendErr)
	}
	if limiter.unitDelay != 2 {
		t.Fatalf("unitDelay changed after backend error: %v", limiter.unitDelay)
	}

	client.err = nil
	limiter.onlyUseDefaultDelay = true
	if err := limiter.leaving("key", &latency); err != nil {
		t.Fatalf("leaving(default delay) error = %v", err)
	}
	if limiter.unitDelay != 2 {
		t.Fatalf("configured unitDelay changed: %v", limiter.unitDelay)
	}

	limiter.onlyUseDefaultDelay = false
	if err := limiter.leaving("key", nil); err != nil {
		t.Fatalf("leaving(nil latency) error = %v", err)
	}
	if limiter.unitDelay != 2 {
		t.Fatalf("unitDelay changed for nil latency: %v", limiter.unitDelay)
	}
}

func TestRedisConnIntRejectsOverflowAndInvalidWireValues(t *testing.T) {
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
		got, ok := redisInt(test.value)
		if got != test.want || ok != test.ok {
			t.Fatalf("redisInt(%#v) = %d, %t; want %d, %t", test.value, got, ok, test.want, test.ok)
		}
	}
}

type fakeRedisPoolStatsProvider struct {
	hits uint32
}

func (f fakeRedisPoolStatsProvider) PoolStats() *redis.PoolStats {
	return &redis.PoolStats{Hits: f.hits}
}

func (f *fakeRedisConnLimiter) incoming(key string, conn int, burst int) (time.Duration, bool, error) {
	f.key = key
	return f.delay, f.allowed, f.err
}

func (f *fakeRedisConnLimiter) leaving(key string, _ *time.Duration) error {
	f.leavingKey = key
	f.left++
	return f.err
}

func TestLimitKeyLogIsDebugLevel(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	entries := make(chan logger.Entry, 4)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		if strings.HasPrefix(entry.Message, "limit key:") {
			entries <- entry
		}
	})
	t.Cleanup(stop)

	p := newTestPlugin(t, Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.1,
		Key:              "remote_addr",
		Policy:           "redis",
		RedisHost:        "127.0.0.1",
	})
	p.redisLimiter = &fakeRedisConnLimiter{allowed: true}

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), "192.0.2.80:12345")
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", res.Code)
	}
	select {
	case entry := <-entries:
		t.Fatalf("limit key logged at info level: %q", entry.Message)
	case <-time.After(100 * time.Millisecond):
	}

	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatalf("configure debug level: %v", err)
	}
	res = performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), "192.0.2.80:12345")
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", res.Code)
	}
	select {
	case entry := <-entries:
		if !strings.Contains(entry.Message, "192.0.2.80") || strings.Contains(entry.Message, "route") {
			t.Fatalf("debug entry = %q, want clean client-IP limit key", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("limit key not logged at debug level")
	}
}
