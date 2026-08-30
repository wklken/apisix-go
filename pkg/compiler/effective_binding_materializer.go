package compiler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	consumerregistry "github.com/wklken/apisix-go/pkg/consumer"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin"
	api_breaker "github.com/wklken/apisix-go/pkg/plugin/api_breaker"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	graphql_proxy_cache "github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_transcode"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

var errEffectiveBindingMaterializationFailed = errors.New("effective binding materialization failed")

type effectiveBindingSourceKind uint8

const (
	effectiveBindingPluginConfig effectiveBindingSourceKind = iota
	effectiveBindingPreparedConsumer
	effectiveBindingSystem
)

type effectiveBindingSource struct {
	kind       effectiveBindingSourceKind
	resource   generation.ResourceKey
	source     capability.SecretDeclarationSource
	occurrence FactoryOccurrence
}

type effectiveBindingContextKind uint8

const (
	effectiveBindingContextNone effectiveBindingContextKind = iota
	effectiveBindingContextHTTP
	effectiveBindingContextStream
)

// effectiveBindingResourceContext is caller-selected Task 7/8 context. It is
// cloned before validation so no caller-owned maps or slices enter a generation.
type effectiveBindingResourceContext struct {
	kind        effectiveBindingContextKind
	route       resource.Route
	service     resource.Service
	streamRoute resource.StreamRoute
}

// effectiveBindingRuntimeContext carries generation-local collaborators that
// affect initialization but not plugin instance identity. Callers must still
// supply all behavioral values through effectiveBindingResourceContext and
// the binding identity fields above.
type effectiveBindingRuntimeContext struct {
	configured        bool
	enabledFactories  []string
	publicAPIRegistry *public_api.Registry
	purgeRegistry     *graphql_proxy_cache.Registry
	serverAddr        string
	proxyCacheZones   []appconfig.Zone
	runtimeAcquirer   traffic_split.RuntimeAcquirer
	upstreamResolver  traffic_split.ResourceUpstreamResolver
	protoResolver     grpc_transcode.ProtoResolver
	apiBreakerState   *api_breaker.State
}

type effectiveBindingSpec struct {
	domain          generation.Domain
	executionOwner  generation.ResourceKey
	source          effectiveBindingSource
	factory         string
	config          resource.PluginConfig
	scope           plugin.Scope
	provenance      plugin.ResourceProvenance
	resourceContext effectiveBindingResourceContext
	runtimeContext  effectiveBindingRuntimeContext
	filterIdentity  any
	errorIdentity   any
}

type effectiveBindingOps struct {
	newFactoryInstance  func(string, base.Dependencies) (plugin.FactoryInstance, error)
	initPlugin          func(plugin.Plugin) error
	validateConfig      func(plugin.Plugin, resource.PluginConfig) error
	decodeConfig        func(resource.PluginConfig, any) error
	applyBootstrap      func(plugin.Plugin, effectiveBindingRuntimeContext)
	preMaterialize      func(plugin.Plugin) error
	applyRouteContext   func(plugin.Plugin, effectiveBindingRuntimeContext, effectiveBindingResourceContext)
	applyContext        func(plugin.Plugin, effectiveBindingResourceContext)
	applyTrafficRuntime func(plugin.Plugin, effectiveBindingRuntimeContext)
	postInit            func(plugin.Plugin) error
	startObserver       func(plugin.Plugin, *runtime.TaskOwner) error
	resolveDescriptor   func(plugin.Descriptor, plugin.Plugin) (plugin.Descriptor, error)
	bind                func(
		plugin.Descriptor,
		plugin.Plugin,
		plugin.Scope,
		plugin.ResourceProvenance,
		plugin.InstanceIdentityInput,
	) (plugin.Binding, error)
	acquire func(
		context.Context,
		*runtime.ResourceRegistry,
		runtime.ResourceKey,
		runtime.ResourceFactory[plugin.Binding],
	) (*runtime.ResourceLease[plugin.Binding], error)
	releaseLease func(*runtime.ResourceLease[plugin.Binding], context.Context) error
	stopPlugin   func(plugin.Plugin)
	trace        func(string)
	closeStarted func()
}

type effectiveBindingAcquisitionSlot struct {
	mu                    sync.Mutex
	operations            effectiveBindingOps
	factory               string
	instance              plugin.Plugin
	lease                 *runtime.ResourceLease[plugin.Binding]
	registryReleased      bool
	cleanupStarted        bool
	stopped               bool
	quiesceOwnershipOnce  sync.Once
	quiesceOwnershipErr   error
	quiesceGenerationOnce sync.Once
	cleanupOnce           sync.Once
	cleanupErr            error
}

type generationTaskQuiescer interface {
	QuiesceGenerationTasks()
}

type validatedEffectiveBindingSpec struct {
	spec       effectiveBindingSpec
	descriptor plugin.Descriptor
	identity   plugin.InstanceIdentityInput
	instance   plugin.InstanceKey
	resource   runtime.ResourceKey
}

type effectiveBindingIdentityConfig struct {
	PluginConfig    resource.PluginConfig           `json:"plugin_config"`
	Source          effectiveBindingSourceIdentity  `json:"source"`
	ResourceContext effectiveBindingContextIdentity `json:"resource_context"`
}

type effectiveBindingSourceIdentity struct {
	Kind     effectiveBindingSourceKind         `json:"kind"`
	Source   capability.SecretDeclarationSource `json:"source"`
	Resource generation.ResourceKey             `json:"resource"`
}

type effectiveBindingContextIdentity struct {
	Kind        effectiveBindingContextKind `json:"kind"`
	Route       resource.Route              `json:"route"`
	Service     resource.Service            `json:"service"`
	StreamRoute resource.StreamRoute        `json:"stream_route"`
}

func (prepared *PreparedGeneration) materializeEffectiveBindings(
	ctx context.Context,
	specs []effectiveBindingSpec,
) ([]plugin.Binding, error) {
	return prepared.materializeEffectiveBindingsWithPolicy(ctx, specs, false, nil)
}

// materializeEffectiveBindingsRecoverable gives HTTP route quarantine an
// isolated cleanup scope. A failed route releases only resources acquired
// after its checkpoint; cleanup failure still terminally closes the generation.
func (prepared *PreparedGeneration) materializeEffectiveBindingsRecoverable(
	ctx context.Context,
	specs []effectiveBindingSpec,
) ([]plugin.Binding, error) {
	return prepared.materializeEffectiveBindingsWithPolicy(ctx, specs, true, nil)
}

func (prepared *PreparedGeneration) materializeEffectiveBindingsRecoverableFinalized(
	ctx context.Context,
	specs []effectiveBindingSpec,
	finalize func([]plugin.Binding) ([]plugin.Binding, error),
) ([]plugin.Binding, error) {
	if finalize == nil {
		return nil, errEffectiveBindingMaterializationFailed
	}
	return prepared.materializeEffectiveBindingsWithPolicy(ctx, specs, true, finalize)
}

func (prepared *PreparedGeneration) materializeEffectiveBindingsWithPolicy(
	ctx context.Context,
	specs []effectiveBindingSpec,
	recoverable bool,
	finalize func([]plugin.Binding) ([]plugin.Binding, error),
) ([]plugin.Binding, error) {
	if prepared == nil {
		return nil, errEffectiveBindingMaterializationFailed
	}
	prepared.materializeMu.Lock()
	if prepared.terminal {
		prepared.materializeMu.Unlock()
		return nil, errEffectiveBindingMaterializationFailed
	}
	if ctx == nil {
		return prepared.failEffectiveBindingMaterializationLocked(
			context.Background(),
			fmt.Errorf("%w: context is required", ErrInvalidInput),
		)
	}
	if err := ctx.Err(); err != nil {
		return prepared.failEffectiveBindingMaterializationLocked(ctx, err)
	}

	prepared.bindingOpsMu.Lock()
	operations := prepared.bindingOps.withDefaults(prepared.preparation.Generation())
	prepared.bindingOpsMu.Unlock()
	validated, err := prepared.validateEffectiveBindingSpecs(specs)
	if err != nil {
		if recoverable {
			prepared.materializeMu.Unlock()
			return nil, recoverableEffectiveBindingError(err)
		}
		return prepared.failEffectiveBindingMaterializationLocked(ctx, err)
	}
	var checkpoint cleanupCheckpoint
	if recoverable {
		checkpoint, err = prepared.cleanup.Checkpoint()
		if err != nil {
			return prepared.failEffectiveBindingMaterializationLocked(ctx, err)
		}
	}
	fail := func(primary error) ([]plugin.Binding, error) {
		if !recoverable {
			return prepared.failEffectiveBindingMaterializationLocked(ctx, primary)
		}
		rollbackErr := prepared.cleanup.Rollback(context.WithoutCancel(ctx), checkpoint)
		if rollbackErr != nil {
			return prepared.failEffectiveBindingMaterializationLocked(
				ctx,
				errors.Join(primary, rollbackErr),
			)
		}
		prepared.materializeMu.Unlock()
		return nil, recoverableEffectiveBindingError(primary)
	}

	bindings := make([]plugin.Binding, 0, len(validated))
	for _, selected := range validated {
		slot := &effectiveBindingAcquisitionSlot{
			operations: operations,
			factory:    selected.spec.factory,
		}
		ownerName := "plugin-binding/" + selected.spec.factory
		if ownErr := prepared.cleanup.Own(cleanupRelease, ownerName, slot.cleanup); ownErr != nil {
			return fail(ownErr)
		}
		lease, acquireErr := prepared.acquireEffectiveBinding(ctx, selected, operations, slot)
		if acquireErr != nil {
			return fail(acquireErr)
		}
		slot.adoptLease(lease)
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		bindings = append(bindings, lease.Value())
	}
	if finalize != nil {
		bindings, err = finalize(bindings)
		if err != nil {
			return fail(err)
		}
	}
	prepared.materializeMu.Unlock()
	return bindings, nil
}

func recoverableEffectiveBindingError(primary error) error {
	causes := []error{errEffectiveBindingMaterializationFailed}
	if errors.Is(primary, context.Canceled) {
		causes = append(causes, context.Canceled)
	}
	if errors.Is(primary, context.DeadlineExceeded) {
		causes = append(causes, context.DeadlineExceeded)
	}
	if len(causes) == 1 && primary != nil {
		causes = append(causes, redactedEffectiveBindingCause{})
	}
	return errors.Join(causes...)
}

func (prepared *PreparedGeneration) validateEffectiveBindingSpecs(
	specs []effectiveBindingSpec,
) ([]validatedEffectiveBindingSpec, error) {
	if len(specs) == 0 || !prepared.preparation.secrets.Valid() ||
		prepared.preparation.Generation() == 0 || prepared.manifest == nil ||
		prepared.registry == nil || prepared.cleanup == nil || prepared.tasks == nil ||
		prepared.effective == nil || prepared.consumers == nil {
		return nil, fmt.Errorf("%w: effective binding owner is incomplete", ErrInvalidInput)
	}
	validated := make([]validatedEffectiveBindingSpec, 0, len(specs))
	seen := make(map[runtime.ResourceKey]struct{}, len(specs))
	for _, supplied := range specs {
		selected, err := prepared.validateEffectiveBindingSpec(supplied)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[selected.resource]; duplicate {
			return nil, fmt.Errorf("%w: duplicate effective binding", ErrInvalidInput)
		}
		seen[selected.resource] = struct{}{}
		validated = append(validated, selected)
	}
	return validated, nil
}

func (prepared *PreparedGeneration) validateEffectiveBindingSpec(
	supplied effectiveBindingSpec,
) (validatedEffectiveBindingSpec, error) {
	if !validEffectiveBindingDomain(supplied.domain) || supplied.factory == "" ||
		!validEffectiveExecutionOwner(supplied.domain, supplied.executionOwner) ||
		isNilInterface(supplied.config) {
		return validatedEffectiveBindingSpec{}, fmt.Errorf("%w: effective binding identity is invalid", ErrInvalidInput)
	}

	entry, exists := prepared.manifest.Plugin(supplied.factory)
	if !exists || !manifestFactorySupportsDomain(entry, supplied.factory, supplied.domain) {
		return validatedEffectiveBindingSpec{}, fmt.Errorf("%w: factory is incompatible with domain", ErrInvalidInput)
	}
	descriptor, err := plugin.DescriptorForFactory(prepared.manifest, supplied.factory)
	if err != nil || !slices.Contains(descriptor.Scopes, supplied.scope) {
		return validatedEffectiveBindingSpec{}, fmt.Errorf("%w: factory is incompatible with scope", ErrInvalidInput)
	}
	if err := prepared.validateEffectiveBindingSource(supplied, entry); err != nil {
		return validatedEffectiveBindingSpec{}, err
	}

	ownedConfig, err := cloneEffectiveBindingValue(supplied.config)
	if err != nil {
		return validatedEffectiveBindingSpec{}, fmt.Errorf(
			"%w: plugin config is not defensively copyable",
			ErrInvalidInput,
		)
	}
	ownedFilter, err := cloneEffectiveBindingValue(supplied.filterIdentity)
	if err != nil {
		return validatedEffectiveBindingSpec{}, fmt.Errorf("%w: filter identity is invalid", ErrInvalidInput)
	}
	ownedError, err := cloneEffectiveBindingValue(supplied.errorIdentity)
	if err != nil {
		return validatedEffectiveBindingSpec{}, fmt.Errorf("%w: error identity is invalid", ErrInvalidInput)
	}
	identityContext := supplied.resourceContext
	resourceOwner := supplied.executionOwner
	if descriptor.InstanceScope == plugin.InstancePerGlobalRule && supplied.scope == plugin.ScopeGlobal {
		if supplied.source.resource.Kind != "global_rules" {
			return validatedEffectiveBindingSpec{}, fmt.Errorf(
				"%w: global-rule instance source is invalid",
				ErrInvalidInput,
			)
		}
		identityContext = effectiveBindingResourceContext{kind: effectiveBindingContextNone}
		resourceOwner = supplied.source.resource
	}
	ownedContext, err := cloneEffectiveBindingContext(supplied.domain, supplied.scope, identityContext)
	if err != nil {
		return validatedEffectiveBindingSpec{}, err
	}
	ownedRuntime, err := cloneEffectiveBindingRuntimeContext(supplied.domain, ownedContext, supplied.runtimeContext)
	if err != nil {
		return validatedEffectiveBindingSpec{}, err
	}
	ownedSpec := supplied
	ownedSpec.config = ownedConfig
	ownedSpec.filterIdentity = ownedFilter
	ownedSpec.errorIdentity = ownedError
	ownedSpec.resourceContext = ownedContext
	ownedSpec.runtimeContext = ownedRuntime
	identity := plugin.InstanceIdentityInput{
		PluginConfig: effectiveBindingIdentityConfig{
			PluginConfig: ownedConfig,
			Source: effectiveBindingSourceIdentity{
				Kind: ownedSpec.source.kind, Source: ownedSpec.source.source,
				Resource: ownedSpec.source.resource,
			},
			ResourceContext: effectiveBindingContextIdentity{
				Kind: ownedContext.kind, Route: ownedContext.route,
				Service: ownedContext.service, StreamRoute: ownedContext.streamRoute,
			},
		},
		Filter:        ownedFilter,
		ErrorResponse: ownedError,
	}
	instance, err := plugin.NewGenerationInstanceKey(
		prepared.preparation.Generation(), descriptor, supplied.scope, supplied.provenance, identity,
	)
	if err != nil {
		return validatedEffectiveBindingSpec{}, fmt.Errorf("%w: plugin instance identity is invalid", ErrInvalidInput)
	}
	resourceKey := effectiveBindingResourceKey(
		supplied.domain, resourceOwner, ownedSpec.source, instance,
	)
	if resourceKey.Kind == "" || resourceKey.Scope == "" || resourceKey.Digest == ([32]byte{}) {
		return validatedEffectiveBindingSpec{}, fmt.Errorf("%w: runtime resource identity is invalid", ErrInvalidInput)
	}
	return validatedEffectiveBindingSpec{
		spec: ownedSpec, descriptor: descriptor, identity: identity,
		instance: instance, resource: resourceKey,
	}, nil
}

func (prepared *PreparedGeneration) validateEffectiveBindingSource(
	spec effectiveBindingSpec,
	entry capability.PluginCapability,
) error {
	source := spec.source
	switch source.kind {
	case effectiveBindingPluginConfig:
		if source.source != capability.SecretPluginConfig ||
			!prepared.preparation.owns(source.occurrence) ||
			source.occurrence.Domain() != spec.domain ||
			source.occurrence.Resource() != source.resource ||
			source.occurrence.Source() != source.source ||
			source.occurrence.Factory() != spec.factory {
			return fmt.Errorf("%w: plugin-config source authority is invalid", ErrInvalidInput)
		}
		wantScope, wantProvenance, ok := effectivePluginSourceIdentity(source.resource)
		if !ok || spec.scope != wantScope || spec.provenance != wantProvenance {
			return fmt.Errorf("%w: plugin-config source was relabeled", ErrInvalidInput)
		}
	case effectiveBindingPreparedConsumer:
		if spec.domain != generation.DomainHTTP || source.source != capability.SecretConsumerConfig ||
			spec.scope != plugin.ScopeConsumer || !prepared.preparation.owns(source.occurrence) ||
			source.occurrence.Domain() != spec.domain ||
			source.occurrence.Resource() != source.resource ||
			source.occurrence.Source() != source.source ||
			source.occurrence.Factory() != spec.factory {
			return fmt.Errorf("%w: prepared-consumer source identity is invalid", ErrInvalidInput)
		}
		if source.resource.Kind != "consumers" ||
			spec.provenance != (plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: source.resource.ID}) {
			return fmt.Errorf("%w: prepared-consumer source is invalid", ErrInvalidInput)
		}
		consumer, exists := prepared.consumers.ConsumerByID(source.resource.ID)
		preparedConfig, found := consumer.Plugins[spec.factory]
		if !exists {
			found = false
		}
		if !found || !reflectEffectiveBindingConfigEqual(
			effectivePreparedConsumerConfig(preparedConfig),
			spec.config,
		) {
			return fmt.Errorf("%w: prepared-consumer config is not authoritative", ErrInvalidInput)
		}
	case effectiveBindingSystem:
		if source.source != "" || validOccurrence(source.occurrence) ||
			spec.scope != plugin.ScopeSystem || source.resource != spec.executionOwner ||
			source.resource.Kind != "system" || source.resource.ID != spec.factory ||
			spec.provenance != (plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: spec.factory}) ||
			(prepared.hasBindingConfigFactoryOccurrence(spec.domain, spec.factory) &&
				spec.factory != "error-log-logger") {
			return fmt.Errorf("%w: system source is not compiler-derived or declares secrets", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: effective binding source is invalid", ErrInvalidInput)
	}
	return nil
}

func effectivePreparedConsumerConfig(config resource.PluginConfig) resource.PluginConfig {
	values, ok := config.(map[string]any)
	if !ok {
		return config
	}
	if _, hasMetadata := values["_meta"]; !hasMetadata {
		return config
	}
	withoutMetadata := make(map[string]any, len(values)-1)
	for name, value := range values {
		if name != "_meta" {
			withoutMetadata[name] = value
		}
	}
	return withoutMetadata
}

func (prepared *PreparedGeneration) acquireEffectiveBinding(
	ctx context.Context,
	selected validatedEffectiveBindingSpec,
	operations effectiveBindingOps,
	slot *effectiveBindingAcquisitionSlot,
) (*runtime.ResourceLease[plugin.Binding], error) {
	return operations.acquire(context.WithoutCancel(ctx), prepared.registry, selected.resource, func(
		context.Context,
	) (binding plugin.Binding, closeBinding func(context.Context) error, resultErr error) {
		taskOwnerPrefix, err := pluginTaskOwnerPrefix(selected.instance)
		if err != nil {
			return plugin.Binding{}, nil, err
		}
		taskOwner, err := runtime.NewTaskOwner(prepared.tasks, taskOwnerPrefix, runtime.TaskPlugin)
		if err != nil {
			return plugin.Binding{}, nil, err
		}
		dependencies := base.Dependencies{
			Config: prepared.effective, Secrets: prepared.preparation.secrets,
			Metadata:  prepared.metadata.ForFactory(selected.spec.factory),
			Consumers: prepared.lookup, Tasks: taskOwner,
		}
		children, err := plugin.NewCompositeChildPreparer(
			dependencies,
			prepared.preparation.Generation(),
			selected.spec.scope,
			selected.spec.provenance,
		)
		if err != nil {
			return plugin.Binding{}, nil, err
		}
		dependencies.CompositeChildren = children
		factoryInstance, err := operations.newFactoryInstance(selected.spec.factory, dependencies)
		if err != nil {
			return plugin.Binding{}, nil, err
		}
		instance := factoryInstance.Plugin()
		slot.adoptPlugin(instance)
		if err := ownEffectiveBindingGenerationTaskQuiescer(prepared.cleanup, slot); err != nil {
			return plugin.Binding{}, nil, err
		}

		if err := operations.initPlugin(instance); err != nil {
			return plugin.Binding{}, nil, err
		}
		if err := operations.validateConfig(instance, selected.spec.config); err != nil {
			return plugin.Binding{}, nil, err
		}
		configuration := instance.Config()
		if configuration == nil {
			return plugin.Binding{}, nil, fmt.Errorf("plugin config destination is unavailable")
		}
		if err := operations.decodeConfig(selected.spec.config, configuration); err != nil {
			return plugin.Binding{}, nil, err
		}
		operations.applyBootstrap(instance, selected.spec.runtimeContext)
		if err := operations.preMaterialize(instance); err != nil {
			return plugin.Binding{}, nil, err
		}
		if selected.spec.source.kind == effectiveBindingPreparedConsumer &&
			!prepared.declaresConsumerScopedSecrets(selected.spec.factory) {
			if preparer, ok := instance.(interface{ PrepareConsumerConfig() error }); ok {
				if err := preparer.PrepareConsumerConfig(); err != nil {
					return plugin.Binding{}, nil, err
				}
			}
		}
		if selected.spec.source.kind == effectiveBindingPluginConfig ||
			(selected.spec.source.kind == effectiveBindingPreparedConsumer &&
				!consumerregistry.Supports(selected.spec.factory) &&
				prepared.declaresConsumerScopedSecrets(selected.spec.factory)) {
			if err := prepared.preparation.PrepareScopedPluginSecrets(
				ctx, selected.spec.source.occurrence, factoryInstance,
			); err != nil {
				return plugin.Binding{}, nil, err
			}
		}
		operations.applyRouteContext(instance, selected.spec.runtimeContext, selected.spec.resourceContext)
		operations.applyContext(instance, selected.spec.resourceContext)
		operations.applyTrafficRuntime(instance, selected.spec.runtimeContext)
		if err := operations.postInit(instance); err != nil {
			return plugin.Binding{}, nil, err
		}
		if err := operations.startObserver(instance, taskOwner); err != nil {
			return plugin.Binding{}, nil, err
		}
		resolved, err := operations.resolveDescriptor(selected.descriptor, instance)
		if err != nil {
			return plugin.Binding{}, nil, err
		}
		binding, err = operations.bind(
			resolved, instance, selected.spec.scope, selected.spec.provenance, selected.identity,
		)
		if err != nil || binding.InstanceKey != selected.instance {
			return plugin.Binding{}, nil, fmt.Errorf("plugin binding identity mismatch")
		}
		return binding, slot.registryRelease, nil
	})
}

func (slot *effectiveBindingAcquisitionSlot) adoptPlugin(instance plugin.Plugin) {
	slot.mu.Lock()
	slot.instance = instance
	slot.mu.Unlock()
}

func (slot *effectiveBindingAcquisitionSlot) adoptLease(lease *runtime.ResourceLease[plugin.Binding]) {
	slot.mu.Lock()
	slot.lease = lease
	slot.mu.Unlock()
}

func ownEffectiveBindingGenerationTaskQuiescer(
	cleanup *cleanupStack,
	slot *effectiveBindingAcquisitionSlot,
) error {
	if cleanup == nil || slot == nil {
		return fmt.Errorf("%w: generation task quiesce owner is incomplete", ErrInvalidInput)
	}
	slot.mu.Lock()
	instance := slot.instance
	slot.mu.Unlock()
	quiescer, ok := instance.(generationTaskQuiescer)
	if !ok {
		return nil
	}
	slot.quiesceOwnershipOnce.Do(func() {
		slot.quiesceOwnershipErr = cleanup.Own(
			cleanupQuiesce,
			"plugin-generation-tasks/"+slot.factory,
			func(context.Context) error {
				slot.quiesceGenerationOnce.Do(quiescer.QuiesceGenerationTasks)
				return nil
			},
		)
	})
	return slot.quiesceOwnershipErr
}

func (slot *effectiveBindingAcquisitionSlot) registryRelease(context.Context) error {
	slot.mu.Lock()
	slot.registryReleased = true
	cleanupStarted := slot.cleanupStarted
	slot.mu.Unlock()
	if cleanupStarted {
		slot.stopPlugin()
	}
	return nil
}

func (slot *effectiveBindingAcquisitionSlot) cleanup(ctx context.Context) error {
	slot.cleanupOnce.Do(func() {
		slot.mu.Lock()
		slot.cleanupStarted = true
		lease := slot.lease
		stopPartial := lease == nil || slot.registryReleased
		slot.mu.Unlock()
		if stopPartial {
			slot.stopPlugin()
		}
		if lease != nil {
			slot.operations.record("lease-release:" + slot.factory)
			slot.cleanupErr = slot.operations.releaseLease(lease, ctx)
		}
	})
	return slot.cleanupErr
}

func (slot *effectiveBindingAcquisitionSlot) stopPlugin() {
	slot.mu.Lock()
	if slot.stopped || isNilInterface(slot.instance) {
		slot.mu.Unlock()
		return
	}
	instance := slot.instance
	slot.stopped = true
	slot.mu.Unlock()
	slot.operations.record("stop:" + slot.factory)
	slot.operations.stopPlugin(instance)
}

func (prepared *PreparedGeneration) failEffectiveBindingMaterializationLocked(
	ctx context.Context,
	primary error,
) ([]plugin.Binding, error) {
	prepared.terminal = true
	prepared.materializeMu.Unlock()
	cleanupErr := prepared.Close(context.WithoutCancel(ctx))
	redacted := []error{errEffectiveBindingMaterializationFailed}
	if primary != nil {
		contextIdentity := false
		if errors.Is(primary, context.Canceled) {
			redacted = append(redacted, context.Canceled)
			contextIdentity = true
		}
		if errors.Is(primary, context.DeadlineExceeded) {
			redacted = append(redacted, context.DeadlineExceeded)
			contextIdentity = true
		}
		if !contextIdentity {
			redacted = append(redacted, redactedEffectiveBindingCause{})
		}
	}
	if cleanupErr != nil {
		redacted = append(redacted, redactedEffectiveBindingCleanup{})
	}
	return nil, errors.Join(redacted...)
}

type redactedEffectiveBindingCause struct{}

func (redactedEffectiveBindingCause) Error() string { return "effective binding cause was redacted" }

type redactedEffectiveBindingCleanup struct{}

func (redactedEffectiveBindingCleanup) Error() string { return "effective binding cleanup failed" }

func defaultEffectiveBindingOps() effectiveBindingOps {
	operations := effectiveBindingOps{}.withDefaults(0)
	// Binding is the only default that captures generation identity. Leave it
	// unset until the generation supplies its number.
	operations.bind = nil
	return operations
}

func (operations effectiveBindingOps) withDefaults(generationNumber uint64) effectiveBindingOps {
	if operations.newFactoryInstance == nil {
		operations.newFactoryInstance = plugin.NewFactoryInstance
	}
	if operations.initPlugin == nil {
		operations.initPlugin = func(instance plugin.Plugin) error { return instance.Init() }
	}
	if operations.validateConfig == nil {
		operations.validateConfig = func(instance plugin.Plugin, config resource.PluginConfig) error {
			compiled, err := util.CompileSchema(instance.GetSchema())
			if err != nil {
				return err
			}
			return compiled.Validate(config)
		}
	}
	if operations.decodeConfig == nil {
		operations.decodeConfig = func(source resource.PluginConfig, destination any) error {
			return util.Parse(source, destination)
		}
	}
	if operations.applyBootstrap == nil {
		operations.applyBootstrap = func(instance plugin.Plugin, value effectiveBindingRuntimeContext) {
			if !value.configured {
				return
			}
			if setter, ok := instance.(interface{ SetPluginEnabledChecker(func(string) bool) }); ok {
				enabled := slices.Clone(value.enabledFactories)
				setter.SetPluginEnabledChecker(func(factory string) bool {
					_, found := slices.BinarySearch(enabled, factory)
					return found
				})
			}
			if setter, ok := instance.(interface{ SetPublicAPIRegistry(*public_api.Registry) }); ok {
				setter.SetPublicAPIRegistry(value.publicAPIRegistry)
			}
			if setter, ok := instance.(interface {
				SetPurgeRegistry(*graphql_proxy_cache.Registry)
			}); ok {
				setter.SetPurgeRegistry(value.purgeRegistry)
			}
			if setter, ok := instance.(interface{ SetConfiguredZones([]appconfig.Zone) }); ok {
				setter.SetConfiguredZones(slices.Clone(value.proxyCacheZones))
			}
			if setter, ok := instance.(interface {
				SetProtoResolver(grpc_transcode.ProtoResolver)
			}); ok {
				setter.SetProtoResolver(value.protoResolver)
			}
			if setter, ok := instance.(interface{ SetState(*api_breaker.State) }); ok &&
				value.apiBreakerState != nil {
				setter.SetState(value.apiBreakerState)
			}
		}
	}
	if operations.preMaterialize == nil {
		operations.preMaterialize = func(instance plugin.Plugin) error {
			if validator, ok := instance.(interface{ ValidatePreMaterialization() error }); ok {
				return validator.ValidatePreMaterialization()
			}
			return nil
		}
	}
	if operations.applyRouteContext == nil {
		operations.applyRouteContext = func(
			instance plugin.Plugin,
			value effectiveBindingRuntimeContext,
			resourceContext effectiveBindingResourceContext,
		) {
			if !value.configured || resourceContext.kind != effectiveBindingContextHTTP {
				return
			}
			if setter, ok := instance.(interface{ SetRouteContext(string, string) }); ok {
				setter.SetRouteContext(resourceContext.route.ID, value.serverAddr)
			}
		}
	}
	if operations.applyTrafficRuntime == nil {
		operations.applyTrafficRuntime = func(instance plugin.Plugin, value effectiveBindingRuntimeContext) {
			if !value.configured {
				return
			}
			if setter, ok := instance.(interface {
				SetRuntimeAcquirer(traffic_split.RuntimeAcquirer)
			}); ok {
				setter.SetRuntimeAcquirer(value.runtimeAcquirer)
			}
			if setter, ok := instance.(interface {
				SetUpstreamResolver(traffic_split.ResourceUpstreamResolver)
			}); ok {
				setter.SetUpstreamResolver(value.upstreamResolver)
			}
		}
	}
	if operations.applyContext == nil {
		operations.applyContext = func(instance plugin.Plugin, value effectiveBindingResourceContext) {
			if value.kind != effectiveBindingContextHTTP {
				return
			}
			if setter, ok := instance.(interface {
				SetResourceContext(resource.Route, resource.Service)
			}); ok {
				setter.SetResourceContext(value.route, value.service)
			}
		}
	}
	if operations.postInit == nil {
		operations.postInit = func(instance plugin.Plugin) error { return instance.PostInit() }
	}
	if operations.startObserver == nil {
		operations.startObserver = func(instance plugin.Plugin, tasks *runtime.TaskOwner) error {
			observer, ok := instance.(interface {
				StartObservingWithTasks(*runtime.TaskOwner) error
			})
			if !ok {
				return nil
			}
			return observer.StartObservingWithTasks(tasks)
		}
	}
	if operations.resolveDescriptor == nil {
		operations.resolveDescriptor = plugin.ResolveDescriptor
	}
	if operations.bind == nil {
		operations.bind = func(
			descriptor plugin.Descriptor,
			instance plugin.Plugin,
			scope plugin.Scope,
			provenance plugin.ResourceProvenance,
			identity plugin.InstanceIdentityInput,
		) (plugin.Binding, error) {
			return plugin.BindGenerationResolvedPlugin(
				generationNumber, descriptor, instance, scope, provenance, identity,
			)
		}
	}
	if operations.acquire == nil {
		operations.acquire = runtime.Acquire[plugin.Binding]
	}
	if operations.releaseLease == nil {
		operations.releaseLease = func(
			lease *runtime.ResourceLease[plugin.Binding],
			ctx context.Context,
		) error {
			return lease.Release(ctx)
		}
	}
	if operations.stopPlugin == nil {
		operations.stopPlugin = func(instance plugin.Plugin) {
			if stopper, ok := instance.(interface{ Stop() }); ok {
				stopper.Stop()
			}
		}
	}
	return operations
}

func (operations effectiveBindingOps) record(value string) {
	if operations.trace != nil {
		operations.trace(value)
	}
}

func effectivePluginSourceIdentity(
	key generation.ResourceKey,
) (plugin.Scope, plugin.ResourceProvenance, bool) {
	switch key.Kind {
	case "routes", "stream_routes":
		return plugin.ScopeRoute, plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: key.ID}, true
	case "services":
		return plugin.ScopeRoute, plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: key.ID}, true
	case "plugin_configs":
		return plugin.ScopeRoute, plugin.ResourceProvenance{Kind: plugin.ResourcePluginConfig, ID: key.ID}, true
	case "global_rules":
		return plugin.ScopeGlobal, plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: key.ID}, true
	case "consumer_groups":
		return plugin.ScopeConsumer, plugin.ResourceProvenance{Kind: plugin.ResourceConsumerGroup, ID: key.ID}, true
	default:
		return 0, plugin.ResourceProvenance{}, false
	}
}

func validEffectiveBindingDomain(domain generation.Domain) bool {
	return domain == generation.DomainHTTP || domain == generation.DomainStream
}

func validEffectiveExecutionOwner(domain generation.Domain, owner generation.ResourceKey) bool {
	if owner.Kind == "" || owner.ID == "" {
		return false
	}
	if owner.Kind == "system" {
		return true
	}
	return slices.Contains(generation.DomainsForResourceKind(owner.Kind), domain)
}

func manifestFactorySupportsDomain(
	entry capability.PluginCapability,
	factory string,
	domain generation.Domain,
) bool {
	declared := slices.ContainsFunc(entry.Factories, func(candidate capability.Factory) bool {
		return candidate.Key == factory
	})
	want := capability.DomainHTTP
	if domain == generation.DomainStream {
		want = capability.DomainStream
	}
	return declared && slices.Contains(entry.Domains, want)
}

func (prepared *PreparedGeneration) hasBindingConfigFactoryOccurrence(
	domain generation.Domain,
	factory string,
) bool {
	for _, source := range []capability.SecretDeclarationSource{
		capability.SecretPluginConfig,
		capability.SecretConsumerConfig,
	} {
		for _, occurrence := range prepared.preparation.Occurrences(source) {
			if occurrence.Domain() == domain && occurrence.Factory() == factory {
				return true
			}
		}
	}
	return false
}

func (prepared *PreparedGeneration) declaresConsumerScopedSecrets(factory string) bool {
	if prepared == nil || prepared.manifest == nil || factory == "" {
		return false
	}
	entry, exists := prepared.manifest.Plugin(factory)
	if !exists {
		return false
	}
	return slices.ContainsFunc(entry.SecretDeclarations, func(declaration capability.SecretDeclaration) bool {
		return declaration.Source == capability.SecretConsumerConfig
	})
}

func cloneEffectiveBindingContext(
	domain generation.Domain,
	scope plugin.Scope,
	value effectiveBindingResourceContext,
) (effectiveBindingResourceContext, error) {
	var cloned effectiveBindingResourceContext
	cloned.kind = value.kind
	switch value.kind {
	case effectiveBindingContextNone:
		if scope != plugin.ScopeSystem && scope != plugin.ScopeGlobal {
			return effectiveBindingResourceContext{}, fmt.Errorf("%w: resource context is required", ErrInvalidInput)
		}
	case effectiveBindingContextHTTP:
		if domain != generation.DomainHTTP || value.route.ID == "" {
			return effectiveBindingResourceContext{}, fmt.Errorf(
				"%w: HTTP resource context is invalid",
				ErrInvalidInput,
			)
		}
		var err error
		cloned.route, err = cloneEffectiveRoute(value.route)
		if err != nil {
			return effectiveBindingResourceContext{}, fmt.Errorf("%w: HTTP route context is invalid", ErrInvalidInput)
		}
		cloned.service, err = cloneEffectiveService(value.service)
		if err != nil {
			return effectiveBindingResourceContext{}, fmt.Errorf("%w: HTTP service context is invalid", ErrInvalidInput)
		}
	case effectiveBindingContextStream:
		if domain != generation.DomainStream || value.streamRoute.ID == "" {
			return effectiveBindingResourceContext{}, fmt.Errorf(
				"%w: stream resource context is invalid",
				ErrInvalidInput,
			)
		}
		var err error
		cloned.streamRoute, err = cloneEffectiveStreamRoute(value.streamRoute)
		if err != nil {
			return effectiveBindingResourceContext{}, fmt.Errorf("%w: stream route context is invalid", ErrInvalidInput)
		}
		cloned.service, err = cloneEffectiveService(value.service)
		if err != nil {
			return effectiveBindingResourceContext{}, fmt.Errorf(
				"%w: stream service context is invalid",
				ErrInvalidInput,
			)
		}
	default:
		return effectiveBindingResourceContext{}, fmt.Errorf("%w: resource context kind is invalid", ErrInvalidInput)
	}
	return cloned, nil
}

func cloneEffectiveBindingRuntimeContext(
	domain generation.Domain,
	resourceContext effectiveBindingResourceContext,
	value effectiveBindingRuntimeContext,
) (effectiveBindingRuntimeContext, error) {
	if !value.configured {
		return effectiveBindingRuntimeContext{}, nil
	}
	if domain != generation.DomainHTTP || value.publicAPIRegistry == nil ||
		(resourceContext.kind != effectiveBindingContextHTTP && resourceContext.kind != effectiveBindingContextNone) ||
		(resourceContext.kind == effectiveBindingContextHTTP &&
			(value.runtimeAcquirer == nil || value.upstreamResolver == nil)) {
		return effectiveBindingRuntimeContext{}, fmt.Errorf(
			"%w: HTTP runtime context is incomplete",
			ErrInvalidInput,
		)
	}
	enabled := slices.Clone(value.enabledFactories)
	slices.Sort(enabled)
	if slices.Contains(enabled, "") {
		return effectiveBindingRuntimeContext{}, fmt.Errorf(
			"%w: enabled plugin factory is invalid",
			ErrInvalidInput,
		)
	}
	enabled = slices.Compact(enabled)
	return effectiveBindingRuntimeContext{
		configured:        true,
		enabledFactories:  enabled,
		publicAPIRegistry: value.publicAPIRegistry,
		purgeRegistry:     value.purgeRegistry,
		serverAddr:        value.serverAddr,
		proxyCacheZones:   slices.Clone(value.proxyCacheZones),
		runtimeAcquirer:   value.runtimeAcquirer,
		upstreamResolver:  value.upstreamResolver,
		protoResolver:     value.protoResolver,
		apiBreakerState:   value.apiBreakerState,
	}, nil
}

func cloneEffectiveRoute(source resource.Route) (resource.Route, error) {
	cloned := source
	cloned.Uris = slices.Clone(source.Uris)
	cloned.Methods = slices.Clone(source.Methods)
	cloned.Hosts = slices.Clone(source.Hosts)
	cloned.RemoteAddrs = slices.Clone(source.RemoteAddrs)
	cloned.Vars = slices.Clone(source.Vars)
	cloned.Script = slices.Clone(source.Script)
	cloned.ScriptID = slices.Clone(source.ScriptID)
	var err error
	cloned.Plugins, err = cloneEffectivePluginConfigs(source.Plugins)
	if err != nil {
		return resource.Route{}, err
	}
	cloned.Labels, err = cloneEffectiveStringAnyMap(source.Labels)
	if err != nil {
		return resource.Route{}, err
	}
	cloned.Upstream, err = cloneEffectiveUpstream(source.Upstream)
	if err != nil {
		return resource.Route{}, err
	}
	return cloned, nil
}

func cloneEffectiveStreamRoute(source resource.StreamRoute) (resource.StreamRoute, error) {
	cloned := source
	var err error
	cloned.Plugins, err = cloneEffectivePluginConfigs(source.Plugins)
	if err != nil {
		return resource.StreamRoute{}, err
	}
	cloned.Upstream, err = cloneEffectiveUpstream(source.Upstream)
	if err != nil {
		return resource.StreamRoute{}, err
	}
	return cloned, nil
}

func cloneEffectiveService(source resource.Service) (resource.Service, error) {
	cloned := source
	var err error
	cloned.Plugins, err = cloneEffectivePluginConfigs(source.Plugins)
	if err != nil {
		return resource.Service{}, err
	}
	cloned.Hosts = slices.Clone(source.Hosts)
	cloned.Upstream, err = cloneEffectiveUpstream(source.Upstream)
	if err != nil {
		return resource.Service{}, err
	}
	return cloned, nil
}

func cloneEffectiveUpstream(source resource.Upstream) (resource.Upstream, error) {
	cloned := source
	cloned.Nodes = slices.Clone(source.Nodes)
	var err error
	cloned.Checks, err = cloneEffectiveStringAnyMap(source.Checks)
	if err != nil {
		return resource.Upstream{}, err
	}
	if source.TLS != nil {
		tls := *source.TLS
		clientCertID, cloneErr := cloneEffectiveBindingValue(source.TLS.ClientCertID)
		if cloneErr != nil {
			return resource.Upstream{}, cloneErr
		}
		tls.ClientCertID = clientCertID
		cloned.TLS = &tls
	}
	return cloned, nil
}

func cloneEffectivePluginConfigs(
	source map[string]resource.PluginConfig,
) (map[string]resource.PluginConfig, error) {
	if source == nil {
		return nil, nil
	}
	cloned := make(map[string]resource.PluginConfig, len(source))
	for factory, value := range source {
		owned, err := cloneEffectiveBindingValue(value)
		if err != nil {
			return nil, err
		}
		cloned[factory] = owned
	}
	return cloned, nil
}

func cloneEffectiveStringAnyMap(source map[string]any) (map[string]any, error) {
	if source == nil {
		return nil, nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		owned, err := cloneEffectiveBindingValue(value)
		if err != nil {
			return nil, err
		}
		cloned[key] = owned
	}
	return cloned, nil
}

func cloneEffectiveBindingValue(value any) (any, error) {
	return cloneEffectiveBindingValuePath(value, make(map[effectiveBindingReferenceIdentity]struct{}))
}

type effectiveBindingReferenceIdentity struct {
	kind     reflect.Kind
	pointer  uintptr
	length   int
	capacity int
}

func cloneEffectiveBindingValuePath(
	value any,
	path map[effectiveBindingReferenceIdentity]struct{},
) (any, error) {
	identity, tracked := effectiveBindingValueIdentity(value)
	if tracked {
		if _, cyclic := path[identity]; cyclic {
			return nil, fmt.Errorf("cyclic effective-binding JSON value %T is unsupported", value)
		}
		path[identity] = struct{}{}
		defer delete(path, identity)
	}
	switch value := value.(type) {
	case nil:
		return nil, nil
	case bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return value, nil
	case json.RawMessage:
		return slices.Clone(value), nil
	case map[string]any:
		cloned := make(map[string]any, len(value))
		for key, nested := range value {
			owned, err := cloneEffectiveBindingValuePath(nested, path)
			if err != nil {
				return nil, err
			}
			cloned[key] = owned
		}
		return cloned, nil
	case []any:
		cloned := make([]any, len(value))
		for index, nested := range value {
			owned, err := cloneEffectiveBindingValuePath(nested, path)
			if err != nil {
				return nil, err
			}
			cloned[index] = owned
		}
		return cloned, nil
	default:
		return nil, fmt.Errorf("unsupported effective-binding JSON value %T", value)
	}
}

func effectiveBindingValueIdentity(value any) (effectiveBindingReferenceIdentity, bool) {
	switch value := value.(type) {
	case map[string]any:
		if value == nil {
			return effectiveBindingReferenceIdentity{}, false
		}
		return effectiveBindingReferenceIdentity{
			kind: reflect.Map, pointer: reflect.ValueOf(value).Pointer(),
		}, true
	case []any:
		if value == nil {
			return effectiveBindingReferenceIdentity{}, false
		}
		return effectiveBindingReferenceIdentity{
			kind: reflect.Slice, pointer: reflect.ValueOf(value).Pointer(),
			length: len(value), capacity: cap(value),
		}, true
	default:
		return effectiveBindingReferenceIdentity{}, false
	}
}

func reflectEffectiveBindingConfigEqual(left, right resource.PluginConfig) bool {
	leftKey, err := plugin.NewInstanceKey(
		plugin.Descriptor{Factory: "effective-binding-config"},
		plugin.ScopeSystem,
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "comparison"},
		plugin.InstanceIdentityInput{PluginConfig: left},
	)
	if err != nil {
		return false
	}
	rightKey, err := plugin.NewInstanceKey(
		plugin.Descriptor{Factory: "effective-binding-config"},
		plugin.ScopeSystem,
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "comparison"},
		plugin.InstanceIdentityInput{PluginConfig: right},
	)
	return err == nil && leftKey.ConfigDigest == rightKey.ConfigDigest
}

func effectiveBindingResourceKey(
	domain generation.Domain,
	owner generation.ResourceKey,
	source effectiveBindingSource,
	instance plugin.InstanceKey,
) runtime.ResourceKey {
	canonical, err := json.Marshal(struct {
		Version        string                         `json:"version"`
		Domain         generation.Domain              `json:"domain"`
		ExecutionOwner generation.ResourceKey         `json:"execution_owner"`
		Source         effectiveBindingSourceIdentity `json:"source"`
		Factory        string                         `json:"factory"`
		Generation     uint64                         `json:"generation"`
		Scope          plugin.Scope                   `json:"scope"`
		Provenance     plugin.ResourceProvenance      `json:"provenance"`
		ConfigDigest   [32]byte                       `json:"config_digest"`
	}{
		Version: "generation-effective-binding/v1", Domain: domain, ExecutionOwner: owner,
		Source: effectiveBindingSourceIdentity{
			Kind: source.kind, Source: source.source, Resource: source.resource,
		},
		Factory: instance.Factory, Generation: instance.Generation, Scope: instance.Scope,
		Provenance: instance.Owner, ConfigDigest: instance.ConfigDigest,
	})
	if err != nil {
		return runtime.ResourceKey{}
	}
	return runtime.ResourceKey{
		Kind: "plugin-binding", Scope: "generation-effective-binding/v1", Digest: sha256.Sum256(canonical),
	}
}
