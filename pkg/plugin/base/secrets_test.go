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
)

type scopedSecretTestRegistration struct {
	id     secret.AttemptID
	scopes []secret.Scope
}

func (registration *scopedSecretTestRegistration) AttemptID() secret.AttemptID {
	return registration.id
}

func (registration *scopedSecretTestRegistration) Materialize(
	_ context.Context,
	scope secret.Scope,
	_ string,
) (secret.Value, error) {
	registration.scopes = append(registration.scopes, scope)
	return secret.Value{}, nil
}

func (*scopedSecretTestRegistration) Close(context.Context) error { return nil }

func scopedSecretTestCapability(
	t *testing.T,
	generationNumber uint64,
	id secret.AttemptID,
) (secret.GenerationCapability, *scopedSecretTestRegistration) {
	t.Helper()
	registration := &scopedSecretTestRegistration{id: id}
	capabilityValue, err := secret.NewGenerationCapability(registration, generationNumber)
	if err != nil {
		t.Fatal(err)
	}
	return capabilityValue, registration
}

func scopedSecretTestScope(generationNumber uint64, id secret.AttemptID) secret.Scope {
	return secret.Scope{
		Generation: generationNumber,
		Attempt:    id,
		Domain:     generation.DomainHTTP,
		Plugin:     "key-auth",
		Resource:   generation.ResourceKey{Kind: "routes", ID: "r1"},
		Source:     capability.SecretPluginConfig,
	}
}

type scopedSecretTestPlugin struct {
	config      any
	scopedErr   error
	scopedCalls int
	legacyCalls int
}

type scopedBrokerCancellationMode uint8

const (
	scopedBrokerCancel scopedBrokerCancellationMode = iota
	scopedBrokerDeadline
)

type scopedCancelingBroker struct {
	mode   scopedBrokerCancellationMode
	cancel context.CancelFunc
	calls  int
}

func (*scopedCancelingBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*scopedCancelingBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by cancellation fixture")
}

func (broker *scopedCancelingBroker) ResolveScoped(
	ctx context.Context,
	_ secret.Scope,
	_ string,
) (string, error) {
	broker.calls++
	if broker.mode == scopedBrokerCancel {
		broker.cancel()
	} else {
		<-ctx.Done()
	}
	return "", errors.New("broker normalized cancellation")
}

func (*scopedCancelingBroker) RevokeAttempt(context.Context, secret.AttemptID) error { return nil }

type scopedCancellationPlugin struct {
	config struct {
		AuthHeader string `json:"auth_header"`
	}
}

func (plugin *scopedCancellationPlugin) Config() any { return &plugin.config }

func (plugin *scopedCancellationPlugin) MaterializeScopedSecrets(
	ctx context.Context,
	access ScopedSecretAccess,
) error {
	_, err := access.Materialize(ctx, "auth_header", plugin.config.AuthHeader)
	return err
}

func (plugin *scopedSecretTestPlugin) Config() any { return plugin.config }

func (plugin *scopedSecretTestPlugin) MaterializeSecrets() error {
	plugin.legacyCalls++
	return nil
}

func (plugin *scopedSecretTestPlugin) MaterializeScopedSecrets(
	_ context.Context,
	_ ScopedSecretAccess,
) error {
	plugin.scopedCalls++
	return plugin.scopedErr
}

func TestScopedSecretAccessBindsAuthorityAndChildOnlyChangesFactory(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, registration := scopedSecretTestCapability(t, 9, id)
	baseScope := scopedSecretTestScope(9, id)
	access := ScopedSecretAccess{scope: baseScope, capability: capabilityValue}

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

	wantParent := baseScope
	wantParent.Field = "key"
	wantChild := baseScope
	wantChild.Plugin = "jwe-decrypt"
	wantChild.Field = "secret"
	if got, want := registration.scopes, []secret.Scope{wantParent, wantChild}; !reflect.DeepEqual(got, want) {
		t.Fatalf("materialized scopes = %#v, want %#v", got, want)
	}
}

func TestCompositeChildPreparerScopedAccessValidForExactCapabilityOnly(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, _ := scopedSecretTestCapability(t, 9, id)
	access := ScopedSecretAccess{
		scope:      scopedSecretTestScope(9, id),
		capability: capabilityValue,
	}
	if !access.ValidFor(capabilityValue) {
		t.Fatal("ValidFor(exact capability) = false")
	}
	otherAttempt, _ := scopedSecretTestCapability(t, 9, secret.AttemptID{2})
	otherGeneration, _ := scopedSecretTestCapability(t, 10, id)
	for name, candidate := range map[string]secret.GenerationCapability{
		"zero":             {},
		"other attempt":    otherAttempt,
		"other generation": otherGeneration,
	} {
		t.Run(name, func(t *testing.T) {
			if access.ValidFor(candidate) {
				t.Fatalf("ValidFor(%s) = true", name)
			}
		})
	}
	if (ScopedSecretAccess{}).ValidFor(capabilityValue) {
		t.Fatal("zero access ValidFor(valid capability) = true")
	}
}

func TestMaterializeScopedPluginSecretsRejectsInvalidAuthorityBeforePlugin(t *testing.T) {
	id := secret.AttemptID{1}
	validCapability, _ := scopedSecretTestCapability(t, 9, id)
	validScope := scopedSecretTestScope(9, id)
	tests := []struct {
		name       string
		scope      secret.Scope
		capability secret.GenerationCapability
		want       error
	}{
		{name: "invalid capability", scope: validScope, want: secret.ErrInvalidCapability},
		{name: "generation mismatch", scope: func() secret.Scope {
			scope := validScope
			scope.Generation++
			return scope
		}(), capability: validCapability, want: secret.ErrCapabilityScopeMismatch},
		{name: "attempt mismatch", scope: func() secret.Scope {
			scope := validScope
			scope.Attempt[0]++
			return scope
		}(), capability: validCapability, want: secret.ErrCapabilityScopeMismatch},
		{name: "invalid domain", scope: func() secret.Scope {
			scope := validScope
			scope.Domain = generation.Domain("other")
			return scope
		}(), capability: validCapability, want: secret.ErrInvalidScope},
		{name: "empty plugin", scope: func() secret.Scope {
			scope := validScope
			scope.Plugin = ""
			return scope
		}(), capability: validCapability, want: secret.ErrInvalidScope},
		{name: "empty resource", scope: func() secret.Scope {
			scope := validScope
			scope.Resource.ID = ""
			return scope
		}(), capability: validCapability, want: secret.ErrInvalidScope},
		{name: "wrong source", scope: func() secret.Scope {
			scope := validScope
			scope.Source = capability.SecretPluginMetadata
			return scope
		}(), capability: validCapability, want: secret.ErrInvalidScope},
		{name: "nonempty field", scope: func() secret.Scope {
			scope := validScope
			scope.Field = "key"
			return scope
		}(), capability: validCapability, want: secret.ErrInvalidScope},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := &scopedSecretTestPlugin{}
			err := MaterializeScopedPluginSecrets(context.Background(), test.scope, test.capability, plugin)
			if !errors.Is(err, test.want) {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v, want %v", err, test.want)
			}
			if plugin.scopedCalls != 0 || plugin.legacyCalls != 0 {
				t.Fatalf("plugin calls = scoped:%d legacy:%d, want zero", plugin.scopedCalls, plugin.legacyCalls)
			}
		})
	}
}

func TestScopedAndLegacySecretWrappersDoNotCrossCall(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, _ := scopedSecretTestCapability(t, 9, id)
	plugin := &scopedSecretTestPlugin{}
	if err := MaterializePluginSecrets(plugin); err != nil {
		t.Fatal(err)
	}
	if plugin.legacyCalls != 1 || plugin.scopedCalls != 0 {
		t.Fatalf("legacy wrapper calls = legacy:%d scoped:%d", plugin.legacyCalls, plugin.scopedCalls)
	}
	if err := MaterializeScopedPluginSecrets(
		context.Background(),
		scopedSecretTestScope(9, id),
		capabilityValue,
		plugin,
	); err != nil {
		t.Fatal(err)
	}
	if plugin.legacyCalls != 1 || plugin.scopedCalls != 1 {
		t.Fatalf("scoped wrapper calls = legacy:%d scoped:%d", plugin.legacyCalls, plugin.scopedCalls)
	}
}

func TestCompositeChildPreparerScopedSecretHelperNeverFallsBackToLegacy(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, _ := scopedSecretTestCapability(t, 9, id)
	access := ScopedSecretAccess{
		scope:      scopedSecretTestScope(9, id),
		capability: capabilityValue,
	}
	plugin := &secretMaterializationTestPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$ENV://COMPOSITE_TOKEN"}}

	err := MaterializeScopedCompositeChildSecrets(context.Background(), access, plugin)
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializeScopedCompositeChildSecrets() error = %v, want unresolved reference", err)
	}
	if plugin.materializeCalls != 0 {
		t.Fatalf("legacy materializer calls = %d, want zero", plugin.materializeCalls)
	}
}

func TestCompositeChildPreparerScopedSecretHelperPreservesCancellation(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, _ := scopedSecretTestCapability(t, 9, id)
	access := ScopedSecretAccess{
		scope:      scopedSecretTestScope(9, id),
		capability: capabilityValue,
	}
	plugin := &scopedSecretTestPlugin{scopedErr: errors.Join(errors.New("poison must stay private"), context.Canceled)}

	err := MaterializeScopedCompositeChildSecrets(context.Background(), access, plugin)
	if err != context.Canceled {
		t.Fatalf("MaterializeScopedCompositeChildSecrets() error = %v, want context.Canceled", err)
	}
	if plugin.legacyCalls != 0 || plugin.scopedCalls != 1 {
		t.Fatalf("materializer calls = scoped:%d legacy:%d, want 1/0", plugin.scopedCalls, plugin.legacyCalls)
	}
}

func TestCompositeChildPreparerScopedSecretHelperRecoversBrokerContextTermination(t *testing.T) {
	for _, test := range []struct {
		name string
		mode scopedBrokerCancellationMode
		want error
	}{
		{name: "cancel", mode: scopedBrokerCancel, want: context.Canceled},
		{name: "deadline", mode: scopedBrokerDeadline, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			resource := generation.ResourceKey{Kind: "routes", ID: "broker-" + test.name}
			snapshot, err := generation.NewSnapshot(82, []generation.Resource{{
				Key: resource, Value: []byte(`{"plugins":{"http-logger":{}}}`),
			}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ticket := generation.ApplyTicket{
				DesiredRevision: 82,
				RequiredDomains: []generation.Domain{generation.DomainHTTP},
			}
			set := generation.PublicationSet{
				DesiredRevision: 82,
				Domains: map[generation.Domain]generation.PublicationCandidate{
					generation.DomainHTTP: {
						Artifact: generation.GenerationArtifact{
							Domain: generation.DomainHTTP, Revision: 82,
							Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
						},
						Snapshot: snapshot,
						Closure:  []generation.ResourceKey{resource},
						Decisions: []generation.ResourceDecision{{
							Key: resource, Disposition: generation.DispositionPublished, Code: "cancel-test",
						}},
					},
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
			broker := &scopedCancelingBroker{mode: test.mode}
			registration, err := secret.NewScopedMaterializer(broker, catalog).
				RegisterCandidate(context.Background(), ticket, set)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = registration.Close(context.Background()) }()
			capabilityValue, err := secret.NewGenerationCapability(registration, 82)
			if err != nil {
				t.Fatal(err)
			}
			access := ScopedSecretAccess{
				scope: secret.Scope{
					Generation: 82,
					Attempt:    registration.AttemptID(),
					Domain:     generation.DomainHTTP,
					Plugin:     "http-logger",
					Resource:   resource,
					Source:     capability.SecretPluginConfig,
				},
				capability: capabilityValue,
			}
			plugin := &scopedCancellationPlugin{}
			plugin.config.AuthHeader = "$ENV://BROKER_CANCELLATION"
			var ctx context.Context
			if test.mode == scopedBrokerCancel {
				ctx, broker.cancel = context.WithCancel(context.Background())
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
			}
			err = MaterializeScopedCompositeChildSecrets(ctx, access, plugin)
			if err != test.want {
				t.Fatalf("MaterializeScopedCompositeChildSecrets() error = %v, want %v", err, test.want)
			}
			if broker.calls != 1 {
				t.Fatalf("broker calls = %d, want 1", broker.calls)
			}
		})
	}
}

func TestMaterializeScopedPluginSecretsDoesNotFallbackToLegacyOwner(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, _ := scopedSecretTestCapability(t, 9, id)
	plugin := &secretMaterializationTestPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$ENV://TOKEN"}}
	err := MaterializeScopedPluginSecrets(
		context.Background(),
		scopedSecretTestScope(9, id),
		capabilityValue,
		plugin,
	)
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v, want unresolved reference", err)
	}
	if plugin.materializeCalls != 0 {
		t.Fatalf("legacy MaterializeSecrets() calls = %d, want zero", plugin.materializeCalls)
	}
}

func TestMaterializeScopedPluginSecretsScansAfterScopedOwner(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, _ := scopedSecretTestCapability(t, 9, id)
	plugin := &scopedSecretTestPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$secret://vault/path"}}
	err := MaterializeScopedPluginSecrets(
		context.Background(),
		scopedSecretTestScope(9, id),
		capabilityValue,
		plugin,
	)
	if err == nil || !strings.Contains(err.Error(), "remains unmaterialized") {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v, want post-owner scan failure", err)
	}
	if strings.Contains(err.Error(), "$secret://vault/path") {
		t.Fatalf("MaterializeScopedPluginSecrets() error exposed reference: %v", err)
	}
	if plugin.scopedCalls != 1 || plugin.legacyCalls != 0 {
		t.Fatalf("plugin calls = scoped:%d legacy:%d", plugin.scopedCalls, plugin.legacyCalls)
	}
}

func TestMaterializeScopedPluginSecretsRedactsOwnerError(t *testing.T) {
	id := secret.AttemptID{1}
	capabilityValue, _ := scopedSecretTestCapability(t, 9, id)
	original := errors.New("failed to resolve $ENV://TOKEN")
	plugin := &scopedSecretTestPlugin{scopedErr: original}
	err := MaterializeScopedPluginSecrets(
		context.Background(),
		scopedSecretTestScope(9, id),
		capabilityValue,
		plugin,
	)
	if err == nil || errors.Is(err, original) || errors.Unwrap(err) != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v, want redacted error without original chain", err)
	}
	if strings.Contains(err.Error(), "$ENV://TOKEN") {
		t.Fatalf("MaterializeScopedPluginSecrets() error exposed reference: %v", err)
	}
}

type secretMaterializationTestPlugin struct {
	config           any
	materialize      func()
	materializeErr   error
	materializeCalls int
}

func (p *secretMaterializationTestPlugin) Config() any {
	return p.config
}

func (p *secretMaterializationTestPlugin) MaterializeSecrets() error {
	p.materializeCalls++
	if p.materialize != nil {
		p.materialize()
	}
	return p.materializeErr
}

type secretMaterializationTestConfig struct {
	Token  string `json:"token"`
	Nested struct {
		Credentials []map[string]string `json:"credentials"`
	} `json:"nested"`
}

func TestSecretMaterializationAllowsLiteralConfigWithoutOwner(t *testing.T) {
	p := &secretMaterializationTestPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "literal-token"}}

	if err := MaterializePluginSecrets(configOnlyPlugin{config: p.config}); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v, want literal config accepted", err)
	}
}

func TestSecretMaterializationRejectsUnownedReferenceAtNestedPath(t *testing.T) {
	config := secretMaterializationTestConfig{}
	config.Nested.Credentials = []map[string]string{{"token": "$ENV://TOKEN"}}

	err := MaterializePluginSecrets(configOnlyPlugin{config: config})
	if err == nil {
		t.Fatal("MaterializePluginSecrets() error = nil, want unowned secret reference")
	}
	if !strings.Contains(err.Error(), "nested.credentials[0][0]") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want bounded nested path", err)
	}
	if strings.Contains(err.Error(), "$ENV://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationRejectsLowercaseEnvironmentReferenceWithoutOwner(t *testing.T) {
	err := MaterializePluginSecrets(configOnlyPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$env://TOKEN"}})
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want lowercase environment reference rejected", err)
	}
	if strings.Contains(err.Error(), "$env://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationRejectsMixedCaseEnvironmentReferenceWithoutOwner(t *testing.T) {
	err := MaterializePluginSecrets(configOnlyPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$eNv://TOKEN"}})
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want mixed-case environment reference rejected", err)
	}
	if strings.Contains(err.Error(), "$eNv://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationAcceptsMixedCaseEnvironmentDescriptorWithoutOwner(t *testing.T) {
	err := MaterializePluginSecrets(configOnlyPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$eNv://TOKEN#sha256:fingerprint"}})
	if err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v, want mixed-case descriptor accepted", err)
	}
}

func TestSecretMaterializationDepthExhaustionFailsClosed(t *testing.T) {
	config := nestedSecretScanValue(32, "literal")
	err := MaterializePluginSecrets(configOnlyPlugin{config: config})
	if err == nil || !strings.Contains(err.Error(), "secret reference scan depth exceeded") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want bounded depth-exhaustion error", err)
	}
	if strings.Contains(err.Error(), "literal") {
		t.Fatalf("MaterializePluginSecrets() error exposed config value: %v", err)
	}
}

func TestSecretMaterializationFindsReferenceAtMaximumInspectableDepth(t *testing.T) {
	config := nestedSecretScanValue(31, "$ENV://TOKEN")
	err := MaterializePluginSecrets(configOnlyPlugin{config: config})
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want reference at maximum inspectable depth rejected", err)
	}
	if strings.Contains(err.Error(), "$ENV://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationInvokesOwnerOnceAndAcceptsDescriptor(t *testing.T) {
	config := &secretMaterializationTestConfig{Token: "$ENV://TOKEN"}
	p := &secretMaterializationTestPlugin{config: config}
	p.materialize = func() {
		config.Token = "$ENV://TOKEN#sha256:fingerprint"
	}

	if err := MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	if p.materializeCalls != 1 {
		t.Fatalf("MaterializeSecrets() calls = %d, want 1", p.materializeCalls)
	}
}

func TestSecretMaterializationRedactsOwnerError(t *testing.T) {
	want := errors.New("failed to resolve $secret://vault/token")
	p := &secretMaterializationTestPlugin{materializeErr: want}

	err := MaterializePluginSecrets(p)
	if errors.Is(err, want) {
		t.Fatalf("MaterializePluginSecrets() error = %v, want no original error chain", err)
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("errors.Unwrap(MaterializePluginSecrets()) = %v, want no error chain", unwrapped)
	}
	if strings.Contains(err.Error(), "$secret://vault/token") {
		t.Fatalf("MaterializePluginSecrets() error exposed secret reference: %v", err)
	}
}

func TestSecretMaterializationDoesNotExposeMapKeyInErrorPath(t *testing.T) {
	const sensitiveMapKey = "credential-$secret://vault/token"
	err := MaterializePluginSecrets(configOnlyPlugin{config: map[string]any{
		sensitiveMapKey: "$ENV://TOKEN",
	}})
	if err == nil {
		t.Fatal("MaterializePluginSecrets() error = nil, want unowned secret reference")
	}
	if strings.Contains(err.Error(), sensitiveMapKey) {
		t.Fatalf("MaterializePluginSecrets() error exposed map key: %v", err)
	}
	if !strings.Contains(err.Error(), "[0]") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want indexed map path", err)
	}
}

type configOnlyPlugin struct {
	config any
}

func (p configOnlyPlugin) Config() any {
	return p.config
}

func nestedSecretScanValue(depth int, value any) any {
	for range depth {
		value = []any{value}
	}
	return value
}
