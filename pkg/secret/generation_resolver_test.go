package secret

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
)

func TestGenerationSecretResolverImplementsFactory(t *testing.T) {
	var factory GenerationResolverFactory = newGenerationSecretResolverForTest(t)
	if _, ok := factory.(*GenerationSecretResolver); !ok {
		t.Fatalf("factory type = %T, want *GenerationSecretResolver", factory)
	}
}

func TestGenerationSecretResolverRejectsUnconfiguredEncryption(t *testing.T) {
	if _, err := NewGenerationSecretResolver(data_encryption.Service{}); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("constructor error = %v, want ErrInvalidCapability", err)
	}
}

func TestGenerationSecretResolverRequiresValidGenerationPublication(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	_, set := testPublication(t, 9, generation.DomainHTTP, resource)
	tests := []struct {
		name       string
		generation uint64
		set        generation.PublicationSet
	}{
		{name: "zero generation", generation: 0, set: set},
		{name: "revision mismatch", generation: 10, set: set},
		{name: "missing domains", generation: 9, set: generation.PublicationSet{DesiredRevision: 9}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := resolver.OpenGeneration(
				context.Background(),
				tt.generation,
				tt.set,
			); !errors.Is(
				err,
				ErrInvalidCapability,
			) {
				t.Fatalf("OpenGeneration() error = %v, want ErrInvalidCapability", err)
			}
		})
	}
}

func TestGenerationSecretResolverClonesPublicationAndEnforcesScope(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	_, set := testPublication(t, 9, generation.DomainHTTP, resource)
	opened, err := resolver.OpenGeneration(context.Background(), 9, set)
	if err != nil {
		t.Fatal(err)
	}
	view := opened.(*generationSecretView)
	delete(set.Domains, generation.DomainHTTP)

	t.Setenv("APISIX_GO_TEST_SECRET", "retained")
	scope := testScope(9, generation.DomainHTTP, "key-auth", resource, capability.SecretPluginConfig, "key")
	got, err := view.ResolveReference(context.Background(), scope, "$ENV://APISIX_GO_TEST_SECRET")
	if err != nil || got != "retained" {
		t.Fatalf("ResolveReference() = %q/%v, want retained", got, err)
	}

	mutations := map[string]func(*Scope){
		"generation": func(scope *Scope) { scope.Generation++ },
		"domain":     func(scope *Scope) { scope.Domain = generation.DomainStream },
		"resource":   func(scope *Scope) { scope.Resource.ID = "other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := scope
			mutate(&changed)
			if _, err := view.ResolveReference(
				context.Background(),
				changed,
				"$ENV://APISIX_GO_TEST_SECRET",
			); !errors.Is(
				err,
				ErrCapabilityScopeMismatch,
			) {
				t.Fatalf("ResolveReference() error = %v, want ErrCapabilityScopeMismatch", err)
			}
		})
	}
}

func TestGenerationSecretResolverResolvesEnvironmentReferences(t *testing.T) {
	view, scope := openGenerationResolverView(t, nil, nil)
	t.Setenv("APISIX_GO_TEST_PLAIN", "plain")
	t.Setenv("APISIX_GO_TEST_JSON", `{"outer":{"inner":"nested"}}`)
	tests := []struct {
		reference string
		want      string
	}{
		{reference: "$ENV://APISIX_GO_TEST_PLAIN", want: "plain"},
		{reference: "$env://APISIX_GO_TEST_JSON/outer/inner", want: "nested"},
	}
	for _, tt := range tests {
		got, err := view.ResolveReference(context.Background(), scope, tt.reference)
		if err != nil || got != tt.want {
			t.Fatalf("ResolveReference(%q) = %q/%v, want %q", tt.reference, got, err, tt.want)
		}
	}
	for _, reference := range []string{
		"$ENV://APISIX_GO_TEST_MISSING",
		"$ENV://APISIX_GO_TEST_JSON/outer/missing",
		"literal",
	} {
		if _, err := view.ResolveReference(
			context.Background(),
			scope,
			reference,
		); !errors.Is(
			err,
			ErrCredentialUnavailable,
		) {
			t.Fatalf("ResolveReference(%q) error = %v, want ErrCredentialUnavailable", reference, err)
		}
	}
}

func TestGenerationSecretResolverResolvesVaultAndCachesPerGeneration(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/kv/apisix/foo" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.Header.Get("X-Vault-Token"); got != "token" {
			t.Errorf("token = %q", got)
		}
		if got := request.Header.Get("X-Vault-Namespace"); got != "team" {
			t.Errorf("namespace = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"password":"resolved"}}}`))
	}))
	defer server.Close()
	config := vaultConfigBytesForResolver(t, server.URL, "token", "team")
	view, scope := openGenerationResolverView(t, config, nil)
	for range 2 {
		got, err := view.ResolveReference(context.Background(), scope, "$secret://vault/test1/foo/password")
		if err != nil || got != "resolved" {
			t.Fatalf("ResolveReference() = %q/%v, want resolved", got, err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("Vault requests = %d, want 1", got)
	}
}

func TestGenerationSecretResolverResolvesVaultTokenFromEnvironment(t *testing.T) {
	t.Setenv("APISIX_GO_TEST_VAULT_TOKEN", "resolved-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("X-Vault-Token"); got != "resolved-token" {
			t.Errorf("token = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":{"password":"resolved"}}`))
	}))
	defer server.Close()
	config := vaultConfigBytesForResolver(t, server.URL, "$ENV://APISIX_GO_TEST_VAULT_TOKEN", "")
	view, scope := openGenerationResolverView(t, config, nil)
	if got, err := view.ResolveReference(
		context.Background(),
		scope,
		"$secret://vault/test1/foo/password",
	); err != nil ||
		got != "resolved" {
		t.Fatalf("ResolveReference() = %q/%v, want resolved", got, err)
	}
}

func TestGenerationSecretResolverRejectsMalformedAndUnavailableVaultReferences(t *testing.T) {
	view, scope := openGenerationResolverView(t, nil, nil)
	for _, reference := range []string{
		"$secret://other/test/foo/password",
		"$secret://vault/test",
		"$secret://vault/test1/foo",
		"$secret://vault/test1/foo/",
	} {
		_, err := view.ResolveReference(context.Background(), scope, reference)
		if !errors.Is(err, ErrCredentialUnavailable) && !errors.Is(err, ErrCapabilityScopeMismatch) {
			t.Fatalf("ResolveReference(%q) error = %v", reference, err)
		}
	}
}

func TestGenerationSecretViewCloseWaitsForInflightAndZeroesOwnedBytes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"resolved"}}`))
	}))
	defer server.Close()
	config := vaultConfigBytesForResolver(t, server.URL, "token", "")
	view, scope := openGenerationResolverView(t, config, nil)
	retained := view.resources[generation.DomainHTTP][generation.ResourceKey{Kind: "secrets", ID: "vault/test1"}]

	resolveDone := make(chan error, 1)
	go func() {
		_, err := view.ResolveReference(context.Background(), scope, "$secret://vault/test1/foo/password")
		resolveDone <- err
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- view.Close(context.Background()) }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before in-flight resolve: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("ResolveReference() error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !allZeroResolverBytes(retained) {
		t.Fatal("retained configuration was not zeroed")
	}
	if _, err := view.ResolveReference(
		context.Background(),
		scope,
		"$ENV://ANY",
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("resolve after close error = %v, want ErrCredentialUnavailable", err)
	}
	if err := view.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestGenerationSecretResolverCloseClosesViewsAndPreventsNewOnes(t *testing.T) {
	transport := &resolverCloseTrackingTransport{}
	service, _ := testService(t, false)
	resolver, err := newGenerationSecretResolver(service, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	_, set := testPublication(t, 9, generation.DomainHTTP, resource)
	opened, err := resolver.OpenGeneration(context.Background(), 9, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !transport.closed.Load() {
		t.Fatal("idle connections were not closed")
	}
	scope := testScope(9, generation.DomainHTTP, "key-auth", resource, capability.SecretPluginConfig, "key")
	if _, err := opened.ResolveReference(
		context.Background(),
		scope,
		"$ENV://ANY",
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("active view error after resolver close = %v, want ErrCredentialUnavailable", err)
	}
	if _, err := resolver.OpenGeneration(context.Background(), 9, set); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("OpenGeneration() after close error = %v, want ErrCredentialUnavailable", err)
	}
	if err := resolver.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func newGenerationSecretResolverForTest(t *testing.T) *GenerationSecretResolver {
	t.Helper()
	service, _ := testService(t, false)
	resolver, err := NewGenerationSecretResolver(service)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resolver.Close(context.Background()) })
	return resolver
}

func openGenerationResolverView(
	t *testing.T,
	secretConfig []byte,
	client *http.Client,
) (*generationSecretView, Scope) {
	t.Helper()
	service, _ := testService(t, false)
	resolver, err := newGenerationSecretResolver(service, client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resolver.Close(context.Background()) })
	routeKey := generation.ResourceKey{Kind: "routes", ID: "route-1"}
	set := generationResolverPublication(t, 9, routeKey, secretConfig)
	opened, err := resolver.OpenGeneration(context.Background(), 9, set)
	if err != nil {
		t.Fatal(err)
	}
	return opened.(*generationSecretView), testScope(
		9, generation.DomainHTTP, "key-auth", routeKey, capability.SecretPluginConfig, "key",
	)
}

func generationResolverPublication(
	t *testing.T,
	revision uint64,
	routeKey generation.ResourceKey,
	secretConfig []byte,
) generation.PublicationSet {
	t.Helper()
	resources := []generation.Resource{{Key: routeKey, Value: []byte("route")}}
	closure := []generation.ResourceKey{routeKey}
	decisions := []generation.ResourceDecision{
		{Key: routeKey, Disposition: generation.DispositionPublished, Code: "ok"},
	}
	if secretConfig != nil {
		secretKey := generation.ResourceKey{Kind: "secrets", ID: "vault/test1"}
		resources = append(resources, generation.Resource{Key: secretKey, Value: secretConfig})
		closure = append(closure, secretKey)
		decisions = append(
			decisions,
			generation.ResourceDecision{Key: secretKey, Disposition: generation.DispositionPublished, Code: "ok"},
		)
	}
	snapshot, err := generation.NewSnapshot(revision, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain:   generation.DomainHTTP,
			Revision: revision,
			Digest:   snapshot.Digest(),
			Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot, Closure: closure, Decisions: decisions,
	}
	return generation.PublicationSet{
		DesiredRevision: revision,
		Domains:         map[generation.Domain]generation.PublicationCandidate{generation.DomainHTTP: candidate},
	}
}

func vaultConfigBytesForResolver(t *testing.T, uri, token, namespace string) []byte {
	t.Helper()
	encoded, err := json.Marshal(generationVaultSecretConfig{
		URI: uri, Prefix: "kv/apisix", Token: token, Namespace: namespace, Timeout: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func allZeroResolverBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

type resolverCloseTrackingTransport struct {
	closed atomic.Bool
}

func (transport *resolverCloseTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected request")
}

func (transport *resolverCloseTrackingTransport) CloseIdleConnections() {
	transport.closed.Store(true)
}
