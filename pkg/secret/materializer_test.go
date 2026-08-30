package secret

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
)

type testGenerationResolver struct {
	resolve func(context.Context, Scope, string) (string, error)
	close   func(context.Context) error
}

func (resolver *testGenerationResolver) ResolveReference(
	ctx context.Context,
	scope Scope,
	raw string,
) (string, error) {
	if resolver.resolve == nil {
		return raw, nil
	}
	return resolver.resolve(ctx, scope, raw)
}

func (resolver *testGenerationResolver) Close(ctx context.Context) error {
	if resolver.close == nil {
		return nil
	}
	return resolver.close(ctx)
}

type testGenerationResolverFactory struct {
	mu         sync.Mutex
	resolver   GenerationResolver
	openErr    error
	openCalls  int
	generation uint64
	set        generation.PublicationSet
}

func (factory *testGenerationResolverFactory) OpenGeneration(
	_ context.Context,
	generationNumber uint64,
	set generation.PublicationSet,
) (GenerationResolver, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	factory.openCalls++
	factory.generation = generationNumber
	factory.set = clonePublicationSet(set)
	if factory.openErr != nil {
		return factory.resolver, factory.openErr
	}
	if factory.resolver != nil {
		return factory.resolver, nil
	}
	return &testGenerationResolver{}, nil
}

func TestMaterializerPreparesGenerationAndMaterializesDeclaredFields(t *testing.T) {
	service, _ := testService(t, false)
	var calls []Scope
	factory := &testGenerationResolverFactory{resolver: &testGenerationResolver{
		resolve: func(_ context.Context, scope Scope, _ string) (string, error) {
			calls = append(calls, scope)
			return "resolved", nil
		},
	}}
	materializer := NewMaterializer(service, factory)
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	_, set := testPublication(t, 9, generation.DomainHTTP, resource)
	owner, err := materializer.PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	secrets := owner.Secrets()
	if !secrets.Valid() || secrets.Generation() != 9 {
		t.Fatalf("generation secrets = valid:%t generation:%d", secrets.Valid(), secrets.Generation())
	}

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "literal", raw: "literal-token", want: "literal-token"},
		{name: "external reference", raw: "$ENV://TOKEN", want: "resolved"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := secrets.Materialize(context.Background(), testScope(
				9, generation.DomainHTTP, "http-logger", resource,
				capability.SecretPluginConfig, "token",
			), test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := valuePlaintext(t, value); got != test.want {
				t.Fatalf("plaintext = %q, want %q", got, test.want)
			}
		})
	}
	if len(calls) != 1 || calls[0].Resource != resource || calls[0].Field != "token" {
		t.Fatalf("resolver scopes = %#v", calls)
	}
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if factory.openCalls != 1 || factory.generation != 9 || factory.set.DesiredRevision != 9 {
		t.Fatalf(
			"open = calls:%d generation:%d revision:%d",
			factory.openCalls,
			factory.generation,
			factory.set.DesiredRevision,
		)
	}
}

func TestGenerationSecretsRejectsScopeOutsidePublicationAndDeclaration(t *testing.T) {
	service, _ := testService(t, false)
	resolveCalls := 0
	materializer := NewMaterializer(service, &testGenerationResolverFactory{resolver: &testGenerationResolver{
		resolve: func(_ context.Context, _ Scope, _ string) (string, error) {
			resolveCalls++
			return "unexpected", nil
		},
	}})
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	_, set := testPublication(t, 9, generation.DomainHTTP, resource)
	owner, err := materializer.PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })

	tests := []struct {
		name  string
		scope Scope
		want  error
	}{
		{name: "zero scope", scope: Scope{}, want: ErrInvalidScope},
		{
			name: "other generation",
			scope: testScope(
				10,
				generation.DomainHTTP,
				"http-logger",
				resource,
				capability.SecretPluginConfig,
				"token",
			),
			want: ErrCapabilityScopeMismatch,
		},
		{
			name: "other resource",
			scope: testScope(
				9,
				generation.DomainHTTP,
				"http-logger",
				generation.ResourceKey{Kind: "routes", ID: "r2"},
				capability.SecretPluginConfig,
				"token",
			),
			want: ErrCapabilityScopeMismatch,
		},
		{
			name: "undeclared field",
			scope: testScope(
				9,
				generation.DomainHTTP,
				"http-logger",
				resource,
				capability.SecretPluginConfig,
				"password",
			),
			want: ErrInvalidScope,
		},
		{
			name:  "undeclared factory",
			scope: testScope(9, generation.DomainHTTP, "missing", resource, capability.SecretPluginConfig, "token"),
			want:  ErrInvalidScope,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := owner.Secrets().Materialize(context.Background(), test.scope, "$ENV://TOKEN")
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	if resolveCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolveCalls)
	}
}

func TestGenerationMaterializationCloseWaitsForUseAndRevokesView(t *testing.T) {
	service, _ := testService(t, false)
	entered := make(chan struct{})
	release := make(chan struct{})
	var closeContext context.Context
	materializer := NewMaterializer(service, &testGenerationResolverFactory{resolver: &testGenerationResolver{
		resolve: func(_ context.Context, _ Scope, _ string) (string, error) {
			close(entered)
			<-release
			return "resolved", nil
		},
		close: func(ctx context.Context) error {
			closeContext = ctx
			return nil
		},
	}})
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	_, set := testPublication(t, 9, generation.DomainHTTP, resource)
	owner, err := materializer.PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	secrets := owner.Secrets()
	scope := testScope(9, generation.DomainHTTP, "http-logger", resource, capability.SecretPluginConfig, "token")
	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := secrets.Materialize(context.Background(), scope, "$ENV://TOKEN")
		resolveDone <- resolveErr
	}()
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(canceled) }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight use: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if closeContext == nil || closeContext.Err() != nil {
		t.Fatalf("resolver close context = %v", closeContext)
	}
	if secrets.Valid() {
		t.Fatal("closed generation secrets remain valid")
	}
	if _, err := secrets.Materialize(
		context.Background(),
		scope,
		"$ENV://TOKEN",
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("materialize after close = %v", err)
	}
}

func TestGenerationMaterializationRedactsResolverFailures(t *testing.T) {
	service, _ := testService(t, false)
	materializer := NewMaterializer(service, &testGenerationResolverFactory{resolver: &testGenerationResolver{
		resolve: func(context.Context, Scope, string) (string, error) {
			return "", errors.New("backend leaked credential-value")
		},
		close: func(context.Context) error { return errors.New("close leaked credential-value") },
	}})
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	_, set := testPublication(t, 9, generation.DomainHTTP, resource)
	owner, err := materializer.PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Secrets().Materialize(context.Background(), testScope(
		9, generation.DomainHTTP, "http-logger", resource, capability.SecretPluginConfig, "token",
	), "$secret://credential-value")
	if !errors.Is(err, ErrCredentialUnavailable) || err.Error() != ErrCredentialUnavailable.Error() {
		t.Fatalf("materialize error = %v", err)
	}
	if err := owner.Close(
		context.Background(),
	); !errors.Is(err, ErrCredentialUnavailable) ||
		err.Error() != ErrCredentialUnavailable.Error() {
		t.Fatalf("close error = %v", err)
	}
}

func TestGenerationSecretsShareLimiterOnlyWithinOwner(t *testing.T) {
	service, _ := testService(t, false)
	_, set := testPublication(t, 9, generation.DomainHTTP, generation.ResourceKey{Kind: "routes", ID: "r1"})
	first, err := NewMaterializer(
		service,
		&testGenerationResolverFactory{},
	).PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMaterializer(
		service,
		&testGenerationResolverFactory{},
	).PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()); _ = second.Close(context.Background()) })
	if !first.Secrets().SameGeneration(first.Secrets()) || first.Secrets().SameGeneration(second.Secrets()) {
		t.Fatal("generation view identity is not owner-local")
	}
	limiter, err := first.Secrets().SharedLimiter("compile", 1)
	if err != nil || !limiter.Valid() {
		t.Fatalf("limiter = %#v/%v", limiter, err)
	}
	release, err := limiter.Acquire(context.Background(), make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestValueUse(t *testing.T) {
	if err := (Value{}).Use(nil); err == nil {
		t.Fatal("Value.Use(nil) returned nil")
	}
	value := newValue("secret")
	if got := valuePlaintext(t, value); got != "secret" || value.Digest() == ([32]byte{}) {
		t.Fatalf("value = %q digest=%x", got, value.Digest())
	}
}

func testCatalog(t *testing.T) *capability.SecretDeclarationCatalog {
	t.Helper()
	manifest := &capability.Manifest{Plugins: []capability.PluginCapability{{
		Name:      "test-secrets",
		Factories: []capability.Factory{{Key: "http-logger"}, {Key: "key-auth"}, {Key: "metadata-plugin"}},
		SecretDeclarations: []capability.SecretDeclaration{
			{Factory: "http-logger", Source: capability.SecretPluginConfig, Field: "token"},
			{Factory: "key-auth", Source: capability.SecretPluginConfig, Field: "key"},
			{Factory: "key-auth", Source: capability.SecretConsumerConfig, Field: "key"},
			{Factory: "metadata-plugin", Source: capability.SecretPluginMetadata, Field: "token"},
		},
	}}}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
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
			Domain:   domain,
			Revision: revision,
			Digest:   snapshot.Digest(),
			Snapshot: snapshot.SnapshotID(),
		},
		Snapshot:  snapshot,
		Closure:   []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{Key: key, Disposition: generation.DispositionPublished, Code: "ok"}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   snapshot.Digest(),
		RequiredDomains: []generation.Domain{domain},
	}
	return ticket, generation.PublicationSet{
		DesiredRevision: revision,
		Domains:         map[generation.Domain]generation.PublicationCandidate{domain: candidate},
	}
}

func testScope(
	revision uint64,
	domain generation.Domain,
	plugin string,
	resource generation.ResourceKey,
	source capability.SecretDeclarationSource,
	field string,
) Scope {
	return Scope{Generation: revision, Domain: domain, Plugin: plugin, Resource: resource, Source: source, Field: field}
}

func valuePlaintext(t *testing.T, value Value) string {
	t.Helper()
	var plaintext string
	if err := value.Use(func(value string) error { plaintext = value; return nil }); err != nil {
		t.Fatal(err)
	}
	return plaintext
}
