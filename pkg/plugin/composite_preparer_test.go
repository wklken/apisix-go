package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

const compositeSentinelSchema = `{
	"type":"object",
	"properties":{
		"auth_header":{"type":"string"},
		"nested":{"type":"object"}
	},
	"required":["auth_header"]
}`

type compositeSentinelConfig struct {
	AuthHeader string         `json:"auth_header"`
	Nested     map[string]any `json:"nested,omitempty"`
}

type compositeSentinelPlugin struct {
	base.BasePlugin

	config           compositeSentinelConfig
	trace            *[]string
	schemaOverride   string
	nilConfig        bool
	initErr          error
	materializeErr   error
	postInitErr      error
	initCalls        int
	materializeCalls int
	postInitCalls    int
	stopCalls        int
}

func (p *compositeSentinelPlugin) SetDependencies(deps base.Dependencies) {
	if p.trace != nil {
		*p.trace = append(*p.trace, "dependencies")
	}
	p.BasePlugin.SetDependencies(deps)
}

func (p *compositeSentinelPlugin) Init() error {
	if p.trace != nil {
		*p.trace = append(*p.trace, "init")
	}
	p.initCalls++
	p.Name = "http-logger"
	p.Priority = 410
	p.Schema = compositeSentinelSchema
	if p.schemaOverride != "" {
		p.Schema = p.schemaOverride
	}
	return p.initErr
}

func (p *compositeSentinelPlugin) PostInit() error {
	if p.trace != nil {
		*p.trace = append(*p.trace, "post-init")
	}
	p.postInitCalls++
	return p.postInitErr
}

func (p *compositeSentinelPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *compositeSentinelPlugin) Config() any {
	if p.trace != nil {
		*p.trace = append(*p.trace, "config")
	}
	if p.nilConfig {
		return nil
	}
	return &p.config
}

func (p *compositeSentinelPlugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	if p.trace != nil {
		*p.trace = append(*p.trace, "materialize")
	}
	p.materializeCalls++
	if p.materializeErr != nil {
		return p.materializeErr
	}
	value, err := access.Materialize(ctx, "auth_header", p.config.AuthHeader)
	if err != nil {
		return err
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return err
	}
	p.config.AuthHeader = descriptor.String()
	return nil
}

func (p *compositeSentinelPlugin) Stop() {
	if p.trace != nil {
		*p.trace = append(*p.trace, "stop")
	}
	p.stopCalls++
}

type compositeOuterMaterializer struct {
	preparer base.CompositeChildPreparer
	spec     base.CompositeChildSpec
	prepared base.PreparedCompositeChild
}

type compositeNeverPreparer struct{}

func (compositeNeverPreparer) Prepare(
	context.Context,
	base.ScopedSecretAccess,
	base.CompositeChildSpec,
) (base.PreparedCompositeChild, error) {
	return nil, errors.New("recursive composite child preparation is forbidden")
}

func (outer *compositeOuterMaterializer) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	prepared, err := outer.preparer.Prepare(ctx, access, outer.spec)
	outer.prepared = prepared
	return err
}

type compositeSecretCall struct {
	scope secret.Scope
	raw   string
}

type compositeSecretBroker struct {
	mu    sync.Mutex
	value string
	err   error
	calls []compositeSecretCall
}

func (broker *compositeSecretBroker) ResolveScoped(
	ctx context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, compositeSecretCall{scope: scope, raw: raw})
	if broker.err != nil {
		return "", broker.err
	}
	return broker.value, nil
}

func (broker *compositeSecretBroker) snapshotCalls() []compositeSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]compositeSecretCall(nil), broker.calls...)
}

type compositeConsumerLookup struct{}

func (compositeConsumerLookup) ConsumerByPluginKey(string, string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (compositeConsumerLookup) ConsumerByID(string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (compositeConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

type compositeSecretHarness struct {
	capability secret.GenerationSecrets
	scope      secret.Scope
	attempt    uint64
	broker     *compositeSecretBroker
	close      func()
}

func newCompositeSecretHarness(t *testing.T, revision uint64, resourceID string) compositeSecretHarness {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
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
			Key: key, Disposition: generation.DispositionPublished, Code: "composite-test",
		}},
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
	broker := &compositeSecretBroker{value: "resolved-secret"}
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	return compositeSecretHarness{
		capability: secrets,
		scope: secret.Scope{
			Generation: revision,
			Domain:     generation.DomainHTTP,
			Plugin:     "workflow",
			Resource:   key,
			Source:     capability.SecretPluginConfig,
		},
		attempt: secrets.Generation(),
		broker:  broker,
		close: func() {
			if closeErr := materialization.Close(context.Background()); closeErr != nil {
				t.Fatalf("close composite secret registration: %v", closeErr)
			}
		},
	}
}

func replaceCompositeFactory(t *testing.T, factory string, create func() Plugin) {
	t.Helper()
	original, ok := pluginRegistry[factory]
	if !ok {
		t.Fatalf("fixture factory %q is not registered", factory)
	}
	pluginRegistry[factory] = create
	t.Cleanup(func() { pluginRegistry[factory] = original })
}

func requireCompositeTraceOrder(t *testing.T, trace []string, ordered ...string) {
	t.Helper()
	position := 0
	for _, event := range trace {
		if position < len(ordered) && event == ordered[position] {
			position++
		}
	}
	if position != len(ordered) {
		t.Fatalf("lifecycle trace = %#v, missing ordered subsequence %#v", trace, ordered)
	}
}

func prepareCompositeChild(
	ctx context.Context,
	harness compositeSecretHarness,
	preparer base.CompositeChildPreparer,
	spec base.CompositeChildSpec,
) (base.PreparedCompositeChild, error) {
	outer := &compositeOuterMaterializer{preparer: preparer, spec: spec}
	err := base.MaterializeScopedPluginSecrets(ctx, harness.scope, harness.capability, outer)
	return outer.prepared, err
}

func TestCompositeChildPreparerPreservesOuterAuthorityAndDependencies(t *testing.T) {
	harness := newCompositeSecretHarness(t, 71, "route-x1")
	defer harness.close()

	var child *compositeSentinelPlugin
	trace := []string{"construct"}
	replaceCompositeFactory(t, "http-logger", func() Plugin {
		child = &compositeSentinelPlugin{trace: &trace}
		return child
	})
	metadata, err := runtime.NewMetadataView(map[string][]byte{"http-logger": []byte(`{"marker":"metadata-x1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	staticConfig := &config.EffectiveConfig{}
	dataEncryption := data_encryption.NewResolver(false, nil)
	lookup := compositeConsumerLookup{}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	taskOwner, err := runtime.NewTaskOwner(tasks, "plugin/test/composite", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	outerChildren := compositeNeverPreparer{}
	deps := base.Dependencies{
		Config:            staticConfig,
		DataEncryption:    dataEncryption,
		Secrets:           harness.capability,
		Metadata:          metadata,
		Consumers:         lookup,
		Tasks:             taskOwner,
		CompositeChildren: outerChildren,
	}
	effectiveOwner := ResourceProvenance{Kind: ResourceService, ID: "service-effective"}
	preparer, err := NewCompositeChildPreparer(deps, harness.attempt, ScopeRoute, effectiveOwner)
	if err != nil {
		t.Fatal(err)
	}
	raw := "$ENV://COMPOSITE_AUTH_HEADER"
	original := map[string]any{
		"auth_header": raw,
		"nested":      map[string]any{"marker": "original"},
	}
	prepared, err := prepareCompositeChild(context.Background(), harness, preparer, base.CompositeChildSpec{
		Factory: "http-logger", Config: original, Position: "workflow/rule/0/action/0",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Factory() != "http-logger" || prepared.Instance() != child {
		t.Fatalf("prepared child = %q/%T, want exact http-logger sentinel", prepared.Factory(), prepared.Instance())
	}
	binding := prepared.(*preparedCompositeChild).binding
	if binding.Scope != ScopeRoute || binding.Provenance != effectiveOwner ||
		binding.InstanceKey.Generation != harness.attempt {
		t.Fatalf("binding identity = %+v, want effective route/service and outer attempt", binding)
	}
	if child.StaticConfig() != staticConfig || !child.DataEncryption().Configured() ||
		child.TaskOwner() != taskOwner || child.ConsumerLookup() != lookup {
		t.Fatal("child did not receive the immutable outer dependency bundle")
	}
	if child.CompositeChildPreparer() != nil {
		t.Fatal("leaf child retained recursive CompositeChildren dependency")
	}
	if !child.ScopedSecrets().Valid() || child.ScopedSecrets().Generation() != harness.attempt {
		t.Fatal("child did not retain the outer generation secrets")
	}
	var metadataDocument map[string]any
	found, err := child.MetadataView().Decode("http-logger", &metadataDocument)
	if err != nil || !found || metadataDocument["marker"] != "metadata-x1" {
		t.Fatalf("child metadata view = %#v/%t/%v", metadataDocument, found, err)
	}
	calls := harness.broker.snapshotCalls()
	if len(calls) != 1 {
		t.Fatalf("secret calls = %d, want 1", len(calls))
	}
	gotScope := calls[0].scope
	if gotScope.Generation != harness.scope.Generation ||
		gotScope.Domain != generation.DomainHTTP || gotScope.Resource != harness.scope.Resource ||
		gotScope.Source != capability.SecretPluginConfig || gotScope.Plugin != "http-logger" ||
		gotScope.Field != "auth_header" || calls[0].raw != raw {
		t.Fatalf(
			"child secret authority = %+v raw=%q, want source route authority with child factory",
			gotScope,
			calls[0].raw,
		)
	}
	if original["auth_header"] != raw || original["nested"].(map[string]any)["marker"] != "original" {
		t.Fatalf("Prepare mutated caller config: %#v", original)
	}
	if strings.Contains(child.config.AuthHeader, raw) ||
		!strings.HasPrefix(child.config.AuthHeader, "plugin_config#sha256:") {
		t.Fatalf("public child config retained credential material: %q", child.config.AuthHeader)
	}
	requireCompositeTraceOrder(
		t,
		trace,
		"construct", "dependencies", "init", "config", "materialize", "post-init", "config",
	)
	var closeCalls sync.WaitGroup
	for range 8 {
		closeCalls.Go(prepared.Close)
	}
	closeCalls.Wait()
	if child.stopCalls != 1 {
		t.Fatalf("Close() stop calls = %d, want 1", child.stopCalls)
	}
}

func TestCompositeChildPreparerUsesExactRegistryFactoryForSameType(t *testing.T) {
	harness := newCompositeSecretHarness(t, 72, "route-serverless")
	defer harness.close()
	preparer, err := NewCompositeChildPreparer(
		base.Dependencies{Secrets: harness.capability},
		harness.attempt,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var children []base.PreparedCompositeChild
	for index, factory := range []string{"serverless-pre-function", "serverless-post-function"} {
		child, childErr := prepareCompositeChild(context.Background(), harness, preparer, base.CompositeChildSpec{
			Factory:  factory,
			Config:   map[string]any{"functions": []any{"return function() end"}},
			Position: fmt.Sprintf("workflow/rule/0/action/%d", index),
		})
		if childErr != nil {
			t.Fatalf("Prepare(%s) error = %v", factory, childErr)
		}
		children = append(children, child)
		t.Cleanup(child.Close)
		binding := child.(*preparedCompositeChild).binding
		if child.Factory() != factory || binding.Descriptor.Factory != factory ||
			binding.InstanceKey.Factory != factory {
			t.Fatalf("Prepare(%s) lost exact registry identity: %+v", factory, binding)
		}
	}
	if reflect.TypeOf(children[0].Instance()) != reflect.TypeOf(children[1].Instance()) {
		t.Fatal("serverless regression fixture no longer shares one concrete Go type")
	}
}

func TestCompositeChildPreparerSeparatesSiblingPositionAndAttemptKeys(t *testing.T) {
	firstHarness := newCompositeSecretHarness(t, 73, "route-equal")
	defer firstHarness.close()
	secondHarness := newCompositeSecretHarness(t, 74, "route-equal")
	defer secondHarness.close()
	owner := ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"}
	firstPreparer, err := NewCompositeChildPreparer(
		base.Dependencies{Secrets: firstHarness.capability}, firstHarness.attempt, ScopeRoute, owner,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondPreparer, err := NewCompositeChildPreparer(
		base.Dependencies{Secrets: secondHarness.capability}, secondHarness.attempt, ScopeRoute, owner,
	)
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"functions": []any{"return function() end"}}
	first, err := prepareCompositeChild(context.Background(), firstHarness, firstPreparer, base.CompositeChildSpec{
		Factory: "serverless-pre-function", Config: config, Position: "workflow/rule/0/action/0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	sibling, err := prepareCompositeChild(context.Background(), firstHarness, firstPreparer, base.CompositeChildSpec{
		Factory: "serverless-pre-function", Config: config, Position: "workflow/rule/0/action/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Close()
	nextAttempt, err := prepareCompositeChild(
		context.Background(),
		secondHarness,
		secondPreparer,
		base.CompositeChildSpec{
			Factory: "serverless-pre-function", Config: config, Position: "workflow/rule/0/action/0",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer nextAttempt.Close()
	firstKey := first.(*preparedCompositeChild).binding.InstanceKey
	siblingKey := sibling.(*preparedCompositeChild).binding.InstanceKey
	nextKey := nextAttempt.(*preparedCompositeChild).binding.InstanceKey
	if firstKey == siblingKey || firstKey == nextKey || siblingKey == nextKey {
		t.Fatalf("composite identities collided: first=%v sibling=%v next=%v", firstKey, siblingKey, nextKey)
	}
}

func TestCompositeChildPreparerRejectsMismatchedOrZeroAccessBeforeConstruction(t *testing.T) {
	firstHarness := newCompositeSecretHarness(t, 77, "route-authority")
	defer firstHarness.close()
	secondHarness := newCompositeSecretHarness(t, 78, "route-authority")
	defer secondHarness.close()
	preparer, err := NewCompositeChildPreparer(
		base.Dependencies{Secrets: secondHarness.capability}, secondHarness.attempt, ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"},
	)
	if err != nil {
		t.Fatal(err)
	}
	original := pluginRegistry["serverless-pre-function"]
	constructorCalls := 0
	replaceCompositeFactory(t, "serverless-pre-function", func() Plugin {
		constructorCalls++
		return original()
	})
	spec := base.CompositeChildSpec{
		Factory:  "serverless-pre-function",
		Config:   map[string]any{"functions": []any{"return function() end"}},
		Position: "workflow/rule/0/action/0",
	}
	prepared, err := prepareCompositeChild(context.Background(), firstHarness, preparer, spec)
	if prepared != nil {
		prepared.Close()
	}
	if err == nil {
		t.Fatal("Prepare(N access, N+1 dependencies) error = nil")
	}
	if constructorCalls != 0 || len(firstHarness.broker.snapshotCalls()) != 0 ||
		len(secondHarness.broker.snapshotCalls()) != 0 {
		t.Fatalf("mismatched authority reached construction/resolution: constructors=%d first=%d second=%d",
			constructorCalls, len(firstHarness.broker.snapshotCalls()), len(secondHarness.broker.snapshotCalls()))
	}

	prepared, err = preparer.Prepare(context.Background(), base.ScopedSecretAccess{}, spec)
	if prepared != nil {
		prepared.Close()
	}
	if err == nil {
		t.Fatal("Prepare(zero access) error = nil")
	}
	if constructorCalls != 0 {
		t.Fatalf("zero access constructed a no-secret child %d times", constructorCalls)
	}
}

func TestCompositeChildPreparerRejectsSameIDForeignRegistrationBeforeConstruction(t *testing.T) {
	firstHarness := newCompositeSecretHarness(t, 81, "route-same-publication")
	defer firstHarness.close()
	secondHarness := newCompositeSecretHarness(t, 81, "route-same-publication")
	defer secondHarness.close()
	if firstHarness.attempt != secondHarness.attempt ||
		firstHarness.capability.Generation() != secondHarness.capability.Generation() {
		t.Fatal("same-publication harness does not share attempt/generation values")
	}
	preparer, err := NewCompositeChildPreparer(
		base.Dependencies{Secrets: secondHarness.capability}, secondHarness.attempt, ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"},
	)
	if err != nil {
		t.Fatal(err)
	}
	original := pluginRegistry["serverless-pre-function"]
	constructorCalls := 0
	replaceCompositeFactory(t, "serverless-pre-function", func() Plugin {
		constructorCalls++
		return original()
	})
	prepared, err := prepareCompositeChild(
		context.Background(),
		firstHarness,
		preparer,
		base.CompositeChildSpec{
			Factory:  "serverless-pre-function",
			Config:   map[string]any{"functions": []any{"return function() end"}},
			Position: "workflow/rule/0/action/0",
		},
	)
	if prepared != nil {
		prepared.Close()
	}
	if err == nil {
		t.Fatal("Prepare(same ID foreign registration) error = nil")
	}
	if constructorCalls != 0 || len(firstHarness.broker.snapshotCalls()) != 0 ||
		len(secondHarness.broker.snapshotCalls()) != 0 {
		t.Fatalf(
			"foreign registration reached construction/resolution: constructors=%d first=%d second=%d",
			constructorCalls,
			len(firstHarness.broker.snapshotCalls()),
			len(secondHarness.broker.snapshotCalls()),
		)
	}
}

func TestCompositeChildPreparerConstructorRejectsGenerationMismatch(t *testing.T) {
	harness := newCompositeSecretHarness(t, 79, "route-constructor")
	defer harness.close()
	owner := ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"}
	for name, test := range map[string]struct {
		deps    base.Dependencies
		attempt uint64
	}{
		"zero capability": {
			deps: base.Dependencies{}, attempt: harness.attempt,
		},
		"zero generation": {
			deps: base.Dependencies{Secrets: harness.capability}, attempt: 0,
		},
		"different generation": {
			deps: base.Dependencies{Secrets: harness.capability}, attempt: 255,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if preparer, err := NewCompositeChildPreparer(
				test.deps,
				test.attempt,
				ScopeRoute,
				owner,
			); err == nil ||
				preparer != nil {
				t.Fatalf("NewCompositeChildPreparer() = %v/%v, want rejection", preparer, err)
			}
		})
	}
}

func TestCompositeChildPreparerRejectsNilFactoryInstanceWithoutPanic(t *testing.T) {
	harness := newCompositeSecretHarness(t, 80, "route-nil-factory")
	defer harness.close()
	replaceCompositeFactory(t, "serverless-pre-function", func() Plugin { return nil })
	preparer, err := NewCompositeChildPreparer(
		base.Dependencies{Secrets: harness.capability}, harness.attempt, ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"},
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareCompositeChild(
		context.Background(),
		harness,
		preparer,
		base.CompositeChildSpec{
			Factory:  "serverless-pre-function",
			Config:   map[string]any{"functions": []any{"return function() end"}},
			Position: "workflow/rule/0/action/0",
		},
	)
	if err == nil || prepared != nil {
		t.Fatalf("Prepare(nil factory instance) = %v/%v, want bounded failure", prepared, err)
	}
}

func TestCompositeChildPreparerFailureStopsConstructedChildOnce(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*compositeSentinelPlugin)
		config     map[string]any
		scope      Scope
		provenance ResourceProvenance
	}{
		{name: "init", configure: func(child *compositeSentinelPlugin) {
			child.initErr = errors.New("init poison must not escape")
		}},
		{name: "schema compile", configure: func(child *compositeSentinelPlugin) {
			child.schemaOverride = "not-json-schema"
		}},
		{name: "schema validation", config: map[string]any{}},
		{name: "nil child config", configure: func(child *compositeSentinelPlugin) {
			child.nilConfig = true
		}},
		{name: "parse", configure: func(child *compositeSentinelPlugin) {
			child.schemaOverride = `{}`
		}, config: map[string]any{"auth_header": map[string]any{"poison": true}}},
		{name: "materialize", configure: func(child *compositeSentinelPlugin) {
			child.materializeErr = errors.New("materialize poison must not escape")
		}},
		{name: "post-init", configure: func(child *compositeSentinelPlugin) {
			child.postInitErr = errors.New("post-init poison must not escape")
		}},
		{
			name:       "binding",
			scope:      ScopeSystem,
			provenance: ResourceProvenance{Kind: ResourceSystem, ID: "system-effective"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newCompositeSecretHarness(
				t,
				75,
				"route-failure-"+strings.ReplaceAll(test.name, " ", "-"),
			)
			defer harness.close()
			var child *compositeSentinelPlugin
			replaceCompositeFactory(t, "http-logger", func() Plugin {
				child = &compositeSentinelPlugin{}
				if test.configure != nil {
					test.configure(child)
				}
				return child
			})
			scope := test.scope
			provenance := test.provenance
			if provenance == (ResourceProvenance{}) {
				scope = ScopeRoute
				provenance = ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"}
			}
			preparer, err := NewCompositeChildPreparer(
				base.Dependencies{Secrets: harness.capability}, harness.attempt, scope, provenance,
			)
			if err != nil {
				t.Fatal(err)
			}
			config := test.config
			if config == nil {
				config = map[string]any{"auth_header": "literal-auth"}
			}
			prepared, err := prepareCompositeChild(
				context.Background(),
				harness,
				preparer,
				base.CompositeChildSpec{
					Factory: "http-logger", Config: config, Position: "workflow/rule/0/action/0",
				},
			)
			if err == nil || prepared != nil {
				t.Fatalf("Prepare() = %v/%v, want failure and no owner", prepared, err)
			}
			if child == nil || child.stopCalls != 1 {
				t.Fatalf("failed constructed child = %#v, want one Stop call", child)
			}
			for _, poison := range []string{"poison", "literal-auth"} {
				if strings.Contains(err.Error(), poison) {
					t.Fatalf("Prepare() leaked %q: %v", poison, err)
				}
			}
		})
	}
}

func TestCompositeChildPreparerRedactsRawCredentialFailures(t *testing.T) {
	harness := newCompositeSecretHarness(t, 76, "route-redaction")
	defer harness.close()
	const (
		raw       = "$secret://vault/tenant/private/password"
		plaintext = "plaintext-super-secret"
	)
	harness.broker.err = fmt.Errorf("resolver rejected %s after reading %s", raw, plaintext)
	replaceCompositeFactory(t, "http-logger", func() Plugin { return &compositeSentinelPlugin{} })
	preparer, err := NewCompositeChildPreparer(
		base.Dependencies{Secrets: harness.capability}, harness.attempt, ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-effective"},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepareCompositeChild(context.Background(), harness, preparer, base.CompositeChildSpec{
		Factory:  "http-logger",
		Config:   map[string]any{"auth_header": raw},
		Position: "workflow/rule/0/action/0",
	})
	if err == nil {
		t.Fatal("Prepare(secret failure) error = nil")
	}
	for _, poison := range []string{raw, "tenant/private", "password", plaintext} {
		if strings.Contains(err.Error(), poison) {
			t.Fatalf("Prepare() error leaked %q: %v", poison, err)
		}
	}

	_, err = preparer.Prepare(context.Background(), base.ScopedSecretAccess{}, base.CompositeChildSpec{
		Factory:  "$secret://vault/private/factory",
		Config:   map[string]any{},
		Position: "$ENV://PRIVATE_POSITION",
	})
	if err == nil || strings.Contains(err.Error(), "vault/private") ||
		strings.Contains(err.Error(), "PRIVATE_POSITION") {
		t.Fatalf("invalid spec error = %v, want redacted diagnostic", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = preparer.Prepare(canceled, base.ScopedSecretAccess{}, base.CompositeChildSpec{
		Factory: "http-logger", Config: map[string]any{}, Position: "workflow/rule/0/action/0",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare(canceled) error = %v, want context.Canceled", err)
	}
}
