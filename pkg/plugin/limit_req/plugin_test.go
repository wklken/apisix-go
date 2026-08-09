package limit_req

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

func performRequest(handler http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = remoteAddr

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestPostInitDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:  1,
		Burst: 1,
		Key:   "remote_addr",
	})

	if p.GetName() != "limit-req" {
		t.Fatalf("GetName() = %q, want limit-req", p.GetName())
	}
	if p.GetPriority() != 1001 {
		t.Fatalf("GetPriority() = %d, want 1001", p.GetPriority())
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
	if p.config.Nodelay == nil || *p.config.Nodelay {
		t.Fatalf("Nodelay = %v, want false", p.config.Nodelay)
	}
}

func TestPostInitAcceptsRedisPolicyDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:      1,
		Burst:     1,
		Key:       "remote_addr",
		Policy:    "redis",
		RedisHost: "127.0.0.1",
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
		"rate":                    1,
		"burst":                   1,
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

func TestSchemaAcceptsRedisClusterPolicyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"rate":                     1,
		"burst":                    1,
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
		Rate:                  1,
		Burst:                 1,
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
}

func TestHandlerScopesRedisClusterKeyByRoute(t *testing.T) {
	redisLimiter := &fakeRedisLimiter{allowed: true}
	p := newTestPlugin(t, Config{
		Rate:              1,
		Burst:             0,
		Key:               "remote_addr",
		Policy:            "redis-cluster",
		RedisClusterNodes: []string{"127.0.0.1:5000"},
		RedisClusterName:  "cluster-1",
		Nodelay:           new(true),
	})
	p.redisLimiter = redisLimiter
	p.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), "192.0.2.40:12345")
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if redisLimiter.key != "route:route-1:192.0.2.40" {
		t.Fatalf("redis key = %q, want route-scoped key", redisLimiter.key)
	}
}

func TestConsumerLimiterSharesQuotaAcrossRouteInstancesAndIsolatesConsumers(t *testing.T) {
	config := Config{
		Rate:         1,
		Burst:        1,
		Key:          "remote_addr",
		RejectedCode: http.StatusTooManyRequests,
		Nodelay:      new(true),
	}
	first := newTestPlugin(t, config)
	first.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})
	second := newTestPlugin(t, config)
	second.SetResourceContext(resource.Route{ID: "route-2"}, resource.Service{})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	request := func(plugin *Plugin, consumer string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://example.com/hello", nil)
		req.RemoteAddr = "192.0.2.40:12345"
		req = apisixctx.WithApisixVars(req, map[string]string{"$consumer_name": consumer})
		req = apisixctx.WithConsumerPluginOverrides(req, map[string]struct{}{name: {}})
		res := httptest.NewRecorder()
		plugin.Handler(next).ServeHTTP(res, req)
		return res
	}

	if got := request(first, "shared-limit-req-consumer").Code; got != http.StatusNoContent {
		t.Fatalf("first route first response = %d, want %d", got, http.StatusNoContent)
	}
	if got := request(first, "shared-limit-req-consumer").Code; got != http.StatusNoContent {
		t.Fatalf("first route burst response = %d, want %d", got, http.StatusNoContent)
	}
	if got := request(second, "shared-limit-req-consumer").Code; got != http.StatusTooManyRequests {
		t.Fatalf("second route response = %d, want shared quota rejection %d", got, http.StatusTooManyRequests)
	}
	if got := request(second, "isolated-limit-req-consumer").Code; got != http.StatusNoContent {
		t.Fatalf("different consumer response = %d, want isolated quota %d", got, http.StatusNoContent)
	}
}

func TestConsumerBucketStoreReleasesLastOwner(t *testing.T) {
	resetConsumerBucketStoresForTest()
	t.Cleanup(resetConsumerBucketStoresForTest)

	config := Config{Rate: 1, Burst: 1, Key: "remote_addr"}
	first := newTestPlugin(t, config)
	second := newTestPlugin(t, config)
	if store := first.consumerBucketStore(); store == nil {
		t.Fatal("first consumer bucket store is nil")
	} else if store != second.consumerBucketStore() {
		t.Fatal("identical consumer configurations did not share a bucket store")
	}

	key := first.consumerStoreKey
	consumerBucketStores.Lock()
	entry, ok := consumerBucketStores.entries[key]
	consumerBucketStores.Unlock()
	if !ok || entry.refs != 2 {
		t.Fatalf("consumer bucket entry = %#v/%t, want refs=2", entry, ok)
	}

	neverUsed := newTestPlugin(t, config)
	neverUsed.Stop()
	consumerBucketStores.Lock()
	entry, ok = consumerBucketStores.entries[key]
	consumerBucketStores.Unlock()
	if !ok || entry.refs != 2 {
		t.Fatalf("entry after unused owner Stop = %#v/%t, want refs=2", entry, ok)
	}

	first.Stop()
	consumerBucketStores.Lock()
	entry, ok = consumerBucketStores.entries[key]
	consumerBucketStores.Unlock()
	if !ok || entry.refs != 1 {
		t.Fatalf("entry after first Stop = %#v/%t, want refs=1", entry, ok)
	}

	second.Stop()
	consumerBucketStores.Lock()
	_, ok = consumerBucketStores.entries[key]
	consumerBucketStores.Unlock()
	if ok {
		t.Fatal("entry remains after final owner Stop")
	}
	second.Stop()
}

func TestConsumerRedisLimiterUsesConsumerScopeInsteadOfRouteScope(t *testing.T) {
	redisLimiter := &fakeRedisLimiter{allowed: true}
	p := newTestPlugin(t, Config{
		Rate:              1,
		Burst:             0,
		Key:               "remote_addr",
		Policy:            "redis-cluster",
		RedisClusterNodes: []string{"127.0.0.1:5000"},
		RedisClusterName:  "cluster-1",
		Nodelay:           new(true),
	})
	p.redisLimiter = redisLimiter
	p.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/hello", nil)
	req.RemoteAddr = "192.0.2.40:12345"
	req = apisixctx.WithApisixVars(req, map[string]string{"$consumer_name": "jack"})
	req = apisixctx.WithConsumerPluginOverrides(req, map[string]struct{}{name: {}})
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if redisLimiter.key != "consumer:jack:192.0.2.40" {
		t.Fatalf("Redis consumer key = %q, want consumer scope", redisLimiter.key)
	}
}

func resetConsumerBucketStoresForTest() {
	consumerBucketStores.Lock()
	consumerBucketStores.entries = map[string]consumerBucketEntry{}
	consumerBucketStores.Unlock()
}

func TestResolveKeyLogsFallbackToClientIP(t *testing.T) {
	p := &Plugin{config: Config{
		Key:     "$http_a $http_b",
		KeyType: "var_combination",
	}}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.RemoteAddr = "192.0.2.90:12345"

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("limit-req-fallback-key-test", func(entry logger.Entry) {
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

func TestHandlerUsesRedisLimiter(t *testing.T) {
	redisLimiter := &fakeRedisLimiter{allowed: true}
	p := newTestPlugin(t, Config{
		Rate:      1,
		Burst:     0,
		Key:       "remote_addr",
		Policy:    "redis",
		RedisHost: "127.0.0.1",
		Nodelay:   new(true),
	})
	p.redisLimiter = redisLimiter

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	res := performRequest(handler, "192.0.2.40:12345")
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if redisLimiter.key != "192.0.2.40" {
		t.Fatalf("redis key = %q, want 192.0.2.40", redisLimiter.key)
	}
	if redisLimiter.rate != 1 {
		t.Fatalf("redis rate = %f, want 1", redisLimiter.rate)
	}
	if redisLimiter.burst != 0 {
		t.Fatalf("redis burst = %f, want 0", redisLimiter.burst)
	}
}

func TestHandlerLogsRedisLimiterError(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:      1,
		Burst:     0,
		Key:       "remote_addr",
		Policy:    "redis",
		RedisHost: "127.0.0.1",
	})
	p.redisLimiter = &fakeRedisLimiter{
		err: errors.New("WRONGPASS invalid username-password pair or user is disabled"),
	}

	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver("limit-req-redis-error-test", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "WRONGPASS") {
			entries <- entry
		}
	})
	defer stop()

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), "192.0.2.41:12345")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusInternalServerError)
	}

	select {
	case entry := <-entries:
		want := "failed to limit req: WRONGPASS invalid username-password pair or user is disabled"
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
	stop := logger.ReplaceObserver("limit-req-redis-reuse-test", func(entry logger.Entry) {
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

func TestHandlerRejectsWhenRedisLimiterRejects(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:        1,
		Burst:       0,
		Key:         "remote_addr",
		Policy:      "redis",
		RedisHost:   "127.0.0.1",
		RejectedMsg: "slow down",
		Nodelay:     new(true),
	})
	p.redisLimiter = &fakeRedisLimiter{allowed: false}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	res := performRequest(handler, "192.0.2.50:12345")
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
	if got := res.Body.String(); got != `{"error_msg":"slow down"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"slow down"}`)
	}
}

func TestHandlerRejectsRequestsAboveRateAndBurst(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:    1,
		Burst:   0,
		Key:     "remote_addr",
		Nodelay: new(true),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := performRequest(handler, "192.0.2.10:12345")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusNoContent)
	}

	second := performRequest(handler, "192.0.2.10:23456")
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second response code = %d, want %d", second.Code, http.StatusServiceUnavailable)
	}
}

func TestHandlerUsesRejectedMessage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:        1,
		Burst:       0,
		Key:         "remote_addr",
		RejectedMsg: "slow down",
		Nodelay:     new(true),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	performRequest(handler, "192.0.2.20:12345")
	rejected := performRequest(handler, "192.0.2.20:23456")

	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", rejected.Code, http.StatusServiceUnavailable)
	}
	if got := rejected.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := rejected.Body.String(); got != `{"error_msg":"slow down"}` {
		t.Fatalf("response body = %q, want %q", got, `{"error_msg":"slow down"}`)
	}
}

func TestHandlerTracksSeparateKeys(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rate:    1,
		Burst:   0,
		Key:     "remote_addr",
		Nodelay: new(true),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	performRequest(handler, "192.0.2.30:12345")

	secondKey := performRequest(handler, "192.0.2.31:12345")
	if secondKey.Code != http.StatusNoContent {
		t.Fatalf("second key response code = %d, want %d", secondKey.Code, http.StatusNoContent)
	}
}

type fakeRedisLimiter struct {
	key     string
	rate    float64
	burst   float64
	delay   time.Duration
	allowed bool
	err     error
}

type fakeRedisPoolStatsProvider struct {
	hits uint32
}

func (f fakeRedisPoolStatsProvider) PoolStats() *redis.PoolStats {
	return &redis.PoolStats{Hits: f.hits}
}

func (f *fakeRedisLimiter) incoming(key string, rate float64, burst float64) (time.Duration, bool, error) {
	f.key = key
	f.rate = rate
	f.burst = burst
	return f.delay, f.allowed, f.err
}

func TestLimitReqLocalBucketsEvictOldestAndExpired(t *testing.T) {
	original := defaultLocalBucketsCapacity
	defaultLocalBucketsCapacity = 4
	t.Cleanup(func() { defaultLocalBucketsCapacity = original })

	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	p := newTestPlugin(t, Config{Rate: 10, Burst: 20, Policy: "local"})
	p.now = func() time.Time { return base }

	for i := range 6 {
		_, allowed, err := p.incomingWithConsumer("user-"+strconv.Itoa(i), "")
		if err != nil {
			t.Fatalf("incoming user-%d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("incoming user-%d not allowed", i)
		}
	}

	// Active keys preserve their counters: user-2 already consumed once.
	// Checked before touching user-0, whose re-insertion would evict the
	// oldest remaining live bucket (user-2) at capacity.
	delay, _, err := p.incomingWithConsumer("user-2", "")
	if err != nil {
		t.Fatalf("incoming user-2: %v", err)
	}
	if delay <= 0 {
		t.Fatalf("active key user-2 lost its counter, delay = %v", delay)
	}

	// Capacity 4 was exceeded, so the two oldest buckets were evicted and
	// user-0 restarts from a fresh counter.
	delay, _, err = p.incomingWithConsumer("user-0", "")
	if err != nil {
		t.Fatalf("incoming user-0 after eviction: %v", err)
	}
	if delay != 0 {
		t.Fatalf("evicted key user-0 delay = %v, want 0", delay)
	}

	// Advancing past the bucket TTL expires user-5 and resets its counter.
	p.now = func() time.Time { return base.Add(time.Hour) }
	delay, _, err = p.incomingWithConsumer("user-5", "")
	if err != nil {
		t.Fatalf("incoming user-5 after expiry: %v", err)
	}
	if delay != 0 {
		t.Fatalf("expired key user-5 delay = %v, want 0", delay)
	}
}
