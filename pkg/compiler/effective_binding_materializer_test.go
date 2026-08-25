package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	http_logger "github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestEffectiveBindingMaterializerRequiresTask7OrTask8Specs(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), nil)
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("materializeEffectiveBindings(nil) = (%#v, %v), want no bindings and redacted error", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
		t.Fatalf(
			"empty specs reached construction: constructed=%d leases=%d",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerRejectsRawOccurrenceEnumeration(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(
		t,
		[]string{"request-id"},
		map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP:   preparedGenerationPublicationSet(t).Domains[generation.DomainHTTP],
			generation.DomainStream: preparedGenerationPublicationSet(t).Domains[generation.DomainStream],
		},
	)
	if got := prepared.attempt.Occurrences(capability.SecretPluginConfig); len(got) != 1 {
		t.Fatalf("plugin-config occurrences = %#v, want one authority record", got)
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{})
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("materializeEffectiveBindings(empty) = (%#v, %v)", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
		t.Fatalf(
			"raw occurrence inventory triggered construction: constructed=%d leases=%d",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerRejectsSourceAuthorityMismatch(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	spec.source.resource.ID = "relabeled-source"

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("source mismatch = (%#v, %v), want no bindings and redacted error", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
		t.Fatalf(
			"source mismatch reached construction: constructed=%d leases=%d",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerRejectsCrossAttemptOrRelabeledSpec(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	foreign, foreignFixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	t.Cleanup(func() { _ = foreign.Close(context.Background()) })
	spec := fixture.pluginSpec("request-id", "route-1")
	spec.source.occurrence = foreignFixture.occurrences["request-id"]

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("cross-attempt source = (%#v, %v), want no bindings and redacted error", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
		t.Fatalf(
			"cross-attempt source reached construction: constructed=%d leases=%d",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerRejectsDuplicateEffectiveSpec(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec, spec})
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("duplicate specs = (%#v, %v), want no bindings and redacted error", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
		t.Fatalf(
			"duplicate specs reached construction: constructed=%d leases=%d",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerPreparedConsumerDoesNotRematerializeSecrets(t *testing.T) {
	consumerConfig := map[string]any{"header_name": "X-Consumer-Request-ID"}
	consumers, err := runtime.NewConsumerBindings([]runtime.ConsumerRecord{{
		ID: "consumer-1",
		Consumer: resource.Consumer{Username: "consumer-1", Plugins: map[string]resource.PluginConfig{
			"request-id": consumerConfig,
		}},
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, fixture := newEffectiveBindingMaterializerFixtureWithConsumers(t, nil, consumers)
	spec := effectiveBindingSpec{
		domain:         generation.DomainHTTP,
		executionOwner: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		source: effectiveBindingSource{
			kind:     effectiveBindingPreparedConsumer,
			resource: generation.ResourceKey{Kind: "consumers", ID: "consumer-1"},
			source:   capability.SecretConsumerConfig,
		},
		factory:    "request-id",
		config:     consumerConfig,
		scope:      plugin.ScopeConsumer,
		provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "consumer-1"},
		resourceContext: effectiveBindingResourceContext{
			kind:  effectiveBindingContextHTTP,
			route: resource.Route{ID: "route-1"},
		},
	}
	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("prepared consumer materialization = (%#v, %v), want one binding", bindings, err)
	}
	if fixture.registration.materializeCalls.Load() != 0 {
		t.Fatalf("prepared consumer rematerialized secrets %d times", fixture.registration.materializeCalls.Load())
	}
}

func TestEffectiveBindingMaterializerRejectsPreparedConsumerGroup(t *testing.T) {
	groupConfig := map[string]any{"header_name": "X-Group-Request-ID"}
	consumers, err := runtime.NewConsumerBindings(nil, []runtime.ConsumerGroupRecord{{
		ID: "group-1",
		Group: resource.ConsumerGroup{Plugins: map[string]resource.PluginConfig{
			"request-id": groupConfig,
		}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, fixture := newEffectiveBindingMaterializerFixtureWithConsumers(t, nil, consumers)
	spec := effectiveBindingSpec{
		domain:         generation.DomainHTTP,
		executionOwner: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		source: effectiveBindingSource{
			kind:     effectiveBindingPreparedConsumer,
			resource: generation.ResourceKey{Kind: "consumer_groups", ID: "group-1"},
			source:   capability.SecretConsumerConfig,
		},
		factory:    "request-id",
		config:     groupConfig,
		scope:      plugin.ScopeConsumer,
		provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumerGroup, ID: "group-1"},
		resourceContext: effectiveBindingResourceContext{
			kind: effectiveBindingContextHTTP, route: resource.Route{ID: "route-1"},
		},
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if bindings != nil || !errors.Is(err, errEffectiveBindingMaterializationFailed) {
		t.Fatalf("prepared consumer-group materialization = (%#v, %v), want fail-closed rejection", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registration.materializeCalls.Load() != 0 {
		t.Fatalf(
			"prepared consumer-group reached construction/secrets: constructed=%d materialized=%d",
			fixture.constructed.Load(), fixture.registration.materializeCalls.Load(),
		)
	}
}

func TestEffectiveBindingMaterializerSystemSourceRequiresNoSecretDeclaration(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	spec := fixture.systemSpec("error-log-logger")

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("secret-declaring system source = (%#v, %v), want no bindings and redacted error", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
		t.Fatalf(
			"secret-declaring system source reached construction: constructed=%d leases=%d",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerRejectsAfterGenerationClose(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("post-close materialization = (%#v, %v), want no bindings and redacted error", bindings, err)
	}
	if fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
		t.Fatalf(
			"post-close materialization reached construction: constructed=%d leases=%d",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerCloseRaceIsLinearized(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	entered := make(chan struct{})
	resume := make(chan struct{})
	closeAttempted := make(chan struct{})
	cleanupStarted := make(chan struct{})
	prepared.bindingOps.closeStarted = func() { close(closeAttempted) }
	defaultQuiesce := prepared.cleanup.quiescers[0].run
	prepared.cleanup.quiescers[0].run = func(ctx context.Context) error {
		close(cleanupStarted)
		return defaultQuiesce(ctx)
	}
	defaultDecode := prepared.bindingOps.decodeConfig
	prepared.bindingOps.decodeConfig = func(config resource.PluginConfig, destination any) error {
		close(entered)
		<-resume
		return defaultDecode(config, destination)
	}

	materialized := make(chan error, 1)
	go func() {
		_, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
		materialized <- err
	}()
	awaitMaterializerSignal(t, entered, "admitted materialization")
	closed := make(chan error, 1)
	go func() { closed <- prepared.Close(context.Background()) }()
	awaitMaterializerSignal(t, closeAttempted, "Close call entry")
	assertMaterializerSignalBlocked(t, cleanupStarted, 100*time.Millisecond, "cleanup before admitted materialization")
	close(resume)
	if err := awaitMaterializerResult(t, materialized, "admitted materialization result"); err != nil {
		t.Fatalf("admitted materialization error = %v", err)
	}
	awaitMaterializerSignal(t, cleanupStarted, "generation cleanup start")
	if err := awaitMaterializerResult(t, closed, "Close result"); err != nil {
		t.Fatalf("Close error = %v", err)
	}
	if fixture.registry.Len() != 0 || fixture.registration.closed.Load() != 1 {
		t.Fatalf(
			"close race leaked ownership: leases=%d registration-close=%d",
			fixture.registry.Len(),
			fixture.registration.closed.Load(),
		)
	}
}

func TestEffectiveBindingMaterializerKeepsConsumerCleanupAuthorityPrivate(t *testing.T) {
	prepared, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	lookup := prepared.ConsumerLookup()
	if lookup == nil {
		t.Fatal("ConsumerLookup() = nil before close")
	}
	if _, ok := lookup.(interface{ Close() }); ok {
		t.Fatalf("ConsumerLookup() type %T exposes Close", lookup)
	}
	if _, ok := lookup.(*runtime.ConsumerBindings); ok {
		t.Fatalf("ConsumerLookup() type %T exposes concrete binding owner", lookup)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup.ConsumerByID("consumer-1"); ok {
		t.Fatal("retained ConsumerLookup remained live after generation close")
	}
}

func TestEffectiveBindingMaterializerInjectsExactDependenciesBeforeOuterConstruction(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	defaultNew := prepared.bindingOps.newFactoryInstance
	prepared.bindingOps.newFactoryInstance = func(factory string, dependencies base.Dependencies) (plugin.FactoryInstance, error) {
		if dependencies.Config != prepared.effective ||
			!dependencies.Secrets.SameAuthority(prepared.attempt.capability) ||
			!reflect.DeepEqual(dependencies.Metadata, prepared.metadata) ||
			dependencies.Consumers == nil ||
			dependencies.Tasks == nil ||
			dependencies.CompositeChildren == nil ||
			dependencies.DataEncryption.Configured() {
			t.Fatalf("outer dependencies are incomplete or expose legacy decryption: %#v", dependencies)
		}
		return defaultNew(factory, dependencies)
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("materialize exact dependencies = (%#v, %v)", bindings, err)
	}
}

func TestEffectiveBindingMaterializerInjectsExactPluginTaskOwner(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	defaultNew := prepared.bindingOps.newFactoryInstance
	var captured *runtime.TaskOwner
	prepared.bindingOps.newFactoryInstance = func(
		factory string,
		dependencies base.Dependencies,
	) (plugin.FactoryInstance, error) {
		captured = dependencies.Tasks
		return defaultNew(factory, dependencies)
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if err != nil || len(bindings) != 1 || captured == nil {
		t.Fatalf("materialize owner = (%#v, %v, %v)", bindings, captured, err)
	}
	started := make(chan struct{})
	if err := captured.Go("health-refresh", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	prefix, err := pluginTaskOwnerPrefix(bindings[0].InstanceKey)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{prefix + "/health-refresh"}
	if got := prepared.tasks.Active(); !reflect.DeepEqual(got, want) {
		t.Fatalf("active owners = %v, want %v", got, want)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveBindingMaterializerLifecycleOrderAndExactSecretScope(t *testing.T) {
	compiler := newTestCompiler(t)
	trace := &materializerTrace{}
	broker := &materializerLifecycleBroker{trace: trace}
	materializer := secret.NewScopedMaterializer(broker, compiler.schemas.catalog)
	factory, err := newAttemptFactory(compiler, materializer)
	if err != nil {
		t.Fatal(err)
	}
	desired := mustGenerationSnapshot(t, 7101, []generation.Resource{
		resourceValue("routes", "route-source", `{
			"id":"route-source",
			"plugins":{"http-logger":{
				"uri":"http://127.0.0.1/logs",
				"auth_header":"$ENV://HTTP_LOGGER_AUTH"
			}}
		}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	registered, err := factory.prepareCandidateAttempt(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumers, err := runtime.NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	cleanup := &cleanupStack{}
	if err := cleanup.Own(cleanupQuiesce, "tasks", func(ctx context.Context) error {
		residuals, err := tasks.Stop(ctx)
		if len(residuals) != 0 {
			return errors.Join(err, errors.New("task residual"))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Own(cleanupRelease, "registration", registered.Close); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Own(cleanupRelease, "consumers", func(context.Context) error {
		consumers.Close()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prepared := &PreparedGeneration{
		publication:  registered.publication,
		attempt:      registered.attempt,
		consumers:    consumers,
		lookup:       consumerLookupView{bindings: consumers},
		tasks:        tasks,
		effective:    &config.EffectiveConfig{},
		manifest:     compiler.manifest,
		registry:     runtime.NewResourceRegistry(),
		materializer: materializer,
		cleanup:      cleanup,
		bindingOps:   defaultEffectiveBindingOps().withDefaults(registered.attempt.AttemptID()),
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	occurrence := registered.attempt.Occurrences(capability.SecretPluginConfig)[0]
	spec := effectiveBindingSpec{
		domain:         generation.DomainHTTP,
		executionOwner: generation.ResourceKey{Kind: "routes", ID: "route-source"},
		source: effectiveBindingSource{
			kind: effectiveBindingPluginConfig, resource: occurrence.Resource(),
			source: capability.SecretPluginConfig, occurrence: occurrence,
		},
		factory:    "http-logger",
		config:     map[string]any{"uri": "http://127.0.0.1/logs", "auth_header": "$ENV://HTTP_LOGGER_AUTH"},
		scope:      plugin.ScopeRoute,
		provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-source"},
		resourceContext: effectiveBindingResourceContext{
			kind:  effectiveBindingContextHTTP,
			route: resource.Route{ID: "route-source", Labels: map[string]any{"owner": "route"}},
		},
	}
	operations := &prepared.bindingOps
	defaultNew := operations.newFactoryInstance
	operations.newFactoryInstance = func(factory string, dependencies base.Dependencies) (plugin.FactoryInstance, error) {
		if dependencies.CompositeChildren == nil {
			t.Fatal("CompositeChildren was not injected before outer construction")
		}
		trace.record("new")
		return defaultNew(factory, dependencies)
	}
	defaultInit := operations.initPlugin
	operations.initPlugin = func(instance plugin.Plugin) error {
		trace.record("init")
		return defaultInit(instance)
	}
	defaultValidate := operations.validateConfig
	operations.validateConfig = func(instance plugin.Plugin, config resource.PluginConfig) error {
		trace.record("schema")
		return defaultValidate(instance, config)
	}
	defaultDecode := operations.decodeConfig
	operations.decodeConfig = func(config resource.PluginConfig, destination any) error {
		trace.record("decode")
		return defaultDecode(config, destination)
	}
	defaultContext := operations.applyContext
	operations.applyContext = func(instance plugin.Plugin, value effectiveBindingResourceContext) {
		trace.record("context")
		defaultContext(instance, value)
	}
	defaultPostInit := operations.postInit
	operations.postInit = func(instance plugin.Plugin) error {
		trace.record("post-init")
		return defaultPostInit(instance)
	}
	defaultObserver := operations.startObserver
	operations.startObserver = func(instance plugin.Plugin, tasks *runtime.TaskOwner) error {
		trace.record("observer")
		return defaultObserver(instance, tasks)
	}
	defaultResolve := operations.resolveDescriptor
	operations.resolveDescriptor = func(descriptor plugin.Descriptor, instance plugin.Plugin) (plugin.Descriptor, error) {
		trace.record("descriptor")
		return defaultResolve(descriptor, instance)
	}
	defaultBind := operations.bind
	operations.bind = func(
		descriptor plugin.Descriptor,
		instance plugin.Plugin,
		scope plugin.Scope,
		provenance plugin.ResourceProvenance,
		identity plugin.InstanceIdentityInput,
	) (plugin.Binding, error) {
		trace.record("bind")
		return defaultBind(descriptor, instance, scope, provenance, identity)
	}
	defaultAcquire := operations.acquire
	operations.acquire = func(
		ctx context.Context,
		registry *runtime.ResourceRegistry,
		key runtime.ResourceKey,
		create runtime.ResourceFactory[plugin.Binding],
	) (*runtime.ResourceLease[plugin.Binding], error) {
		lease, err := defaultAcquire(ctx, registry, key, create)
		if err == nil {
			trace.record("acquire")
		}
		return lease, err
	}
	broker.reset()

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("lifecycle materialization = (%#v, %v)", bindings, err)
	}
	want := []string{
		"new", "init", "schema", "decode", "secret", "context", "post-init",
		"observer", "descriptor", "bind", "acquire",
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle trace = %v, want %v", got, want)
	}
	scope, raw, calls := broker.lastCall()
	if calls != 1 || raw != "$ENV://HTTP_LOGGER_AUTH" || scope.Attempt != prepared.attempt.AttemptID() ||
		scope.Generation != prepared.attempt.Generation() || scope.Domain != generation.DomainHTTP ||
		scope.Plugin != "http-logger" || scope.Resource != occurrence.Resource() ||
		scope.Source != capability.SecretPluginConfig || scope.Field != "auth_header" {
		t.Fatalf("scoped secret call = (%#v, %q, %d), want exact occurrence authority", scope, raw, calls)
	}
}

func TestEffectiveBindingMaterializerConsumerGroupUsesPluginConfigSecretScope(t *testing.T) {
	trace := &materializerTrace{}
	broker := &materializerLifecycleBroker{trace: trace}
	desired := mustGenerationSnapshot(t, 7102, []generation.Resource{
		resourceValue("consumer_groups", "group-1", `{
			"id":"group-1",
			"plugins":{"http-logger":{
				"uri":"http://127.0.0.1/logs",
				"auth_header":"$ENV://GROUP_HTTP_LOGGER_AUTH"
			}}
		}`),
	}, nil)
	prepared, occurrence := newRealEffectiveBindingPrepared(t, desired, broker)
	if occurrence.Resource() != (generation.ResourceKey{Kind: "consumer_groups", ID: "group-1"}) ||
		occurrence.Source() != capability.SecretPluginConfig || occurrence.Factory() != "http-logger" {
		t.Fatalf("consumer-group occurrence = %#v, want exact plugin-config authority", occurrence)
	}
	spec := effectiveBindingSpec{
		domain:         generation.DomainHTTP,
		executionOwner: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		source: effectiveBindingSource{
			kind: effectiveBindingPluginConfig, resource: occurrence.Resource(),
			source: occurrence.Source(), occurrence: occurrence,
		},
		factory: "http-logger",
		config: map[string]any{
			"uri": "http://127.0.0.1/logs", "auth_header": "$ENV://GROUP_HTTP_LOGGER_AUTH",
		},
		scope:      plugin.ScopeConsumer,
		provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumerGroup, ID: "group-1"},
		resourceContext: effectiveBindingResourceContext{
			kind: effectiveBindingContextHTTP, route: resource.Route{ID: "route-1"},
		},
	}
	broker.reset()

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("consumer-group materialization = (%#v, %v), want one scoped binding", bindings, err)
	}
	instance, ok := bindings[0].Plugin.(*http_logger.Plugin)
	if !ok {
		t.Fatalf("consumer-group plugin type = %T, want *http_logger.Plugin", bindings[0].Plugin)
	}
	configuration, ok := instance.Config().(*http_logger.Config)
	if !ok || configuration.AuthHeader == nil {
		t.Fatalf("consumer-group http-logger config = %#v", instance.Config())
	}
	descriptor, err := secret.NewDescriptor(
		capability.SecretPluginConfig, sha256.Sum256([]byte("Bearer resolved")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if *configuration.AuthHeader != descriptor.String() {
		t.Fatalf(
			"consumer-group auth_header = %q, want resolved plaintext descriptor %q",
			*configuration.AuthHeader,
			descriptor,
		)
	}
	scope, raw, calls := broker.lastCall()
	if calls != 1 || raw != "$ENV://GROUP_HTTP_LOGGER_AUTH" ||
		scope.Attempt != prepared.attempt.AttemptID() || scope.Generation != prepared.attempt.Generation() ||
		scope.Domain != generation.DomainHTTP || scope.Plugin != "http-logger" ||
		scope.Resource != occurrence.Resource() || scope.Source != capability.SecretPluginConfig ||
		scope.Field != "auth_header" {
		t.Fatalf(
			"consumer-group scoped secret call = (%#v, %q, %d), want exact plugin-config authority",
			scope,
			raw,
			calls,
		)
	}
}

func TestEffectiveBindingMaterializerSameConfigDifferentContextDoesNotShare(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	first := fixture.pluginSpec("request-id", "route-1")
	second := fixture.pluginSpec("request-id", "route-2")
	second.executionOwner = generation.ResourceKey{Kind: "routes", ID: "route-2"}
	second.resourceContext.route = resource.Route{ID: "route-2", Labels: map[string]any{"owner": "second"}}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{first, second})
	if err != nil || len(bindings) != 2 {
		t.Fatalf("different contexts = (%#v, %v), want two bindings", bindings, err)
	}
	if bindings[0].InstanceKey == bindings[1].InstanceKey || fixture.registry.Len() != 2 {
		t.Fatalf(
			"different contexts shared identity: keys=%+v/%+v leases=%d",
			bindings[0].InstanceKey,
			bindings[1].InstanceKey,
			fixture.registry.Len(),
		)
	}
}

func TestEffectiveBindingMaterializerDefensivelyOwnsAllMutableInputs(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	prepared.bindingOps = prepared.bindingOps.withDefaults(prepared.attempt.AttemptID())
	configValue := map[string]any{"header_name": "X-Original"}
	filterValue := map[string]any{"nested": []any{"filter-original"}}
	errorValue := map[string]any{"nested": []any{"error-original"}}
	spec := fixture.pluginSpec("request-id", "route-original")
	spec.config = configValue
	spec.filterIdentity = filterValue
	spec.errorIdentity = errorValue
	spec.resourceContext.route.Uris = []string{"/original"}
	spec.resourceContext.route.Vars = json.RawMessage(`["vars-original"]`)
	spec.resourceContext.route.Script = json.RawMessage(`{"script":"original"}`)
	spec.resourceContext.route.ScriptID = json.RawMessage(`"script-id-original"`)
	spec.resourceContext.route.Labels = map[string]any{"nested": []any{"label-original"}}
	spec.resourceContext.service.Hosts = []string{"service-original.test"}

	var decoded resource.PluginConfig
	var applied effectiveBindingResourceContext
	var identity plugin.InstanceIdentityInput
	defaultDecode := prepared.bindingOps.decodeConfig
	prepared.bindingOps.decodeConfig = func(config resource.PluginConfig, destination any) error {
		decoded = config
		return defaultDecode(config, destination)
	}
	defaultApply := prepared.bindingOps.applyContext
	prepared.bindingOps.applyContext = func(instance plugin.Plugin, value effectiveBindingResourceContext) {
		applied = value
		defaultApply(instance, value)
	}
	defaultBind := prepared.bindingOps.bind
	prepared.bindingOps.bind = func(
		descriptor plugin.Descriptor,
		instance plugin.Plugin,
		scope plugin.Scope,
		provenance plugin.ResourceProvenance,
		input plugin.InstanceIdentityInput,
	) (plugin.Binding, error) {
		identity = input
		return defaultBind(descriptor, instance, scope, provenance, input)
	}
	validated := make(chan struct{})
	resume := make(chan struct{})
	defaultAcquire := prepared.bindingOps.acquire
	prepared.bindingOps.acquire = func(
		ctx context.Context,
		registry *runtime.ResourceRegistry,
		key runtime.ResourceKey,
		create runtime.ResourceFactory[plugin.Binding],
	) (*runtime.ResourceLease[plugin.Binding], error) {
		close(validated)
		<-resume
		return defaultAcquire(ctx, registry, key, create)
	}

	result := make(chan error, 1)
	go func() {
		_, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
		result <- err
	}()
	awaitMaterializerSignal(t, validated, "validated defensive inputs")
	configValue["header_name"] = "X-Mutated"
	filterValue["nested"].([]any)[0] = "filter-mutated"
	errorValue["nested"].([]any)[0] = "error-mutated"
	spec.resourceContext.route.Uris[0] = "/mutated"
	spec.resourceContext.route.Vars[2] = 'X'
	spec.resourceContext.route.Script[11] = 'X'
	spec.resourceContext.route.ScriptID[2] = 'X'
	spec.resourceContext.route.Labels["nested"].([]any)[0] = "label-mutated"
	spec.resourceContext.service.Hosts[0] = "service-mutated.test"
	close(resume)
	if err := awaitMaterializerResult(t, result, "defensive materialization"); err != nil {
		t.Fatal(err)
	}

	if got := decoded.(map[string]any); got["header_name"] != "X-Original" {
		t.Fatalf("decoded config aliased caller input: %#v", got)
	}
	if string(applied.route.Script) != `{"script":"original"}` ||
		string(applied.route.ScriptID) != `"script-id-original"` ||
		string(applied.route.Vars) != `["vars-original"]` || applied.route.Uris[0] != "/original" ||
		applied.route.Labels["nested"].([]any)[0] != "label-original" ||
		applied.service.Hosts[0] != "service-original.test" {
		t.Fatalf("resource context aliased caller input: %#v", applied)
	}
	if got := identity.Filter.(map[string]any)["nested"].([]any)[0]; got != "filter-original" {
		t.Fatalf("filter identity aliased caller input: %v", got)
	}
	if got := identity.ErrorResponse.(map[string]any)["nested"].([]any)[0]; got != "error-original" {
		t.Fatalf("error identity aliased caller input: %v", got)
	}
}

func TestEffectiveBindingMaterializerRejectsCyclicJSONValues(t *testing.T) {
	cycleKind := os.Getenv("APISIX_GO_EFFECTIVE_BINDING_CYCLE")
	if cycleKind != "" {
		var value any
		switch cycleKind {
		case "map":
			cycle := map[string]any{}
			cycle["self"] = cycle
			value = cycle
		case "slice":
			cycle := make([]any, 1)
			cycle[0] = cycle
			value = cycle
		default:
			t.Fatalf("unknown cycle kind %q", cycleKind)
		}
		if cloned, err := cloneEffectiveBindingValue(value); err == nil || cloned != nil {
			t.Fatalf("cyclic %s clone = %#v/%v, want nil/error", cycleKind, cloned, err)
		}
		return
	}

	for _, cycleKind := range []string{"map", "slice"} {
		t.Run(cycleKind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(
				ctx, os.Args[0], "-test.run=^TestEffectiveBindingMaterializerRejectsCyclicJSONValues$",
			)
			command.Env = append(os.Environ(), "APISIX_GO_EFFECTIVE_BINDING_CYCLE="+cycleKind)
			command.Stdout = io.Discard
			command.Stderr = io.Discard
			err := command.Run()
			if ctx.Err() != nil {
				t.Fatalf("cyclic %s clone did not fail closed before timeout", cycleKind)
			}
			if err != nil {
				t.Fatalf("cyclic %s clone subprocess exited with %v, want clean rejection", cycleKind, err)
			}
		})
	}

	t.Run("shared map siblings", func(t *testing.T) {
		shared := map[string]any{"nested": []any{"original"}}
		clonedValue, err := cloneEffectiveBindingValue(map[string]any{"left": shared, "right": shared})
		if err != nil {
			t.Fatal(err)
		}
		cloned := clonedValue.(map[string]any)
		left := cloned["left"].(map[string]any)["nested"].([]any)
		right := cloned["right"].(map[string]any)["nested"].([]any)
		shared["nested"].([]any)[0] = "caller-mutated"
		left[0] = "left-mutated"
		if right[0] != "original" {
			t.Fatalf("shared map sibling was rejected or aliased: right=%#v", right)
		}
	})

	t.Run("shared slice siblings", func(t *testing.T) {
		shared := []any{map[string]any{"value": "original"}}
		clonedValue, err := cloneEffectiveBindingValue([]any{shared, shared})
		if err != nil {
			t.Fatal(err)
		}
		cloned := clonedValue.([]any)
		left := cloned[0].([]any)[0].(map[string]any)
		right := cloned[1].([]any)[0].(map[string]any)
		shared[0].(map[string]any)["value"] = "caller-mutated"
		left["value"] = "left-mutated"
		if right["value"] != "original" {
			t.Fatalf("shared slice sibling was rejected or aliased: right=%#v", right)
		}
	})
}

func TestEffectiveBindingMaterializerRejectsNonJSONMutableValues(t *testing.T) {
	type typedMap map[string]any
	type typedSlice []any
	plainMap := map[string]any{"value": "pointer"}
	for name, value := range map[string]any{
		"named map":      typedMap{"value": "named"},
		"named slice":    typedSlice{"named"},
		"map pointer":    &plainMap,
		"string pointer": new(string),
		"function":       func() {},
		"channel":        make(chan struct{}),
	} {
		t.Run(name, func(t *testing.T) {
			if cloned, err := cloneEffectiveBindingValue(value); err == nil {
				t.Fatalf("cloneEffectiveBindingValue(%T) = %#v, want fail-closed rejection", value, cloned)
			}
		})
	}
}

func TestEffectiveBindingMaterializerCandidateAndRecoveryAttemptsDoNotShare(t *testing.T) {
	first, firstFixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	second, secondFixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	firstSpec := firstFixture.pluginSpec("request-id", "route-1")
	secondSpec := secondFixture.pluginSpec("request-id", "route-1")

	firstBindings, err := first.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{firstSpec})
	if err != nil {
		t.Fatal(err)
	}
	secondBindings, err := second.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{secondSpec})
	if err != nil {
		t.Fatal(err)
	}
	if firstBindings[0].InstanceKey.Attempt == secondBindings[0].InstanceKey.Attempt {
		t.Fatalf("candidate/recovery identities shared attempt: %+v", firstBindings[0].InstanceKey)
	}
}

func TestEffectiveBindingResourceKeyIncludesSourceAndUsesStructuredEncoding(t *testing.T) {
	attempt := secret.AttemptID{1, 2, 3}
	baseInstance := plugin.InstanceKey{
		Factory: "request-id", Attempt: attempt, Scope: plugin.ScopeRoute,
		Owner:        plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "owner"},
		ConfigDigest: sha256.Sum256([]byte("same config/context/filter/error")),
	}
	owner := generation.ResourceKey{Kind: "routes", ID: "route-1"}
	pluginConfigSource := effectiveBindingSource{
		kind:     effectiveBindingPluginConfig,
		resource: generation.ResourceKey{Kind: "plugin_configs", ID: "shared"},
		source:   capability.SecretPluginConfig,
	}
	consumerSource := effectiveBindingSource{
		kind:     effectiveBindingPreparedConsumer,
		resource: generation.ResourceKey{Kind: "consumers", ID: "shared"},
		source:   capability.SecretConsumerConfig,
	}
	first := effectiveBindingResourceKey(generation.DomainHTTP, owner, pluginConfigSource, baseInstance)
	second := effectiveBindingResourceKey(generation.DomainHTTP, owner, consumerSource, baseInstance)
	if first == second {
		t.Fatalf("different declaration sources shared runtime identity: %#v", first)
	}

	firstInstance := baseInstance
	firstInstance.Factory = "b/c"
	firstOwner := generation.ResourceKey{Kind: "routes", ID: "a"}
	secondInstance := baseInstance
	secondInstance.Factory = "c"
	secondOwner := generation.ResourceKey{Kind: "routes", ID: "a/b"}
	first = effectiveBindingResourceKey(generation.DomainHTTP, firstOwner, pluginConfigSource, firstInstance)
	second = effectiveBindingResourceKey(generation.DomainHTTP, secondOwner, pluginConfigSource, secondInstance)
	if first == second {
		t.Fatalf("slash-delimited adversarial IDs collided: %#v", first)
	}
	if first.Scope != "generation-effective-binding/v1" || second.Scope != first.Scope {
		t.Fatalf("resource key scope = %q/%q, want fixed non-sensitive version label", first.Scope, second.Scope)
	}
}

func TestEffectiveBindingMaterializerObserverStartFailureIsTerminal(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	taskExited := make(chan struct{})
	var stoppedBeforeTaskExit atomic.Bool
	defaultStop := prepared.bindingOps.stopPlugin
	prepared.bindingOps.stopPlugin = func(instance plugin.Plugin) {
		select {
		case <-taskExited:
		default:
			stoppedBeforeTaskExit.Store(true)
		}
		defaultStop(instance)
	}
	prepared.bindingOps.startObserver = func(_ plugin.Plugin, tasks *runtime.TaskOwner) error {
		if err := tasks.Go(
			"observer",
			func(ctx context.Context) error {
				<-ctx.Done()
				close(taskExited)
				return nil
			},
		); err != nil {
			return err
		}
		return errors.New("observer plaintext must not leak")
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("observer failure = (%#v, %v), want no bindings and redacted error", bindings, err)
	}
	if fixture.registry.Len() != 0 || fixture.registration.closed.Load() != 1 ||
		reflect.ValueOf(prepared.ConsumerLookup()).IsValid() {
		t.Fatalf(
			"observer failure did not terminally clean generation: leases=%d registration=%d lookup=%#v",
			fixture.registry.Len(),
			fixture.registration.closed.Load(),
			prepared.ConsumerLookup(),
		)
	}
	if err != nil && reflect.ValueOf(err.Error()).String() == "observer plaintext must not leak" {
		t.Fatalf("observer error leaked: %v", err)
	}
	if errorTreeContainsForMaterializerTest(err, "observer plaintext must not leak") {
		t.Fatalf("observer error remained reachable through Unwrap: %v", err)
	}
	select {
	case <-taskExited:
	default:
		t.Fatal("observer-start failure left its admitted task live")
	}
	if stoppedBeforeTaskExit.Load() {
		t.Fatal("observer-start failure stopped its partial plugin before task quiescence")
	}
}

func TestEffectiveBindingMaterializerContextRedactionRebuildsOnlyExactSentinels(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	const secretText = "plaintext://handle/must-not-survive"
	prepared.bindingOps.decodeConfig = func(resource.PluginConfig, any) error {
		return errors.Join(
			fmt.Errorf("wrapped cancel with %s: %w", secretText, context.Canceled),
			fmt.Errorf("wrapped deadline with opaque handle: %w", context.DeadlineExceeded),
			errors.New(secretText),
		)
	}

	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
	if bindings != nil || !errors.Is(err, errEffectiveBindingMaterializationFailed) ||
		!errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context redaction = (%#v, %v), want both canonical context identities", bindings, err)
	}
	for _, forbidden := range []string{secretText, "wrapped cancel", "wrapped deadline", "opaque handle"} {
		if errorTreeContainsForMaterializerTest(err, forbidden) {
			t.Fatalf("redacted context error retained %q in error tree: %v", forbidden, err)
		}
	}
}

func TestEffectiveBindingMaterializerCancellationAfterFactoryUsesGenerationCleanup(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-1")
	type contextKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "retained"))
	taskExited := make(chan struct{})
	var stoppedBeforeTaskExit atomic.Bool
	var transactionInheritedCancellation atomic.Bool
	var transactionLostValue atomic.Bool
	prepared.bindingOps = prepared.bindingOps.withDefaults(prepared.attempt.AttemptID())
	defaultAcquire := prepared.bindingOps.acquire
	prepared.bindingOps.acquire = func(
		transactionCtx context.Context,
		registry *runtime.ResourceRegistry,
		key runtime.ResourceKey,
		create runtime.ResourceFactory[plugin.Binding],
	) (*runtime.ResourceLease[plugin.Binding], error) {
		lease, err := defaultAcquire(transactionCtx, registry, key, create)
		if transactionCtx.Err() != nil {
			transactionInheritedCancellation.Store(true)
		}
		if transactionCtx.Value(contextKey{}) != "retained" {
			transactionLostValue.Store(true)
		}
		return lease, err
	}
	defaultStop := prepared.bindingOps.stopPlugin
	prepared.bindingOps.stopPlugin = func(instance plugin.Plugin) {
		select {
		case <-taskExited:
		default:
			stoppedBeforeTaskExit.Store(true)
		}
		defaultStop(instance)
	}
	prepared.bindingOps.startObserver = func(_ plugin.Plugin, tasks *runtime.TaskOwner) error {
		return tasks.Go(
			"observer",
			func(taskCtx context.Context) error {
				<-taskCtx.Done()
				close(taskExited)
				return nil
			},
		)
	}
	defaultBind := prepared.bindingOps.bind
	prepared.bindingOps.bind = func(
		descriptor plugin.Descriptor,
		instance plugin.Plugin,
		scope plugin.Scope,
		provenance plugin.ResourceProvenance,
		identity plugin.InstanceIdentityInput,
	) (plugin.Binding, error) {
		binding, err := defaultBind(descriptor, instance, scope, provenance, identity)
		cancel()
		return binding, err
	}

	bindings, err := prepared.materializeEffectiveBindings(ctx, []effectiveBindingSpec{spec})
	if bindings != nil || !errors.Is(err, errEffectiveBindingMaterializationFailed) ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("post-factory cancellation = (%#v, %v), want terminal context cancellation", bindings, err)
	}
	select {
	case <-taskExited:
	default:
		t.Fatal("post-factory cancellation left its admitted task live")
	}
	if stoppedBeforeTaskExit.Load() {
		t.Fatal("runtime Acquire cancellation stopped the plugin before generation task quiescence")
	}
	if transactionInheritedCancellation.Load() || transactionLostValue.Load() {
		t.Fatalf(
			"runtime Acquire transaction context = (canceled=%t, lost-value=%t), want values without cancellation",
			transactionInheritedCancellation.Load(), transactionLostValue.Load(),
		)
	}
}

func errorTreeContainsForMaterializerTest(err error, secretText string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), secretText) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if errorTreeContainsForMaterializerTest(child, secretText) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return errorTreeContainsForMaterializerTest(wrapped.Unwrap(), secretText)
	}
	return false
}

func TestPrepareGenerationThirdPluginFailure(t *testing.T) {
	trace := &materializerTrace{}
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	prepared.bindingOps.trace = trace.record
	defaultDecode := prepared.bindingOps.decodeConfig
	var decodes atomic.Int64
	prepared.bindingOps.decodeConfig = func(config resource.PluginConfig, destination any) error {
		if decodes.Add(1) == 3 {
			return errors.New("third plugin secret plaintext")
		}
		return defaultDecode(config, destination)
	}
	prepared.cleanup.quiescers[0].run = func(ctx context.Context) error {
		trace.record("tasks-stop")
		residuals, err := prepared.tasks.Stop(ctx)
		if len(residuals) != 0 {
			return errors.New("task residual")
		}
		return err
	}
	prepared.cleanup.releases[0].run = func(ctx context.Context) error {
		trace.record("registration-close")
		return fixture.registration.Close(ctx)
	}
	prepared.cleanup.releases[1].run = func(context.Context) error {
		trace.record("consumers-close")
		prepared.consumers.Close()
		return nil
	}

	specs := []effectiveBindingSpec{
		fixture.pluginSpec("request-id", "route-1"),
		fixture.pluginSpec("request-id", "route-2"),
		fixture.pluginSpec("request-id", "route-3"),
	}
	for index := range specs {
		id := "route-" + string(rune('1'+index))
		specs[index].executionOwner.ID = id
		specs[index].resourceContext.route.ID = id
	}
	bindings, err := prepared.materializeEffectiveBindings(context.Background(), specs)
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("third plugin failure = (%#v, %v), want no partial bindings", bindings, err)
	}
	if fixture.registry.Len() != 0 || fixture.registration.closed.Load() != 1 {
		t.Fatalf(
			"third plugin failure leaked owners: leases=%d registration=%d",
			fixture.registry.Len(),
			fixture.registration.closed.Load(),
		)
	}
	got := trace.snapshot()
	wantSuffix := []string{
		"tasks-stop", "stop:request-id", "lease-release:request-id", "stop:request-id",
		"lease-release:request-id", "stop:request-id", "consumers-close", "registration-close",
	}
	if len(got) < len(wantSuffix) || !reflect.DeepEqual(got[len(got)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("third-plugin cleanup trace = %v, want suffix %v", got, wantSuffix)
	}
}

func TestEffectiveBindingMaterializerSurfacesStayCompilerPrivate(t *testing.T) {
	file, err := parser.ParseFile(
		token.NewFileSet(),
		"effective_binding_materializer.go",
		nil,
		parser.SkipObjectResolution,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if ok && typeSpec.Name.IsExported() {
					t.Fatalf("materializer exports type %s", typeSpec.Name.Name)
				}
			}
		case *ast.FuncDecl:
			if declaration.Recv == nil && declaration.Name.IsExported() {
				t.Fatalf("materializer exports function/method %s", declaration.Name.Name)
			}
		}
	}
}

type materializerTestRegistration struct {
	id               secret.AttemptID
	materializeCalls atomic.Int64
	closed           atomic.Int64
}

func (registration *materializerTestRegistration) AttemptID() secret.AttemptID {
	return registration.id
}

func (registration *materializerTestRegistration) Materialize(
	context.Context,
	secret.Scope,
	string,
) (secret.Value, error) {
	registration.materializeCalls.Add(1)
	return secret.Value{}, nil
}

func (registration *materializerTestRegistration) Close(context.Context) error {
	registration.closed.Add(1)
	return nil
}

type effectiveBindingMaterializerFixture struct {
	prepared     *PreparedGeneration
	registry     *runtime.ResourceRegistry
	registration *materializerTestRegistration
	occurrences  map[string]FactoryOccurrence
	constructed  atomic.Int64
}

func newEffectiveBindingMaterializerFixture(
	t *testing.T,
	factories []string,
	candidates map[generation.Domain]generation.PublicationCandidate,
) (*PreparedGeneration, *effectiveBindingMaterializerFixture) {
	t.Helper()
	consumers, err := runtime.NewConsumerBindings([]runtime.ConsumerRecord{{
		ID: "consumer-1",
		Consumer: resource.Consumer{Username: "consumer-1", Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{"header_name": "X-Consumer-ID"},
		}},
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newEffectiveBindingMaterializerFixtureWithConsumers(t, factories, consumers, candidates)
}

func newEffectiveBindingMaterializerFixtureWithConsumers(
	t *testing.T,
	factories []string,
	consumers *runtime.ConsumerBindings,
	candidates ...map[generation.Domain]generation.PublicationCandidate,
) (*PreparedGeneration, *effectiveBindingMaterializerFixture) {
	t.Helper()
	compiler := newTestCompiler(t)
	registration := &materializerTestRegistration{id: secret.AttemptID{byte(materializerFixtureAttempt.Add(1))}}
	capabilityValue, err := secret.NewGenerationCapability(registration, uint64(registration.id[0])+100)
	if err != nil {
		t.Fatal(err)
	}
	occurrenceSpecs := make([]factoryOccurrenceSpec, 0, len(factories))
	for _, factory := range factories {
		occurrenceSpecs = append(occurrenceSpecs, factoryOccurrenceSpec{
			domain:   generation.DomainHTTP,
			resource: generation.ResourceKey{Kind: "plugin_configs", ID: "plugin-config-1"},
			source:   capability.SecretPluginConfig,
			factory:  factory,
		})
	}
	candidateSet := map[generation.Domain]generation.PublicationCandidate{}
	if len(candidates) != 0 && candidates[0] != nil {
		candidateSet = candidates[0]
	}
	attempt, err := newPreparationAttempt(
		capabilityValue.Generation(), candidateSet, capabilityValue, occurrenceSpecs,
	)
	if err != nil {
		t.Fatal(err)
	}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	registry := runtime.NewResourceRegistry()
	observers := workerTestRuntimeObservers()
	clusterObservers, err := newClusterObserverRegistry(observers.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := &cleanupStack{}
	if err := cleanup.Own(cleanupQuiesce, "tasks", func(ctx context.Context) error {
		residuals, err := tasks.Stop(ctx)
		if len(residuals) != 0 {
			return errors.Join(err, errors.New("task residual"))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Own(cleanupRelease, "registration", registration.Close); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Own(cleanupRelease, "consumers", func(context.Context) error {
		consumers.Close()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prepared := &PreparedGeneration{
		publication:      generation.PublicationSet{DesiredRevision: attempt.Generation(), Domains: candidateSet},
		attempt:          attempt,
		consumers:        consumers,
		lookup:           consumerLookupView{bindings: consumers},
		tasks:            tasks,
		effective:        &config.EffectiveConfig{},
		manifest:         compiler.manifest,
		registry:         registry,
		observers:        observers,
		clusterObservers: clusterObservers,
		cleanup:          cleanup,
		bindingOps:       defaultEffectiveBindingOps(),
	}
	fixture := &effectiveBindingMaterializerFixture{
		prepared: prepared, registry: registry, registration: registration,
		occurrences: make(map[string]FactoryOccurrence, len(factories)),
	}
	for _, occurrence := range attempt.Occurrences(capability.SecretPluginConfig) {
		fixture.occurrences[occurrence.Factory()] = occurrence
	}
	defaultNew := prepared.bindingOps.newFactoryInstance
	prepared.bindingOps.newFactoryInstance = func(factory string, dependencies base.Dependencies) (plugin.FactoryInstance, error) {
		fixture.constructed.Add(1)
		return defaultNew(factory, dependencies)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	return prepared, fixture
}

func (fixture *effectiveBindingMaterializerFixture) pluginSpec(factory, routeID string) effectiveBindingSpec {
	occurrence := fixture.occurrences[factory]
	return effectiveBindingSpec{
		domain:         generation.DomainHTTP,
		executionOwner: generation.ResourceKey{Kind: "routes", ID: routeID},
		source: effectiveBindingSource{
			kind: effectiveBindingPluginConfig, resource: occurrence.Resource(),
			source: capability.SecretPluginConfig, occurrence: occurrence,
		},
		factory: factory,
		config:  map[string]any{},
		scope:   plugin.ScopeRoute,
		provenance: plugin.ResourceProvenance{
			Kind: plugin.ResourcePluginConfig, ID: occurrence.Resource().ID,
		},
		resourceContext: effectiveBindingResourceContext{
			kind:    effectiveBindingContextHTTP,
			route:   resource.Route{ID: routeID, Labels: map[string]any{"fixture": routeID}},
			service: resource.Service{ID: "service-1", Hosts: []string{"example.test"}},
		},
	}
}

func (fixture *effectiveBindingMaterializerFixture) systemSpec(factory string) effectiveBindingSpec {
	return effectiveBindingSpec{
		domain:         generation.DomainHTTP,
		executionOwner: generation.ResourceKey{Kind: "system", ID: factory},
		source: effectiveBindingSource{
			kind:     effectiveBindingSystem,
			resource: generation.ResourceKey{Kind: "system", ID: factory},
		},
		factory:    factory,
		config:     map[string]any{},
		scope:      plugin.ScopeSystem,
		provenance: plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: factory},
	}
}

type materializerTrace struct {
	mu     sync.Mutex
	values []string
}

type materializerLifecycleBroker struct {
	mu    sync.Mutex
	trace *materializerTrace
	scope secret.Scope
	raw   string
	calls int
}

func newRealEffectiveBindingPrepared(
	t *testing.T,
	desired generation.Snapshot,
	broker secret.ScopedAttemptBroker,
) (*PreparedGeneration, FactoryOccurrence) {
	t.Helper()
	compiler := newTestCompiler(t)
	materializer := secret.NewScopedMaterializer(broker, compiler.schemas.catalog)
	factory, err := newAttemptFactory(compiler, materializer)
	if err != nil {
		t.Fatal(err)
	}
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	registered, err := factory.prepareCandidateAttempt(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumers, err := runtime.NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	cleanup := &cleanupStack{}
	if err := cleanup.Own(cleanupQuiesce, "tasks", func(ctx context.Context) error {
		residuals, stopErr := tasks.Stop(ctx)
		if len(residuals) != 0 {
			return errors.Join(stopErr, errors.New("task residual"))
		}
		return stopErr
	}); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Own(cleanupRelease, "registration", registered.Close); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Own(cleanupRelease, "consumers", func(context.Context) error {
		consumers.Close()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prepared := &PreparedGeneration{
		publication: registered.publication, attempt: registered.attempt,
		consumers: consumers, lookup: consumerLookupView{bindings: consumers}, tasks: tasks,
		effective: &config.EffectiveConfig{}, manifest: compiler.manifest,
		registry: runtime.NewResourceRegistry(), materializer: materializer, cleanup: cleanup,
		bindingOps: defaultEffectiveBindingOps().withDefaults(registered.attempt.AttemptID()),
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	occurrences := registered.attempt.Occurrences(capability.SecretPluginConfig)
	if len(occurrences) != 1 {
		t.Fatalf("plugin-config occurrences = %#v, want exactly one", occurrences)
	}
	return prepared, occurrences[0]
}

func (*materializerLifecycleBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*materializerLifecycleBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *materializerLifecycleBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	broker.scope = scope
	broker.raw = raw
	broker.calls++
	broker.mu.Unlock()
	broker.trace.record("secret")
	return "Bearer resolved", nil
}

func (*materializerLifecycleBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *materializerLifecycleBroker) reset() {
	broker.mu.Lock()
	broker.scope = secret.Scope{}
	broker.raw = ""
	broker.calls = 0
	broker.mu.Unlock()
	broker.trace.mu.Lock()
	broker.trace.values = nil
	broker.trace.mu.Unlock()
}

func (broker *materializerLifecycleBroker) lastCall() (secret.Scope, string, int) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.scope, broker.raw, broker.calls
}

func (trace *materializerTrace) record(value string) {
	trace.mu.Lock()
	trace.values = append(trace.values, value)
	trace.mu.Unlock()
}

func (trace *materializerTrace) snapshot() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]string(nil), trace.values...)
}

var materializerFixtureAttempt atomic.Uint32

func awaitMaterializerSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func awaitMaterializerResult(t *testing.T, result <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
}

func assertMaterializerSignalBlocked(
	t *testing.T,
	signal <-chan struct{},
	duration time.Duration,
	operation string,
) {
	t.Helper()
	select {
	case <-signal:
		t.Fatalf("unexpected %s", operation)
	case <-time.After(duration):
	}
}
