package limit_count

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type scopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type scopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []scopedSecretCall
	hook   func(scopedSecretCall)
}

func (broker *scopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	call := scopedSecretCall{Scope: scope, Raw: raw}
	broker.mu.Lock()
	broker.calls = append(broker.calls, call)
	failure := broker.fail[raw]
	value, found := broker.values[raw]
	hook := broker.hook
	broker.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if failure != nil {
		return "", failure
	}
	if found {
		return value, nil
	}
	return raw, nil
}

func (broker *scopedSecretBroker) callsSnapshot() []scopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]scopedSecretCall(nil), broker.calls...)
}

func (broker *scopedSecretBroker) setFailure(raw string, err error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if err == nil {
		delete(broker.fail, raw)
	} else {
		broker.fail[raw] = err
	}
}

func (broker *scopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func (broker *scopedSecretBroker) resetCalls() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = nil
}

func (broker *scopedSecretBroker) setHook(hook func(scopedSecretCall)) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.hook = hook
}

func newScopedSecretHarness(
	t *testing.T, revision uint64, resourceID string, values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{}}`),
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
			Key: key, Disposition: generation.DispositionPublished, Code: "leaf-test",
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
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return secrets, scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func limitCountDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func materializeScopedLimitCount(
	t *testing.T, p *Plugin, secrets secret.GenerationSecrets, scope secret.Scope,
) {
	t.Helper()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
}

func assertLimitCountCalls(
	t *testing.T, scope secret.Scope, calls []scopedSecretCall, fields, raws []string,
) {
	t.Helper()
	if len(calls) != len(fields) || len(fields) != len(raws) {
		t.Fatalf("broker calls = %#v, want fields=%#v raws=%#v", calls, fields, raws)
	}
	for index := range fields {
		wantScope := scope
		wantScope.Field = fields[index]
		if calls[index].Scope != wantScope || calls[index].Raw != raws[index] {
			t.Fatalf("call[%d] = %#v, want scope %#v raw %q", index, calls[index], wantScope, raws[index])
		}
	}
}

func TestScopedSecretsPreserveRootRedisHostDeclaration(t *testing.T) {
	const raw = "$ENV://LIMIT_COUNT_ROOT_REDIS_HOST"
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 7, "root-host", map[string]string{raw: "redis-root.test"},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{Count: 1, TimeWindow: 60, Policy: "redis", RedisHost: raw}}
	materializeScopedLimitCount(t, p, secrets, scope)
	assertLimitCountCalls(t, scope, broker.callsSnapshot(), []string{"redis_host"}, []string{raw})
	want := limitCountDescriptor("redis-root.test")
	if p.config.RedisHost != want || p.config.Redis.RedisHost != want {
		t.Fatalf("root/mirror host = %q/%q, want %q", p.config.RedisHost, p.config.Redis.RedisHost, want)
	}
}

func TestScopedSecretsPreserveNestedRedisHostDeclaration(t *testing.T) {
	const raw = "$secret://vault/limit-count/nested-host"
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 8, "nested-host", map[string]string{raw: "redis-nested.test"},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Policy: "redis", Redis: RedisConfig{RedisHost: raw},
	}}
	materializeScopedLimitCount(t, p, secrets, scope)
	assertLimitCountCalls(
		t, scope, broker.callsSnapshot(), []string{"redis_config.redis_host"}, []string{raw},
	)
	want := limitCountDescriptor("redis-nested.test")
	if p.config.Redis.RedisHost != want || p.config.RedisHost != "" {
		t.Fatalf("nested/root host = %q/%q, want %q/empty", p.config.Redis.RedisHost, p.config.RedisHost, want)
	}
}

func TestScopedSecretsPreserveRootClusterContainerDeclaration(t *testing.T) {
	raws := []string{"$ENV://LIMIT_COUNT_ROOT_NODE_0", "$ENV://LIMIT_COUNT_ROOT_NODE_1"}
	values := map[string]string{raws[0]: "redis-0.test:6379", raws[1]: "redis-1.test:6379"}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, 9, "root-nodes", values)
	defer closeAttempt()
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Policy: "redis-cluster", RedisClusterNodes: slices.Clone(raws),
		RedisClusterName: "cluster",
	}}
	materializeScopedLimitCount(t, p, secrets, scope)
	fields := []string{"redis_cluster_nodes", "redis_cluster_nodes"}
	assertLimitCountCalls(t, scope, broker.callsSnapshot(), fields, raws)
	want := []string{limitCountDescriptor(values[raws[0]]), limitCountDescriptor(values[raws[1]])}
	if !slices.Equal(p.config.RedisClusterNodes, want) ||
		!slices.Equal(p.config.RedisCluster.RedisClusterNodes, want) {
		t.Fatalf(
			"root/mirror nodes = %#v/%#v, want %#v",
			p.config.RedisClusterNodes,
			p.config.RedisCluster.RedisClusterNodes,
			want,
		)
	}
}

func TestScopedSecretsPreserveNestedClusterContainerDeclaration(t *testing.T) {
	raws := []string{"$secret://vault/limit-count/node-0", "$secret://vault/limit-count/node-1"}
	values := map[string]string{raws[0]: "nested-0.test:6379", raws[1]: "nested-1.test:6379"}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, 10, "nested-nodes", values)
	defer closeAttempt()
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Policy: "redis-cluster", RedisCluster: RedisClusterConfig{
			RedisClusterNodes: slices.Clone(raws), RedisClusterName: "cluster",
		},
	}}
	materializeScopedLimitCount(t, p, secrets, scope)
	fields := []string{"redis_cluster_config.redis_cluster_nodes", "redis_cluster_config.redis_cluster_nodes"}
	assertLimitCountCalls(t, scope, broker.callsSnapshot(), fields, raws)
	want := []string{limitCountDescriptor(values[raws[0]]), limitCountDescriptor(values[raws[1]])}
	if !slices.Equal(p.config.RedisCluster.RedisClusterNodes, want) || len(p.config.RedisClusterNodes) != 0 {
		t.Fatalf("nested/root nodes = %#v/%#v", p.config.RedisCluster.RedisClusterNodes, p.config.RedisClusterNodes)
	}
}

func TestScopedSecretsResolveManagedLimitCountKey(t *testing.T) {
	const raw = "$secret://vault/limit-count/key"
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 11, "managed-key", map[string]string{raw: "remote_addr"},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{Count: 1, TimeWindow: 60, Key: raw}}
	materializeScopedLimitCount(t, p, secrets, scope)
	assertLimitCountCalls(t, scope, broker.callsSnapshot(), []string{"key"}, []string{raw})
	if p.config.Key != limitCountDescriptor("remote_addr") {
		t.Fatalf("key descriptor = %q", p.config.Key)
	}
}

func TestScopedSecretsSkipEmptyLimitCountOptionalFields(t *testing.T) {
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, 12, "empty", nil)
	defer closeAttempt()
	p := &Plugin{config: Config{Count: 1, TimeWindow: 60}}
	materializeScopedLimitCount(t, p, secrets, scope)
	if calls := broker.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("empty optional broker calls = %#v", calls)
	}
}

func TestPrepareConsumerConfigAdmitsLiteralValues(t *testing.T) {
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Key: "remote_addr", KeyType: "var",
		Policy: "redis", RedisHost: "redis.test",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PrepareConsumerConfig(); err != nil {
		t.Fatalf("PrepareConsumerConfig() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	defer p.Stop()
	if !p.scopedSet {
		t.Fatal("literal consumer config did not enter the scoped credential lifecycle")
	}
	if err := p.withLimitCountKey(func(value string) error {
		if value != "remote_addr" {
			t.Fatalf("consumer key = %q, want remote_addr", value)
		}
		return nil
	}); err != nil {
		t.Fatalf("withLimitCountKey() error = %v", err)
	}
	if err := p.withLimitCountRedisHost(func(value string) error {
		if value != "redis.test" {
			t.Fatalf("consumer Redis host = %q, want redis.test", value)
		}
		return nil
	}); err != nil {
		t.Fatalf("withLimitCountRedisHost() error = %v", err)
	}
}

func TestPrepareConsumerConfigRejectsUnmaterializedSecret(t *testing.T) {
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Key: "$ENV://LIMIT_COUNT_CONSUMER_KEY",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PrepareConsumerConfig(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PrepareConsumerConfig() error = %v, want credential unavailable", err)
	}
	if p.scopedSet {
		t.Fatal("rejected consumer config installed scoped credentials")
	}
}

func TestScopedSecretsLimitCountNodeFailureIsAtomic(t *testing.T) {
	const rawKey = "$ENV://LIMIT_COUNT_RETRY_KEY"
	raws := []string{"$ENV://LIMIT_COUNT_RETRY_NODE_0", "$ENV://LIMIT_COUNT_RETRY_NODE_1"}
	values := map[string]string{rawKey: "remote_addr", raws[0]: "node-0.test:6379", raws[1]: "node-1.test:6379"}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, 13, "node-failure", values)
	defer closeAttempt()
	broker.setFailure(raws[1], errors.New("broker leaked "+raws[1]))
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Key: rawKey, Policy: "redis-cluster",
		RedisClusterNodes: slices.Clone(raws), RedisClusterName: "cluster",
	}}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("node failure = %v, want redacted credential unavailable", err)
	}
	for _, sensitive := range []string{rawKey, raws[0], raws[1], "broker leaked"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error %q contains %q", err, sensitive)
		}
	}
	if p.config.Key != rawKey || !slices.Equal(p.config.RedisClusterNodes, raws) ||
		len(p.config.RedisCluster.RedisClusterNodes) != 0 {
		t.Fatalf("failed materialization changed config: %#v", p.config)
	}
	assertLimitCountCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"key", "redis_cluster_nodes", "redis_cluster_nodes"},
		[]string{rawKey, raws[0], raws[1]},
	)
	broker.setFailure(raws[1], nil)
	broker.resetCalls()
	materializeScopedLimitCount(t, p, secrets, scope)
	assertLimitCountCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"key", "redis_cluster_nodes", "redis_cluster_nodes"},
		[]string{rawKey, raws[0], raws[1]},
	)
}

func TestScopedSecretsLimitCountResolvedBlanksAreAtomicAndRetryable(t *testing.T) {
	const raw = "$ENV://LIMIT_COUNT_BLANK_KEY"
	for _, blank := range []string{"", " \t\n"} {
		t.Run(fmt.Sprintf("blank-%q", blank), func(t *testing.T) {
			secrets, scope, broker, closeAttempt := newScopedSecretHarness(
				t, 20, "blank-"+fmt.Sprint(len(blank)), map[string]string{raw: blank},
			)
			defer closeAttempt()
			p := &Plugin{config: Config{Count: 1, TimeWindow: 60, Key: raw}}
			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
			if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
				t.Fatalf("blank materialization error = %v", err)
			}
			if p.config.Key != raw || p.scopedSet {
				t.Fatalf("blank materialization installed state: %#v", p.config)
			}
			broker.setValue(raw, "remote_addr")
			broker.resetCalls()
			materializeScopedLimitCount(t, p, secrets, scope)
			assertLimitCountCalls(t, scope, broker.callsSnapshot(), []string{"key"}, []string{raw})
		})
	}
}

func TestScopedSecretsLimitCountLiteralsAreDescriptorOnly(t *testing.T) {
	nodes := []string{"node-0.test:6379", "node-1.test:6379"}
	secrets, scope, _, closeAttempt := newScopedSecretHarness(t, 26, "literal-descriptors", nil)
	defer closeAttempt()
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Key: "remote_addr", Policy: "redis-cluster",
		RedisHost: "redis.test:6379", RedisClusterNodes: slices.Clone(nodes),
	}}
	materializeScopedLimitCount(t, p, secrets, scope)
	defer p.Stop()
	if p.config.Key != limitCountDescriptor("remote_addr") ||
		p.config.RedisHost != limitCountDescriptor("redis.test:6379") ||
		p.config.Redis.RedisHost != limitCountDescriptor("redis.test:6379") {
		t.Fatalf("literal key/host remained public: %#v", p.config)
	}
	wantNodes := []string{limitCountDescriptor(nodes[0]), limitCountDescriptor(nodes[1])}
	if !slices.Equal(p.config.RedisClusterNodes, wantNodes) ||
		!slices.Equal(p.config.RedisCluster.RedisClusterNodes, wantNodes) {
		t.Fatalf("literal cluster nodes remained public: %#v", p.config)
	}
	if err := p.withLimitCountKey(func(value string) error {
		if value != "remote_addr" {
			t.Fatalf("private key = %q", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.withLimitCountRedisHost(func(value string) error {
		if value != "redis.test:6379" {
			t.Fatalf("private host = %q", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.withLimitCountRedisNodes(func(values []string) error {
		if !slices.Equal(values, nodes) {
			t.Fatalf("private nodes = %#v", values)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScopedSecretsLimitCountNestedAliasesWinBeforeNormalization(t *testing.T) {
	const (
		rootHost   = "$ENV://LIMIT_COUNT_ROOT_HOST_IGNORED"
		nestedHost = "$ENV://LIMIT_COUNT_NESTED_WINNER"
	)
	rootNodes := []string{"$ENV://LIMIT_COUNT_ROOT_NODE_IGNORED"}
	nestedNodes := []string{"$ENV://LIMIT_COUNT_NESTED_NODE"}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 21, "nested-wins", map[string]string{
			nestedHost: "nested-winner.test", nestedNodes[0]: "nested-node.test:6379",
		},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, RedisHost: rootHost,
		Redis: RedisConfig{RedisHost: nestedHost}, RedisClusterNodes: rootNodes,
		RedisCluster: RedisClusterConfig{RedisClusterNodes: nestedNodes},
	}}
	materializeScopedLimitCount(t, p, secrets, scope)
	assertLimitCountCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"redis_config.redis_host", "redis_cluster_config.redis_cluster_nodes"},
		[]string{nestedHost, nestedNodes[0]},
	)
	if p.redisHostField != "redis_config.redis_host" ||
		p.redisNodesField != "redis_cluster_config.redis_cluster_nodes" {
		t.Fatalf("private provenance = %q/%q", p.redisHostField, p.redisNodesField)
	}
	if p.config.RedisHost != limitCountDescriptor("nested-winner.test") ||
		!slices.Equal(p.config.RedisClusterNodes, []string{limitCountDescriptor("nested-node.test:6379")}) {
		t.Fatalf(
			"ignored root aliases were not normalized safely: %q/%#v",
			p.config.RedisHost,
			p.config.RedisClusterNodes,
		)
	}
}

func TestScopedSecretsLimitCountConcurrentMaterializationIsSingleFlight(t *testing.T) {
	const (
		raw     = "$ENV://LIMIT_COUNT_SINGLEFLIGHT_KEY"
		workers = 32
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 22, "singleflight", map[string]string{raw: "remote_addr"},
	)
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(scopedSecretCall) {
		once.Do(func() { close(entered) })
		<-release
	})
	p := &Plugin{config: Config{Count: 1, TimeWindow: 60, Key: raw}}
	start := make(chan struct{})
	errs := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for materialization leader")
	}
	close(release)
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent materialization error = %v", err)
		}
	}
	assertLimitCountCalls(t, scope, broker.callsSnapshot(), []string{"key"}, []string{raw})
}

func prepareScopedLimitCountPlugin(
	t *testing.T, revision uint64, resourceID string, config Config, values map[string]string,
) (*Plugin, func()) {
	t.Helper()
	secrets, scope, _, closeAttempt := newScopedSecretHarness(
		t, revision, resourceID, values,
	)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	materializeScopedLimitCount(t, p, secrets, scope)
	if err := p.PostInit(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	return p, closeAttempt
}

func TestScopedSecretsLimitCountResolveKeyAndRedisBackendsUsePrivateValues(t *testing.T) {
	t.Run("key", func(t *testing.T) {
		const raw = "$secret://vault/limit-count/runtime-key"
		p, closeAttempt := prepareScopedLimitCountPlugin(
			t, 23, "runtime-key", Config{Count: 1, TimeWindow: 60, Key: raw},
			map[string]string{raw: "remote_addr"},
		)
		defer closeAttempt()
		defer p.Stop()
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "192.0.2.33:1234"
		if got := resolvedLimitCountKeyForTest(t, p, request); got != "192.0.2.33" {
			t.Fatalf("resolveKey() = %q", got)
		}
	})

	t.Run("redis host", func(t *testing.T) {
		const raw = "$ENV://LIMIT_COUNT_RUNTIME_HOST"
		p, closeAttempt := prepareScopedLimitCountPlugin(
			t, 24, "runtime-host", Config{
				Count: "$http_x_limit", TimeWindow: 60, Policy: "redis", RedisHost: raw,
			}, map[string]string{raw: "resolved-redis.test"},
		)
		defer closeAttempt()
		defer p.Stop()
		client, err := p.redisBackendClient()
		if err != nil {
			t.Fatal(err)
		}
		if got := client.(*redis.Client).Options().Addr; got != "resolved-redis.test:6379" {
			t.Fatalf("Redis address = %q", got)
		}
		if strings.Contains(fmt.Sprintf("%#v", client.(*redis.Client).Options()), raw) {
			t.Fatal("Redis options retained raw host reference")
		}
	})

	t.Run("redis cluster", func(t *testing.T) {
		raws := []string{"$ENV://LIMIT_COUNT_RUNTIME_NODE_0", "$ENV://LIMIT_COUNT_RUNTIME_NODE_1"}
		p, closeAttempt := prepareScopedLimitCountPlugin(
			t, 25, "runtime-nodes", Config{
				Count: "$http_x_limit", TimeWindow: 60, Policy: "redis-cluster",
				RedisClusterNodes: slices.Clone(raws), RedisClusterName: "cluster",
			}, map[string]string{raws[0]: "node-0.test:6379", raws[1]: "node-1.test:6379"},
		)
		defer closeAttempt()
		defer p.Stop()
		client, err := p.redisBackendClient()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"node-0.test:6379", "node-1.test:6379"}
		if got := client.(*redis.ClusterClient).Options().Addrs; !slices.Equal(got, want) {
			t.Fatalf("cluster addresses = %#v", got)
		}
		options := fmt.Sprintf("%#v", client.(*redis.ClusterClient).Options())
		for _, raw := range raws {
			if strings.Contains(options, raw) {
				t.Fatalf("cluster options retained raw reference %q", raw)
			}
		}
	})
}

func TestScopedSecretsLimitCountBackendIdentityUsesResolvedDigests(t *testing.T) {
	const raw = "$ENV://LIMIT_COUNT_IDENTITY_HOST"
	clients := make([]redis.UniversalClient, 0, 2)
	plugins := make([]*Plugin, 0, 2)
	closures := make([]func(), 0, 2)
	for index, resolved := range []string{"identity-a.test", "identity-b.test"} {
		p, closeAttempt := prepareScopedLimitCountPlugin(
			t, uint64(30+index), fmt.Sprintf("identity-%d", index), Config{
				Count: "$http_x_limit", TimeWindow: 60, Policy: "redis", RedisHost: raw,
			}, map[string]string{raw: resolved},
		)
		client, err := p.redisBackendClient()
		if err != nil {
			t.Fatal(err)
		}
		plugins = append(plugins, p)
		closures = append(closures, closeAttempt)
		clients = append(clients, client)
	}
	defer closures[0]()
	defer closures[1]()
	defer plugins[0].Stop()
	defer plugins[1].Stop()
	if clients[0] == clients[1] {
		t.Fatal("different resolved hosts shared one Redis client")
	}
	for index, resolved := range []string{"identity-a.test:6379", "identity-b.test:6379"} {
		if got := clients[index].(*redis.Client).Options().Addr; got != resolved {
			t.Fatalf("client[%d] address = %q", index, got)
		}
	}
}

func TestScopedSecretsLimitCountGenerationInstancesDoNotCrossUseKeys(t *testing.T) {
	const raw = "$ENV://LIMIT_COUNT_GENERATION_KEY"
	plugins := make([]*Plugin, 0, 2)
	closures := make([]func(), 0, 2)
	for index, resolved := range []string{"arg_user", "http_host"} {
		p, closeAttempt := prepareScopedLimitCountPlugin(
			t, uint64(35+index), fmt.Sprintf("generation-%d", index),
			Config{Count: 1, TimeWindow: 60, Key: raw}, map[string]string{raw: resolved},
		)
		plugins = append(plugins, p)
		closures = append(closures, closeAttempt)
	}
	defer closures[0]()
	defer closures[1]()
	defer plugins[0].Stop()
	defer plugins[1].Stop()
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/?user=alice", nil)
	if got := resolvedLimitCountKeyForTest(t, plugins[0], request); got != "alice" {
		t.Fatalf("generation N key = %q", got)
	}
	if got := resolvedLimitCountKeyForTest(t, plugins[1], request); got != "gateway.test" {
		t.Fatalf("generation N+1 key = %q", got)
	}
	plugins[0].Stop()
	if got := resolvedLimitCountKeyForTest(t, plugins[1], request); got != "gateway.test" {
		t.Fatalf("generation N+1 after N Stop key = %q", got)
	}
}

func TestLimitCountStopDrainsScopedCallbacksAndDestroysSecrets(t *testing.T) {
	const plaintext = "stop-redis.test"
	const raw = "$ENV://LIMIT_COUNT_STOP_HOST"
	secrets, scope, _, closeAttempt := newScopedSecretHarness(
		t, 40, "stop-scoped", map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{Count: 1, TimeWindow: 60, RedisHost: raw}}
	materializeScopedLimitCount(t, p, secrets, scope)
	entered := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- p.withLimitCountRedisHost(func(host string) error {
			if host != plaintext {
				return fmt.Errorf("host = %q", host)
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	firstStop := make(chan struct{})
	secondStop := make(chan struct{})
	go func() { p.Stop(); close(firstStop) }()
	go func() { p.Stop(); close(secondStop) }()
	deadline := time.Now().Add(time.Second)
	for {
		p.credentialMu.Lock()
		retired, active := p.retired, p.activeUses
		p.credentialMu.Unlock()
		if retired && active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for credential drain")
		}
		time.Sleep(time.Millisecond)
	}
	for _, stopped := range []chan struct{}{firstStop, secondStop} {
		select {
		case <-stopped:
			t.Fatal("Stop() returned before callback release")
		default:
		}
	}
	if err := p.withLimitCountRedisHost(
		func(string) error { return nil },
	); !errors.Is(
		err,
		errLimitCountCredentialsUnavailable,
	) {
		t.Fatalf("credential use after retirement error = %v", err)
	}
	close(release)
	if err := <-callbackDone; err != nil {
		t.Fatal(err)
	}
	<-firstStop
	<-secondStop
	p.Stop()
	p.credentialMu.Lock()
	retained := p.scopedKeySecret != (secret.Value{}) ||
		p.scopedRedisHost != (secret.Value{}) || len(p.scopedRedisClusterNodes) != 0 ||
		p.scopedSet || len(p.redisNodeDigests) != 0 || p.activeUses != 0
	p.credentialMu.Unlock()
	if retained {
		t.Fatal("Stop() retained credential state")
	}
}

func TestScopedSecretsLimitCountStopDuringMaterializeCannotRevive(t *testing.T) {
	const raw = "$ENV://LIMIT_COUNT_STOP_MATERIALIZE"
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 41, "stop-materialize", map[string]string{raw: "remote_addr"},
	)
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(scopedSecretCall) {
		once.Do(func() { close(entered) })
		<-release
	})
	p := &Plugin{config: Config{Count: 1, TimeWindow: 60, Key: raw}}
	done := make(chan error, 1)
	go func() {
		done <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	}()
	<-entered
	p.Stop()
	close(release)
	if err := <-done; err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("materialization racing Stop() error = %v", err)
	}
	if p.config.Key != raw || p.scopedSet || p.scopedKeySecret != (secret.Value{}) {
		t.Fatal("materialization revived stopped plugin")
	}
	calls := len(broker.callsSnapshot())
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err == nil {
		t.Fatal("materialization after Stop() error = nil")
	}
	if len(broker.callsSnapshot()) != calls {
		t.Fatal("materialization after Stop() called broker")
	}
}

type blockingLimitCountStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	key     string
}

func newBlockingLimitCountStore() *blockingLimitCountStore {
	return &blockingLimitCountStore{entered: make(chan struct{}), release: make(chan struct{})}
}

func (store *blockingLimitCountStore) consume(
	ctx context.Context, key string, rate limiter.Rate,
) (limiter.Context, error) {
	store.key = key
	store.once.Do(func() { close(store.entered) })
	select {
	case <-store.release:
		return limiter.Context{
			Limit: rate.Limit, Remaining: rate.Limit - 1,
			Reset: time.Now().Add(rate.Period).Unix(),
		}, nil
	case <-ctx.Done():
		return limiter.Context{}, ctx.Err()
	}
}

func (store *blockingLimitCountStore) Get(
	ctx context.Context, key string, rate limiter.Rate,
) (limiter.Context, error) {
	return store.consume(ctx, key, rate)
}

func (store *blockingLimitCountStore) Peek(
	ctx context.Context, key string, rate limiter.Rate,
) (limiter.Context, error) {
	return store.consume(ctx, key, rate)
}

func (store *blockingLimitCountStore) Reset(
	ctx context.Context, key string, rate limiter.Rate,
) (limiter.Context, error) {
	return store.consume(ctx, key, rate)
}

func (store *blockingLimitCountStore) Increment(
	ctx context.Context, key string, _ int64, rate limiter.Rate,
) (limiter.Context, error) {
	return store.consume(ctx, key, rate)
}

func assertLimitCountStoppedPublicationState(t *testing.T, p *Plugin) {
	t.Helper()
	if p.groupRegistered || p.limiter != nil || p.sliding != nil || p.delayed != nil ||
		p.slidingStore != nil || p.localLimiterStore != nil || p.fixedStore != nil || p.backendClient != nil ||
		p.clientRelease != nil || p.limiters != nil || p.slidingByKey != nil ||
		p.delayedByKey != nil || p.ruleLimiters != nil {
		t.Fatalf("stopped plugin retained publication state: %#v", p)
	}
	if p.config.Group != "" {
		limitCountGroups.Lock()
		_, registered := limitCountGroups.entries[p.config.Group]
		limitCountGroups.Unlock()
		if registered {
			t.Fatalf("stopped plugin retained group %q", p.config.Group)
		}
	}
}

func TestLimitCountPostInitAfterStopPublishesNothing(t *testing.T) {
	resetLimitCountGroupsForTest()
	t.Cleanup(resetLimitCountGroupsForTest)
	for _, test := range []struct {
		name   string
		config Config
	}{
		{name: "local-group", config: Config{
			Count: 1, TimeWindow: 60, Key: "constant-key", KeyType: "constant",
			Policy: "local", Group: "post-stop-local",
		}},
		{name: "redis", config: Config{
			Count: 1, TimeWindow: 60, Key: "constant-key", KeyType: "constant",
			Policy: "redis", RedisHost: "redis.test", Group: "post-stop-redis",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: test.config}
			p.Stop()
			if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
				t.Fatalf("PostInit() error = %v, want ErrCredentialUnavailable", err)
			}
			p.Stop()
			assertLimitCountStoppedPublicationState(t, p)
		})
	}
}

func TestLimitCountPostInitAndStopAreLifecycleSerialized(t *testing.T) {
	resetLimitCountGroupsForTest()
	t.Cleanup(resetLimitCountGroupsForTest)
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Key: "constant-key", KeyType: "constant",
		Policy: "local", Group: "postinit-first",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, closeAttempt := newScopedSecretHarness(t, 88, "postinit-first", nil)
	defer closeAttempt()
	materializeScopedLimitCount(t, p, secrets, scope)
	p.credentialMu.Lock()
	postDone := make(chan error, 1)
	go func() { postDone <- p.PostInit() }()
	deadline := time.Now().Add(time.Second)
	for p.lifecycleMu.TryLock() {
		p.lifecycleMu.Unlock()
		if time.Now().After(deadline) {
			p.credentialMu.Unlock()
			t.Fatal("PostInit() did not enter lifecycle gate")
		}
		runtime.Gosched()
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		p.credentialMu.Unlock()
		t.Fatal("Stop() passed PostInit lifecycle gate")
	case <-time.After(20 * time.Millisecond):
	}
	p.credentialMu.Unlock()
	if err := <-postDone; err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	<-stopDone
	assertLimitCountStoppedPublicationState(t, p)

	stoppedFirst := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Key: "constant-key", KeyType: "constant",
		Policy: "local", Group: "stop-first",
	}}
	if err := stoppedFirst.Init(); err != nil {
		t.Fatal(err)
	}
	stoppedCapability, stoppedScope, _, stoppedClose := newScopedSecretHarness(t, 89, "stop-first", nil)
	defer stoppedClose()
	materializeScopedLimitCount(t, stoppedFirst, stoppedCapability, stoppedScope)
	stoppedFirst.Stop()
	if err := stoppedFirst.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("stop-first PostInit() error = %v", err)
	}
	assertLimitCountStoppedPublicationState(t, stoppedFirst)
}

func TestLimitCountStopWaitsForActualKeyConsumption(t *testing.T) {
	const raw = "$ENV://LIMIT_COUNT_BLOCKED_CONSTANT"
	p := &Plugin{config: Config{
		Count: 1, TimeWindow: 60, Key: raw, KeyType: "constant", Policy: "local",
	}}
	secrets, scope, _, closeAttempt := newScopedSecretHarness(
		t, 90, "blocked-key", map[string]string{raw: "private-limit-key"},
	)
	defer closeAttempt()
	materializeScopedLimitCount(t, p, secrets, scope)
	store := newBlockingLimitCountStore()
	p.localLimiterStore = store
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	handlerDone := make(chan struct{})
	go func() {
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
			httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil),
		)
		close(handlerDone)
	}()
	<-store.entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop() returned before limiter consumed the resolved key")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	<-handlerDone
	<-stopDone
	if store.key != "route:unknown:private-limit-key" {
		t.Fatalf("limiter consumed key %q", store.key)
	}
	assertLimitCountStoppedPublicationState(t, p)
}
