package kafka_proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, closeAttempt := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	t.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestKafkaProxyDoesNotImplementRequestPhasePlugin(t *testing.T) {
	var candidate any = &Plugin{}
	if _, ok := candidate.(base.RequestPhasePlugin); ok {
		t.Fatal(
			"*Plugin implements base.RequestPhasePlugin; production would release the SASL callback before terminal use",
		)
	}
}

func TestMaterializeScopedSecretsFailureIsAtomicAndRetryable(t *testing.T) {
	const raw = "$secret://vault/kafka-failure"
	capabilityValue, scope, broker, closeAttempt := newKafkaProxyScopedSecretHarness(
		t, 30, "kafka-failure", map[string]string{raw: "private-password"},
	)
	defer closeAttempt()
	broker.fail[raw] = errors.New("private resolver failure")
	p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: raw}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-password") ||
		strings.Contains(err.Error(), "private resolver") {
		t.Fatalf("materialization error leaked secret details: %v", err)
	}
	if p.config.SASL.Password != raw || p.saslPassword != nil {
		t.Fatalf(
			"failed materialization retained state: config=%q scoped=%#v",
			p.config.SASL.Password,
			p.saslPassword,
		)
	}

	broker.mu.Lock()
	delete(broker.fail, raw)
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("retry materialization error = %v", err)
	}
	assertKafkaProxySecretDescriptorFor(t, p.config.SASL.Password, "private-password")
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() after retry error = %v", err)
	}
}

func TestMaterializeScopedSecretsRejectsEmptyPasswordAndRetriesAtomically(t *testing.T) {
	const raw = "$ENV://KAFKA_EMPTY"
	for _, resolved := range []string{"", "   "} {
		t.Run("resolved-"+strings.ReplaceAll(resolved, " ", "space"), func(t *testing.T) {
			capabilityValue, scope, broker, closeAttempt := newKafkaProxyScopedSecretHarness(
				t, 31, "kafka-empty", map[string]string{raw: resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: raw}}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
			if err == nil {
				t.Fatal("empty admitted password materialized successfully")
			}
			if strings.Contains(err.Error(), raw) ||
				(resolved != "" && strings.Contains(err.Error(), resolved)) {
				t.Fatalf("empty-password error leaked secret details: %v", err)
			}
			if p.config.SASL.Password != raw || p.saslPassword != nil {
				t.Fatalf(
					"failed empty materialization retained state: config=%q scoped=%#v",
					p.config.SASL.Password,
					p.saslPassword,
				)
			}

			broker.mu.Lock()
			broker.values[raw] = "retry-password"
			broker.mu.Unlock()
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("retry materialization error = %v", err)
			}
			assertKafkaProxySecretDescriptorFor(t, p.config.SASL.Password, "retry-password")
		})
	}
}

func TestMaterializeScopedSecretsIsIdempotent(t *testing.T) {
	const raw = "$ENV://KAFKA_IDEMPOTENT"
	capabilityValue, scope, broker, closeAttempt := newKafkaProxyScopedSecretHarness(
		t, 32, "kafka-idempotent", map[string]string{raw: "idempotent-password"},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: raw}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatal(err)
	}
	descriptor := p.config.SASL.Password
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatal(err)
	}
	if len(broker.calls) != 1 || p.config.SASL.Password != descriptor {
		t.Fatalf(
			"idempotent materialization calls/config = %d/%q, want 1/%q",
			len(broker.calls),
			p.config.SASL.Password,
			descriptor,
		)
	}
}

func materializeKafkaProxyScopedPlugin(
	t *testing.T, revision uint64, resourceID, raw, resolved string,
) (*Plugin, func()) {
	t.Helper()
	capabilityValue, scope, _, closeAttempt := newKafkaProxyScopedSecretHarness(
		t, revision, resourceID, map[string]string{raw: resolved},
	)
	p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: raw}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p, closeAttempt
}

func TestKafkaProxyPasswordRotationDoesNotCrossGenerationsAndStopIsIdempotent(t *testing.T) {
	pN, closeN := materializeKafkaProxyScopedPlugin(
		t, 40, "kafka-n", "$ENV://KAFKA_N", "password-n",
	)
	defer closeN()
	pNPlusOne, closeNPlusOne := materializeKafkaProxyScopedPlugin(
		t, 41, "kafka-n-plus-one", "$ENV://KAFKA_N_PLUS_ONE", "password-n-plus-one",
	)
	defer closeNPlusOne()

	password := func(p *Plugin) string {
		t.Helper()
		var got string
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = SASLPassword(r)
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/kafka", nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("handler response = %d, want 204", response.Code)
		}
		return got
	}
	if got := password(pN); got != "password-n" {
		t.Fatalf("generation N password = %q", got)
	}
	if got := password(pNPlusOne); got != "password-n-plus-one" {
		t.Fatalf("generation N+1 password = %q", got)
	}

	owned := pN.saslPassword
	pN.Stop()
	pN.Stop()
	if pN.saslPassword != nil || !pN.stopped {
		t.Fatalf(
			"generation N state after Stop = scoped:%#v stopped:%v",
			pN.saslPassword,
			pN.stopped,
		)
	}
	if owned == nil || *owned != (secret.Value{}) {
		t.Fatalf("generation N retained scoped value after Stop = %#v", owned)
	}
	if got := password(pNPlusOne); got != "password-n-plus-one" {
		t.Fatalf("generation N+1 password after N retirement = %q", got)
	}
}

func TestKafkaProxyHandlerClearsRetainedRequestPassword(t *testing.T) {
	p := newTestPlugin(t, Config{SASL: &SASL{Username: "user", Password: "password"}})
	var (
		retained *http.Request
		calls    int
	)
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		retained = r
		if got := SASLPassword(r); got != "password" {
			t.Fatalf("SASLPassword() during next = %q, want password", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/kafka", nil))
	if calls != 1 || response.Code != http.StatusNoContent {
		t.Fatalf("handler calls/status = %d/%d, want 1/204", calls, response.Code)
	}
	if got := SASLPassword(retained); got != "" {
		t.Fatalf("SASLPassword() after next returned = %q, want cleared", got)
	}
}

func TestKafkaProxyHandlerClearsRetainedRequestPasswordAfterPanic(t *testing.T) {
	p := newTestPlugin(t, Config{SASL: &SASL{Username: "user", Password: "password"}})
	var retained *http.Request
	func() {
		defer func() {
			if recovered := recover(); recovered != "terminal panic" {
				t.Fatalf("recovered panic = %#v, want terminal panic", recovered)
			}
		}()
		p.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			retained = r
			if got := SASLPassword(r); got != "password" {
				t.Fatalf("SASLPassword() before panic = %q, want password", got)
			}
			panic("terminal panic")
		})).ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/kafka", nil),
		)
	}()
	if retained == nil {
		t.Fatal("panic terminal did not retain request")
	}
	if got := SASLPassword(retained); got != "" {
		t.Fatalf("SASLPassword() after panic = %q, want cleared", got)
	}
}

func TestKafkaProxyStopZerosPasswordAndRetiresHandler(t *testing.T) {
	p, cleanup := materializeKafkaProxyScopedPlugin(
		t, 59, "kafka-stop", "$ENV://KAFKA_STOP", "scoped-password",
	)
	defer cleanup()
	owned := p.saslPassword
	p.Stop()
	p.Stop()
	if owned == nil || *owned != (secret.Value{}) {
		t.Fatalf("scoped password after Stop = %#v, want zeroed", owned)
	}
	if p.saslPassword != nil || !p.stopped {
		t.Fatalf("scoped state after Stop = %#v", p)
	}
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("retired kafka-proxy plugin called next handler")
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/retired", nil))
	if response.Code != http.StatusInternalServerError ||
		strings.TrimSpace(response.Body.String()) != "Kafka SASL credentials unavailable" {
		t.Fatalf(
			"retired handler response = %d/%q, want 500/unavailable",
			response.Code,
			response.Body.String(),
		)
	}
}

func TestKafkaProxyStopWaitsForScopedSASLTerminal(t *testing.T) {
	p, cleanup := materializeKafkaProxyScopedPlugin(
		t, 60, "kafka-stop-scoped", "$ENV://KAFKA_STOP_SCOPED", "scoped-password",
	)
	defer cleanup()
	saved := p.saslPassword
	entered := make(chan struct{})
	release := make(chan struct{})
	requestDone := make(chan struct{})
	stopAttempted := make(chan struct{}, 2)
	stopDone := make(chan struct{}, 2)
	p.stopBeforeLock = func() { stopAttempted <- struct{}{} }

	go func() {
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := SASLPassword(r); got == "" {
				t.Errorf("terminal SASLPassword() = empty")
			}
			close(entered)
			<-release
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/kafka", nil),
		)
		close(requestDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal entry")
	}
	for range 2 {
		go func() {
			p.Stop()
			stopDone <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-stopAttempted:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for Stop write-lock attempt")
		}
	}
	select {
	case <-stopDone:
		t.Fatal("concurrent Stop returned while terminal held the credential read lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal release")
	}
	for range 2 {
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent Stop completion")
		}
	}
	if p.saslPassword != nil || !p.stopped {
		t.Fatalf("retired state = scoped:%#v stopped:%v", p.saslPassword, p.stopped)
	}
	if saved == nil || *saved != (secret.Value{}) {
		t.Fatalf("saved scoped owner after Stop = %#v, want zero", saved)
	}
	p.Stop()
}

func runConcurrentKafkaScopedMaterialization(
	p *Plugin, scope secret.Scope, capabilityValue secret.GenerationCapability,
) []error {
	const workers = 32
	start := make(chan struct{})
	errorsByWorker := make([]error, workers)
	var group sync.WaitGroup
	for index := range workers {
		group.Go(func() {
			<-start
			errorsByWorker[index] = base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
		})
	}
	close(start)
	group.Wait()
	return errorsByWorker
}

func TestMaterializeScopedSecretsConcurrentAdmissionIsSingleAndRetryable(t *testing.T) {
	t.Run("fresh success", func(t *testing.T) {
		const raw = "$ENV://KAFKA_CONCURRENT_FRESH"
		capabilityValue, scope, broker, closeAttempt := newKafkaProxyScopedSecretHarness(
			t, 61, "kafka-concurrent-fresh", map[string]string{raw: "fresh-password"},
		)
		defer closeAttempt()
		p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: raw}}}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		for index, err := range runConcurrentKafkaScopedMaterialization(p, scope, capabilityValue) {
			if err != nil {
				t.Fatalf("concurrent materialization %d error = %v", index, err)
			}
		}
		if len(broker.calls) != 1 {
			t.Fatalf("fresh scoped broker calls = %d, want 1", len(broker.calls))
		}
		assertKafkaProxySecretDescriptorFor(t, p.config.SASL.Password, "fresh-password")
	})

	t.Run("failure then retry", func(t *testing.T) {
		const raw = "$secret://vault/kafka-concurrent"
		capabilityValue, scope, broker, closeAttempt := newKafkaProxyScopedSecretHarness(
			t, 62, "kafka-concurrent-retry", map[string]string{raw: "concurrent-password"},
		)
		defer closeAttempt()
		p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: raw}}}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}

		broker.fail[raw] = errors.New("private resolver failure")
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err == nil {
			t.Fatal("initial materialization failure = nil")
		}
		if p.config.SASL.Password != raw || p.saslPassword != nil {
			t.Fatalf("failed initial materialization retained state")
		}
		broker.mu.Lock()
		delete(broker.fail, raw)
		broker.mu.Unlock()
		for index, err := range runConcurrentKafkaScopedMaterialization(p, scope, capabilityValue) {
			if err != nil {
				t.Fatalf("concurrent retry %d error = %v", index, err)
			}
		}
		if len(broker.calls) != 2 {
			t.Fatalf("scoped broker calls after failure/retry = %d, want exact sequence of 2", len(broker.calls))
		}
		if broker.calls[0].Raw != raw || broker.calls[1].Raw != raw ||
			broker.calls[0].Scope != broker.calls[1].Scope {
			t.Fatalf("scoped broker call sequence = %#v, want identical exact authority", broker.calls)
		}
		assertKafkaProxySecretDescriptorFor(t, p.config.SASL.Password, "concurrent-password")
	})
}

type kafkaProxyScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type kafkaProxyScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []kafkaProxyScopedSecretCall
}

func (*kafkaProxyScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*kafkaProxyScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (broker *kafkaProxyScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, kafkaProxyScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func (*kafkaProxyScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func newKafkaProxyScopedSecretHarness(
	t *testing.T, revision uint64, resourceID string, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *kafkaProxyScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{"kafka-proxy":{}}}`),
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
			Key: key, Disposition: generation.DispositionPublished, Code: "kafka-proxy-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker := &kafkaProxyScopedSecretBroker{
		values: values,
		fail:   make(map[string]error),
	}
	registration, err := secret.NewScopedMaterializer(broker, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func assertKafkaProxySecretDescriptorFor(t *testing.T, value, plaintext string) {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if value != want {
		t.Fatalf("kafka-proxy descriptor = %q, want %q", value, want)
	}
}

func TestMaterializeScopedSecretsOwnsKafkaProxySASLPassword(t *testing.T) {
	contextual, err := data_encryption.EncryptForContext(
		"contextual-password", "0123456789abcdef", "kafka-proxy.sasl.password",
	)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "literal", raw: "literal-password", resolved: "literal-password"},
		{name: "contextual ciphertext", raw: contextual, resolved: "contextual-password"},
		{name: "environment", raw: "$ENV://KAFKA_PASSWORD", resolved: "environment-password"},
		{name: "managed", raw: "$secret://vault/kafka-password", resolved: "managed-password"},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capabilityValue, scope, broker, closeAttempt := newKafkaProxyScopedSecretHarness(
				t, uint64(index+1), "kafka-route", map[string]string{tt.raw: tt.resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: tt.raw}}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatal(err)
			}
			if err := p.PostInit(); err != nil {
				t.Fatal(err)
			}
			if len(broker.calls) != 1 {
				t.Fatalf("scoped calls = %#v, want one exact sasl.password call", broker.calls)
			}
			call := broker.calls[0]
			wantScope := scope
			wantScope.Field = "sasl.password"
			if call.Raw != tt.raw || call.Scope != wantScope {
				t.Fatalf("scoped call = %#v, want exact kafka-proxy.sasl.password authority", call)
			}
			assertKafkaProxySecretDescriptorFor(t, p.config.SASL.Password, tt.resolved)
			if strings.Contains(p.config.SASL.Password, tt.raw) ||
				strings.Contains(p.config.SASL.Password, tt.resolved) {
				t.Fatalf("public config retained secret material: %q", p.config.SASL.Password)
			}

			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := SASLPassword(r); got != tt.resolved {
					t.Fatalf("SASLPassword() = %q, want admitted value", got)
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/kafka", nil))
			if response.Code != http.StatusNoContent {
				t.Fatalf("response code = %d, want 204", response.Code)
			}
		})
	}

	t.Run("nil SASL", func(t *testing.T) {
		capabilityValue, scope, broker, closeAttempt := newKafkaProxyScopedSecretHarness(
			t, 20, "kafka-no-sasl", nil,
		)
		defer closeAttempt()
		p := &Plugin{}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err != nil {
			t.Fatal(err)
		}
		if err := p.PostInit(); err != nil {
			t.Fatal(err)
		}
		if len(broker.calls) != 0 {
			t.Fatalf("nil SASL materialization calls = %#v, want none", broker.calls)
		}
	})
}

func TestPostInitWithSASLRequiresMaterialization(t *testing.T) {
	p := &Plugin{config: Config{SASL: &SASL{
		Username: "user", Password: "$ENV://KAFKA_UNPREPARED",
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err == nil || !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() error = %v, want unavailable credential", err)
	}
	if p.config.SASL.Password != "$ENV://KAFKA_UNPREPARED" {
		t.Fatalf("unprepared PostInit changed public password = %q", p.config.SASL.Password)
	}
}

func TestHandlerStoresSASLConfigForKafkaUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{
		SASL: &SASL{
			Username: "user",
			Password: "pwd",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/kafka", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !SASLEnabled(r) {
			t.Fatal("SASLEnabled() = false, want true")
		}
		if got := SASLUsername(r); got != "user" {
			t.Fatalf("SASLUsername() = %q, want user", got)
		}
		if got := SASLPassword(r); got != "pwd" {
			t.Fatalf("SASLPassword() = %q, want pwd", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerDoesNotSetSASLContextWhenDisabled(t *testing.T) {
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/kafka", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SASLEnabled(r) {
			t.Fatal("SASLEnabled() = true, want false")
		}
		if got := SASLUsername(r); got != "" {
			t.Fatalf("SASLUsername() = %q, want empty", got)
		}
		if got := SASLPassword(r); got != "" {
			t.Fatalf("SASLPassword() = %q, want empty", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
}
