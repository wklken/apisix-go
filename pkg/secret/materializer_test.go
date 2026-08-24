package secret

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
)

func TestGenerationCapabilityExposesNoRegistrationLifecycleAuthority(t *testing.T) {
	if _, exists := reflect.TypeFor[GenerationCapability]().MethodByName("Close"); exists {
		t.Fatal("GenerationCapability exposes Close registration lifecycle authority")
	}
}

type testAttemptResolver struct {
	resolve func(context.Context, Scope, string) (string, error)
	close   func(context.Context) error
}

type countingAttemptRegistration struct {
	id    AttemptID
	mu    sync.Mutex
	calls int
}

func (registration *countingAttemptRegistration) AttemptID() AttemptID {
	return registration.id
}

func (registration *countingAttemptRegistration) Materialize(context.Context, Scope, string) (Value, error) {
	registration.mu.Lock()
	registration.calls++
	registration.mu.Unlock()
	return newValue("delegated"), nil
}

func (registration *countingAttemptRegistration) Close(context.Context) error {
	return nil
}

func (registration *countingAttemptRegistration) callCount() int {
	registration.mu.Lock()
	defer registration.mu.Unlock()
	return registration.calls
}

func (resolver *testAttemptResolver) ResolveScoped(ctx context.Context, scope Scope, raw string) (string, error) {
	if resolver.resolve == nil {
		return raw, nil
	}
	return resolver.resolve(ctx, scope, raw)
}

func (resolver *testAttemptResolver) Close(ctx context.Context) error {
	if resolver.close == nil {
		return nil
	}
	return resolver.close(ctx)
}

type testResolverFactory struct {
	mu            sync.Mutex
	openErr       error
	openResolver  AttemptResolver
	openCalls     int
	closeCalls    int
	lastCandidate generation.PublicationSet
	lastRecovery  map[generation.Domain]generation.PublishedGeneration
}

func (factory *testResolverFactory) OpenCandidate(
	_ context.Context,
	_ AttemptID,
	_ generation.ApplyTicket,
	set generation.PublicationSet,
) (AttemptResolver, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	factory.openCalls++
	factory.lastCandidate = clonePublicationSet(set)
	if factory.openErr != nil {
		return factory.openResolver, factory.openErr
	}
	return factory.newResolver(), nil
}

func (factory *testResolverFactory) OpenRecovery(
	_ context.Context,
	_ AttemptID,
	_ generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (AttemptResolver, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	factory.openCalls++
	factory.lastRecovery = clonePublishedGenerations(published)
	if factory.openErr != nil {
		return factory.openResolver, factory.openErr
	}
	return factory.newResolver(), nil
}

func (factory *testResolverFactory) newResolver() AttemptResolver {
	return &testAttemptResolver{
		resolve: func(_ context.Context, _ Scope, raw string) (string, error) { return raw, nil },
		close: func(context.Context) error {
			factory.mu.Lock()
			defer factory.mu.Unlock()
			factory.closeCalls++
			return nil
		},
	}
}

type testScopedBroker struct {
	mu                      sync.Mutex
	authorized              map[AttemptID]bool
	revoked                 map[AttemptID]bool
	authorizeErr            error
	revokeErr               error
	authorizeCandidateCalls int
	authorizeRecoveryCalls  int
	revokeCalls             int
	resolveCalls            int
	resolve                 func(context.Context, Scope, string) (string, error)
	resolveCall             chan struct{}
	closeCtx                context.Context
}

func (broker *testScopedBroker) AuthorizeCandidate(
	_ context.Context,
	id AttemptID,
	_ generation.ApplyTicket,
	_ generation.PublicationSet,
) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.authorizeCandidateCalls++
	return broker.authorizeLocked(id)
}

func (broker *testScopedBroker) authorizeLocked(id AttemptID) error {
	if broker.authorizeErr != nil {
		return broker.authorizeErr
	}
	if broker.authorized == nil {
		broker.authorized = make(map[AttemptID]bool)
	}
	broker.authorized[id] = true
	return nil
}

func (broker *testScopedBroker) AuthorizeRecovery(
	_ context.Context,
	id AttemptID,
	_ generation.RevisionSet,
	_ map[generation.Domain]generation.PublishedGeneration,
) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.authorizeRecoveryCalls++
	return broker.authorizeLocked(id)
}

func (broker *testScopedBroker) ResolveScoped(ctx context.Context, scope Scope, raw string) (string, error) {
	broker.mu.Lock()
	broker.resolveCalls++
	broker.mu.Unlock()
	if broker.resolveCall != nil {
		select {
		case broker.resolveCall <- struct{}{}:
		default:
		}
	}
	if broker.resolve != nil {
		return broker.resolve(ctx, scope, raw)
	}
	return raw, nil
}

func (broker *testScopedBroker) RevokeAttempt(ctx context.Context, id AttemptID) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.revokeCalls++
	broker.closeCtx = ctx
	if broker.revokeErr != nil {
		return broker.revokeErr
	}
	if broker.revoked == nil {
		broker.revoked = make(map[AttemptID]bool)
	}
	broker.revoked[id] = true
	return nil
}

func testCatalog(t *testing.T) *capability.SecretDeclarationCatalog {
	t.Helper()
	manifest := &capability.Manifest{Plugins: []capability.PluginCapability{{
		Name: "test-secrets",
		Factories: []capability.Factory{
			{Key: "http-logger"},
			{Key: "key-auth"},
			{Key: "metadata-plugin"},
		},
		SecretDeclarations: []capability.SecretDeclaration{
			{Factory: "http-logger", Source: capability.SecretPluginConfig, Field: "token", Strict: true},
			{Factory: "key-auth", Source: capability.SecretPluginConfig, Field: "key"},
			{Factory: "key-auth", Source: capability.SecretConsumerConfig, Field: "key"},
			{Factory: "metadata-plugin", Source: capability.SecretPluginConfig, Field: "token"},
			{Factory: "metadata-plugin", Source: capability.SecretPluginMetadata, Field: "token", Strict: true},
		},
	}}}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestMaterializerAcceptsDeclaredConsumerConfigScope(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactoryWithResolver{
		factory: &testResolverFactory{},
		resolve: func(_ context.Context, _ Scope, _ string) (string, error) {
			return "consumer-key", nil
		},
	}
	materializer := NewMaterializer(service, factory)
	resource := generation.ResourceKey{Kind: "consumers", ID: "c1"}
	ticket, set := testPublication(t, 9, generation.DomainHTTP, resource)
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = registration.Close(context.Background()) }()

	value, err := registration.Materialize(context.Background(), testScope(
		registration,
		generation.DomainHTTP,
		"key-auth",
		resource,
		capability.SecretConsumerConfig,
		"key",
	), "$ENV://CONSUMER_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if got := valuePlaintext(t, value); got != "consumer-key" {
		t.Fatalf("plaintext = %q, want consumer-key", got)
	}
}

func testService(
	t *testing.T,
	enabled bool,
	keyring ...string,
) (data_encryption.Service, *capability.SecretDeclarationCatalog) {
	t.Helper()
	catalog := testCatalog(t)
	return data_encryption.NewService(enabled, keyring, catalog), catalog
}

func testPublication(
	t *testing.T,
	revision uint64,
	domain generation.Domain,
	key generation.ResourceKey,
) (generation.ApplyTicket, generation.PublicationSet) {
	t.Helper()
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{Key: key, Value: []byte("resource")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: domain, Revision: revision, Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "ok",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   snapshot.Digest(),
		Cursor:          generation.ProviderCursor{Provider: "test", Revision: "cursor"},
		RequiredDomains: []generation.Domain{domain},
	}
	return ticket, generation.PublicationSet{
		DesiredRevision: revision,
		Domains:         map[generation.Domain]generation.PublicationCandidate{domain: candidate},
	}
}

func testScope(
	registration AttemptRegistration,
	domain generation.Domain,
	plugin string,
	resource generation.ResourceKey,
	source capability.SecretDeclarationSource,
	field string,
) Scope {
	return Scope{
		Generation: testGenerationForRegistration(registration),
		Attempt:    registration.AttemptID(),
		Domain:     domain,
		Plugin:     plugin,
		Resource:   resource,
		Source:     source,
		Field:      field,
	}
}

func testGenerationForRegistration(_ AttemptRegistration) uint64 {
	return 9
}

func valuePlaintext(t *testing.T, value Value) string {
	t.Helper()
	var plaintext string
	if err := value.Use(func(value string) error {
		plaintext = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func TestMaterializerRequiresOwnedScopeAndNeverLeaksPlaintext(t *testing.T) {
	service, catalog := testService(t, false)
	factory := &testResolverFactory{}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	if _, err := registration.Materialize(
		context.Background(),
		Scope{},
		"credential-value",
	); !errors.Is(
		err,
		ErrInvalidScope,
	) {
		t.Fatalf("invalid scope error = %v, want ErrInvalidScope", err)
	}
	scope := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		capability.SecretPluginConfig,
		"token",
	)
	value, err := registration.Materialize(context.Background(), scope, "$ENV://TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := valuePlaintext(t, value), "$ENV://TOKEN"; got != want {
		t.Fatalf("plaintext = %q, want %q", got, want)
	}
	if got, want := value.Digest(), sha256.Sum256([]byte("$ENV://TOKEN")); got != want {
		t.Fatalf("digest = %x, want %x", got, want)
	}
	_ = catalog
}

func TestMaterializerUsesCanonicalDecryptThenReferenceResolution(t *testing.T) {
	service, _ := testService(t, true, "0123456789abcdef")
	resolverCalls := 0
	factory := &testResolverFactory{}
	materializer := NewMaterializer(
		service,
		&testResolverFactoryWithResolver{
			factory: factory,
			resolve: func(_ context.Context, _ Scope, raw string) (string, error) {
				resolverCalls++
				if raw != "$ENV://TOKEN" {
					t.Fatalf("resolver raw = %q, want $ENV://TOKEN", raw)
				}
				return "credential-value", nil
			},
		},
	)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	scope := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		capability.SecretPluginConfig,
		"token",
	)
	contextualCiphertext, err := service.EncryptForContext("$ENV://TOKEN", "http-logger.token")
	if err != nil {
		t.Fatal(err)
	}
	value, err := registration.Materialize(context.Background(), scope, contextualCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if got := valuePlaintext(t, value); got != "credential-value" {
		t.Fatalf("plaintext = %q, want credential-value", got)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}
}

func TestMaterializerPreservesStrictAndOptionalDeclarationPolicies(t *testing.T) {
	service, _ := testService(t, true, "0123456789abcdef")
	materializer := NewMaterializer(service, &testResolverFactory{})
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	strictScope := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		resource,
		capability.SecretPluginConfig,
		"token",
	)
	strictCiphertext, err := service.EncryptForContext("strict-secret", "http-logger.token")
	if err != nil {
		t.Fatal(err)
	}
	value, err := registration.Materialize(context.Background(), strictScope, strictCiphertext)
	if err != nil || valuePlaintext(t, value) != "strict-secret" {
		t.Fatalf("strict ciphertext materialization = %q/%v", valuePlaintext(t, value), err)
	}
	if _, err := registration.Materialize(
		context.Background(),
		strictScope,
		"strict-secret",
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("strict plaintext error = %v, want ErrCredentialUnavailable", err)
	}

	optionalScope := testScope(
		registration,
		generation.DomainHTTP,
		"key-auth",
		resource,
		capability.SecretPluginConfig,
		"key",
	)
	optionalCiphertext, err := service.EncryptForContext("optional-secret", "key-auth.key")
	if err != nil {
		t.Fatal(err)
	}
	value, err = registration.Materialize(context.Background(), optionalScope, optionalCiphertext)
	if err != nil || valuePlaintext(t, value) != "optional-secret" {
		t.Fatalf("optional ciphertext materialization = %q/%v", valuePlaintext(t, value), err)
	}
	value, err = registration.Materialize(context.Background(), optionalScope, "legacy-plaintext")
	if err != nil || valuePlaintext(t, value) != "legacy-plaintext" {
		t.Fatalf("optional plaintext materialization = %q/%v", valuePlaintext(t, value), err)
	}
}

func TestRegistrationOwnsDefensivePublicationInputs(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactory{}
	materializer := NewMaterializer(service, factory)
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	ticket, set := testPublication(t, 9, generation.DomainHTTP, key)
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	ticket.RequiredDomains[0] = generation.DomainStream
	set.Domains[generation.DomainHTTP] = generation.PublicationCandidate{}
	set.Domains = nil
	scope := testScope(registration, generation.DomainHTTP, "http-logger", key, capability.SecretPluginConfig, "token")
	if _, err := registration.Materialize(context.Background(), scope, "$ENV://TOKEN"); err != nil {
		t.Fatalf("materialization after caller mutation = %v", err)
	}
}

type testResolverFactoryWithResolver struct {
	factory *testResolverFactory
	resolve func(context.Context, Scope, string) (string, error)
}

func (factory *testResolverFactoryWithResolver) OpenCandidate(
	ctx context.Context,
	id AttemptID,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (AttemptResolver, error) {
	resolver, err := factory.factory.OpenCandidate(ctx, id, ticket, set)
	if err != nil {
		return nil, err
	}
	return &testAttemptResolver{resolve: factory.resolve, close: resolver.Close}, nil
}

func (factory *testResolverFactoryWithResolver) OpenRecovery(
	ctx context.Context,
	id AttemptID,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (AttemptResolver, error) {
	resolver, err := factory.factory.OpenRecovery(ctx, id, revisions, published)
	if err != nil {
		return nil, err
	}
	return &testAttemptResolver{resolve: factory.resolve, close: resolver.Close}, nil
}

func TestMaterializerRejectsUndeclaredAndWrongSourceBeforeBackend(t *testing.T) {
	service, _ := testService(t, false)
	resolverCalls := 0
	factory := &testResolverFactoryWithResolver{
		factory: &testResolverFactory{},
		resolve: func(_ context.Context, _ Scope, raw string) (string, error) {
			resolverCalls++
			return raw, nil
		},
	}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	for _, scope := range []Scope{
		testScope(registration, generation.DomainHTTP, "http-logger", generation.ResourceKey{Kind: "routes", ID: "r1"}, capability.SecretPluginMetadata, "token"),
		testScope(registration, generation.DomainHTTP, "http-logger", generation.ResourceKey{Kind: "routes", ID: "r1"}, capability.SecretPluginConfig, "missing"),
	} {
		if _, err := registration.Materialize(
			context.Background(),
			scope,
			"$ENV://TOKEN",
		); !errors.Is(
			err,
			ErrInvalidScope,
		) {
			t.Fatalf("Materialize() error = %v, want ErrInvalidScope", err)
		}
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want zero", resolverCalls)
	}
}

func TestScopedMaterializerAuthorizesAndUsesExactCatalog(t *testing.T) {
	catalog := testCatalog(t)
	broker := &testScopedBroker{
		resolve: func(_ context.Context, _ Scope, _ string) (string, error) { return "credential-value", nil },
	}
	materializer := NewScopedMaterializer(broker, catalog)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	scope := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		capability.SecretPluginConfig,
		"token",
	)
	value, err := registration.Materialize(context.Background(), scope, "$secret://manager/token")
	if err != nil {
		t.Fatal(err)
	}
	if got := valuePlaintext(t, value); got != "credential-value" {
		t.Fatalf("plaintext = %q, want credential-value", got)
	}
	if err := registration.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	broker.mu.Lock()
	revoked := broker.revoked[registration.AttemptID()]
	broker.mu.Unlock()
	if !revoked {
		t.Fatal("attempt was not revoked")
	}
}

func TestCandidateAndRecoverySameRevisionHaveDistinctAttempts(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactory{}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	candidate, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := candidate.Close(context.Background()); err != nil {
			t.Errorf("candidate Close() error = %v", err)
		}
	}()
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(set.Domains[generation.DomainHTTP]),
	}
	recovery, err := materializer.RegisterRecovery(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 9},
		published,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := recovery.Close(context.Background()); err != nil {
			t.Errorf("recovery Close() error = %v", err)
		}
	}()
	if candidate.AttemptID() == recovery.AttemptID() {
		t.Fatal("candidate and recovery attempts aliased")
	}
}

func TestDuplicateAttemptReleasesAfterSuccessfulCloseAndQuarantinesAfterFailure(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactory{}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	first, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrAttemptAlreadyRegistered,
	) {
		t.Fatalf("duplicate error = %v, want ErrAttemptAlreadyRegistered", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second Close() error = %v", err)
		}
	}()
}

func TestOpenAndAuthorizeFailuresReleaseReservation(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactory{openErr: errors.New("open failed")}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	if _, err := materializer.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("open error = %v, want ErrCredentialUnavailable", err)
	}
	factory.openErr = nil
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	_ = registration.Close(context.Background())

	broker := &testScopedBroker{authorizeErr: errors.New("authorization failed")}
	scoped := NewScopedMaterializer(broker, testCatalog(t))
	if _, err := scoped.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("authorization error = %v, want ErrCredentialUnavailable", err)
	}
	broker.authorizeErr = nil
	registration, err = scoped.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	_ = registration.Close(context.Background())
}

func TestAuthorizeFailuresDoNotRevokeCandidateOrRecovery(t *testing.T) {
	_, _ = testService(t, false)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	broker := &testScopedBroker{authorizeErr: errors.New("authorization failed")}
	materializer := NewScopedMaterializer(broker, testCatalog(t))
	if _, err := materializer.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("candidate authorization error = %v, want ErrCredentialUnavailable", err)
	}
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(set.Domains[generation.DomainHTTP]),
	}
	if _, err := materializer.RegisterRecovery(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 9},
		published,
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("recovery authorization error = %v, want ErrCredentialUnavailable", err)
	}
	broker.mu.Lock()
	revokeCalls := broker.revokeCalls
	authorizeCandidateCalls := broker.authorizeCandidateCalls
	authorizeRecoveryCalls := broker.authorizeRecoveryCalls
	broker.mu.Unlock()
	if revokeCalls != 0 {
		t.Fatalf("revoke calls after authorization failures = %d, want zero", revokeCalls)
	}
	if authorizeCandidateCalls != 1 || authorizeRecoveryCalls != 1 {
		t.Fatalf(
			"authorization calls = candidate %d/recovery %d, want one each",
			authorizeCandidateCalls,
			authorizeRecoveryCalls,
		)
	}
}

func TestMalformedRegistrationsDoNotReachResolverOrBroker(t *testing.T) {
	service, _ := testService(t, false)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	set.Domains[generation.DomainHTTP] = generation.PublicationCandidate{}
	factory := &testResolverFactory{}
	materializer := NewMaterializer(service, factory)
	if _, err := materializer.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("malformed candidate error = %v, want ErrInvalidCapability", err)
	}
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(set.Domains[generation.DomainHTTP]),
	}
	if _, err := materializer.RegisterRecovery(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 9},
		published,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("malformed recovery error = %v, want ErrInvalidCapability", err)
	}
	factory.mu.Lock()
	openCalls := factory.openCalls
	factory.mu.Unlock()
	if openCalls != 0 {
		t.Fatalf("resolver factory calls = %d, want zero", openCalls)
	}

	broker := &testScopedBroker{}
	scoped := NewScopedMaterializer(broker, testCatalog(t))
	if _, err := scoped.RegisterCandidate(context.Background(), ticket, set); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("scoped malformed candidate error = %v, want ErrInvalidCapability", err)
	}
	if _, err := scoped.RegisterRecovery(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 9},
		published,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("scoped malformed recovery error = %v, want ErrInvalidCapability", err)
	}
	broker.mu.Lock()
	authorizeCandidateCalls := broker.authorizeCandidateCalls
	authorizeRecoveryCalls := broker.authorizeRecoveryCalls
	broker.mu.Unlock()
	if authorizeCandidateCalls != 0 || authorizeRecoveryCalls != 0 {
		t.Fatalf(
			"broker authorization calls = candidate %d/recovery %d, want zero",
			authorizeCandidateCalls,
			authorizeRecoveryCalls,
		)
	}
}

func TestOpenCleanupFailureQuarantinesCandidateAndRecoveryIDs(t *testing.T) {
	service, _ := testService(t, false)
	_, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	ticket := generation.ApplyTicket{
		DesiredRevision: 9,
		DesiredDigest:   set.Domains[generation.DomainHTTP].Snapshot.Digest(),
		Cursor:          generation.ProviderCursor{Provider: "test", Revision: "cursor"},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	cleanupErr := errors.New("cleanup included credential-value")
	factory := &testResolverFactory{
		openErr: cleanupErr,
		openResolver: &testAttemptResolver{close: func(context.Context) error {
			return cleanupErr
		}},
	}
	materializer := NewMaterializer(service, factory)
	if _, err := materializer.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("candidate open error = %v, want ErrCredentialUnavailable", err)
	}
	factory.openErr = nil
	if _, err := materializer.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrAttemptAlreadyRegistered,
	) {
		t.Fatalf("candidate retry error = %v, want ErrAttemptAlreadyRegistered", err)
	}

	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(set.Domains[generation.DomainHTTP]),
	}
	revisions := generation.RevisionSet{Desired: 9, HTTP: 9}
	recoveryFactory := &testResolverFactory{
		openErr: cleanupErr,
		openResolver: &testAttemptResolver{close: func(context.Context) error {
			return cleanupErr
		}},
	}
	recoveryMaterializer := NewMaterializer(service, recoveryFactory)
	if _, err := recoveryMaterializer.RegisterRecovery(
		context.Background(),
		revisions,
		published,
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("recovery open error = %v, want ErrCredentialUnavailable", err)
	}
	recoveryFactory.openErr = nil
	if _, err := recoveryMaterializer.RegisterRecovery(
		context.Background(),
		revisions,
		published,
	); !errors.Is(
		err,
		ErrAttemptAlreadyRegistered,
	) {
		t.Fatalf("recovery retry error = %v, want ErrAttemptAlreadyRegistered", err)
	}
}

func TestClosureIsDomainAndDispositionBound(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactoryWithResolver{
		factory: &testResolverFactory{},
		resolve: func(_ context.Context, _ Scope, raw string) (string, error) { return raw, nil },
	}
	materializer := NewMaterializer(service, factory)
	key := generation.ResourceKey{Kind: "services", ID: "shared"}
	ticket, set := testPublication(t, 9, generation.DomainHTTP, key)
	_, streamSet := testPublication(t, 9, generation.DomainStream, key)
	ticket.RequiredDomains = []generation.Domain{generation.DomainHTTP, generation.DomainStream}
	ticket.DesiredDigest = set.Domains[generation.DomainHTTP].Snapshot.Digest()
	set.Domains[generation.DomainStream] = streamSet.Domains[generation.DomainStream]
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	if _, err := registration.Materialize(
		context.Background(),
		testScope(registration, generation.DomainStream, "http-logger", key, capability.SecretPluginConfig, "token"),
		"value",
	); err != nil {
		t.Fatal(err)
	}
	wrong := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "missing"},
		capability.SecretPluginConfig,
		"token",
	)
	if _, err := registration.Materialize(
		context.Background(),
		wrong,
		"value",
	); !errors.Is(
		err,
		ErrCapabilityScopeMismatch,
	) {
		t.Fatalf("ungranted resource error = %v, want ErrCapabilityScopeMismatch", err)
	}
}

func TestGenerationCapabilityRejectsCrossGenerationAndAttempt(t *testing.T) {
	service, _ := testService(t, false)
	materializer := NewMaterializer(service, &testResolverFactory{})
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	capabilityValue, err := NewGenerationCapability(registration, 9)
	if err != nil || !capabilityValue.Valid() {
		t.Fatalf("NewGenerationCapability() = %#v/%v", capabilityValue, err)
	}
	scope := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		capability.SecretPluginConfig,
		"token",
	)
	scope.Generation++
	if _, err := capabilityValue.Materialize(
		context.Background(),
		scope,
		"value",
	); !errors.Is(
		err,
		ErrCapabilityScopeMismatch,
	) {
		t.Fatalf("generation mismatch error = %v", err)
	}
	scope = testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		capability.SecretPluginConfig,
		"token",
	)
	scope.Attempt[0]++
	if _, err := capabilityValue.Materialize(
		context.Background(),
		scope,
		"value",
	); !errors.Is(
		err,
		ErrCapabilityScopeMismatch,
	) {
		t.Fatalf("attempt mismatch error = %v", err)
	}
	if err := registration.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !capabilityValue.Valid() {
		t.Fatal("closed registration should remain structurally valid")
	}
	if _, err := capabilityValue.Materialize(
		context.Background(),
		testScope(
			registration,
			generation.DomainHTTP,
			"http-logger",
			generation.ResourceKey{Kind: "routes", ID: "r1"},
			capability.SecretPluginConfig,
			"token",
		),
		"value",
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("closed capability error = %v", err)
	}
}

func TestGenerationCapabilityRejectsMismatchesBeforeDelegation(t *testing.T) {
	registration := &countingAttemptRegistration{id: AttemptID{1}}
	capabilityValue, err := NewGenerationCapability(registration, 9)
	if err != nil {
		t.Fatal(err)
	}
	if got := capabilityValue.Generation(); got != 9 {
		t.Fatalf("Generation() = %d, want 9", got)
	}
	baseScope := Scope{
		Generation: 9,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     "http-logger",
		Resource:   generation.ResourceKey{Kind: "routes", ID: "r1"},
		Source:     capability.SecretPluginConfig,
		Field:      "token",
	}
	wrongGeneration := baseScope
	wrongGeneration.Generation++
	if _, err := capabilityValue.Materialize(
		context.Background(),
		wrongGeneration,
		"value",
	); !errors.Is(
		err,
		ErrCapabilityScopeMismatch,
	) {
		t.Fatalf("generation mismatch error = %v, want ErrCapabilityScopeMismatch", err)
	}
	wrongAttempt := baseScope
	wrongAttempt.Attempt[0]++
	if _, err := capabilityValue.Materialize(
		context.Background(),
		wrongAttempt,
		"value",
	); !errors.Is(
		err,
		ErrCapabilityScopeMismatch,
	) {
		t.Fatalf("attempt mismatch error = %v, want ErrCapabilityScopeMismatch", err)
	}
	if got := registration.callCount(); got != 0 {
		t.Fatalf("delegation calls after mismatches = %d, want zero", got)
	}
	if _, err := capabilityValue.Materialize(context.Background(), baseScope, "value"); err != nil {
		t.Fatal(err)
	}
	if got := registration.callCount(); got != 1 {
		t.Fatalf("delegation calls after matching scope = %d, want one", got)
	}
}

func TestCloseWaitsForMaterializationAndUsesNonCancelledContext(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	broker := &testScopedBroker{resolve: func(_ context.Context, _ Scope, raw string) (string, error) {
		close(entered)
		<-release
		return raw, nil
	}}
	materializer := NewScopedMaterializer(broker, testCatalog(t))
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	scope := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		capability.SecretPluginConfig,
		"token",
	)
	materializeDone := make(chan struct{})
	go func() {
		value, materializeErr := registration.Materialize(context.Background(), scope, "$ENV://TOKEN")
		if materializeErr != nil {
			t.Errorf("Materialize() error = %v", materializeErr)
		}
		_ = value
		close(materializeDone)
	}()
	select {
	case <-entered:
	case <-materializeDone:
		t.Fatal("materialization returned before entering the blocking resolver")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- registration.Close(canceled) }()
	select {
	case <-closeDone:
		t.Fatal("Close returned before in-flight materialization completed")
	case <-time.After(20 * time.Millisecond):
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := registration.Materialize(context.Background(), scope, "$ENV://SECOND")
		secondDone <- err
	}()
	close(release)
	<-materializeDone
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("second Materialize() error = %v, want ErrCredentialUnavailable", err)
	}
	broker.mu.Lock()
	closeContext := broker.closeCtx
	resolveCalls := broker.resolveCalls
	broker.mu.Unlock()
	if closeContext == nil || closeContext.Err() != nil {
		t.Fatalf("revoke context = %v, want non-cancelled context", closeContext)
	}
	if resolveCalls != 1 {
		t.Fatalf("resolver calls = %d, want only the in-flight call", resolveCalls)
	}
}

func TestCloseFailureQuarantinesAndRedactsError(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactory{}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the resolver cleanup behavior through a scoped broker, where the
	// close failure is directly controllable.
	_ = registration.Close(context.Background())
	broker := &testScopedBroker{revokeErr: errors.New("backend included credential-value")}
	scoped := NewScopedMaterializer(broker, testCatalog(t))
	registration, err = scoped.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := registration.Close(
		context.Background(),
	); !errors.Is(err, ErrCredentialUnavailable) ||
		strings.Contains(err.Error(), "credential-value") {
		t.Fatalf("Close() error = %v, want redacted ErrCredentialUnavailable", err)
	}
	if _, err := scoped.RegisterCandidate(
		context.Background(),
		ticket,
		set,
	); !errors.Is(
		err,
		ErrAttemptAlreadyRegistered,
	) {
		t.Fatalf("quarantined duplicate error = %v, want ErrAttemptAlreadyRegistered", err)
	}
}

func TestRecoveryValidationRejectsInvalidRevisionAndEmptyMap(t *testing.T) {
	service, _ := testService(t, false)
	materializer := NewMaterializer(service, &testResolverFactory{})
	if _, err := materializer.RegisterRecovery(
		context.Background(),
		generation.RevisionSet{},
		nil,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("empty recovery error = %v", err)
	}
	_, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(set.Domains[generation.DomainHTTP]),
	}
	if _, err := materializer.RegisterRecovery(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 8},
		published,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("revision mismatch error = %v", err)
	}
	if _, err := materializer.RegisterRecovery(
		context.Background(),
		generation.RevisionSet{Desired: 9, HTTP: 9, Stream: 8},
		published,
	); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("missing committed stream error = %v, want ErrInvalidCapability", err)
	}
}

func TestValueUseAndErrorsAreRedacted(t *testing.T) {
	if err := (Value{}).Use(nil); err == nil {
		t.Fatal("Value.Use(nil) returned nil")
	}
	service, _ := testService(t, false)
	factory := &testResolverFactoryWithResolver{
		factory: &testResolverFactory{},
		resolve: func(_ context.Context, _ Scope, raw string) (string, error) {
			return "", errors.New("backend included " + raw + " credential-value")
		},
	}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	scope := testScope(
		registration,
		generation.DomainHTTP,
		"http-logger",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
		capability.SecretPluginConfig,
		"token",
	)
	_, err = registration.Materialize(context.Background(), scope, "$secret://credential-value")
	if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), "credential-value") {
		t.Fatalf("Materialize() error = %v, want redacted ErrCredentialUnavailable", err)
	}
}
