package multi_auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type scopedMultiAuthBroker struct{}

func (scopedMultiAuthBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (scopedMultiAuthBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery not used")
}

func (scopedMultiAuthBroker) ResolveScoped(
	context.Context,
	secret.Scope,
	string,
) (string, error) {
	return "", errors.New("multi-auth route configs must not resolve compiler-discard or consumer credentials")
}

func (scopedMultiAuthBroker) RevokeAttempt(context.Context, secret.AttemptID) error { return nil }

type scopedOuterHarness struct {
	outer        plugin.Plugin
	registration secret.AttemptRegistration
	consumers    *runtime.ConsumerBindings
}

func (h scopedOuterHarness) close(t *testing.T) {
	t.Helper()
	if stopper, ok := h.outer.(interface{ Stop() }); ok {
		stopper.Stop()
	}
	if h.consumers != nil {
		h.consumers.Close()
	}
	if err := h.registration.Close(context.Background()); err != nil {
		t.Fatalf("close registration: %v", err)
	}
}

func newScopedOuterHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	authPlugins []any,
	consumers *runtime.ConsumerBindings,
) scopedOuterHarness {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{"multi-auth":{}}}`),
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
			Key: key, Disposition: generation.DispositionPublished, Code: "multi-auth-scoped-test",
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
	registration, err := testutil.NewSecretMaterializer(scopedMultiAuthBroker{}, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	metadataDocument, err := apisixjson.Marshal(map[string]any{"generation": revision})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := runtime.NewMetadataView(map[string][]byte{"multi-auth": metadataDocument})
	if err != nil {
		t.Fatal(err)
	}
	deps := base.Dependencies{
		Secrets: capabilityValue, Metadata: metadata, Consumers: consumers,
	}
	preparer, err := plugin.NewCompositeChildPreparer(
		deps,
		registration.AttemptID(),
		plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: resourceID},
	)
	if err != nil {
		t.Fatal(err)
	}
	deps.CompositeChildren = preparer
	outer := plugin.New("multi-auth", deps)
	if outer == nil {
		t.Fatal("multi-auth factory unavailable")
	}
	if err := outer.Init(); err != nil {
		t.Fatal(err)
	}
	rawConfig := map[string]any{"auth_plugins": authPlugins}
	compiled, err := util.CompileSchema(outer.GetSchema())
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(rawConfig); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(rawConfig, outer.Config()); err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     "multi-auth",
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, outer); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := outer.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return scopedOuterHarness{outer: outer, registration: registration, consumers: consumers}
}

func TestMaterializeScopedSecretsPreparesEveryAuthChildBeforePostInit(t *testing.T) {
	harness := newScopedOuterHarness(t, 81, "all-auth-factories", []any{
		map[string]any{"basic-auth": map[string]any{}},
		map[string]any{"key-auth": map[string]any{}},
		map[string]any{"jwt-auth": map[string]any{}},
		map[string]any{"hmac-auth": map[string]any{}},
		map[string]any{"ldap-auth": map[string]any{"base_dn": "dc=example,dc=test", "ldap_uri": "ldap://127.0.0.1"}},
		map[string]any{"jwe-decrypt": map[string]any{"header": "Authorization", "forward_header": "Authorization"}},
		map[string]any{"wolf-rbac": map[string]any{"server": "https://127.0.0.1"}},
	}, nil)
	defer harness.close(t)

	body, err := apisixjson.Marshal(harness.outer.Config())
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		AuthPlugins []map[string]map[string]any `json:"auth_plugins"`
	}
	if err := apisixjson.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.AuthPlugins) != 7 {
		t.Fatalf("prepared auth entries = %d, want 7", len(config.AuthPlugins))
	}
}

func TestScopedMultiAuthGenerationOverlapUsesOnlyItsOwnConsumerBindings(t *testing.T) {
	const credential = "generation-local-key"
	newBindings := func(username string) *runtime.ConsumerBindings {
		bindings, err := runtime.NewConsumerBindings(
			[]runtime.ConsumerRecord{{ID: username, Consumer: resource.Consumer{Username: username}}},
			nil,
			[]runtime.ConsumerCredentialBinding{{Plugin: "key-auth", Key: credential, ConsumerID: username}},
		)
		if err != nil {
			t.Fatal(err)
		}
		return bindings
	}
	config := []any{
		map[string]any{"basic-auth": map[string]any{}},
		map[string]any{"key-auth": map[string]any{"header": "apikey"}},
	}
	first := newScopedOuterHarness(t, 82, "same-route", config, newBindings("generation-n"))
	second := newScopedOuterHarness(t, 83, "same-route", config, newBindings("generation-n-plus-one"))
	defer first.close(t)
	defer second.close(t)

	assertConsumer := func(h scopedOuterHarness, want string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		request.Header.Set("apikey", credential)
		request = apisixctx.WithApisixVars(request, map[string]string{})
		request = apisixctx.WithRequestVars(request)
		response := httptest.NewRecorder()
		got := ""
		h.outer.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			state, ok := apisixctx.AuthenticationStateFrom(r)
			if ok {
				got = state.Consumer().Username
			}
		})).ServeHTTP(response, request)
		if got != want {
			t.Fatalf("authenticated consumer = %q, want %q; status=%d", got, want, response.Code)
		}
	}

	assertConsumer(first, "generation-n")
	assertConsumer(second, "generation-n-plus-one")
	if stopper, ok := first.outer.(interface{ Stop() }); ok {
		stopper.Stop()
	}
	assertConsumer(second, "generation-n-plus-one")
}
