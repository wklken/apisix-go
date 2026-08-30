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

func TestGenerationCapabilitySameAuthorityRequiresExactRegistration(t *testing.T) {
	resource := generation.ResourceKey{Kind: "routes", ID: "same-publication"}
	ticket, set := testPublication(t, 9, generation.DomainHTTP, resource)
	firstRegistration, err := newTestMaterializer(t).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = firstRegistration.Close(context.Background()) }()
	secondRegistration, err := newTestMaterializer(t).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondRegistration.Close(context.Background()) }()
	first, err := NewGenerationCapability(firstRegistration, 9)
	if err != nil {
		t.Fatal(err)
	}
	firstAlias, err := NewGenerationCapability(firstRegistration, 9)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewGenerationCapability(secondRegistration, 9)
	if err != nil {
		t.Fatal(err)
	}
	if first.AttemptID() != second.AttemptID() || first.Generation() != second.Generation() {
		t.Fatal("same-publication fixture does not share public attempt/generation identity")
	}
	if !first.SameAuthority(firstAlias) {
		t.Fatal("SameAuthority(alias of exact registration) = false")
	}
	if first.SameAuthority(second) || second.SameAuthority(first) {
		t.Fatal("SameAuthority accepted an independent registration with the same public identity")
	}
	if first.SameAuthority(GenerationCapability{}) || (GenerationCapability{}).SameAuthority(first) {
		t.Fatal("SameAuthority accepted a zero capability")
	}
}

func TestGenerationCapabilitySharedLimiterUsesExactAttemptLifetime(t *testing.T) {
	resource := generation.ResourceKey{Kind: "routes", ID: "shared-limiter"}
	ticket, set := testPublication(t, 10, generation.DomainHTTP, resource)
	firstRegistration, err := newTestMaterializer(t).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewGenerationCapability(firstRegistration, 10)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := NewGenerationCapability(firstRegistration, 10)
	if err != nil {
		t.Fatal(err)
	}
	firstLimiter, err := first.SharedLimiter("request-validation", 4)
	if err != nil {
		t.Fatal(err)
	}
	aliasLimiter, err := alias.SharedLimiter("request-validation", 4)
	if err != nil {
		t.Fatal(err)
	}
	releases := make([]func(), 0, 4)
	for range 4 {
		release, err := firstLimiter.Acquire(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := aliasLimiter.Acquire(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("alias limiter error = %v, want shared capacity", err)
	}
	if err := firstRegistration.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := aliasLimiter.Acquire(context.Background(), nil); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("closed attempt limiter error = %v, want credential unavailable", err)
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
	resolver      AttemptResolver
	openCalls     int
	closeCalls    int
	lastCandidate generation.PublicationSet
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

func (factory *testResolverFactory) newResolver() AttemptResolver {
	if factory.resolver != nil {
		return factory.resolver
	}
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
			{Factory: "http-logger", Source: capability.SecretPluginConfig, Field: "token"},
			{Factory: "key-auth", Source: capability.SecretPluginConfig, Field: "key"},
			{Factory: "key-auth", Source: capability.SecretConsumerConfig, Field: "key"},
			{Factory: "metadata-plugin", Source: capability.SecretPluginConfig, Field: "token"},
			{Factory: "metadata-plugin", Source: capability.SecretPluginMetadata, Field: "token"},
		},
	}}}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func newTestMaterializer(t *testing.T) Materializer {
	t.Helper()
	service, _ := testService(t, false)
	return NewMaterializer(service, &testResolverFactory{})
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

func TestMaterializerUsesAPISIXFallbackForAllDeclarations(t *testing.T) {
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
	tests := []struct {
		name    string
		plugin  string
		field   string
		context string
	}{
		{name: "http logger", plugin: "http-logger", field: "token", context: "http-logger.token"},
		{name: "key auth", plugin: "key-auth", field: "key", context: "key-auth.key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := testScope(
				registration,
				generation.DomainHTTP,
				tt.plugin,
				resource,
				capability.SecretPluginConfig,
				tt.field,
			)
			ciphertext, err := service.EncryptForContext("encrypted-secret", tt.context)
			if err != nil {
				t.Fatal(err)
			}
			for name, raw := range map[string]string{
				"ciphertext": ciphertext,
				"plaintext":  "plain-secret",
			} {
				t.Run(name, func(t *testing.T) {
					value, err := registration.Materialize(context.Background(), scope, raw)
					if err != nil {
						t.Fatal(err)
					}
					want := "plain-secret"
					if name == "ciphertext" {
						want = "encrypted-secret"
					}
					if got := valuePlaintext(t, value); got != want {
						t.Fatalf("Materialize() = %q, want %q", got, want)
					}
				})
			}
		})
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

func TestOpenFailureReleasesReservation(t *testing.T) {
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
}

func TestMalformedRegistrationsDoNotReachResolver(t *testing.T) {
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
	factory.mu.Lock()
	openCalls := factory.openCalls
	factory.mu.Unlock()
	if openCalls != 0 {
		t.Fatalf("resolver factory calls = %d, want zero", openCalls)
	}
}

func TestOpenCleanupFailureQuarantinesCandidateID(t *testing.T) {
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
	var observationMu sync.Mutex
	var closeContext context.Context
	resolveCalls := 0
	service, _ := testService(t, false)
	materializer := NewMaterializer(service, &testResolverFactory{resolver: &testAttemptResolver{
		resolve: func(_ context.Context, _ Scope, raw string) (string, error) {
			observationMu.Lock()
			resolveCalls++
			observationMu.Unlock()
			close(entered)
			<-release
			return raw, nil
		},
		close: func(ctx context.Context) error {
			observationMu.Lock()
			closeContext = ctx
			observationMu.Unlock()
			return nil
		},
	}})
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
	observationMu.Lock()
	observedCloseContext := closeContext
	observedResolveCalls := resolveCalls
	observationMu.Unlock()
	if observedCloseContext == nil || observedCloseContext.Err() != nil {
		t.Fatalf("resolver close context = %v, want non-cancelled context", observedCloseContext)
	}
	if observedResolveCalls != 1 {
		t.Fatalf("resolver calls = %d, want only the in-flight call", observedResolveCalls)
	}
}

func TestCloseFailureQuarantinesAndRedactsError(t *testing.T) {
	service, _ := testService(t, false)
	factory := &testResolverFactory{resolver: &testAttemptResolver{
		close: func(context.Context) error {
			return errors.New("backend included credential-value")
		},
	}}
	materializer := NewMaterializer(service, factory)
	ticket, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	registration, err := materializer.RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := registration.Close(
		context.Background(),
	); !errors.Is(err, ErrCredentialUnavailable) ||
		strings.Contains(err.Error(), "credential-value") {
		t.Fatalf("Close() error = %v, want redacted ErrCredentialUnavailable", err)
	}
	if _, err := materializer.RegisterCandidate(
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
