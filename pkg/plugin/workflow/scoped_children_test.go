package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	runtimeplugin "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/workflow"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type scopedWorkflowSecretCall struct {
	scope secret.Scope
	raw   string
}

type scopedWorkflowBroker struct {
	mu      sync.Mutex
	value   string
	wantRaw string
	err     error
	calls   []scopedWorkflowSecretCall
}

func (*scopedWorkflowBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*scopedWorkflowBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by scoped workflow tests")
}

func (broker *scopedWorkflowBroker) ResolveScoped(
	ctx context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, scopedWorkflowSecretCall{scope: scope, raw: raw})
	if broker.wantRaw != "" && raw != broker.wantRaw {
		return "", errors.New("scoped workflow broker received rewritten source")
	}
	if broker.err != nil {
		return "", broker.err
	}
	return broker.value, nil
}

func (*scopedWorkflowBroker) RevokeAttempt(context.Context, secret.AttemptID) error { return nil }

func (broker *scopedWorkflowBroker) snapshotCalls() []scopedWorkflowSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]scopedWorkflowSecretCall(nil), broker.calls...)
}

type scopedWorkflowHarness struct {
	revision   uint64
	routeID    string
	key        generation.ResourceKey
	attempt    secret.AttemptID
	capability secret.GenerationCapability
	scope      secret.Scope
	broker     *scopedWorkflowBroker
	close      func()
}

func newScopedWorkflowHarness(
	t *testing.T,
	revision uint64,
	routeID string,
	value string,
) scopedWorkflowHarness {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: routeID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{"workflow":{}}}`),
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
			Key: key, Disposition: generation.DispositionPublished, Code: "workflow-scoped-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
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
	broker := &scopedWorkflowBroker{value: value}
	registration, err := testutil.NewSecretMaterializer(broker, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	generationCapability, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	return scopedWorkflowHarness{
		revision:   revision,
		routeID:    routeID,
		key:        key,
		attempt:    registration.AttemptID(),
		capability: generationCapability,
		scope: secret.Scope{
			Generation: revision,
			Attempt:    registration.AttemptID(),
			Domain:     generation.DomainHTTP,
			Plugin:     "workflow",
			Resource:   key,
			Source:     capability.SecretPluginConfig,
		},
		broker: broker,
		close: func() {
			if closeErr := registration.Close(context.Background()); closeErr != nil {
				t.Fatalf("close scoped workflow registration: %v", closeErr)
			}
		},
	}
}

func prepareScopedWorkflow(
	t *testing.T,
	harness scopedWorkflowHarness,
	rawKey string,
) *workflow.Plugin {
	t.Helper()
	dependencies := base.Dependencies{Secrets: harness.capability}
	preparer, err := runtimeplugin.NewCompositeChildPreparer(
		dependencies,
		harness.attempt,
		runtimeplugin.ScopeRoute,
		runtimeplugin.ResourceProvenance{Kind: runtimeplugin.ResourceRoute, ID: harness.routeID},
	)
	if err != nil {
		t.Fatal(err)
	}
	dependencies.CompositeChildren = preparer
	p := &workflow.Plugin{}
	p.SetDependencies(dependencies)
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"actions": []any{[]any{"limit-count", map[string]any{
				"count": 2, "time_window": 60, "key": rawKey,
			}}},
		}},
	}, p.Config()); err != nil {
		t.Fatal(err)
	}
	p.SetResourceContext(resource.Route{ID: harness.routeID}, resource.Service{})
	return p
}

func TestMaterializeScopedSecretsBindsLimitCountToOuterAttemptAndRoute(t *testing.T) {
	harness := newScopedWorkflowHarness(t, 91, "route-workflow-x1", "tenant-one")
	defer harness.close()
	raw := "$ENV://WORKFLOW_LIMIT_KEY_X1"
	p := prepareScopedWorkflow(t, harness, raw)

	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), harness.scope, harness.capability, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	calls := harness.broker.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("broker calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.scope.Generation != harness.revision || got.scope.Attempt != harness.attempt ||
		got.scope.Domain != generation.DomainHTTP || got.scope.Resource != harness.key ||
		got.scope.Plugin != "limit-count" || got.scope.Field != "key" || got.raw != raw {
		t.Fatalf("child authority = %+v raw=%q, want outer route attempt with limit-count field", got.scope, got.raw)
	}
	encoded, err := json.Marshal(p.Config())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), raw) || strings.Contains(string(encoded), "tenant-one") {
		t.Fatalf("public workflow config retained credential material: %s", encoded)
	}
	p.Stop()
}

func TestScopedWorkflowGenerationOverlapKeepsChildrenAndSecretsIsolated(t *testing.T) {
	firstHarness := newScopedWorkflowHarness(t, 101, "route-overlap", "tenant-generation-one")
	defer firstHarness.close()
	secondHarness := newScopedWorkflowHarness(t, 102, "route-overlap", "tenant-generation-two")
	defer secondHarness.close()
	first := prepareScopedWorkflow(t, firstHarness, "$ENV://WORKFLOW_OVERLAP_ONE")
	second := prepareScopedWorkflow(t, secondHarness, "$ENV://WORKFLOW_OVERLAP_TWO")
	for _, candidate := range []struct {
		plugin  *workflow.Plugin
		harness scopedWorkflowHarness
	}{{first, firstHarness}, {second, secondHarness}} {
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), candidate.harness.scope, candidate.harness.capability, candidate.plugin,
		); err != nil {
			t.Fatal(err)
		}
		if err := candidate.plugin.PostInit(); err != nil {
			t.Fatal(err)
		}
	}
	firstConfig, _ := json.Marshal(first.Config())
	secondConfig, _ := json.Marshal(second.Config())
	if string(firstConfig) == string(secondConfig) {
		t.Fatalf("overlapping generations published identical child descriptors: %s", firstConfig)
	}
	first.Stop()
	secondAfterFirstRetired, _ := json.Marshal(second.Config())
	if string(secondAfterFirstRetired) != string(secondConfig) {
		t.Fatalf("retiring generation N changed N+1 config: before=%s after=%s", secondConfig, secondAfterFirstRetired)
	}
	firstCalls := firstHarness.broker.snapshotCalls()
	secondCalls := secondHarness.broker.snapshotCalls()
	if len(firstCalls) != 1 || len(secondCalls) != 1 ||
		firstCalls[0].scope.Attempt == secondCalls[0].scope.Attempt ||
		firstCalls[0].scope.Generation == secondCalls[0].scope.Generation {
		t.Fatalf("generation authority was not isolated: first=%+v second=%+v", firstCalls, secondCalls)
	}
	second.Stop()
}

func TestScopedWorkflowRematerializationUsesImmutableRawSource(t *testing.T) {
	harness := newScopedWorkflowHarness(t, 106, "route-rematerialize", "tenant-rematerialized")
	defer harness.close()
	raw := "$ENV://WORKFLOW_REMATERIALIZE_KEY"
	harness.broker.wantRaw = raw
	p := prepareScopedWorkflow(t, harness, raw)

	for attempt := range 2 {
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), harness.scope, harness.capability, p,
		); err != nil {
			t.Fatalf("materialization %d error = %v", attempt+1, err)
		}
		if err := p.PostInit(); err != nil {
			t.Fatalf("PostInit %d error = %v", attempt+1, err)
		}
		encoded, err := json.Marshal(p.Config())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), raw) || strings.Contains(string(encoded), "tenant-rematerialized") {
			t.Fatalf("public config %d retained credential material: %s", attempt+1, encoded)
		}
	}
	calls := harness.broker.snapshotCalls()
	if len(calls) != 2 {
		t.Fatalf("broker calls = %d, want one real resolution per child generation", len(calls))
	}
	for index, call := range calls {
		if call.raw != raw {
			t.Fatalf("broker call %d raw = %q, want immutable source %q", index+1, call.raw, raw)
		}
	}
	p.Stop()
}

func TestScopedWorkflowErrorsAndConfigHideCredentialMaterial(t *testing.T) {
	harness := newScopedWorkflowHarness(t, 111, "route-redaction", "unused")
	defer harness.close()
	poison := "WORKFLOW_VAULT_PATH_PASSWORD_POISON"
	harness.broker.err = errors.New(poison)
	p := prepareScopedWorkflow(t, harness, "$ENV://"+poison)

	err := base.MaterializeScopedPluginSecrets(
		context.Background(), harness.scope, harness.capability, p,
	)
	if err == nil || strings.Contains(err.Error(), poison) || strings.Contains(err.Error(), "$ENV://") {
		t.Fatalf("scoped workflow error = %v, want fixed redacted failure", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err = base.MaterializeScopedPluginSecrets(canceled, harness.scope, harness.capability, p)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scoped workflow error = %v, want context.Canceled", err)
	}
}
