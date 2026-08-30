package base

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type scopedSecretTestResolver struct {
	scopes []secret.Scope
	value  string
	err    error
	cancel context.CancelFunc
	wait   bool
}

func (resolver *scopedSecretTestResolver) ResolveScoped(
	ctx context.Context,
	scope secret.Scope,
	_ string,
) (string, error) {
	resolver.scopes = append(resolver.scopes, scope)
	if resolver.cancel != nil {
		resolver.cancel()
	}
	if resolver.wait {
		<-ctx.Done()
	}
	if resolver.err != nil {
		return "", resolver.err
	}
	return resolver.value, nil
}

type scopedSecretTestPlugin struct {
	config      any
	scopedErr   error
	scopedCalls int
}

func (plugin *scopedSecretTestPlugin) Config() any { return plugin.config }

func (plugin *scopedSecretTestPlugin) MaterializeScopedSecrets(
	_ context.Context,
	_ ScopedSecretAccess,
) error {
	plugin.scopedCalls++
	return plugin.scopedErr
}

type scopedCancellationPlugin struct {
	config struct {
		Key string `json:"key"`
	}
}

func (plugin *scopedCancellationPlugin) Config() any { return &plugin.config }

func (plugin *scopedCancellationPlugin) MaterializeScopedSecrets(
	ctx context.Context,
	access ScopedSecretAccess,
) error {
	_, err := access.Materialize(ctx, "key", plugin.config.Key)
	return err
}

func TestScopedSecretAccessBindsGenerationAndChildOnlyChangesFactory(t *testing.T) {
	secrets, scope, resolver := scopedSecretTestGeneration(t, 9)
	access := ScopedSecretAccess{scope: scope, secrets: secrets}
	if _, err := access.Materialize(context.Background(), "key", "$ENV://KEY"); err != nil {
		t.Fatal(err)
	}
	child, err := access.Child("jwe-decrypt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Materialize(context.Background(), "secret", "$ENV://SECRET"); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Child(""); err == nil {
		t.Fatal("Child(\"\") error = nil")
	}
	wantParent := scope
	wantParent.Field = "key"
	wantChild := scope
	wantChild.Plugin = "jwe-decrypt"
	wantChild.Field = "secret"
	if want := []secret.Scope{wantParent, wantChild}; !reflect.DeepEqual(resolver.scopes, want) {
		t.Fatalf("materialized scopes = %#v, want %#v", resolver.scopes, want)
	}
}

func TestScopedSecretAccessValidForExactGenerationViewOnly(t *testing.T) {
	secrets, scope, _ := scopedSecretTestGeneration(t, 9)
	access := ScopedSecretAccess{scope: scope, secrets: secrets}
	if !access.ValidFor(secrets) {
		t.Fatal("ValidFor(exact secrets) = false")
	}
	otherView, _, _ := scopedSecretTestGeneration(t, 9)
	otherGeneration, _, _ := scopedSecretTestGeneration(t, 10)
	for name, candidate := range map[string]secret.GenerationSecrets{
		"zero":             {},
		"other view":       otherView,
		"other generation": otherGeneration,
	} {
		t.Run(name, func(t *testing.T) {
			if access.ValidFor(candidate) {
				t.Fatalf("ValidFor(%s) = true", name)
			}
		})
	}
}

func TestMaterializeScopedPluginSecretsRejectsInvalidScopeBeforePlugin(t *testing.T) {
	secrets, validScope, _ := scopedSecretTestGeneration(t, 9)
	tests := []struct {
		name       string
		scope      secret.Scope
		capability secret.GenerationSecrets
		want       error
	}{
		{name: "invalid capability", scope: validScope, want: secret.ErrInvalidCapability},
		{
			name:       "generation mismatch",
			scope:      mutateScope(validScope, func(scope *secret.Scope) { scope.Generation++ }),
			capability: secrets,
			want:       secret.ErrCapabilityScopeMismatch,
		},
		{
			name:       "invalid domain",
			scope:      mutateScope(validScope, func(scope *secret.Scope) { scope.Domain = "other" }),
			capability: secrets,
			want:       secret.ErrInvalidScope,
		},
		{
			name:       "empty plugin",
			scope:      mutateScope(validScope, func(scope *secret.Scope) { scope.Plugin = "" }),
			capability: secrets,
			want:       secret.ErrInvalidScope,
		},
		{
			name:       "empty resource",
			scope:      mutateScope(validScope, func(scope *secret.Scope) { scope.Resource.ID = "" }),
			capability: secrets,
			want:       secret.ErrInvalidScope,
		},
		{
			name: "wrong source",
			scope: mutateScope(
				validScope,
				func(scope *secret.Scope) { scope.Source = capability.SecretPluginMetadata },
			),
			capability: secrets,
			want:       secret.ErrInvalidScope,
		},
		{
			name:       "nonempty field",
			scope:      mutateScope(validScope, func(scope *secret.Scope) { scope.Field = "key" }),
			capability: secrets,
			want:       secret.ErrInvalidScope,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &scopedSecretTestPlugin{}
			err := MaterializeScopedPluginSecrets(context.Background(), tt.scope, tt.capability, plugin)
			if !errors.Is(err, tt.want) {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v, want %v", err, tt.want)
			}
			if plugin.scopedCalls != 0 {
				t.Fatalf("scoped plugin calls = %d, want zero", plugin.scopedCalls)
			}
		})
	}
}

func TestScopedSecretHelpersRejectUnownedReferences(t *testing.T) {
	secrets, scope, _ := scopedSecretTestGeneration(t, 9)
	access := ScopedSecretAccess{scope: scope, secrets: secrets}
	configOnly := configOnlyPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$ENV://TOKEN"}}
	if err := MaterializeScopedCompositeChildSecrets(context.Background(), access, configOnly); err == nil ||
		!strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializeScopedCompositeChildSecrets() error = %v", err)
	}
	if err := MaterializeScopedPluginSecrets(context.Background(), scope, secrets, configOnly); err == nil ||
		!strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
}

func TestScopedSecretHelpersPreserveCancellationAndRedactErrors(t *testing.T) {
	secrets, scope, _ := scopedSecretTestGeneration(t, 9)
	access := ScopedSecretAccess{scope: scope, secrets: secrets}
	plugin := &scopedSecretTestPlugin{
		scopedErr: errors.Join(errors.New("failed to resolve $ENV://TOKEN"), context.Canceled),
	}
	if err := MaterializeScopedCompositeChildSecrets(context.Background(), access, plugin); err != context.Canceled {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
	plugin.scopedErr = errors.New("failed to resolve $ENV://TOKEN")
	err := MaterializeScopedPluginSecrets(context.Background(), scope, secrets, plugin)
	if err == nil || errors.Is(err, plugin.scopedErr) || strings.Contains(err.Error(), "$ENV://TOKEN") {
		t.Fatalf("redacted error = %v", err)
	}
}

func TestScopedSecretHelperRecoversResolverContextTermination(t *testing.T) {
	for _, tt := range []struct {
		name string
		wait bool
		want error
	}{
		{name: "cancel", want: context.Canceled},
		{name: "deadline", wait: true, want: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &scopedSecretTestResolver{err: errors.New("resolver normalized context"), wait: tt.wait}
			secrets, scope := scopedSecretTestGenerationWithResolver(t, 82, resolver)
			access := ScopedSecretAccess{scope: scope, secrets: secrets}
			plugin := &scopedCancellationPlugin{}
			plugin.config.Key = "$ENV://BROKER_CANCELLATION"
			var ctx context.Context
			if tt.wait {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
			} else {
				ctx, resolver.cancel = context.WithCancel(context.Background())
			}
			if err := MaterializeScopedCompositeChildSecrets(ctx, access, plugin); err != tt.want {
				t.Fatalf("MaterializeScopedCompositeChildSecrets() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func scopedSecretTestGeneration(
	t *testing.T,
	revision uint64,
) (secret.GenerationSecrets, secret.Scope, *scopedSecretTestResolver) {
	t.Helper()
	resolver := &scopedSecretTestResolver{value: "resolved"}
	secrets, scope := scopedSecretTestGenerationWithResolver(t, revision, resolver)
	return secrets, scope, resolver
}

func scopedSecretTestGenerationWithResolver(
	t *testing.T,
	revision uint64,
	resolver testutil.SecretResolver,
) (secret.GenerationSecrets, secret.Scope) {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	snapshot, err := generation.NewSnapshot(
		revision,
		[]generation.Resource{{Key: resource, Value: []byte(`{"plugins":{}}`)}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: {
				Artifact: generation.GenerationArtifact{
					Domain:   generation.DomainHTTP,
					Revision: revision,
					Digest:   snapshot.Digest(),
					Snapshot: snapshot.SnapshotID(),
				},
				Snapshot: snapshot,
				Closure:  []generation.ResourceKey{resource},
				Decisions: []generation.ResourceDecision{{
					Key: resource, Disposition: generation.DispositionPublished, Code: "test",
				}},
			},
		},
	}
	materialization, err := testutil.NewSecretMaterializer(resolver, catalog).
		PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = materialization.Close(context.Background()) })
	return materialization.Secrets(), secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     "key-auth",
		Resource:   resource,
		Source:     capability.SecretPluginConfig,
	}
}

func mutateScope(scope secret.Scope, mutate func(*secret.Scope)) secret.Scope {
	mutate(&scope)
	return scope
}

type configOnlyPlugin struct {
	config any
}

func (plugin configOnlyPlugin) Config() any { return plugin.config }
