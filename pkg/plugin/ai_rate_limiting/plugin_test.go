package ai_rate_limiting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy_multi"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config, now func() time.Time) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg, now: now}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, closeAttempt := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	t.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

type scopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type scopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []scopedSecretCall
}

func (broker *scopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, scopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func newScopedSecretHarness(
	t *testing.T, values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	const revision = uint64(7)
	key := generation.ResourceKey{Kind: "routes", ID: "ai-rate-test"}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{"ai-rate-limiting":{}}}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "ai-rate-test",
		}},
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &scopedSecretBroker{values: maps.Clone(values), fail: make(map[string]error)}
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).
		PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return materialization.Secrets(), scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret materialization: %v", err)
		}
	}
}

func assertSecretDescriptor(t *testing.T, field, value string) {
	t.Helper()
	const prefix = "plugin_config#sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		t.Fatalf("%s = %q, want descriptor", field, value)
	}
}

type constructorCapture struct {
	redisOptions    []*redis.Options
	failoverOptions []*redis.FailoverOptions
	client          redisClient
}

func captureRedisConstructors(t *testing.T, client redisClient) *constructorCapture {
	t.Helper()
	capture := &constructorCapture{client: client}
	oldRedisClient := newRedisClient
	oldFailoverClient := newRedisFailoverClient
	newRedisClient = func(options *redis.Options) redisClient {
		capture.redisOptions = append(capture.redisOptions, options)
		return capture.client
	}
	newRedisFailoverClient = func(options *redis.FailoverOptions) redisClient {
		capture.failoverOptions = append(capture.failoverOptions, options)
		return capture.client
	}
	t.Cleanup(func() {
		newRedisClient = oldRedisClient
		newRedisFailoverClient = oldFailoverClient
	})
	return capture
}

func TestMaterializeScopedSecretsOwnsRedisAndSentinelPasswords(t *testing.T) {
	const (
		redisRaw    = "$ENV://AI_REDIS_PASSWORD"
		sentinelRaw = "$secret://vault/ai-sentinel-password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, map[string]string{
		redisRaw:    "redis-password",
		sentinelRaw: "sentinel-password",
	})
	defer closeAttempt()

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "redis-sentinel",
		RedisMasterName:  "mymaster",
		RedisSentinels:   []RedisSentinel{{Host: "127.0.0.1", Port: 26379}},
		RedisPassword:    redisRaw,
		SentinelPassword: sentinelRaw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if len(broker.calls) != 2 {
		t.Fatalf("scoped calls = %d, want 2", len(broker.calls))
	}
	if got := broker.calls[0].Scope.Field; got != "redis_password" {
		t.Fatalf("first scope field = %q, want redis_password", got)
	}
	if got := broker.calls[1].Scope.Field; got != "sentinel_password" {
		t.Fatalf("second scope field = %q, want sentinel_password", got)
	}
	for _, call := range broker.calls {
		if call.Scope.Generation != scope.Generation ||
			call.Scope.Domain != generation.DomainHTTP ||
			call.Scope.Plugin != name ||
			call.Scope.Resource != scope.Resource ||
			call.Scope.Source != capability.SecretPluginConfig {
			t.Fatalf(
				"scoped authority = %#v, want generation/http/%s/%#v/%s",
				call.Scope, name, scope.Resource, capability.SecretPluginConfig,
			)
		}
	}
	assertSecretDescriptor(t, "redis_password", p.config.RedisPassword)
	assertSecretDescriptor(t, "sentinel_password", p.config.SentinelPassword)
	if p.redis != nil {
		t.Fatal("scoped materialization constructed Redis client before PostInit")
	}
	if p.redisPassword == nil || p.sentinelPassword == nil {
		t.Fatal("scoped materialization did not retain private credential values")
	}

	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	client := p.redis.(*redis.Client)
	if client.Options().Password != "redis-password" {
		t.Fatalf("Redis client did not receive admitted credentials")
	}
	var gotSentinelPassword string
	if err := p.sentinelPassword.Use(func(value string) error {
		gotSentinelPassword = value
		return nil
	}); err != nil || gotSentinelPassword != "sentinel-password" {
		t.Fatalf("sentinel private credential = %q, err = %v", gotSentinelPassword, err)
	}
	p.Stop()
	if p.redisPassword != nil || p.sentinelPassword != nil {
		t.Fatal("Stop() retained scoped credential values")
	}
	if err := client.Ping(t.Context()).Err(); !strings.Contains(err.Error(), redis.ErrClosed.Error()) {
		t.Fatalf("Ping() after Stop = %v, want closed client", err)
	}
}

func TestMaterializeScopedSecretsIsIdempotent(t *testing.T) {
	const (
		redisRaw    = "$ENV://AI_IDEMPOTENT_REDIS_PASSWORD"
		sentinelRaw = "$secret://vault/ai-idempotent-sentinel-password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, map[string]string{
		redisRaw:    "redis-password",
		sentinelRaw: "sentinel-password",
	})
	defer closeAttempt()

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "redis-sentinel",
		RedisMasterName:  "mymaster",
		RedisSentinels:   []RedisSentinel{{Host: "127.0.0.1", Port: 26379}},
		RedisPassword:    redisRaw,
		SentinelPassword: sentinelRaw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
			t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
		}
	}
	if len(broker.calls) != 2 {
		t.Fatalf("scoped calls = %d, want exactly 2 after repeated materialization", len(broker.calls))
	}
}

func TestMaterializeScopedSecretsConcurrentCallsMaterializeOnce(t *testing.T) {
	const (
		redisRaw    = "$ENV://AI_CONCURRENT_REDIS_PASSWORD"
		sentinelRaw = "$secret://vault/ai-concurrent-sentinel-password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, map[string]string{
		redisRaw:    "redis-password",
		sentinelRaw: "sentinel-password",
	})
	defer closeAttempt()

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "redis-sentinel",
		RedisMasterName:  "mymaster",
		RedisSentinels:   []RedisSentinel{{Host: "127.0.0.1", Port: 26379}},
		RedisPassword:    redisRaw,
		SentinelPassword: sentinelRaw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent MaterializeScopedPluginSecrets() error = %v", err)
		}
	}
	if len(broker.calls) != 2 {
		t.Fatalf("scoped calls = %d, want exactly 2 for concurrent materialization", len(broker.calls))
	}
}

func TestPostInitPassesScopedCredentialsToFailoverConstructor(t *testing.T) {
	const (
		redisRaw    = "$ENV://AI_CONSTRUCTOR_REDIS_PASSWORD"
		sentinelRaw = "$secret://vault/ai-constructor-sentinel-password"
	)
	secrets, scope, _, closeAttempt := newScopedSecretHarness(t, map[string]string{
		redisRaw:    "resolved-redis-password",
		sentinelRaw: "resolved-sentinel-password",
	})
	defer closeAttempt()
	fake := &countingRedis{}
	capture := captureRedisConstructors(t, fake)

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "redis-sentinel",
		RedisMasterName:  "mymaster",
		RedisSentinels:   []RedisSentinel{{Host: "127.0.0.1", Port: 26379}},
		RedisPassword:    redisRaw,
		SentinelPassword: sentinelRaw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if len(capture.failoverOptions) != 0 {
		t.Fatal("constructor ran during scoped materialization")
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if len(capture.failoverOptions) != 1 {
		t.Fatalf("failover constructor calls = %d, want 1", len(capture.failoverOptions))
	}
	options := capture.failoverOptions[0]
	if options.Password != "resolved-redis-password" || options.SentinelPassword != "resolved-sentinel-password" {
		t.Fatalf(
			"constructor credentials = (%q, %q), want resolved private values",
			options.Password, options.SentinelPassword,
		)
	}
	if options.Password == p.config.RedisPassword || options.SentinelPassword == p.config.SentinelPassword {
		t.Fatal("constructor received public descriptor instead of private credential")
	}
	p.Stop()
}

func TestPostInitPassesScopedPasswordToRedisConstructor(t *testing.T) {
	const raw = "$ENV://AI_REDIS_CONSTRUCTOR_PASSWORD"
	secrets, scope, _, closeAttempt := newScopedSecretHarness(t, map[string]string{
		raw: "resolved-redis-password",
	})
	defer closeAttempt()
	fake := &countingRedis{}
	capture := captureRedisConstructors(t, fake)

	p := &Plugin{config: Config{
		Limit:         1,
		TimeWindow:    60,
		Policy:        "redis",
		RedisHost:     "127.0.0.1",
		RedisPassword: raw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if len(capture.redisOptions) != 1 || len(capture.failoverOptions) != 0 {
		t.Fatalf(
			"constructor calls = (%d, %d), want one Redis constructor",
			len(capture.redisOptions), len(capture.failoverOptions),
		)
	}
	options := capture.redisOptions[0]
	if options.Password != "resolved-redis-password" || options.Password == p.config.RedisPassword {
		t.Fatalf("Redis constructor password = %q, public config = %q", options.Password, p.config.RedisPassword)
	}
	p.Stop()
}

func TestPostInitValidatesBeforeRedisConstructors(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
	}{
		{
			name:   "missing redis endpoint",
			config: Config{Limit: 1, TimeWindow: 60, Policy: "redis"},
		},
		{
			name: "invalid rule",
			config: Config{
				Limit: 1, TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1",
				Rules: []Rule{{Count: 0, TimeWindow: 60, Key: "${remote_addr}"}},
			},
		},
		{
			name:   "invalid quota",
			config: Config{Limit: 0, TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &countingRedis{}
			capture := captureRedisConstructors(t, fake)
			p := &Plugin{config: test.config}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := p.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want validation failure")
			}
			if len(capture.redisOptions) != 0 || len(capture.failoverOptions) != 0 {
				t.Fatalf(
					"constructor calls = (%d, %d), want zero",
					len(capture.redisOptions), len(capture.failoverOptions),
				)
			}
		})
	}
}

func TestMaterializeScopedSecretsSkipsEmptyLocalCredentials(t *testing.T) {
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, nil)
	defer closeAttempt()

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "local",
		RedisPassword:    "",
		SentinelPassword: "",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if len(broker.calls) != 0 {
		t.Fatalf("scoped calls = %d, want zero for local policy", len(broker.calls))
	}
	if p.redis != nil || p.redisPassword != nil || p.sentinelPassword != nil {
		t.Fatal("local policy constructed or retained Redis credentials")
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("local PostInit() error = %v", err)
	}
}

func TestMaterializeScopedSecretsSkipsNonEmptyLocalCredentialsWithoutParsing(t *testing.T) {
	const (
		redisRaw    = "$ENV://AI_LOCAL_REDIS_PASSWORD"
		sentinelRaw = "$secret://vault/ai-local-sentinel-password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, nil)
	defer closeAttempt()

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "local",
		RedisPassword:    redisRaw,
		SentinelPassword: sentinelRaw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if len(broker.calls) != 0 {
		t.Fatalf("scoped calls = %d, want zero for local policy", len(broker.calls))
	}
	if p.config.RedisPassword != "" || p.config.SentinelPassword != "" {
		t.Fatalf("local unused credentials retained in public config: %#v", p.config)
	}
}

func TestMaterializeScopedSecretsIsAtomicAndRedactsFailure(t *testing.T) {
	const (
		redisRaw    = "$ENV://AI_ATOMIC_REDIS_PASSWORD"
		sentinelRaw = "$secret://vault/ai-atomic-sentinel-password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, map[string]string{
		redisRaw: "redis-password",
	})
	defer closeAttempt()
	broker.fail[sentinelRaw] = errors.New("sentinel-plaintext-must-not-escape")

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "redis-sentinel",
		RedisMasterName:  "mymaster",
		RedisSentinels:   []RedisSentinel{{Host: "127.0.0.1", Port: 26379}},
		RedisPassword:    redisRaw,
		SentinelPassword: sentinelRaw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v, want redacted credential error", err)
	}
	if strings.Contains(err.Error(), sentinelRaw) || strings.Contains(err.Error(), "sentinel-plaintext") {
		t.Fatalf("MaterializeScopedPluginSecrets() leaked credential reference: %v", err)
	}
	if p.config.RedisPassword != redisRaw || p.config.SentinelPassword != sentinelRaw {
		t.Fatalf("failed materialization partially changed public config: %#v", p.config)
	}
	if p.redisPassword != nil || p.sentinelPassword != nil || p.secretsMaterialized {
		t.Fatal("failed materialization retained private state")
	}
}

func localCounterUsed(t *testing.T, p *Plugin, key string) int64 {
	t.Helper()
	state, ok := p.counters.Get(key)
	if !ok {
		return 0
	}
	return state.used
}

func TestHandlerChargesTotalTokensAndRejectsNextRequest(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		Limit:         10,
		TimeWindow:    60,
		RejectedCode:  http.StatusTooManyRequests,
		RejectedMsg:   "token quota exceeded",
		LimitStrategy: "total_tokens",
	}, func() time.Time { return now })

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":4,"completion_tokens":8,"total_tokens":12}}`))
	})

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-RateLimit-Limit-global"); got != "10" {
		t.Fatalf("limit header = %q, want 10", got)
	}
	if got := first.Header().Get("X-AI-RateLimit-Remaining-global"); got != "10" {
		t.Fatalf("remaining header = %q, want pre-charge quota 10", got)
	}
	if got := first.Header().Get("X-AI-RateLimit-Reset-global"); got != "60" {
		t.Fatalf("reset header = %q, want remaining window duration 60", got)
	}

	second := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want 429", second.Code)
	}
	if !strings.Contains(second.Body.String(), "token quota exceeded") {
		t.Fatalf("second response body = %q, want custom rejection message", second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want only first request to pass", calls)
	}
}

func TestHandlerIsolatesGlobalQuotaByAuthenticatedConsumer(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 10, TimeWindow: 60}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":10}}`))
	})
	request := func(consumer string) *http.Request {
		return apisixctx.WithApisixVars(
			httptest.NewRequest(http.MethodPost, "/", nil),
			map[string]string{"$consumer_name": consumer},
		)
	}

	p.Handler(upstream).ServeHTTP(httptest.NewRecorder(), request("jack1"))

	jack2 := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(jack2, request("jack2"))
	if jack2.Code != http.StatusOK {
		t.Fatalf("jack2 response code = %d, want isolated quota", jack2.Code)
	}

	jack1 := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(jack1, request("jack1"))
	if jack1.Code != http.StatusServiceUnavailable {
		t.Fatalf("jack1 response code = %d, want exhausted quota", jack1.Code)
	}
}

func TestPostInitRejectsConflictingQuotaModes(t *testing.T) {
	tests := []Config{
		{Limit: 1, Rules: []Rule{{Count: 1, TimeWindow: 60, Key: "${remote_addr}"}}},
		{
			Instances: []InstanceLimit{{Name: "one", Limit: 1, TimeWindow: 60}},
			Rules:     []Rule{{Count: 1, TimeWindow: 60, Key: "${remote_addr}"}},
		},
		{Limit: 1, Instances: []InstanceLimit{{Name: "one", Limit: 1, TimeWindow: 60}}},
	}
	for i, config := range tests {
		plugin := &Plugin{config: config}
		plugin.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
		if err := plugin.Init(); err != nil {
			t.Fatal(err)
		}
		if err := plugin.PostInit(); err == nil {
			t.Fatalf("config %d PostInit() unexpectedly succeeded", i)
		}
	}
}

func TestRedisCounterKeyIncludesResourceAndConfigurationIdentity(t *testing.T) {
	first := newTestPlugin(t, Config{Limit: 10, TimeWindow: 60}, time.Now)
	first.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{ID: "service-1"})
	second := newTestPlugin(t, Config{Limit: 10, TimeWindow: 60}, time.Now)
	second.SetResourceContext(resource.Route{ID: "route-2"}, resource.Service{ID: "service-1"})
	reloaded := newTestPlugin(t, Config{Limit: 20, TimeWindow: 60}, time.Now)
	reloaded.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{ID: "service-1"})

	q := quota{key: "global"}
	firstKey := first.redisKey(q)
	if firstKey == second.redisKey(q) {
		t.Fatalf("two routes share Redis key %q", firstKey)
	}
	if firstKey == reloaded.redisKey(q) {
		t.Fatalf("changed configuration retained Redis key %q", firstKey)
	}

	collisionA := newTestPlugin(t, Config{Limit: 10, TimeWindow: 60}, time.Now)
	collisionA.SetResourceContext(resource.Route{ID: "a:service:b"}, resource.Service{ID: "c"})
	collisionB := newTestPlugin(t, Config{Limit: 10, TimeWindow: 60}, time.Now)
	collisionB.SetResourceContext(resource.Route{ID: "a"}, resource.Service{ID: "b:service:c"})
	if collisionA.redisKey(q) == collisionB.redisKey(q) {
		t.Fatalf(
			"delimiter-containing resource IDs share Redis counter key %q",
			collisionA.redisKey(q),
		)
	}
	if !strings.Contains(firstKey, "resource:7:route-1:9:service-1") {
		t.Fatalf("Redis key = %q, want length-prefixed route and service identity", firstKey)
	}
}

func TestHandlerUsesInstancePromptTokenQuota(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		LimitStrategy: "prompt_tokens",
		Instances: []InstanceLimit{
			{Name: "deepseek-main", Limit: 5, TimeWindow: 30},
		},
	}, func() time.Time { return now })

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":20,"total_tokens":23}}`))
	})

	for i := range 2 {
		rr := httptest.NewRecorder()
		req := WithPickedAIInstanceName(
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			"deepseek-main",
		)
		p.Handler(upstream).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("response %d code = %d, want 200", i+1, rr.Code)
		}
	}

	rr := httptest.NewRecorder()
	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "deepseek-main")
	p.Handler(upstream).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("third response code = %d, want default 503", rr.Code)
	}
	if got := rr.Header().Get("X-AI-RateLimit-Limit-deepseek-main"); got != "5" {
		t.Fatalf("instance limit header = %q, want 5", got)
	}
}

func TestHandlerSkipsUnconfiguredInstance(t *testing.T) {
	p := newTestPlugin(t, Config{
		LimitStrategy: "total_tokens",
		Instances: []InstanceLimit{
			{Name: "limited", Limit: 1, TimeWindow: 60},
		},
	}, time.Now)

	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "other")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":100}}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want pass-through for unconfigured instance", rr.Code)
	}
}

func TestHandlerUsesGlobalQuotaForInstanceWithoutOverride(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:      10,
		TimeWindow: 60,
		Instances: []InstanceLimit{
			{Name: "overridden", Limit: 20, TimeWindow: 60},
		},
	}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":10}}`))
	})
	request := func() *http.Request {
		return WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/", nil), "global-model")
	}

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, request())
	if first.Code != http.StatusOK || first.Header().Get("X-AI-RateLimit-Limit-global-model") != "10" {
		t.Fatalf(
			"first response = (%d, %q), want selected instance global limit 10",
			first.Code,
			first.Header().Get("X-AI-RateLimit-Limit-global-model"),
		)
	}
	second := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(second, request())
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second response code = %d, want exhausted global quota", second.Code)
	}
}

func TestHandlerResetsQuotaAfterWindow(t *testing.T) {
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{
		Limit:      1,
		TimeWindow: 1,
	}, func() time.Time { return now })

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	p.Handler(upstream).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, httptest.NewRequest(http.MethodPost, "/", nil))
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}

	now = now.Add(2 * time.Second)
	allowed := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(allowed, httptest.NewRequest(http.MethodPost, "/", nil))
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed response code = %d, want 200 after reset", allowed.Code)
	}
}

func TestHandlerReportsAPISIX317PreChargeSnapshots(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 30, TimeWindow: 60}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":10}}`))
	})
	for i, want := range []string{"30", "20", "10", "0"} {
		response := httptest.NewRecorder()
		p.Handler(upstream).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", nil))
		if got := response.Header().Get("X-AI-RateLimit-Remaining-global"); got != want {
			t.Fatalf("response %d remaining = %q, want pre-charge snapshot %q", i+1, got, want)
		}
		wantStatus := http.StatusOK
		if i == 3 {
			wantStatus = http.StatusServiceUnavailable
		}
		if response.Code != wantStatus {
			t.Fatalf("response %d status = %d, want %d", i+1, response.Code, wantStatus)
		}
	}
}

func TestHandlerUsesAPISIX317ProviderTotalTokensWhenItDiffersFromComponentSum(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 30, TimeWindow: 60}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$llm_raw_usage", map[string]any{
			"prompt_tokens":     float64(8),
			"completion_tokens": float64(5),
			"total_tokens":      float64(10),
		})
		apisixctx.RegisterRequestVar(r, "$ai_token_usage", map[string]any{
			"prompt_tokens":     int64(8),
			"completion_tokens": int64(5),
			"total_tokens":      int64(13),
		})
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":10}}`))
	})

	for i, want := range []string{"30", "20", "10"} {
		response := httptest.NewRecorder()
		request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/ai", nil))
		p.Handler(upstream).ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("response %d status = %d, want 200", i+1, response.Code)
		}
		if got := response.Header().Get("X-AI-RateLimit-Remaining-global"); got != want {
			t.Fatalf("response %d remaining = %q, want provider-total snapshot %q", i+1, got, want)
		}
	}
}

func TestHandlerWritesAPISIX317CustomRejectionResponse(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:        1,
		TimeWindow:   60,
		RejectedCode: http.StatusForbidden,
		RejectedMsg:  "rate limit exceeded",
	}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	p.Handler(upstream).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/ai", nil))

	rejected := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rejected, httptest.NewRequest(http.MethodPost, "/ai", nil))

	if rejected.Code != http.StatusForbidden {
		t.Fatalf("rejected status = %d, want 403", rejected.Code)
	}
	if got := rejected.Header().Get("X-AI-RateLimit-Remaining-global"); got != "0" {
		t.Fatalf("rejected remaining = %q, want 0", got)
	}
	if got := rejected.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("rejected Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := rejected.Header().Get("X-Content-Type-Options"); got != "" {
		t.Fatalf("rejected X-Content-Type-Options = %q, want absent", got)
	}
	if got := rejected.Body.String(); got != "{\"error_msg\":\"rate limit exceeded\"}\n" {
		t.Fatalf("rejected body = %q, want APISIX error object", got)
	}
}

func TestAIProxyAndRateLimiterExecuteInAPISIXPhaseOrder(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy := &ai_proxy.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy.Config) = ai_proxy.Config{
		Provider: "openai-compatible",
		Auth:     ai_proxy.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
		Override: ai_proxy.Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Limit: 2, TimeWindow: 60}, time.Now)
	fallbackCalls := 0
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		fallbackCalls++
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}]
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-RateLimit-Remaining-ai-proxy-openai-compatible"); got != "2" {
		t.Fatalf("remaining header = %q, want pre-charge quota 2", got)
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if upstreamCalls.Load() != 1 || fallbackCalls != 0 {
		t.Fatalf("upstream calls = %d, fallback calls = %d, want 1 and 0", upstreamCalls.Load(), fallbackCalls)
	}
}

func TestAIProxyStreamingResponseIsFlushedAndCharged(t *testing.T) {
	var upstreamCalls atomic.Int64
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"model\":\"gpt-stream\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamBody))
	}))
	defer upstream.Close()

	proxy := &ai_proxy.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy.Config) = ai_proxy.Config{
		Provider: "openai-compatible",
		Override: ai_proxy.Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Limit: 2, TimeWindow: 60}, time.Now)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for streaming AI request")
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}],
		  "stream":true
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusOK || first.Body.String() != streamBody {
		t.Fatalf("first response = (%d, %q), want exact stream", first.Code, first.Body.String())
	}
	if !first.Flushed {
		t.Fatal("streaming response was buffered by rate limiter")
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

func TestAIProxyMultiPublishesInstanceBeforeRateLimitPreflight(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	proxy := &ai_proxy_multi.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy_multi.Config) = ai_proxy_multi.Config{Instances: []ai_proxy_multi.Instance{{
		Name:     "model-a",
		Provider: "openai-compatible",
		Weight:   1,
		Auth:     ai_proxy_multi.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
		Override: ai_proxy_multi.Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	}}}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Instances: []InstanceLimit{{Name: "model-a", Limit: 2, TimeWindow: 60}}}, time.Now)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for AI request")
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}]
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request())
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-RateLimit-Remaining-model-a"); got != "2" {
		t.Fatalf("remaining header = %q, want pre-charge quota 2", got)
	}

	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

func TestAIProxyMultiFallbackPublishesOnlyFinalInstanceHeaders(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failed.Close()
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"total_tokens":3}}`))
	}))
	defer success.Close()

	proxy := newFallbackMultiProxy(t, failed.URL, success.URL)
	rate := newTestPlugin(t, Config{Instances: []InstanceLimit{
		{Name: "model-a", Limit: 10, TimeWindow: 60},
		{Name: "model-b", Limit: 20, TimeWindow: 60},
	}}, time.Now)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for AI request")
	})))))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newMultiProxyRequest(false))
	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200", response.Code)
	}
	assertFinalInstanceHeaders(t, response.Header(), "model-b", "20")
	if got := localCounterUsed(t, rate, "instance:model-b"); got != 3 {
		t.Fatalf("model-b used tokens = %d, want 3", got)
	}
	if got := localCounterUsed(t, rate, "instance:model-a"); got != 0 {
		t.Fatalf("retryable model-a used tokens = %d, want 0", got)
	}
}

func TestAIProxyMultiStreamingFallbackPublishesOnlyFinalInstanceHeaders(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failed.Close()
	streamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamBody))
	}))
	defer success.Close()

	proxy := newFallbackMultiProxy(t, failed.URL, success.URL)
	rate := newTestPlugin(t, Config{Instances: []InstanceLimit{
		{Name: "model-a", Limit: 10, TimeWindow: 60},
		{Name: "model-b", Limit: 20, TimeWindow: 60},
	}}, time.Now)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for streaming AI request")
	})))))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newMultiProxyRequest(true))
	if response.Code != http.StatusOK || response.Body.String() != streamBody {
		t.Fatalf("response = (%d, %q), want exact successful stream", response.Code, response.Body.String())
	}
	if !response.Flushed {
		t.Fatal("streaming fallback response was not flushed")
	}
	assertFinalInstanceHeaders(t, response.Header(), "model-b", "20")
	if got := localCounterUsed(t, rate, "instance:model-b"); got != 6 {
		t.Fatalf("model-b used tokens = %d, want 6", got)
	}
	if got := localCounterUsed(t, rate, "instance:model-a"); got != 0 {
		t.Fatalf("retryable model-a stream used tokens = %d, want 0", got)
	}
}

func newFallbackMultiProxy(t *testing.T, failedURL, successURL string) *ai_proxy_multi.Plugin {
	t.Helper()
	proxy := &ai_proxy_multi.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy_multi.Config) = ai_proxy_multi.Config{
		FallbackStrategy: "http_5xx",
		MaxRetries:       new(1),
		Instances: []ai_proxy_multi.Instance{
			{
				Name: "model-a", Provider: "openai-compatible", Priority: 10, Weight: 1,
				Override: ai_proxy_multi.Override{Endpoint: failedURL + "/v1/chat/completions"},
			},
			{
				Name: "model-b", Provider: "openai-compatible", Priority: 0, Weight: 1,
				Override: ai_proxy_multi.Override{Endpoint: successURL + "/v1/chat/completions"},
			},
		},
	}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	return proxy
}

func newMultiProxyRequest(streaming bool) *http.Request {
	body := `{"messages":[{"role":"user","content":"ping"}]}`
	if streaming {
		body = `{"messages":[{"role":"user","content":"ping"}],"stream":true}`
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request = apisixctx.WithRequestVars(request)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func assertFinalInstanceHeaders(t *testing.T, header http.Header, instance, remaining string) {
	t.Helper()
	for _, suffix := range []string{"Limit", "Remaining", "Reset"} {
		if got := header.Get("X-AI-RateLimit-" + suffix + "-model-a"); got != "" {
			t.Fatalf("initial model-a %s header = %q, want absent", suffix, got)
		}
	}
	if got := header.Get("X-AI-RateLimit-Limit-" + instance); got != remaining {
		t.Fatalf("final instance limit header = %q, want %q", got, remaining)
	}
	if got := header.Get("X-AI-RateLimit-Remaining-" + instance); got != remaining {
		t.Fatalf("final instance remaining header = %q, want %q", got, remaining)
	}
	if got := header.Get("X-AI-RateLimit-Reset-" + instance); got != "60" {
		t.Fatalf("final instance reset header = %q, want 60", got)
	}
}

func TestGlobalQuotaUsesSelectedInstanceCounter(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 10, TimeWindow: 60}, time.Now)
	for _, instance := range []string{"model-a", "model-b"} {
		request := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/", nil), instance)
		q, ok, err := p.quotaForRequest(request)
		if err != nil || !ok {
			t.Fatalf("quotaForRequest(%q) = (%#v, %v, %v)", instance, q, ok, err)
		}
		if q.key != "instance:"+instance {
			t.Fatalf("quota key = %q, want instance:%s", q.key, instance)
		}
	}
}

func TestAIProxyMultiSkipsRateLimitedInstance(t *testing.T) {
	var firstCalls atomic.Int64
	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`))
	}))
	defer firstUpstream.Close()
	var secondCalls atomic.Int64
	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`))
	}))
	defer secondUpstream.Close()

	proxy := &ai_proxy_multi.Plugin{}
	if err := proxy.Init(); err != nil {
		t.Fatalf("proxy Init() error = %v", err)
	}
	*proxy.Config().(*ai_proxy_multi.Config) = ai_proxy_multi.Config{
		Instances: []ai_proxy_multi.Instance{
			{
				Name: "model-a", Provider: "openai-compatible", Weight: 1,
				Auth:     ai_proxy_multi.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
				Override: ai_proxy_multi.Override{Endpoint: firstUpstream.URL + "/v1/chat/completions"},
			},
			{
				Name: "model-b", Provider: "openai-compatible", Weight: 1,
				Auth:     ai_proxy_multi.Auth{Header: map[string]string{"Authorization": "Bearer test"}},
				Override: ai_proxy_multi.Override{Endpoint: secondUpstream.URL + "/v1/chat/completions"},
			},
		},
		FallbackStrategy: []string{"rate_limiting"},
	}
	if err := proxy.PostInit(); err != nil {
		t.Fatalf("proxy PostInit() error = %v", err)
	}
	rate := newTestPlugin(t, Config{Instances: []InstanceLimit{
		{Name: "model-a", Limit: 1, TimeWindow: 60},
		{Name: "model-b", Limit: 5, TimeWindow: 60},
	}}, time.Now)
	rate.reconcile(
		context.Background(),
		quota{key: "instance:model-a", headerName: "model-a", limit: 1, window: time.Minute},
		1,
		true,
	)
	handler := ai_runtime.EnableTerminal(proxy.Handler(rate.Handler(ai_runtime.TerminalHandler(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		t.Fatal("ordinary upstream called for AI request")
	})))))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		  "messages":[{"role":"user","content":"ping"}]
		}`))
		req = apisixctx.WithRequestVars(req)
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, request())
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed response code = %d, want 200", allowed.Code)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("provider calls = (%d, %d), want (0, 1)", firstCalls.Load(), secondCalls.Load())
	}
	if got := allowed.Header().Get("X-AI-RateLimit-Remaining-model-b"); got != "5" {
		t.Fatalf("model-b remaining header = %q, want pre-charge quota 5", got)
	}

	rate.reconcile(
		context.Background(),
		quota{key: "instance:model-b", headerName: "model-b", limit: 5, window: time.Minute},
		4,
		true,
	)
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("provider calls after rejection = (%d, %d), want (0, 1)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestPostInitAcceptsExpressionCostStrategy(t *testing.T) {
	p := &Plugin{config: Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "prompt_tokens + completion_tokens",
	}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
}

type configAdmissionStage string

const (
	configAdmissionAccepted configAdmissionStage = "accepted"
	configAdmissionSchema   configAdmissionStage = "schema"
	configAdmissionPostInit configAdmissionStage = "post_init"
)

func admitAPISIX317Config(t *testing.T, raw map[string]any) (configAdmissionStage, error) {
	t.Helper()

	p := &Plugin{}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(raw, p.GetSchema()); err != nil {
		return configAdmissionSchema, err
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := json.Unmarshal(payload, p.Config()); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	secrets, scope, closeAttempt := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	t.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		return configAdmissionPostInit, err
	}
	return configAdmissionAccepted, nil
}

func TestAPISIX317ExpressionConfigurationAdmissionMatrix(t *testing.T) {
	tests := []struct {
		name      string
		config    map[string]any
		wantStage configAdmissionStage
	}{
		{
			name: "missing cost expression",
			config: map[string]any{
				"limit": 100, "time_window": 60, "limit_strategy": "expression",
			},
			wantStage: configAdmissionPostInit,
		},
		{
			name: "empty cost expression",
			config: map[string]any{
				"limit": 100, "time_window": 60, "limit_strategy": "expression", "cost_expr": "",
			},
			wantStage: configAdmissionSchema,
		},
		{
			name: "invalid cost expression syntax",
			config: map[string]any{
				"limit": 100, "time_window": 60, "limit_strategy": "expression",
				"cost_expr": "invalid $$$ syntax %%%",
			},
			wantStage: configAdmissionPostInit,
		},
		{
			name: "simple cost expression",
			config: map[string]any{
				"limit": 100, "time_window": 60, "limit_strategy": "expression",
				"cost_expr": "input_tokens + output_tokens",
			},
			wantStage: configAdmissionAccepted,
		},
		{
			name: "complex cost expression",
			config: map[string]any{
				"limit":          100,
				"time_window":    60,
				"limit_strategy": "expression",
				"cost_expr":      "(input_tokens - cache_read_input_tokens) + cache_creation_input_tokens * 1.25 + output_tokens",
			},
			wantStage: configAdmissionAccepted,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stage, err := admitAPISIX317Config(t, test.config)
			if stage != test.wantStage {
				t.Fatalf("admission stage = %q, error = %v, want %q", stage, err, test.wantStage)
			}
			if test.wantStage == configAdmissionAccepted && err != nil {
				t.Fatalf("accepted config error = %v", err)
			}
			if test.wantStage != configAdmissionAccepted && err == nil {
				t.Fatalf("rejected config error = nil at %q", stage)
			}
		})
	}
}

func TestAPISIX317QuotaShapeConfigurationAdmissionMatrix(t *testing.T) {
	instance := func() map[string]any {
		return map[string]any{"name": "instance1", "limit": 30, "time_window": 60}
	}
	tests := []struct {
		name      string
		config    map[string]any
		wantStage configAdmissionStage
	}{
		{
			name:      "missing global limit",
			config:    map[string]any{"time_window": 60},
			wantStage: configAdmissionSchema,
		},
		{
			name:      "missing global time window",
			config:    map[string]any{"limit": 30},
			wantStage: configAdmissionSchema,
		},
		{
			name:      "rejected code below minimum",
			config:    map[string]any{"limit": 30, "time_window": 60, "rejected_code": 199},
			wantStage: configAdmissionSchema,
		},
		{
			name:      "unknown limit strategy",
			config:    map[string]any{"limit": 30, "time_window": 60, "limit_strategy": "invalid"},
			wantStage: configAdmissionSchema,
		},
		{
			name: "instance missing name",
			config: map[string]any{"instances": []any{
				instance(), map[string]any{"limit": 30, "time_window": 60},
			}},
			wantStage: configAdmissionSchema,
		},
		{
			name:      "global limit missing beside instances",
			config:    map[string]any{"time_window": 60, "instances": []any{instance()}},
			wantStage: configAdmissionSchema,
		},
		{
			name:      "global time window missing beside instances",
			config:    map[string]any{"limit": 30, "instances": []any{instance()}},
			wantStage: configAdmissionSchema,
		},
		{
			name: "instances and rules are mutually exclusive",
			config: map[string]any{
				"instances": []any{instance()},
				"rules": []any{map[string]any{
					"count": 1, "time_window": 10, "key": "${http_company}",
				}},
			},
			wantStage: configAdmissionSchema,
		},
		{
			name:      "instances only",
			config:    map[string]any{"instances": []any{instance()}},
			wantStage: configAdmissionAccepted,
		},
		{
			name: "global only",
			config: map[string]any{
				"limit": 30, "time_window": 60, "limit_strategy": "completion_tokens",
				"rejected_code": 403, "rejected_msg": "rate limit exceeded",
			},
			wantStage: configAdmissionAccepted,
		},
		{
			name: "global and instances",
			config: map[string]any{
				"limit": 30, "time_window": 60, "instances": []any{instance()},
			},
			wantStage: configAdmissionAccepted,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stage, err := admitAPISIX317Config(t, test.config)
			if stage != test.wantStage {
				t.Fatalf("admission stage = %q, error = %v, want %q", stage, err, test.wantStage)
			}
			if test.wantStage == configAdmissionAccepted && err != nil {
				t.Fatalf("accepted config error = %v", err)
			}
			if test.wantStage != configAdmissionAccepted && err == nil {
				t.Fatalf("rejected config error = nil at %q", stage)
			}
		})
	}
}

func TestHandlerResolvesGlobalQuotaFromRequestVariables(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"limit":"$http_x_limit","time_window":"${http_x_window}"}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	p := newTestPlugin(t, cfg, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Limit", "2")
	req.Header.Set("X-Window", "10")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("X-AI-RateLimit-Limit-global"); got != "2" {
		t.Fatalf("limit header = %q, want 2", got)
	}
}

func TestHandlerResolvesQuotaVariableDefaultsAndOverrides(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{
		"limit":"${http_openai_count ?? 20}",
		"time_window":"${http_time_window ?? 60}"
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	p := newTestPlugin(t, cfg, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})

	defaults := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(defaults, httptest.NewRequest(http.MethodPost, "/", nil))
	if defaults.Code != http.StatusOK || defaults.Header().Get("X-AI-RateLimit-Limit-global") != "20" {
		t.Fatalf(
			"default response = (%d, %q), want limit 20",
			defaults.Code,
			defaults.Header().Get("X-AI-RateLimit-Limit-global"),
		)
	}

	overrideRequest := httptest.NewRequest(http.MethodPost, "/", nil)
	overrideRequest.Header.Set("OpenAI-Count", "30")
	overrideRequest.Header.Set("Time-Window", "10")
	override := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(override, overrideRequest)
	if override.Code != http.StatusOK || override.Header().Get("X-AI-RateLimit-Limit-global") != "30" {
		t.Fatalf(
			"override response = (%d, %q), want limit 30",
			override.Code,
			override.Header().Get("X-AI-RateLimit-Limit-global"),
		)
	}
}

func TestHandlerResolvesInstanceQuotaFromRequestVariables(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{
		"instances":[{"name":"model-a","limit":"$http_x_limit","time_window":"$http_x_window"}]
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	p := newTestPlugin(t, cfg, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})

	req := WithPickedAIInstanceName(httptest.NewRequest(http.MethodPost, "/", nil), "model-a")
	req.Header.Set("X-Limit", "3")
	req.Header.Set("X-Window", "10")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("X-AI-RateLimit-Limit-model-a"); got != "3" {
		t.Fatalf("limit header = %q, want 3", got)
	}
}

func TestHandlerRejectsInvalidResolvedQuotaValues(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"limit":"$http_x_limit","time_window":"$http_x_window"}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	p := newTestPlugin(t, cfg, time.Now)

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Limit", "0")
	req.Header.Set("X-Window", "not-a-number")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHandlerRejectsMalformedResolvedWindow(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:      "$http_x_limit",
		TimeWindow: "$http_x_window",
	}, time.Now)

	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Limit", "1")
	req.Header.Set("X-Window", "not-a-number")
	rr := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestHandlerAppliesSingleRule(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{
		{Count: 1, TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Tenant"},
	}}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Tenant", "team-a")
		return req
	}

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, request())
	if got := first.Header().Get("X-AI-Tenant-RateLimit-Limit"); got != "1" {
		t.Fatalf("tenant limit header = %q, want 1", got)
	}

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
}

func TestHandlerAppliesIndependentRulesWithRuleHeaders(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Count: 2, TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Tenant"},
			{Count: 5, TimeWindow: 60, Key: "$http_x_model"},
		},
	}, now: time.Now}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Tenant", "team-a")
		req.Header.Set("X-Model", "model-a")
		return req
	}

	for i := range 2 {
		rr := httptest.NewRecorder()
		p.Handler(upstream).ServeHTTP(rr, request())
		if rr.Code != http.StatusOK {
			t.Fatalf("response %d code = %d, want 200", i+1, rr.Code)
		}
		if got := rr.Header().Get("X-AI-Tenant-RateLimit-Limit"); got != "2" {
			t.Fatalf("tenant limit header = %q, want 2", got)
		}
		if got := rr.Header().Get("X-AI-2-RateLimit-Limit"); got != "5" {
			t.Fatalf("default rule limit header = %q, want 5", got)
		}
	}

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
	if got := blocked.Header().Get("X-AI-2-RateLimit-Limit"); got != "" {
		t.Fatalf("later rule limit header = %q, want omitted after earlier rejection", got)
	}
}

func TestHandlerRuleUsesFixedWindowAndDefaultIndexHeaders(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	p := newTestPlugin(t, Config{Rules: []Rule{{
		Count: 1, TimeWindow: 2, Key: "$http_x_tenant",
	}}}, func() time.Time { return now })
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Tenant", "team-a")
		return req
	}

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, request())
	for header, want := range map[string]string{
		"X-AI-1-RateLimit-Limit":     "1",
		"X-AI-1-RateLimit-Remaining": "1",
		"X-AI-1-RateLimit-Reset":     "2",
	} {
		if got := first.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response status = %d, want 503", blocked.Code)
	}

	now = now.Add(2100 * time.Millisecond)
	replayed := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(replayed, request())
	if replayed.Code != http.StatusOK {
		t.Fatalf("replayed response status = %d, want 200 after fixed-window reset", replayed.Code)
	}
	if got := replayed.Header().Get("X-AI-1-RateLimit-Remaining"); got != "1" {
		t.Fatalf("replayed remaining = %q, want pre-charge quota 1 after reset", got)
	}
}

func TestHandlerSkipsInvalidDynamicRuleAndAppliesValidRule(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{
		{Count: "$http_x_bad_count", TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Bad"},
		{Count: 1, TimeWindow: 60, Key: "$http_x_tenant", HeaderPrefix: "Tenant"},
	}}, time.Now)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
	})
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Bad-Count", "not-a-number")
		req.Header.Set("X-Tenant", "team-a")
		return req
	}

	first := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(first, request())
	if first.Code != http.StatusOK {
		t.Fatalf("first response code = %d, want 200", first.Code)
	}
	if got := first.Header().Get("X-AI-Tenant-RateLimit-Limit"); got != "1" {
		t.Fatalf("valid rule limit header = %q, want 1", got)
	}
	if got := first.Header().Get("X-AI-Bad-RateLimit-Limit"); got != "" {
		t.Fatalf("invalid rule limit header = %q, want omitted", got)
	}

	blocked := httptest.NewRecorder()
	p.Handler(upstream).ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked response code = %d, want 503", blocked.Code)
	}
}

func TestHandlerReturnsInternalServerErrorWhenNoRuleResolves(t *testing.T) {
	p := newTestPlugin(t, Config{Rules: []Rule{
		{Count: "$http_x_bad_count", TimeWindow: 60, Key: "$http_x_tenant"},
	}}, time.Now)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Bad-Count", "not-a-number")
	req.Header.Set("X-Tenant", "team-a")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream called when no rule resolved")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
}

func TestResponseTokenCostEvaluatesExpressionAgainstRawUsage(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "math.max(prompt_tokens, math.abs(completion_tokens)) + missing_tokens + 0.6",
	}, time.Now)

	got := p.responseTokenCost([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":-4}}`))
	if got != 5 {
		t.Fatalf("expression cost = %d, want 5", got)
	}
}

func TestHandlerExpressionUsesRawUsageFromRequestContext(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:         20,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "input_tokens + output_tokens",
	}, time.Now)
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	apisixctx.RegisterRequestVar(req, "$llm_raw_usage", map[string]any{
		"input_tokens":  float64(6),
		"output_tokens": float64(4),
	})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("X-AI-RateLimit-Remaining-global"); got != "20" {
		t.Fatalf("remaining header = %q, want pre-charge quota 20", got)
	}
}

func TestPostInitAcceptsAdditionalSafeMathFunctions(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:         20,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "math.sqrt(prompt_tokens) + math.pow(completion_tokens, 2)",
	}, time.Now)

	if got := p.responseTokenCost([]byte(`{"usage":{"prompt_tokens":9,"completion_tokens":2}}`)); got != 7 {
		t.Fatalf("expression cost = %d, want 7", got)
	}
}

func TestHandlerRejectsOverflowingDynamicWindow(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 1, TimeWindow: "$http_x_window"}, time.Now)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Window", "9223372036854775807")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("upstream called for overflowing window")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
}

func TestResponseTokenCostClampsInvalidExpressionResults(t *testing.T) {
	for _, test := range []struct {
		name string
		expr string
		body string
	}{
		{name: "negative", expr: "-prompt_tokens", body: `{"usage":{"prompt_tokens":2}}`},
		{name: "non finite", expr: "1 / 0", body: `{"usage":{}}`},
		{name: "non numeric", expr: "prompt_tokens > 1", body: `{"usage":{"prompt_tokens":2}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Limit:         10,
				TimeWindow:    60,
				LimitStrategy: "expression",
				CostExpr:      test.expr,
			}, time.Now)

			if got := p.responseTokenCost([]byte(test.body)); got != 0 {
				t.Fatalf("expression cost = %d, want 0", got)
			}
		})
	}
}

func TestPostInitRejectsInvalidCostExpression(t *testing.T) {
	p := &Plugin{config: Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
		CostExpr:      "prompt_tokens +",
	}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid cost expression error")
	}
}

func TestPostInitRequiresCostExpression(t *testing.T) {
	p := &Plugin{config: Config{
		Limit:         10,
		TimeWindow:    60,
		LimitStrategy: "expression",
	}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want missing cost_expr error")
	}
}

func TestResponseTokenCostPreservesFixedStrategies(t *testing.T) {
	for _, test := range []struct {
		strategy string
		want     int64
	}{
		{strategy: "total_tokens", want: 9},
		{strategy: "prompt_tokens", want: 4},
		{strategy: "completion_tokens", want: 5},
	} {
		t.Run(test.strategy, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Limit:         10,
				TimeWindow:    60,
				LimitStrategy: test.strategy,
			}, time.Now)
			if got := p.responseTokenCost(
				[]byte(`{"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}`),
			); got != test.want {
				t.Fatalf("responseTokenCost() = %d, want %d", got, test.want)
			}
		})
	}
}

// rateLimitTestKey is the typed context key used by the Redis decision tests.
type rateLimitTestKey struct{}

// countingRedis records every command with its context and returns scripted
// replies, so tests can assert context propagation and round trips.
type countingRedis struct {
	mu        sync.Mutex
	commands  []string
	contexts  []context.Context
	getResult int64
	getError  error
	ttlResult int64
	evalErr   error
}

func (c *countingRedis) record(command string, ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = append(c.commands, command)
	c.contexts = append(c.contexts, ctx)
}

func (c *countingRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	c.record("GET "+key, ctx)
	return redis.NewStringResult(strconv.FormatInt(c.getResult, 10), c.getError)
}

func (c *countingRedis) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	c.record("EVAL "+keys[0], ctx)
	if c.evalErr != nil {
		return redis.NewCmdResult(nil, c.evalErr)
	}
	if strings.Contains(script, "AI quota reservation") {
		if c.getError != nil && !errors.Is(c.getError, redis.Nil) {
			return redis.NewCmdResult(nil, c.getError)
		}
		current := c.getResult
		if errors.Is(c.getError, redis.Nil) {
			current = 0
		}
		cost := snapshotInteger(args[0])
		limit := snapshotInteger(args[1])
		if current > limit-cost {
			return redis.NewCmdResult(int64(0), nil)
		}
		c.getResult = current + cost
		c.getError = nil
		return redis.NewCmdResult(int64(1), nil)
	}
	if strings.Contains(script, "AI quota response reconciliation") {
		delta := snapshotInteger(args[0])
		limit := snapshotInteger(args[1])
		c.getResult = max(min(c.getResult+delta, limit), 0)
		return redis.NewCmdResult(c.getResult, nil)
	}
	return redis.NewCmdResult([]any{int64(c.getResult), int64(c.ttlResult)}, nil)
}

func (c *countingRedis) Close() error { return nil }

type blockingCloseRedis struct {
	started    chan struct{}
	release    chan struct{}
	closeCalls atomic.Int32
}

func (c *blockingCloseRedis) Eval(context.Context, string, []string, ...any) *redis.Cmd {
	return redis.NewCmdResult(nil, nil)
}

func (c *blockingCloseRedis) Close() error {
	if c.closeCalls.Add(1) == 1 {
		close(c.started)
	}
	<-c.release
	return nil
}

func TestStopClosesClientOnceBeforeDroppingPrivateValues(t *testing.T) {
	const (
		redisRaw    = "$ENV://AI_STOP_REDIS_PASSWORD"
		sentinelRaw = "$secret://vault/ai-stop-sentinel-password"
	)
	secrets, scope, _, closeAttempt := newScopedSecretHarness(t, map[string]string{
		redisRaw:    "resolved-redis-password",
		sentinelRaw: "resolved-sentinel-password",
	})
	defer closeAttempt()

	p := &Plugin{config: Config{
		Limit:            1,
		TimeWindow:       60,
		Policy:           "redis-sentinel",
		RedisMasterName:  "mymaster",
		RedisSentinels:   []RedisSentinel{{Host: "127.0.0.1", Port: 26379}},
		RedisPassword:    redisRaw,
		SentinelPassword: sentinelRaw,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	legacyRedis := "legacy-redis-password"
	legacySentinel := "legacy-sentinel-password"
	p.redisPasswordLegacy = &legacyRedis
	p.sentinelPasswordLegacy = &legacySentinel
	fake := &blockingCloseRedis{started: make(chan struct{}), release: make(chan struct{})}
	p.redis = fake

	done := make(chan struct{}, 2)
	go func() {
		p.Stop()
		done <- struct{}{}
	}()
	<-fake.started
	if p.redisPassword == nil || p.sentinelPassword == nil ||
		p.redisPasswordLegacy == nil || p.sentinelPasswordLegacy == nil {
		t.Fatal("Stop() dropped private values before client close completed")
	}
	go func() {
		p.Stop()
		done <- struct{}{}
	}()
	close(fake.release)
	<-done
	<-done
	if got := fake.closeCalls.Load(); got != 1 {
		t.Fatalf("client Close() calls = %d, want exactly once", got)
	}
	if p.redisPassword != nil || p.sentinelPassword != nil ||
		p.redisPasswordLegacy != nil || p.sentinelPasswordLegacy != nil {
		t.Fatal("Stop() retained private credential values after client close")
	}
}

func (c *countingRedis) commandsOf(command string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, got := range c.commands {
		if got == command {
			count++
		}
	}
	return count
}

func (c *countingRedis) contextsAreRequestContexts(want context.Context) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	problems := make([]string, 0)
	for i, got := range c.contexts {
		if got == context.Background() {
			problems = append(problems, fmt.Sprintf("command %d used context.Background()", i+1))
		}
		if got != want {
			problems = append(problems, fmt.Sprintf("command %d context = %v, want request context %v", i+1, got, want))
		}
	}
	return problems
}

func TestRedisDecisionsUseRequestContextAndSingleRoundTrip(t *testing.T) {
	redisFake := &countingRedis{getResult: 0, ttlResult: 60000}
	p := newTestPlugin(
		t,
		Config{Limit: 10, TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1", RedisPort: 6379},
		time.Now,
	)
	_ = p.redis.Close()
	p.redis = redisFake

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(context.WithValue(req.Context(), rateLimitTestKey{}, "value"))
	ctx := req.Context()

	q := quota{key: "global", limit: 10, window: 60 * time.Second}

	if !p.reserve(ctx, q, 1) {
		t.Fatalf("reserve() = false with used 0, want true")
	}
	p.reconcile(ctx, q, 4, false)
	if _, reset := p.snapshot(ctx, q); reset != 60 {
		t.Fatalf("snapshot() reset = %d, want 60", reset)
	}

	if got := redisFake.commandsOf("GET " + p.redisKey(q)); got != 0 {
		t.Fatalf("reservation issued %d GET commands, want atomic Lua only", got)
	}
	if got := redisFake.commandsOf("EVAL " + p.redisKey(q)); got != 3 {
		t.Fatalf("reserve+reconcile+snapshot issued %d EVAL commands, want exactly 3", got)
	}
	if problems := redisFake.contextsAreRequestContexts(ctx); len(problems) > 0 {
		t.Fatalf("redis commands did not use the request context: %v", problems)
	}
}

func TestRedisRejectsAtLimitAndFailsClosedOnBackendError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx := req.Context()

	atLimit := &countingRedis{getResult: 10, ttlResult: 60000}
	atLimitPlugin := newTestPlugin(
		t,
		Config{Limit: 10, TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1", RedisPort: 6379},
		time.Now,
	)
	_ = atLimitPlugin.redis.Close()
	atLimitPlugin.redis = atLimit
	if atLimitPlugin.reserve(ctx, quota{key: "global", limit: 10, window: 60 * time.Second}, 1) {
		t.Fatalf("reserve() = true at used == limit, want false")
	}

	expired := &countingRedis{getError: redis.Nil, ttlResult: 60000}
	expiredPlugin := newTestPlugin(
		t,
		Config{Limit: 10, TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1", RedisPort: 6379},
		time.Now,
	)
	_ = expiredPlugin.redis.Close()
	expiredPlugin.redis = expired
	if !expiredPlugin.reserve(ctx, quota{key: "global", limit: 10, window: 60 * time.Second}, 1) {
		t.Fatalf("reserve() = false on expired key, want true")
	}

	backendError := &countingRedis{getError: fmt.Errorf("backend down"), ttlResult: 60000}
	backendPlugin := newTestPlugin(
		t,
		Config{Limit: 10, TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1", RedisPort: 6379},
		time.Now,
	)
	_ = backendPlugin.redis.Close()
	backendPlugin.redis = backendError
	if backendPlugin.reserve(ctx, quota{key: "global", limit: 10, window: 60 * time.Second}, 1) {
		t.Fatalf("reserve() = true on backend error, want false (fail-closed decision)")
	}
}

func TestRedisSnapshotFailsOpenOnBackendError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx := req.Context()

	backendError := &countingRedis{getResult: 8, evalErr: fmt.Errorf("backend down"), ttlResult: 60000}
	p := newTestPlugin(
		t,
		Config{Limit: 10, TimeWindow: 60, Policy: "redis", RedisHost: "127.0.0.1", RedisPort: 6379},
		time.Now,
	)
	_ = p.redis.Close()
	p.redis = backendError

	used, reset := p.snapshot(ctx, quota{key: "global", limit: 10, window: 60 * time.Second})
	if used != 0 || reset != 60 {
		t.Fatalf("snapshot() = (%d, %d) on backend error, want (0, 60) fail-open", used, reset)
	}
}

func TestConcurrentRequestPhaseReservesQuotaAtomically(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 1, TimeWindow: 60}, time.Now)
	const requests = 100
	start := make(chan struct{})
	var allowed atomic.Int64
	var wait sync.WaitGroup
	wait.Add(requests)
	for range requests {
		go func() {
			defer wait.Done()
			<-start
			result := p.RunRequestPhase(
				httptest.NewRecorder(),
				httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
			)
			if result.Decision == base.RequestContinue {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := allowed.Load(); got != 1 {
		t.Fatalf("concurrent admissions = %d, want exactly 1", got)
	}
}

func TestLocalQuotaStateIsBoundedAndResponseDeltaIsCapped(t *testing.T) {
	p := newTestPlugin(t, Config{Limit: 10, TimeWindow: 60}, time.Now)
	q := quota{key: "bounded-delta", limit: 10, window: time.Minute}
	p.reconcile(context.Background(), q, 1000, true)
	if got := localCounterUsed(t, p, q.key); got != q.limit {
		t.Fatalf("charged counter = %d, want capped limit %d", got, q.limit)
	}

	const capacity = 100000
	for i := 0; i <= capacity; i++ {
		p.reserve(context.Background(), quota{
			key:    "high-cardinality-" + strconv.Itoa(i),
			limit:  10,
			window: time.Minute,
		}, 1)
	}
	if got := p.counters.Len(); got > capacity {
		t.Fatalf("live AI quota counters = %d, want at most %d", got, capacity)
	}
}

func TestBufferedResponseFallbackStillReservesQuota(t *testing.T) {
	p := newTestPlugin(t, Config{
		Limit:         1,
		TimeWindow:    60,
		LimitStrategy: "total_tokens",
	}, time.Now)
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	response := &base.ResponseState{Body: []byte(`{"usage":{"total_tokens":1}}`)}

	if err := p.RunBufferedBodyFilter(request, response); err != nil {
		t.Fatalf("first RunBufferedBodyFilter() error = %v", err)
	}
	if err := p.RunBufferedBodyFilter(request, response); err == nil {
		t.Fatal("second RunBufferedBodyFilter() error = nil, want exhausted quota")
	}
}
