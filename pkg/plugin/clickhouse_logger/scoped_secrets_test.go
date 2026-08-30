package clickhouse_logger

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	t *testing.T, factory string, values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	return newScopedSecretHarnessAt(t, factory, 7, "r1", values)
}

func newScopedSecretHarnessAt(
	t *testing.T,
	factory string,
	revision uint64,
	resourceID string,
	values map[string]string,
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
	baseScope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     factory,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return secrets, baseScope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func clickHouseScopedConfig(user, password string) Config {
	return Config{
		EndpointAddrs: []string{"http://127.0.0.1:8123"},
		User:          user,
		Password:      password,
		Database:      "default",
		LogTable:      "apisix_logs",
		LogFormat:     map[string]string{"request_id": "$request_id"},
	}
}

func TestScopedSecretsMaterializeClickHouseUserAndStrictPassword(t *testing.T) {
	const (
		rawUser     = "$ENV://CLICK_HOUSE_USER"
		rawPassword = "$secret://vault/clickhouse/password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawUser:     "fixture-user",
		rawPassword: "fixture-password",
	})
	defer closeAttempt()

	p := &Plugin{config: clickHouseScopedConfig(rawUser, rawPassword)}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	if len(broker.calls) != 2 {
		t.Fatalf("broker calls = %d, want 2", len(broker.calls))
	}
	if got := broker.calls[0].Scope.Field; got != "user" {
		t.Fatalf("first field = %q, want user", got)
	}
	if got := broker.calls[1].Scope.Field; got != "password" {
		t.Fatalf("second field = %q, want password", got)
	}
	if broker.calls[0].Raw != rawUser || broker.calls[1].Raw != rawPassword {
		t.Fatalf("broker raws = %#v, want user/password inputs", broker.calls)
	}
	for field, value := range map[string]string{"user": p.config.User, "password": p.config.Password} {
		if !strings.HasPrefix(value, "plugin_config#sha256:") || len(value) != len("plugin_config#sha256:")+64 {
			t.Fatalf("%s descriptor = %q, want plugin_config sha256 descriptor", field, value)
		}
		if strings.Contains(value, "fixture-") ||
			strings.Contains(value, rawUser) || strings.Contains(value, rawPassword) {
			t.Fatalf("%s descriptor = %q, contains credential material", field, value)
		}
	}
}

func TestScopedSecretsResolveManagedClickHouseUser(t *testing.T) {
	const (
		rawUser     = "$secret://vault/clickhouse/user"
		rawPassword = "$secret://vault/clickhouse/password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawUser:     "managed-user",
		rawPassword: "managed-password",
	})
	defer closeAttempt()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-ClickHouse-User"); got != "managed-user" {
			t.Errorf("X-ClickHouse-User = %q, want managed-user", got)
		}
		if got := r.Header.Get("X-ClickHouse-Key"); got != "managed-password" {
			t.Errorf("X-ClickHouse-Key = %q, want managed-password", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := &Plugin{config: clickHouseScopedConfig(rawUser, rawPassword)}
	p.config.EndpointAddrs = []string{server.URL}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if len(broker.calls) != 2 || broker.calls[0].Scope.Field != "user" || broker.calls[1].Scope.Field != "password" {
		t.Fatalf("broker calls = %#v, want user then password", broker.calls)
	}
}

func TestScopedSecretsSkipEmptyClickHouseUser(t *testing.T) {
	const rawPassword = "$secret://vault/clickhouse/password"
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawPassword: "managed-password",
	})
	defer closeAttempt()

	p := &Plugin{config: clickHouseScopedConfig("", rawPassword)}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if len(broker.calls) != 1 || broker.calls[0].Scope.Field != "password" {
		t.Fatalf("broker calls = %#v, want password only", broker.calls)
	}
	if p.config.User != "" || !strings.HasPrefix(p.config.Password, "plugin_config#sha256:") {
		t.Fatalf("config after empty optional user = %#v, want empty user and password descriptor", p.config)
	}
}

func TestScopedSecretsClickHousePasswordFailureIsAtomic(t *testing.T) {
	const (
		rawUser     = "$secret://vault/clickhouse/user"
		rawPassword = "$secret://vault/clickhouse/password"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawUser: "managed-user",
	})
	defer closeAttempt()
	broker.fail[rawPassword] = errors.New("test broker unavailable")

	p := &Plugin{config: clickHouseScopedConfig(rawUser, rawPassword)}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	originalUser, originalPassword := p.config.User, p.config.Password
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil, want password failure")
	}
	if strings.Contains(err.Error(), rawUser) ||
		strings.Contains(err.Error(), rawPassword) ||
		strings.Contains(err.Error(), "test broker unavailable") {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v, leaked credential material", err)
	}
	if p.config.User != originalUser || p.config.Password != originalPassword {
		t.Fatalf("config changed on failed preparation: user=%q password=%q", p.config.User, p.config.Password)
	}
	if p.scopedUser != (secret.Value{}) || p.scopedPassword != (secret.Value{}) {
		t.Fatal("scoped values installed after failed preparation")
	}
	if p.scopedUserSet || p.scopedPasswordSet {
		t.Fatalf(
			"scoped presence flags installed after failed preparation: user=%v password=%v",
			p.scopedUserSet, p.scopedPasswordSet,
		)
	}
}

func TestPostInitNeverCallsClickHouseDataEncryption(t *testing.T) {
	p := &Plugin{config: clickHouseScopedConfig("default", "secret")}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, name, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want no data-encryption lookup", err)
	}
	t.Cleanup(p.Stop)
}

func TestClickHouseClientIdentityUsesDigestOnly(t *testing.T) {
	const credential = "literal-client-credential"
	p := &Plugin{config: clickHouseScopedConfig("literal-user", credential)}
	for field, identity := range map[string]string{
		"user":     p.userIdentity(),
		"password": p.passwordIdentity(),
	} {
		if !strings.HasPrefix(identity, "plugin_config#sha256:") {
			t.Fatalf("%s identity = %q, want digest descriptor", field, identity)
		}
		if strings.Contains(identity, credential) || strings.Contains(identity, "literal-user") {
			t.Fatalf("%s identity = %q, contains plaintext", field, identity)
		}
	}
}

type clickHouseHeaders struct {
	user     string
	password string
}

func newScopedClickHousePlugin(
	t *testing.T,
	endpoint string,
	revision uint64,
	resourceID string,
	resolvedUser string,
	resolvedPassword string,
) (*Plugin, func()) {
	t.Helper()
	const (
		rawUser     = "$secret://vault/clickhouse/user"
		rawPassword = "$secret://vault/clickhouse/password"
	)
	secrets, scope, _, closeAttempt := newScopedSecretHarnessAt(
		t,
		name,
		revision,
		resourceID,
		map[string]string{
			rawUser:     resolvedUser,
			rawPassword: resolvedPassword,
		},
	)
	p := &Plugin{config: clickHouseScopedConfig(rawUser, rawPassword)}
	p.config.EndpointAddrs = []string{endpoint}
	p.config.BatchMaxSize = 1
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p, closeAttempt
}

func TestScopedClickHouseInstancesDoNotCrossUseCredentials(t *testing.T) {
	headers := make(chan clickHouseHeaders, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- clickHouseHeaders{
			user:     r.Header.Get("X-ClickHouse-User"),
			password: r.Header.Get("X-ClickHouse-Key"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p1, closeAttempt1 := newScopedClickHousePlugin(
		t, server.URL, 11, "generation-n", "generation-one-user", "generation-one-password",
	)
	p2, closeAttempt2 := newScopedClickHousePlugin(
		t, server.URL, 12, "generation-n1", "generation-two-user", "generation-two-password",
	)
	defer closeAttempt1()
	defer closeAttempt2()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, plugin := range []*Plugin{p1, p2} {
		wg.Add(1)
		go func(plugin *Plugin) {
			defer wg.Done()
			_, err := plugin.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1)
			errs <- err
		}(plugin)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("SendBatch() error = %v", err)
		}
	}

	seen := make(map[clickHouseHeaders]bool)
	for range 2 {
		select {
		case got := <-headers:
			seen[got] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for first-generation deliveries")
		}
	}
	if !seen[clickHouseHeaders{user: "generation-one-user", password: "generation-one-password"}] ||
		!seen[clickHouseHeaders{user: "generation-two-user", password: "generation-two-password"}] {
		t.Fatalf("headers = %#v, want one isolated pair per generation", seen)
	}

	p1.Stop()
	if _, err := p2.SendBatch(context.Background(), []map[string]any{{"path": "/after-stop"}}, 1); err != nil {
		t.Fatalf("generation-two SendBatch() after generation-one Stop error = %v", err)
	}
	select {
	case got := <-headers:
		if got != (clickHouseHeaders{user: "generation-two-user", password: "generation-two-password"}) {
			t.Fatalf("post-stop headers = %#v, want generation-two credentials", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for post-stop generation-two delivery")
	}
	p2.Stop()
}

func TestStopReturnsBeforeClickHouseDeliveryAndDefersSecretCleanup(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*testing.T, string) (*Plugin, func())
		wantScoped bool
	}{
		{
			name: "scoped",
			prepare: func(t *testing.T, endpoint string) (*Plugin, func()) {
				return newScopedClickHousePlugin(
					t, endpoint, 13, "lifecycle-scoped", "scoped-user", "scoped-password",
				)
			},
			wantScoped: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			releaseDelivery := make(chan struct{})
			var releaseDeliveryOnce sync.Once
			releaseDeliveryNow := func() { releaseDeliveryOnce.Do(func() { close(releaseDelivery) }) }
			defer releaseDeliveryNow()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-started:
				default:
					close(started)
				}
				<-releaseDelivery
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)

			p, closeAttempt := test.prepare(t, server.URL)
			defer closeAttempt()
			if test.wantScoped && (!p.scopedUserSet || !p.scopedPasswordSet) {
				t.Fatalf(
					"scoped preparation flags = user %v/password %v, want both set",
					p.scopedUserSet, p.scopedPasswordSet,
				)
			}

			var releaseCalls atomic.Int32
			originalRelease := p.clientRelease
			if originalRelease == nil {
				t.Fatal("client release is nil before Stop")
			}
			p.clientRelease = func() {
				releaseCalls.Add(1)
				originalRelease()
			}

			if !p.BatchProcessor.Push(map[string]any{"path": "/blocked"}) {
				t.Fatal("batch push was rejected")
			}
			processor := p.BatchProcessor
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("delivery did not start")
			}

			stopDone := make(chan struct{})
			go func() {
				p.Stop()
				close(stopDone)
			}()
			shutdownObserved := time.After(time.Second)
			for p.BatchProcessor.Push(map[string]any{"path": "/shutdown-probe"}) {
				select {
				case <-shutdownObserved:
					t.Fatal("Stop did not enter batch shutdown")
				default:
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case <-stopDone:
			case <-time.After(time.Second):
				t.Fatal("Stop blocked on the active batch delivery")
			}
			if got := releaseCalls.Load(); got != 0 {
				t.Fatalf("client release calls before delivery exit = %d, want zero", got)
			}
			if test.wantScoped && (!p.scopedUserSet || !p.scopedPasswordSet) {
				t.Fatal("scoped values were cleared before delivery exited")
			}

			releaseDeliveryNow()
			if err := processor.Shutdown(context.Background()); err != nil {
				t.Fatalf("batch Shutdown() error = %v", err)
			}
			if got := releaseCalls.Load(); got != 1 {
				t.Fatalf("client release calls = %d, want exactly one", got)
			}
			if p.clientRelease != nil {
				t.Fatal("client release retained after Stop")
			}
			if test.wantScoped {
				if p.scopedUser != (secret.Value{}) || p.scopedPassword != (secret.Value{}) {
					t.Fatal("scoped values retained after Stop")
				}
				if p.scopedUserSet || p.scopedPasswordSet {
					t.Fatalf(
						"scoped flags after Stop = user %v/password %v, want false",
						p.scopedUserSet, p.scopedPasswordSet,
					)
				}
			}

			p.Stop()
			if got := releaseCalls.Load(); got != 1 {
				t.Fatalf("client release calls after idempotent Stop = %d, want exactly one", got)
			}
		})
	}
}
