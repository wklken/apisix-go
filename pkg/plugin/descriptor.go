package plugin

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// Phase is one APISIX execution phase declared by the capability manifest.
type Phase string

const (
	PhaseRewrite         Phase = "rewrite"
	PhaseConsumerRewrite Phase = "consumer_rewrite"
	PhaseAccess          Phase = "access"
	PhaseBeforeProxy     Phase = "before_proxy"
	PhaseHeaderFilter    Phase = "header_filter"
	PhaseBodyFilter      Phase = "body_filter"
	PhaseLog             Phase = "log"
	PhaseFinalizer       Phase = "finalizer"
	PhaseProtocol        Phase = "protocol"
)

type InstanceScope string

const (
	InstancePerRoute        InstanceScope = "route"
	InstancePerService      InstanceScope = "service"
	InstancePerConsumer     InstanceScope = "consumer"
	InstancePerGlobalRule   InstanceScope = "global_rule"
	InstanceEffectiveConfig InstanceScope = "effective-config"
)

type ResponseOwnerKind uint8

const (
	ResponseOwnerNone ResponseOwnerKind = iota
	ResponseOwnerHeaderFilter
	ResponseOwnerBufferedBodyFilter
	ResponseOwnerFinalStore
	ResponseOwnerStreamingHeaderFilter
	ResponseOwnerStreamingBodyFilter
	ResponseOwnerCompressionOffer
	ResponseOwnerStreamingProducer
	ResponseOwnerExclusiveProtocol
	ResponseOwnerSeparateSubsystemProtocol
)

type FinalizerKind uint8

const (
	FinalizerNone FinalizerKind = iota
	FinalizerSnapshot
	FinalizerDynamic
)

type GenerationOwnerKind uint8

const (
	GenerationOwnerNone GenerationOwnerKind = iota
	GenerationOwnerProcess
	GenerationOwnerRoute
)

type CompressionSet uint8

const (
	CompressionOfferGzip CompressionSet = 1 << iota
	CompressionOfferDeflate
	CompressionOfferBrotli
)

type ResolvedResponsePhases struct {
	Owners            []ResponseOwnerKind
	CompressionOffers CompressionSet
	ExclusiveProtocol ProtocolKind
}

// Descriptor is the immutable runtime view of one manifest factory entry.
// Phases, Priority, Scopes and InstanceScope come only from the manifest;
// ResolveDescriptor may narrow phases for one initialized config.
type Descriptor struct {
	Factory        string
	Implementation string
	Phases         []Phase
	Priority       int
	Scopes         []Scope
	InstanceScope  InstanceScope

	requestStage          RequestStage
	authenticatesConsumer bool
	response              ResolvedResponsePhases
	responseCapability    ResponseCapability
	finalizer             FinalizerKind
	generationOwner       GenerationOwnerKind
	conditionalTerminal   bool
	separateSubsystem     bool
	phaseSelection        base.BindingPhaseDescriptor
	configAware           bool
	resolved              bool
}

var runtimeManifest struct {
	once     sync.Once
	manifest *capability.Manifest
	err      error
}

func descriptorForRuntimeFactory(factory string, p Plugin) (Descriptor, error) {
	runtimeManifest.once.Do(func() {
		runtimeManifest.manifest, runtimeManifest.err = capability.Load()
	})
	if runtimeManifest.err != nil {
		return Descriptor{}, fmt.Errorf("load plugin capability manifest: %w", runtimeManifest.err)
	}
	descriptor, err := DescriptorForFactory(runtimeManifest.manifest, factory)
	if err != nil {
		return Descriptor{}, err
	}
	return ResolveDescriptor(descriptor, p)
}

// ResolveDescriptorForFactory loads the embedded manifest and resolves one
// initialized plugin for compiler-owned materialization.
func ResolveDescriptorForFactory(factory string, p Plugin) (Descriptor, error) {
	return descriptorForRuntimeFactory(factory, p)
}

func DescriptorForFactory(manifest *capability.Manifest, factory string) (Descriptor, error) {
	if manifest == nil {
		return Descriptor{}, errors.New("plugin descriptor: manifest is required")
	}
	entry, ok := manifest.Plugin(factory)
	if !ok {
		return Descriptor{}, fmt.Errorf("plugin descriptor: unknown factory %q", factory)
	}
	factoryDeclared := false
	for _, candidate := range entry.Factories {
		if candidate.Key == factory {
			factoryDeclared = true
			break
		}
	}
	if !factoryDeclared {
		return Descriptor{}, fmt.Errorf(
			"plugin descriptor: key %q is not a factory declared by capability %q",
			factory,
			entry.Name,
		)
	}
	phases, err := parseManifestPhases(entry.Phases)
	if err != nil {
		return Descriptor{}, fmt.Errorf("plugin descriptor %q: %w", factory, err)
	}
	scopes, err := parseManifestScopes(entry.Scopes)
	if err != nil {
		return Descriptor{}, fmt.Errorf("plugin descriptor %q: %w", factory, err)
	}
	instanceScope, err := parseInstanceScope(entry.InstanceScope)
	if err != nil {
		return Descriptor{}, fmt.Errorf("plugin descriptor %q: %w", factory, err)
	}
	return Descriptor{
		Factory:             factory,
		Implementation:      entry.Implementation,
		Phases:              phases,
		Priority:            entry.Priority,
		Scopes:              scopes,
		InstanceScope:       instanceScope,
		requestStage:        requestStageForPhases(phases),
		conditionalTerminal: entry.ConditionalTerminal,
	}, nil
}

func parseManifestPhases(values []string) ([]Phase, error) {
	phases := make([]Phase, 0, len(values))
	for _, value := range values {
		phase := Phase(value)
		switch phase {
		case PhaseRewrite, PhaseConsumerRewrite, PhaseAccess, PhaseBeforeProxy,
			PhaseHeaderFilter, PhaseBodyFilter, PhaseLog, PhaseFinalizer, PhaseProtocol:
		default:
			return nil, fmt.Errorf("unknown manifest phase %q", value)
		}
		if slices.Contains(phases, phase) {
			return nil, fmt.Errorf("duplicate manifest phase %q", value)
		}
		phases = append(phases, phase)
	}
	return phases, nil
}

func parseManifestScopes(values []string) ([]Scope, error) {
	scopes := make([]Scope, 0, len(values))
	for _, value := range values {
		var scope Scope
		switch value {
		case "system":
			scope = ScopeSystem
		case "global_rule":
			scope = ScopeGlobal
		case "route", "service":
			scope = ScopeRoute
		case "consumer", "consumer_group":
			scope = ScopeConsumer
		default:
			return nil, fmt.Errorf("unknown manifest scope %q", value)
		}
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	return scopes, nil
}

func parseInstanceScope(value string) (InstanceScope, error) {
	scope := InstanceScope(value)
	switch scope {
	case InstancePerRoute, InstancePerService, InstancePerConsumer,
		InstancePerGlobalRule, InstanceEffectiveConfig:
		return scope, nil
	case "":
		return "", nil
	default:
		return "", fmt.Errorf("unknown instance scope %q", value)
	}
}

func requestStageForPhases(phases []Phase) RequestStage {
	for _, candidate := range []struct {
		phase Phase
		stage RequestStage
	}{
		{PhaseRewrite, RequestStageRewrite},
		{PhaseConsumerRewrite, RequestStageConsumerRewrite},
		{PhaseAccess, RequestStageAccess},
		{PhaseBeforeProxy, RequestStageBeforeProxy},
	} {
		if slices.Contains(phases, candidate.phase) {
			return candidate.stage
		}
	}
	return RequestStageNone
}

func parseRequestStage(stage string) (RequestStage, error) {
	switch stage {
	case "", "legacy":
		return RequestStageLegacy, nil
	case "none":
		return RequestStageNone, nil
	case "rewrite":
		return RequestStageRewrite, nil
	case "consumer_rewrite":
		return RequestStageConsumerRewrite, nil
	case "access":
		return RequestStageAccess, nil
	case "before_proxy":
		return RequestStageBeforeProxy, nil
	default:
		return RequestStageLegacy, fmt.Errorf("unsupported request stage %q", stage)
	}
}

func validateBindingPhaseDescriptor(factory string, descriptor base.BindingPhaseDescriptor) error {
	if _, err := parseRequestStage(descriptor.RequestStage); err != nil {
		return fmt.Errorf("factory %q: %w", factory, err)
	}
	if !responseFactoryAllowsDescriptor(factory, descriptor) {
		return fmt.Errorf("factory %q descriptor declares an unsupported response phase", factory)
	}
	return nil
}

func phaseForRequestStage(stage RequestStage) Phase {
	switch stage {
	case RequestStageRewrite:
		return PhaseRewrite
	case RequestStageConsumerRewrite:
		return PhaseConsumerRewrite
	case RequestStageAccess:
		return PhaseAccess
	case RequestStageBeforeProxy:
		return PhaseBeforeProxy
	default:
		return ""
	}
}

func (d Descriptor) HasPhase(phase Phase) bool {
	return slices.Contains(d.Phases, phase)
}

func (d Descriptor) RequestStage() RequestStage { return d.requestStage }

func (d Descriptor) HasResponseOwner(owner ResponseOwnerKind) bool {
	return slices.Contains(d.response.Owners, owner)
}

func (d Descriptor) ResponseCapability() ResponseCapability { return d.responseCapability }

func manifestRequestStage(factory string) (RequestStage, error) {
	runtimeManifest.once.Do(func() {
		runtimeManifest.manifest, runtimeManifest.err = capability.Load()
	})
	if runtimeManifest.err != nil {
		return RequestStageNone, runtimeManifest.err
	}
	descriptor, err := DescriptorForFactory(runtimeManifest.manifest, factory)
	if err != nil {
		return RequestStageNone, err
	}
	return descriptor.requestStage, nil
}

func (d Descriptor) OwnsSnapshotFinalizer() bool { return d.finalizer == FinalizerSnapshot }

// ResolveDescriptor resolves config-aware phase selection once, while a
// binding is constructed. It never permits config to add a phase absent from
// the capability manifest.
func ResolveDescriptor(descriptor Descriptor, p Plugin) (Descriptor, error) {
	if p == nil {
		return Descriptor{}, fmt.Errorf("plugin descriptor %q: plugin is nil", descriptor.Factory)
	}
	resolved := descriptor
	resolved.Phases = append([]Phase(nil), descriptor.Phases...)
	resolved.Scopes = append([]Scope(nil), descriptor.Scopes...)
	responseSpec, configAware := responseFactoryRegistry[descriptor.Factory]
	if configAware && responseSpec.configAware {
		describer, ok := p.Config().(base.BindingPhaseDescriber)
		if !ok {
			return Descriptor{}, fmt.Errorf(
				"plugin descriptor %q requires a binding phase descriptor",
				descriptor.Factory,
			)
		}
		selection, err := describer.DescribeBindingPhases()
		if err != nil {
			return Descriptor{}, fmt.Errorf(
				"plugin descriptor %q binding phases: %w",
				descriptor.Factory,
				err,
			)
		}
		if err := narrowDescriptorPhases(&resolved, selection); err != nil {
			return Descriptor{}, err
		}
		resolved.phaseSelection = selection
		resolved.configAware = true
	}
	resolved.requestStage = requestStageForPhases(resolved.Phases)
	if resolved.configAware && resolved.phaseSelection.RequestStage != "" {
		stage, stageErr := parseRequestStage(resolved.phaseSelection.RequestStage)
		if stageErr != nil {
			return Descriptor{}, fmt.Errorf("plugin descriptor %q: %w", descriptor.Factory, stageErr)
		}
		resolved.requestStage = stage
	}
	resolved.authenticatesConsumer = authenticatesConsumerFactory(resolved.Factory)
	resolved.finalizer = FinalizerNone
	if resolved.HasPhase(PhaseFinalizer) {
		if _, ok := p.(base.SnapshotFinalizerPlugin); ok {
			resolved.finalizer = FinalizerSnapshot
		} else {
			resolved.finalizer = FinalizerDynamic
		}
	}
	resolved.generationOwner = generationOwnerForFactory(resolved.Factory)
	responseCapability, err := resolveResponseCapability(resolved, p)
	if err != nil {
		return Descriptor{}, err
	}
	resolved.responseCapability = responseCapability
	response, err := resolveDescriptorResponse(resolved, p, responseCapability)
	if err != nil {
		return Descriptor{}, err
	}
	resolved.response = response
	resolved.resolved = true
	if capability, ok := ResponseCapabilityFor(resolved.Factory); ok {
		resolved.separateSubsystem = capability.SeparateSubsystem
	}
	return resolved, nil
}

func narrowDescriptorPhases(descriptor *Descriptor, selection base.BindingPhaseDescriptor) error {
	if err := validateBindingPhaseDescriptor(descriptor.Factory, selection); err != nil {
		return err
	}
	requestStage, err := parseRequestStage(selection.RequestStage)
	if err != nil {
		return fmt.Errorf("plugin descriptor %q: %w", descriptor.Factory, err)
	}
	selected := make([]Phase, 0, len(descriptor.Phases))
	requestPhase := phaseForRequestStage(requestStage)
	if requestPhase != "" {
		selected = append(selected, requestPhase)
	}
	if selection.Header || selection.StreamingHeader {
		selected = append(selected, PhaseHeaderFilter)
	}
	if selection.BufferedBody {
		selected = append(selected, PhaseBodyFilter)
	}
	if selection.Log {
		selected = append(selected, PhaseLog)
	}
	for _, phase := range selected {
		if !descriptor.HasPhase(phase) {
			return fmt.Errorf(
				"plugin descriptor %q config selected undeclared phase %q",
				descriptor.Factory,
				phase,
			)
		}
	}
	for _, phase := range descriptor.Phases {
		if phase == PhaseFinalizer || phase == PhaseProtocol {
			selected = append(selected, phase)
		}
	}
	descriptor.Phases = uniquePhases(selected)
	return nil
}

func uniquePhases(phases []Phase) []Phase {
	result := make([]Phase, 0, len(phases))
	for _, phase := range phases {
		if !slices.Contains(result, phase) {
			result = append(result, phase)
		}
	}
	return result
}

func authenticatesConsumerFactory(factory string) bool {
	switch factory {
	case "basic-auth", "hmac-auth", "jwt-auth", "key-auth", "ldap-auth", "multi-auth", "wolf-rbac":
		return true
	default:
		return false
	}
}

func generationOwnerForFactory(factory string) GenerationOwnerKind {
	switch factory {
	case "log-rotate":
		return GenerationOwnerRoute
	case "error-log-logger", "server-info":
		return GenerationOwnerProcess
	default:
		return GenerationOwnerNone
	}
}

func resolveDescriptorResponse(
	descriptor Descriptor,
	p Plugin,
	capability ResponseCapability,
) (ResolvedResponsePhases, error) {
	owners := responseOwnerKinds(capability)
	if responseSpec, ok := responseFactoryRegistry[descriptor.Factory]; ok {
		if responseSpec.mask&ResponsePhaseHeader != 0 {
			owners = append(owners, ResponseOwnerHeaderFilter)
		}
		if responseSpec.mask&ResponsePhaseBufferedBody != 0 {
			owners = append(owners, ResponseOwnerBufferedBodyFilter)
		}
		if responseSpec.mask&ResponsePhaseFinalStore != 0 {
			owners = append(owners, ResponseOwnerFinalStore)
		}
	}
	if descriptor.configAware {
		owners = slices.DeleteFunc(owners, func(owner ResponseOwnerKind) bool {
			return owner == ResponseOwnerHeaderFilter ||
				owner == ResponseOwnerStreamingHeaderFilter ||
				owner == ResponseOwnerBufferedBodyFilter
		})
		if descriptor.phaseSelection.Header {
			owners = append(owners, ResponseOwnerHeaderFilter)
		}
		if descriptor.phaseSelection.StreamingHeader {
			owners = append(owners, ResponseOwnerStreamingHeaderFilter)
		}
		if descriptor.phaseSelection.BufferedBody {
			owners = append(owners, ResponseOwnerBufferedBodyFilter)
		}
	}
	owners = slices.DeleteFunc(owners, func(owner ResponseOwnerKind) bool {
		switch owner {
		case ResponseOwnerHeaderFilter, ResponseOwnerStreamingHeaderFilter:
			return !descriptor.HasPhase(PhaseHeaderFilter)
		case ResponseOwnerBufferedBodyFilter, ResponseOwnerFinalStore,
			ResponseOwnerStreamingBodyFilter, ResponseOwnerCompressionOffer:
			return !descriptor.HasPhase(PhaseBodyFilter)
		case ResponseOwnerStreamingProducer, ResponseOwnerExclusiveProtocol,
			ResponseOwnerSeparateSubsystemProtocol:
			return !descriptor.HasPhase(PhaseProtocol)
		default:
			return false
		}
	})
	if modeDescriber, ok := p.Config().(base.ResponseModeDescriber); ok {
		mode, modeErr := modeDescriber.DescribeResponseMode()
		if modeErr != nil {
			return ResolvedResponsePhases{}, fmt.Errorf(
				"factory %q response mode: %w",
				descriptor.Factory,
				modeErr,
			)
		}
		if mode.Modes&^(base.ResponseModeBounded|base.ResponseModeStreaming|base.ResponseModeHijack) != 0 {
			return ResolvedResponsePhases{}, fmt.Errorf(
				"factory %q response mode has unsupported mask %d",
				descriptor.Factory,
				mode.Modes,
			)
		}
		owners = resolveResponseModeOwners(owners, mode.Modes)
	}
	return ResolvedResponsePhases{
		Owners:            uniqueResponseOwners(owners),
		CompressionOffers: compressionOffersFor(descriptor.Factory),
		ExclusiveProtocol: capability.ExclusiveProtocol,
	}, nil
}

func responseOwnerKinds(capability ResponseCapability) []ResponseOwnerKind {
	owners := make([]ResponseOwnerKind, 0, 7)
	if capability.HeaderFilter {
		owners = append(owners, ResponseOwnerHeaderFilter)
		if capability.StreamingBodyFilter {
			owners = append(owners, ResponseOwnerStreamingHeaderFilter)
		}
	}
	if capability.BufferedBodyFilter {
		owners = append(owners, ResponseOwnerBufferedBodyFilter)
	}
	if capability.StreamingBodyFilter {
		owners = append(owners, ResponseOwnerStreamingBodyFilter)
	}
	if capability.CompressionOffer {
		owners = append(owners, ResponseOwnerCompressionOffer)
	}
	if capability.StreamingResponseOwner {
		owners = append(owners, ResponseOwnerStreamingProducer)
	}
	if capability.ExclusiveProtocol != ProtocolNone && capability.ExclusiveProtocol != ProtocolMQTT {
		owners = append(owners, ResponseOwnerExclusiveProtocol)
	}
	if capability.SeparateSubsystem && capability.ExclusiveProtocol == ProtocolMQTT {
		owners = append(owners, ResponseOwnerSeparateSubsystemProtocol)
	}
	return owners
}

func uniqueResponseOwners(owners []ResponseOwnerKind) []ResponseOwnerKind {
	seen := make(map[ResponseOwnerKind]struct{}, len(owners))
	result := make([]ResponseOwnerKind, 0, len(owners))
	for _, owner := range owners {
		if _, exists := seen[owner]; exists {
			continue
		}
		seen[owner] = struct{}{}
		result = append(result, owner)
	}
	return result
}

func resolveResponseModeOwners(owners []ResponseOwnerKind, modes base.ResponseModeMask) []ResponseOwnerKind {
	result := make([]ResponseOwnerKind, 0, len(owners))
	for _, owner := range owners {
		switch owner {
		case ResponseOwnerHeaderFilter:
			if modes&base.ResponseModeStreaming != 0 && modes&base.ResponseModeBounded == 0 {
				continue
			}
		case ResponseOwnerStreamingHeaderFilter:
			if modes&base.ResponseModeBounded != 0 && modes&base.ResponseModeStreaming == 0 {
				continue
			}
		case ResponseOwnerBufferedBodyFilter:
			if modes&base.ResponseModeStreaming != 0 && modes&base.ResponseModeBounded == 0 {
				continue
			}
		case ResponseOwnerStreamingBodyFilter, ResponseOwnerStreamingProducer:
			if modes&base.ResponseModeBounded != 0 && modes&base.ResponseModeStreaming == 0 {
				continue
			}
		}
		result = append(result, owner)
	}
	return result
}

func compressionOffersFor(factory string) CompressionSet {
	switch factory {
	case "gzip":
		return CompressionOfferGzip | CompressionOfferDeflate
	case "brotli":
		return CompressionOfferBrotli
	default:
		return 0
	}
}
