package limit_count

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/real_ip"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
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

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, 1, "test-route", nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() {
		p.Stop()
	})

	return p
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata map[string]any) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		Metadata: mustMetadataView(t, metadata),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, 1, "test-route", nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() {
		p.Stop()
	})
	return p
}

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	view, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func resolvedLimitCountKeyForTest(t *testing.T, p *Plugin, request *http.Request) string {
	t.Helper()
	var resolved string
	if err := p.withResolvedLimitCountKey(request, func(key string) error {
		resolved = key
		return nil
	}); err != nil {
		t.Fatalf("withResolvedLimitCountKey() error = %v", err)
	}
	return resolved
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

	if p.config.RedisHost != limitCountDescriptor("127.0.0.1") {
		t.Fatalf("RedisHost = %q, want content descriptor", p.config.RedisHost)
	}
	if err := p.withLimitCountRedisHost(func(host string) error {
		if host != "127.0.0.1" {
			t.Fatalf("private Redis host = %q, want 127.0.0.1", host)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if p.config.RedisPort != 6380 {
		t.Fatalf("RedisPort = %d, want 6380", p.config.RedisPort)
	}
	if p.config.RedisUsername != "default" {
		t.Fatalf("RedisUsername = %q, want default", p.config.RedisUsername)
	}
	if p.config.RedisPassword != "secret" {
		t.Fatalf("RedisPassword = %q, want secret", p.config.RedisPassword)
	}
	if p.config.RedisDatabase != 2 {
		t.Fatalf("RedisDatabase = %d, want 2", p.config.RedisDatabase)
	}
	if p.config.RedisTimeout != 1500 {
		t.Fatalf("RedisTimeout = %d, want 1500", p.config.RedisTimeout)
	}
	options := p.redisConnConfig().Options()
	if options.PoolSize != 80 || options.ConnMaxIdleTime != 12*time.Second {
		t.Fatalf("Redis pool = %d, idle timeout = %s; want 80 and 12s", options.PoolSize, options.ConnMaxIdleTime)
	}
}

func TestPostInitAppliesRedisBackendDefaultsToRootFields(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		check  func(*testing.T, Config)
	}{
		{
			name: "redis",
			config: Config{
				Count: "$http_x_limit", TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1",
			},
			check: func(t *testing.T, config Config) {
				t.Helper()
				if config.RedisPort != 6379 || config.RedisTimeout != 1000 ||
					config.RedisKeepaliveTimeout != 10000 || config.RedisKeepalivePool != 100 {
					t.Fatalf("Redis defaults = port %d timeout %d keepalive %d/%d", config.RedisPort,
						config.RedisTimeout, config.RedisKeepaliveTimeout, config.RedisKeepalivePool)
				}
				if config.RedisSSL == nil || *config.RedisSSL ||
					config.RedisSSLVerify == nil || *config.RedisSSLVerify {
					t.Fatalf("Redis TLS defaults = %#v/%#v, want false/false", config.RedisSSL, config.RedisSSLVerify)
				}
			},
		},
		{
			name: "redis cluster",
			config: Config{
				Count: "$http_x_limit", TimeWindow: 60, Policy: "redis-cluster",
				RedisClusterNodes: []string{"127.0.0.1:7000"}, RedisClusterName: "fixture-cluster",
			},
			check: func(t *testing.T, config Config) {
				t.Helper()
				if config.RedisTimeout != 1000 || config.RedisKeepaliveTimeout != 10000 ||
					config.RedisKeepalivePool != 100 {
					t.Fatalf("Redis cluster defaults = timeout %d keepalive %d/%d", config.RedisTimeout,
						config.RedisKeepaliveTimeout, config.RedisKeepalivePool)
				}
				if config.RedisClusterSSL == nil || *config.RedisClusterSSL ||
					config.RedisClusterSSLVerify == nil || *config.RedisClusterSSLVerify {
					t.Fatalf("Redis cluster TLS defaults = %#v/%#v, want false/false",
						config.RedisClusterSSL, config.RedisClusterSSLVerify)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, test.config)
			test.check(t, p.config)
		})
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

func TestMetadataSchemaAcceptsQuotaHeaderNames(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(map[string]any{
		"limit_header":     "X-Limit-N",
		"remaining_header": "X-Remaining-N",
		"reset_header":     "X-Reset-N",
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for _, field := range []string{"limit_header", "remaining_header", "reset_header"} {
		if err := util.Validate(map[string]any{field: 1}, p.GetMetadataSchema()); err == nil {
			t.Fatalf("non-string %s accepted", field)
		}
	}
}

func TestPreparedGenerationsRetainMetadataHeaders(t *testing.T) {
	first := newTestPluginWithMetadata(t, Config{Count: 2, TimeWindow: 60}, map[string]any{
		"limit_header":     "X-Limit-N",
		"remaining_header": "X-Remaining-N",
		"reset_header":     "X-Reset-N",
	})
	second := newTestPluginWithMetadata(t, Config{Count: 2, TimeWindow: 60}, map[string]any{
		"limit_header":     "X-Limit-N-Plus-One",
		"remaining_header": "X-Remaining-N-Plus-One",
		"reset_header":     "X-Reset-N-Plus-One",
	})

	serve := func(p *Plugin) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "192.0.2.10:1234"
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("response status = %d, want %d", recorder.Code, http.StatusNoContent)
		}
		return recorder
	}

	firstResponse := serve(first)
	for _, header := range []string{"X-Limit-N", "X-Remaining-N", "X-Reset-N"} {
		if firstResponse.Header().Get(header) == "" {
			t.Fatalf("generation N response missing %s", header)
		}
	}
	if got := firstResponse.Header().Get("X-Limit-N-Plus-One"); got != "" {
		t.Fatalf("generation N response leaked N+1 header = %q", got)
	}

	secondResponse := serve(second)
	for _, header := range []string{"X-Limit-N-Plus-One", "X-Remaining-N-Plus-One", "X-Reset-N-Plus-One"} {
		if secondResponse.Header().Get(header) == "" {
			t.Fatalf("generation N+1 response missing %s", header)
		}
	}
	if got := secondResponse.Header().Get("X-Limit-N"); got != "" {
		t.Fatalf("generation N+1 response leaked N header = %q", got)
	}
}

func TestMetadataDecodeFailsBeforeLimitCountGroupRegistration(t *testing.T) {
	p := &Plugin{config: Config{
		Count:      1,
		TimeWindow: 60,
		Group:      t.Name(),
	}}
	p.SetDependencies(base.Dependencies{Metadata: mustMetadataView(t, map[string]any{
		"remaining_header": 1,
	})})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, 3, "metadata-invalid", nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.groupRegistered || p.limiter != nil {
		t.Fatalf("decode failure registered resources: group=%v limiter=%v", p.groupRegistered, p.limiter)
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

	var options *redis.ClusterOptions
	if err := p.withLimitCountRedisNodes(func(nodes []string) error {
		runtimeConfig := p.redisClusterConnConfig()
		runtimeConfig.Nodes = slices.Clone(nodes)
		options = runtimeConfig.ClusterOptions()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
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

func TestRealIPWrapsLimitCountWithIndependentQuota(t *testing.T) {
	limitCount := newTestPlugin(t, Config{
		Count:        1,
		TimeWindow:   60,
		Key:          "remote_addr",
		RejectedCode: http.StatusTooManyRequests,
	})
	realIP := &real_ip.Plugin{}
	*realIP.Config().(*real_ip.Config) = real_ip.Config{
		Source:           "http_x_forwarded_for",
		TrustedAddresses: []string{"10.0.0.0/8"},
	}
	if err := realIP.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := realIP.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	handler := realIP.Handler(limitCount.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	request := func(xff string) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-Forwarded-For", xff)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Code
	}

	if status := request("203.0.113.1"); status != http.StatusNoContent {
		t.Fatalf("first client status = %d, want %d", status, http.StatusNoContent)
	}
	if status := request("203.0.113.2"); status != http.StatusNoContent {
		t.Fatalf("second client status = %d, want independent quota %d", status, http.StatusNoContent)
	}
	if status := request("203.0.113.1"); status != http.StatusTooManyRequests {
		t.Fatalf("first client repeat status = %d, want shared quota rejection %d", status, http.StatusTooManyRequests)
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

func TestScopedSecretsRedactEnvironmentKeyAndStopCleanly(t *testing.T) {
	t.Setenv("LIMIT_COUNT_KEY", "remote_addr")
	const raw = "$ENV://LIMIT_COUNT_KEY"
	p, cleanup := prepareScopedLimitCountPlugin(t, 50, "plugin-key", Config{
		Count:      2,
		TimeWindow: 60,
		Key:        raw,
	}, map[string]string{raw: "remote_addr"})
	defer cleanup()
	if p.config.Key != limitCountDescriptor("remote_addr") {
		t.Fatalf("key = %q, want resolved content descriptor", p.config.Key)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	if got := resolvedLimitCountKeyForTest(t, p, request); got != "192.0.2.10" {
		t.Fatalf("resolveKey() = %q, want materialized remote_addr behavior", got)
	}
	p.Stop()
	if p.scopedSet || p.scopedKeySecret != (secret.Value{}) {
		t.Fatal("Stop() retained scoped key state")
	}
}

func TestScopedSecretsRedactRedisHostAndBuildResolvedClient(t *testing.T) {
	t.Setenv("LIMIT_COUNT_REDIS_HOST", "127.0.0.2")
	const raw = "$ENV://LIMIT_COUNT_REDIS_HOST"
	p, cleanup := prepareScopedLimitCountPlugin(t, 51, "plugin-host", Config{
		Count:      2,
		TimeWindow: 60,
		Policy:     "redis",
		RedisHost:  raw,
	}, map[string]string{raw: "127.0.0.2"})
	defer cleanup()
	if p.config.RedisHost != limitCountDescriptor("127.0.0.2") {
		t.Fatalf("Redis host = %q, want resolved content descriptor", p.config.RedisHost)
	}
	client, err := p.redisBackendClient()
	if err != nil {
		t.Fatalf("redisBackendClient() error = %v", err)
	}
	if options := client.(*redis.Client).Options(); options.Addr != "127.0.0.2:6379" {
		t.Fatalf("Redis address = %q, want 127.0.0.2:6379", options.Addr)
	}
	p.Stop()
	if p.scopedSet || p.scopedRedisHost != (secret.Value{}) {
		t.Fatal("Stop() retained scoped Redis host state")
	}
}

func TestScopedSecretsRedactRedisClusterNodesAndBuildResolvedClient(t *testing.T) {
	t.Setenv("LIMIT_COUNT_REDIS_NODE_0", "127.0.0.1:5000")
	t.Setenv("LIMIT_COUNT_REDIS_NODE_1", "127.0.0.1:5001")

	raws := []string{"$ENV://LIMIT_COUNT_REDIS_NODE_0", "$ENV://LIMIT_COUNT_REDIS_NODE_1"}
	p, cleanup := prepareScopedLimitCountPlugin(t, 52, "plugin-cluster", Config{
		Count:             2,
		TimeWindow:        60,
		Policy:            "redis-cluster",
		RedisClusterNodes: raws,
		RedisClusterName:  "redis-cluster-1",
	}, map[string]string{raws[0]: "127.0.0.1:5000", raws[1]: "127.0.0.1:5001"})
	defer cleanup()
	for i, resolved := range []string{"127.0.0.1:5000", "127.0.0.1:5001"} {
		if p.config.RedisClusterNodes[i] != limitCountDescriptor(resolved) {
			t.Fatalf(
				"Redis cluster node %d = %q, want resolved content descriptor",
				i,
				p.config.RedisClusterNodes[i],
			)
		}
	}
	client, err := p.redisBackendClient()
	if err != nil {
		t.Fatalf("redisBackendClient() error = %v", err)
	}
	want := []string{"127.0.0.1:5000", "127.0.0.1:5001"}
	if options := client.(*redis.ClusterClient).Options(); !slices.Equal(options.Addrs, want) {
		t.Fatalf("Redis cluster addresses = %#v, want %#v", options.Addrs, want)
	}
	p.Stop()
	if p.scopedSet || len(p.scopedRedisClusterNodes) != 0 {
		t.Fatal("Stop() retained scoped Redis cluster node state")
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

func TestHandlerResolvesRuleKeyDefaultValue(t *testing.T) {
	p := newTestPlugin(t, Config{
		RejectedCode: http.StatusServiceUnavailable,
		Rules: []Rule{{
			Count:      1,
			TimeWindow: 60,
			Key:        "${http_project ?? apisix}",
		}},
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want default-key request allowed", first.Code)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want shared default-key quota rejection", second.Code)
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

func TestRequestResolvedRedisLimitsAcquireBackendOnce(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      "$http_x_count",
		TimeWindow: 60,
		Policy:     "redis",
		RedisHost:  "127.0.0.1",
	})
	t.Cleanup(p.Stop)

	var first redis.UniversalClient
	for i := range 32 {
		client, err := p.redisBackendClient()
		if err != nil {
			t.Fatalf("redisBackendClient(%d) error = %v", i, err)
		}
		if first == nil {
			first = client
		} else if client != first {
			t.Fatalf("redisBackendClient(%d) = %p, want first client %p", i, client, first)
		}
	}
	if p.clientRelease == nil {
		t.Fatal("plugin did not retain the single backend release owner")
	}
}

func TestStopReleasesRedisBackendOnce(t *testing.T) {
	p := &Plugin{}
	var releases atomic.Int64
	p.clientRelease = func() { releases.Add(1) }

	p.Stop()
	p.Stop()
	if got := releases.Load(); got != 1 {
		t.Fatalf("backend releases = %d, want exactly 1", got)
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
	secrets, scope, _, cleanup := newScopedSecretHarness(t, 60, "string-count", nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
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

func TestHandlerSkipsInvalidRuleBeforeApplyingLaterValidRule(t *testing.T) {
	p := newTestPlugin(t, Config{
		RejectedCode: http.StatusTooManyRequests,
		Rules: []Rule{
			{
				Count:      "$http_x_bad_count",
				TimeWindow: 60,
				Key:        "$http_x_bad_user",
			},
			{
				Count:        1,
				TimeWindow:   60,
				Key:          "$http_x_user",
				HeaderPrefix: "User",
			},
		},
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Bad-Count", "not-a-number")
	request.Header.Set("X-Bad-User", "invalid-rule-key")
	request.Header.Set("X-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want later valid rule to reach upstream", response.Code)
	}
	if got := response.Header().Get("X-User-RateLimit-Limit"); got != "1" {
		t.Fatalf("later valid rule limit header = %q, want 1", got)
	}
}

func TestPostInitDefersEmptyStringLimitsToRequest(t *testing.T) {
	p := newTestPlugin(t, Config{Count: "", TimeWindow: ""})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("empty string limits reached upstream")
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want request-time 500", response.Code)
	}
}

func TestHandlerTreatsExplicitEmptyRulesAsConfigured(t *testing.T) {
	for _, test := range []struct {
		name             string
		allowDegradation bool
		wantStatus       int
	}{
		{name: "fails closed", wantStatus: http.StatusInternalServerError},
		{name: "degrades", allowDegradation: true, wantStatus: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowDegradation := test.allowDegradation
			var config Config
			if err := util.Parse(map[string]any{"rules": []any{}}, &config); err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if config.Rules == nil {
				t.Fatal("explicit rules presence was lost during parsing")
			}
			config.AllowDegradation = &allowDegradation
			p := newTestPlugin(t, config)
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

			if response.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d", response.Code, test.wantStatus)
			}
		})
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
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := response.Body.String(); got != `{"error_msg":"failed to limit count"}` {
		t.Fatalf("response body = %q, want APISIX backend error JSON", got)
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

func TestResolveKeySupportsAPISIXArgumentAndHTTPHostVariables(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/hello?key=redis-user", nil)
	request.Host = "example-1.com"

	argumentPlugin := &Plugin{config: Config{KeyType: "var", Key: "arg_key"}}
	argumentCapability, argumentScope, _, argumentCleanup := newScopedSecretHarness(t, 61, "argument-key", nil)
	t.Cleanup(argumentCleanup)
	materializeScopedLimitCount(t, argumentPlugin, argumentCapability, argumentScope)
	if got := resolvedLimitCountKeyForTest(t, argumentPlugin, request); got != "redis-user" {
		t.Fatalf("arg_key = %q, want redis-user", got)
	}

	hostPlugin := &Plugin{config: Config{KeyType: "var", Key: "http_host"}}
	hostCapability, hostScope, _, hostCleanup := newScopedSecretHarness(t, 62, "host-key", nil)
	t.Cleanup(hostCleanup)
	materializeScopedLimitCount(t, hostPlugin, hostCapability, hostScope)
	if got := resolvedLimitCountKeyForTest(t, hostPlugin, request); got != "example-1.com" {
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
	secrets, scope, _, cleanup := newScopedSecretHarness(t, 64, "string-rule", nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
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

func TestGroupRegistryReleasesLastOwner(t *testing.T) {
	resetLimitCountGroupsForTest()
	t.Cleanup(resetLimitCountGroupsForTest)

	config := Config{Count: 2, TimeWindow: 60, Policy: "local", Group: "shared"}
	first := newTestPlugin(t, config)
	second := newTestPlugin(t, config)

	limitCountGroups.Lock()
	entry, ok := limitCountGroups.entries[config.Group]
	limitCountGroups.Unlock()
	if !ok || entry.refs != 2 {
		t.Fatalf("group entry = %#v/%t, want refs=2", entry, ok)
	}

	first.Stop()
	limitCountGroups.Lock()
	entry, ok = limitCountGroups.entries[config.Group]
	limitCountGroups.Unlock()
	if !ok || entry.refs != 1 {
		t.Fatalf("group entry after first Stop = %#v/%t, want refs=1", entry, ok)
	}

	second.Stop()
	limitCountGroups.Lock()
	_, ok = limitCountGroups.entries[config.Group]
	limitCountGroups.Unlock()
	if ok {
		t.Fatal("group entry remains after final owner Stop")
	}

	second.Stop()
	limitCountGroups.Lock()
	_, ok = limitCountGroups.entries[config.Group]
	limitCountGroups.Unlock()
	if ok {
		t.Fatal("group entry recreated by idempotent Stop")
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

func TestStaticLimitValue(t *testing.T) {
	if _, _, err := staticLimitValue(nil, "count"); err == nil {
		t.Fatal("staticLimitValue(nil) error = nil, want required field error")
	}
	if value, ok, err := staticLimitValue("$request_method", "count"); err != nil || ok || value != 0 {
		t.Fatalf("staticLimitValue(expression) = %d/%t/%v, want unresolved", value, ok, err)
	}
	if value, ok, err := staticLimitValue("42", "count"); err != nil || !ok || value != 42 {
		t.Fatalf("staticLimitValue(string) = %d/%t/%v, want 42/true", value, ok, err)
	}
	if value, ok, err := staticLimitValue(int64(7), "count"); err != nil || !ok || value != 7 {
		t.Fatalf("staticLimitValue(int64) = %d/%t/%v, want 7/true", value, ok, err)
	}
	if _, _, err := staticLimitValue("not-a-number", "count"); err == nil {
		t.Fatal("staticLimitValue(invalid) error = nil")
	}
}

func TestNumericLimitValue(t *testing.T) {
	if value, err := numericLimitValue(3, "count"); err != nil || value != 3 {
		t.Fatalf("numericLimitValue(int) = %d/%v, want 3", value, err)
	}
	if value, err := numericLimitValue(int64(4), "count"); err != nil || value != 4 {
		t.Fatalf("numericLimitValue(int64) = %d/%v, want 4", value, err)
	}
	if value, err := numericLimitValue(json.Number("5"), "count"); err != nil || value != 5 {
		t.Fatalf("numericLimitValue(json.Number) = %d/%v, want 5", value, err)
	}
	if _, err := numericLimitValue(1.5, "count"); err == nil {
		t.Fatal("numericLimitValue(fractional) error = nil")
	}
	if _, err := numericLimitValue(true, "count"); err == nil {
		t.Fatal("numericLimitValue(wrong type) error = nil")
	}
	if _, err := numericLimitValue(int64(-1), "count"); err == nil {
		t.Fatal("numericLimitValue(negative) error = nil")
	}
	if _, err := numericLimitValue(float64(maxSafeInteger)+1, "count"); err == nil {
		t.Fatal("numericLimitValue(overflow) error = nil")
	}
}

func TestResolveLimitValueExpressions(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/orders", nil)
	request.Header.Set("Content-Length", "7")

	if value, err := resolveLimitValue(request, int64(10), "count"); err != nil || value != 10 {
		t.Fatalf("resolveLimitValue(int) = %d/%v, want 10", value, err)
	}
	if value, err := resolveLimitValue(request, "${content_length ?? 33}", "count"); err != nil || value != 7 {
		t.Fatalf("resolveLimitValue(default expr) = %d/%v, want 7", value, err)
	}
	noHeaderRequest := httptest.NewRequest(http.MethodPost, "http://example.test/orders", nil)
	if value, err := resolveLimitValue(noHeaderRequest, "${content_length ?? 33}", "count"); err != nil || value != 33 {
		t.Fatalf("resolveLimitValue(default fallback) = %d/%v, want 33", value, err)
	}
	if value, err := resolveLimitValue(request, "$content_length", "count"); err != nil || value != 7 {
		t.Fatalf("resolveLimitValue(var expr) = %d/%v, want 7", value, err)
	}
}

func TestLimiterFactoriesCacheStaticLimitsAndIsolateDynamicLimits(t *testing.T) {
	fixed := &Plugin{config: Config{Policy: "local"}}
	first, err := fixed.limiterFor(10, 60)
	if err != nil {
		t.Fatalf("limiterFor(first) error = %v", err)
	}
	second, err := fixed.limiterFor(10, 60)
	if err != nil {
		t.Fatalf("limiterFor(second) error = %v", err)
	}
	other, err := fixed.limiterFor(20, 60)
	if err != nil {
		t.Fatalf("limiterFor(other) error = %v", err)
	}
	if first != second || first == other {
		t.Fatalf("static limiter identities = %p, %p, %p", first, second, other)
	}

	dynamic := &Plugin{config: Config{Policy: "local"}, dynamicLimits: true}
	dynamicFirst, err := dynamic.limiterFor(10, 60)
	if err != nil {
		t.Fatalf("dynamic limiterFor(first) error = %v", err)
	}
	dynamicSecond, err := dynamic.limiterFor(10, 60)
	if err != nil {
		t.Fatalf("dynamic limiterFor(second) error = %v", err)
	}
	if dynamicFirst == dynamicSecond {
		t.Fatal("dynamic limiterFor() reused a limiter with request-resolved limits")
	}
}

func TestLimitCountLocalStoreEvictsOldestAndExpired(t *testing.T) {
	original := defaultLocalStoreCapacity
	defaultLocalStoreCapacity = 4
	t.Cleanup(func() { defaultLocalStoreCapacity = original })

	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	now := base
	store := newLocalFixedWindowStore(func() time.Time { return now }, defaultLocalStoreCapacity)
	rate := limiter.Rate{Period: 60 * time.Second, Limit: 100}

	for i := range 6 {
		ctx, err := store.Increment(context.Background(), "key-"+strconv.Itoa(i), 1, rate)
		if err != nil {
			t.Fatalf("increment key-%d: %v", i, err)
		}
		if ctx.Remaining != 99 {
			t.Fatalf("increment key-%d remaining = %d, want 99", i, ctx.Remaining)
		}
	}

	// Capacity 4 was exceeded, so the two oldest counters were evicted and
	// key-0 restarts from a fresh counter.
	ctx, err := store.Peek(context.Background(), "key-0", rate)
	if err != nil {
		t.Fatalf("peek key-0 after eviction: %v", err)
	}
	if ctx.Remaining != 100 {
		t.Fatalf("evicted key-0 remaining = %d, want 100", ctx.Remaining)
	}

	// Active keys preserve their counters: key-2 was incremented once.
	ctx, err = store.Peek(context.Background(), "key-2", rate)
	if err != nil {
		t.Fatalf("peek key-2: %v", err)
	}
	if ctx.Remaining != 99 {
		t.Fatalf("active key-2 remaining = %d, want 99", ctx.Remaining)
	}

	// Advancing past the window expires key-5 and resets its counter.
	now = base.Add(2 * time.Minute)
	ctx, err = store.Peek(context.Background(), "key-5", rate)
	if err != nil {
		t.Fatalf("peek key-5 after expiry: %v", err)
	}
	if ctx.Remaining != 100 {
		t.Fatalf("expired key-5 remaining = %d, want 100", ctx.Remaining)
	}
}

func TestLocalFixedWindowDoesNotSlideOnIncrement(t *testing.T) {
	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	now := base
	store := newLocalFixedWindowStore(func() time.Time { return now }, defaultLocalStoreCapacity)
	rate := limiter.Rate{Period: 60 * time.Second, Limit: 100}

	ctx, err := store.Increment(context.Background(), "anchored", 1, rate)
	if err != nil {
		t.Fatalf("increment at t=0: %v", err)
	}
	if ctx.Remaining != 99 {
		t.Fatalf("increment at t=0 remaining = %d, want 99", ctx.Remaining)
	}

	now = base.Add(59 * time.Second)
	ctx, err = store.Increment(context.Background(), "anchored", 1, rate)
	if err != nil {
		t.Fatalf("increment at t=59s: %v", err)
	}
	if ctx.Remaining != 98 {
		t.Fatalf("increment at t=59s remaining = %d, want 98", ctx.Remaining)
	}
	if ctx.Reset != base.Add(time.Minute).Unix() {
		t.Fatalf(
			"increment at t=59s reset = %d, want anchored window reset %d",
			ctx.Reset,
			base.Add(time.Minute).Unix(),
		)
	}

	now = base.Add(61 * time.Second)
	ctx, err = store.Increment(context.Background(), "anchored", 1, rate)
	if err != nil {
		t.Fatalf("increment at t=61s: %v", err)
	}
	if ctx.Remaining != 99 {
		t.Fatalf("increment at t=61s remaining = %d, want a fresh count of 1", ctx.Remaining)
	}
	if want := now.Add(rate.Period).Unix(); ctx.Reset != want {
		t.Fatalf("increment at t=61s reset = %d, want fresh window reset %d", ctx.Reset, want)
	}
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
		RejectedCode: http.StatusServiceUnavailable,
		Rules: []Rule{{
			Key:        "${http_user}",
			Count:      "${http_count ?? 2}",
			TimeWindow: 60,
		}},
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("User", "jack")
	request.Header.Set("Count", "5")
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	select {
	case entry := <-entries:
		t.Fatalf("limit key logged at info level: %q", entry.Message)
	case <-time.After(100 * time.Millisecond):
	}

	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatalf("configure debug level: %v", err)
	}
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("User", "jack")
	request.Header.Set("Count", "5")
	response = httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	select {
	case entry := <-entries:
		if !strings.Contains(entry.Message, "jack") {
			t.Fatalf("debug entry = %q, want the resolved limit key", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("limit key not logged at debug level")
	}
}
