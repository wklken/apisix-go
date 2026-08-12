package plugin

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
)

const ResourceUpstream ResourceKind = "upstream"

type ProtocolKind string

const (
	ProtocolNone      ProtocolKind = ""
	ProtocolAI        ProtocolKind = "ai"
	ProtocolGRPCWeb   ProtocolKind = "grpc-web"
	ProtocolKafka     ProtocolKind = "kafka"
	ProtocolDubbo     ProtocolKind = "dubbo"
	ProtocolHTTPDubbo ProtocolKind = "http-dubbo"
	ProtocolMQTT      ProtocolKind = "mqtt"
)

// ResponseCapability is the build-time declaration for response ownership.
// Request-stage ownership remains in RequestStageSpec; keeping these tables
// separate prevents a Handler type assertion from accidentally becoming a
// response phase declaration.
type ResponseCapability struct {
	HeaderFilter           bool
	BufferedBodyFilter     bool
	StreamingBodyFilter    bool
	StreamingResponseOwner bool
	CompressionOffer       bool
	ExclusiveProtocol      ProtocolKind
	SeparateSubsystem      bool
}

type ResponseCapabilityPlugin interface {
	ResponseCapability() ResponseCapability
}

type ResponseCapabilityDescriber interface {
	DescribeResponseCapability() (ResponseCapability, error)
}

// CompressionOfferPlugin is the root-owned structural contract. It keeps the
// complete Offer/State/Decision values from shared negotiation.
type CompressionOfferPlugin interface {
	RegisterCompressionOffers(*http.Request, *compression.State) []compression.Offer
	WrapCompression(
		http.ResponseWriter,
		*http.Request,
		*compression.State,
		compression.Decision,
	) (http.ResponseWriter, error)
}

var responseCapabilityRegistry = map[string]ResponseCapability{
	"ai-aliyun-content-moderation": {BufferedBodyFilter: true, StreamingBodyFilter: true},
	"ai-proxy": {
		StreamingResponseOwner: true, ExclusiveProtocol: ProtocolAI,
	},
	"ai-proxy-multi":   {StreamingResponseOwner: true, ExclusiveProtocol: ProtocolAI},
	"ai-rate-limiting": {BufferedBodyFilter: true, StreamingBodyFilter: true},
	"aws-lambda":       {},
	"azure-functions":  {},
	"brotli":           {HeaderFilter: true, StreamingBodyFilter: true, CompressionOffer: true},
	"cors":             {HeaderFilter: true},
	"dubbo-proxy":      {ExclusiveProtocol: ProtocolDubbo, SeparateSubsystem: true},
	"fault-injection":  {},
	"grpc-transcode":   {BufferedBodyFilter: true},
	"grpc-web":         {HeaderFilter: true, StreamingBodyFilter: true, ExclusiveProtocol: ProtocolGRPCWeb},
	"gzip":             {HeaderFilter: true, StreamingBodyFilter: true, CompressionOffer: true},
	"http-dubbo":       {ExclusiveProtocol: ProtocolHTTPDubbo, SeparateSubsystem: true},
	"kafka-proxy":      {StreamingResponseOwner: true, ExclusiveProtocol: ProtocolKafka, SeparateSubsystem: true},
	"mcp-bridge":       {StreamingResponseOwner: true},
	"mocking":          {},
	"mqtt-proxy":       {StreamingResponseOwner: true, ExclusiveProtocol: ProtocolMQTT, SeparateSubsystem: true},
	"openfunction":     {},
	"openwhisk":        {},
	"proxy-buffering":  {StreamingBodyFilter: true},
	"public-api":       {SeparateSubsystem: true},
	"redirect":         {},
}

func ResponseCapabilityFor(factory string) (ResponseCapability, bool) {
	capability, ok := responseCapabilityRegistry[factory]
	return capability, ok
}

func responseCapabilityForBinding(binding Binding) (ResponseCapability, error) {
	if binding.Plugin == nil {
		return ResponseCapability{}, fmt.Errorf(
			"response capability binding has nil plugin (factory=%q resource=%s/%s)",
			binding.factoryName, binding.Provenance.Kind, binding.Provenance.ID,
		)
	}
	var capability ResponseCapability
	var found bool
	if describer, ok := binding.Plugin.(ResponseCapabilityDescriber); ok {
		resolved, err := describer.DescribeResponseCapability()
		if err != nil {
			return ResponseCapability{}, fmt.Errorf(
				"factory %q response capability descriptor: %w", binding.factoryName, err,
			)
		}
		capability, found = resolved, true
	}
	if !found {
		if describer, ok := binding.Plugin.(ResponseCapabilityPlugin); ok {
			capability, found = describer.ResponseCapability(), true
		}
	}
	if !found {
		capability, found = responseCapabilityRegistry[binding.factoryName]
	}
	if describer, ok := binding.Plugin.Config().(base.ResponseModeDescriber); ok {
		descriptor, err := describer.DescribeResponseMode()
		if err != nil {
			return ResponseCapability{}, fmt.Errorf("factory %q response mode descriptor: %w", binding.factoryName, err)
		}
		if descriptor.Modes&^(base.ResponseModeBounded|base.ResponseModeStreaming|base.ResponseModeHijack) != 0 {
			return ResponseCapability{}, fmt.Errorf(
				"factory %q response mode descriptor has unsupported modes %d",
				binding.factoryName, descriptor.Modes,
			)
		}
		if capability.StreamingBodyFilter && descriptor.Modes != base.ResponseModeNone {
			capability.StreamingBodyFilter = descriptor.Modes&base.ResponseModeStreaming != 0
		}
	}
	if !found {
		return ResponseCapability{}, nil
	}
	return capability, nil
}

type RouteTerminalCandidate struct {
	Identity   string
	Scope      Scope
	Priority   int
	Provenance ResourceProvenance
	Protocol   ProtocolKind
	Terminal   base.ExclusiveProtocolTerminal
}

type ResponsePlanInput struct {
	StaticBindings []Binding
	RouteTerminals []RouteTerminalCandidate
	BufferedConfig base.BufferedResponseConfig
}

// ResponsePlan is an immutable generation recipe. Slices are private and
// every accessor returns a copy; request-local dynamic materialization never
// mutates the published generation.
type ResponsePlan struct {
	staticBindings     []Binding
	bufferedBindings   []ResponseBinding
	streamingBindings  []Binding
	terminalCandidates []RouteTerminalCandidate
	bufferedConfig     base.BufferedResponseConfig
}

func (p ResponsePlan) StaticBindings() []Binding { return cloneBindings(p.staticBindings) }

func (p ResponsePlan) BufferedBindings() []ResponseBinding {
	return append([]ResponseBinding(nil), p.bufferedBindings...)
}

func (p ResponsePlan) StreamingBindings() []Binding { return cloneBindings(p.streamingBindings) }

func (p ResponsePlan) RouteTerminals() []RouteTerminalCandidate {
	return append([]RouteTerminalCandidate(nil), p.terminalCandidates...)
}

func (p ResponsePlan) BufferedConfig() base.BufferedResponseConfig { return p.bufferedConfig }

// BuildResponsePlan validates and freezes the response ownership recipe. The
// any input accepts the original []Binding prototype as well as the explicit
// Plan 16 input, allowing existing callers to migrate without a second plan
// constructor.
func BuildResponsePlan(input any) (ResponsePlan, error) {
	var spec ResponsePlanInput
	switch value := input.(type) {
	case ResponsePlanInput:
		spec = value
	case *ResponsePlanInput:
		if value == nil {
			return ResponsePlan{}, fmt.Errorf("response plan input is nil")
		}
		spec = *value
	case []Binding:
		spec.StaticBindings = value
	default:
		return ResponsePlan{}, fmt.Errorf("unsupported response plan input %T", input)
	}
	if spec.BufferedConfig.MaxBytes == 0 {
		spec.BufferedConfig.MaxBytes = base.DefaultBufferedResponseMaxBytes
	}
	if spec.BufferedConfig.MaxBytes < 0 {
		return ResponsePlan{}, fmt.Errorf(
			"buffered response max bytes must not be negative: %d", spec.BufferedConfig.MaxBytes,
		)
	}
	static := cloneBindings(spec.StaticBindings)
	for _, binding := range static {
		if binding.Plugin == nil {
			return ResponsePlan{}, fmt.Errorf(
				"response plan binding has nil plugin (factory=%q resource=%s/%s)",
				binding.factoryName, binding.Provenance.Kind, binding.Provenance.ID,
			)
		}
	}
	responseBindings, err := MaterializeResponseBindings(bindingsToEffectiveSet(static))
	if err != nil {
		return ResponsePlan{}, err
	}
	plan := ResponsePlan{
		staticBindings:   static,
		bufferedBindings: responseBindings,
		bufferedConfig:   spec.BufferedConfig,
	}
	for _, binding := range static {
		capability, capErr := responseCapabilityForBinding(binding)
		if capErr != nil {
			return ResponsePlan{}, capErr
		}
		if capability.SeparateSubsystem && binding.factoryName == "mqtt-proxy" {
			// MQTT is classified for completeness, but is never installed in
			// the HTTP request/response executor.
			continue
		}
		if capability.StreamingBodyFilter || capability.StreamingResponseOwner {
			plan.streamingBindings = append(plan.streamingBindings, binding)
		}
		if capability.ExclusiveProtocol != ProtocolNone {
			candidate, candidateErr := terminalCandidateFromBinding(binding, capability, spec.RouteTerminals)
			if candidateErr != nil {
				return ResponsePlan{}, candidateErr
			}
			_, pluginOwns := binding.Plugin.(base.ExclusiveProtocolTerminal)
			if routeTerminalMatches(spec.RouteTerminals, candidate) &&
				(!pluginOwns || candidate.Protocol == ProtocolKafka || routeTerminalOwnerMatches(spec.RouteTerminals, candidate)) {
				continue
			}
			spec.RouteTerminals = append(spec.RouteTerminals, candidate)
		}
	}
	plan.terminalCandidates = append([]RouteTerminalCandidate(nil), spec.RouteTerminals...)
	if err := validateResponsePlanCompatibility(plan); err != nil {
		return ResponsePlan{}, err
	}
	return plan, nil
}

func routeTerminalMatches(candidates []RouteTerminalCandidate, want RouteTerminalCandidate) bool {
	for _, candidate := range candidates {
		if candidate.Identity == want.Identity && candidate.Provenance == want.Provenance &&
			candidate.Protocol == want.Protocol {
			return true
		}
	}
	return false
}

func routeTerminalOwnerMatches(candidates []RouteTerminalCandidate, want RouteTerminalCandidate) bool {
	for _, candidate := range candidates {
		if candidate.Identity != want.Identity || candidate.Provenance != want.Provenance ||
			candidate.Protocol != want.Protocol || candidate.Terminal == nil || want.Terminal == nil {
			continue
		}
		left, right := reflect.ValueOf(candidate.Terminal), reflect.ValueOf(want.Terminal)
		if left.Type() == right.Type() && left.Type().Comparable() && left.Interface() == right.Interface() {
			return true
		}
	}
	return false
}

func terminalCandidateFromBinding(
	binding Binding,
	capability ResponseCapability,
	provided []RouteTerminalCandidate,
) (RouteTerminalCandidate, error) {
	terminal, ok := binding.Plugin.(base.ExclusiveProtocolTerminal)
	if !ok {
		for _, candidate := range provided {
			if candidate.Identity == binding.factoryName && candidate.Provenance == binding.Provenance &&
				candidate.Protocol == capability.ExclusiveProtocol {
				return candidate, nil
			}
		}
		return RouteTerminalCandidate{}, fmt.Errorf(
			"exclusive protocol identity=%q resource=%s/%s has no terminal owner",
			binding.factoryName, binding.Provenance.Kind, binding.Provenance.ID,
		)
	}
	candidate := RouteTerminalCandidate{
		Identity: binding.factoryName, Scope: binding.Scope, Priority: binding.Plugin.GetPriority(),
		Provenance: binding.Provenance, Protocol: capability.ExclusiveProtocol, Terminal: terminal,
	}
	for _, existing := range provided {
		if existing.Identity == candidate.Identity && existing.Provenance == candidate.Provenance &&
			existing.Protocol == candidate.Protocol && candidate.Protocol == ProtocolKafka {
			return existing, nil
		}
	}
	return candidate, nil
}

func validateResponsePlanCompatibility(plan ResponsePlan) error {
	terminals := plan.terminalCandidates
	if len(terminals) > 1 {
		left, right := terminals[0], terminals[1]
		return fmt.Errorf(
			"exclusive protocol identities %q (resource=%s/%s) and %q (resource=%s/%s) conflict",
			left.Identity, left.Provenance.Kind, left.Provenance.ID,
			right.Identity, right.Provenance.Kind, right.Provenance.ID,
		)
	}
	if len(plan.bufferedBindings) == 0 {
		return nil
	}
	allBufferedDualMode := responseBindingsAreDualMode(plan.bufferedBindings)
	for _, binding := range plan.streamingBindings {
		capability, err := responseCapabilityForBinding(binding)
		if err != nil {
			return err
		}
		if allBufferedDualMode && isDualModeResponseBinding(binding, capability) {
			continue
		}
		if allBufferedDualMode && len(terminals) == 1 && terminals[0].Protocol == ProtocolAI &&
			capability.ExclusiveProtocol == ProtocolAI {
			continue
		}
		if len(terminals) == 0 && compatibleBoundedAdapter(binding, capability) {
			continue
		}
		if capability.StreamingResponseOwner || capability.ExclusiveProtocol != ProtocolNone ||
			!compatibleBoundedAdapter(binding, capability) {
			left := plan.bufferedBindings[0]
			identity, provenance := binding.factoryName, binding.Provenance
			if len(terminals) > 0 {
				identity, provenance = terminals[0].Identity, terminals[0].Provenance
			}
			return fmt.Errorf(
				"buffered response identity=%q resource=%s/%s conflicts with %q resource=%s/%s",
				left.factoryKey, left.Provenance.Kind, left.Provenance.ID,
				identity, provenance.Kind, provenance.ID,
			)
		}
	}
	if len(terminals) > 0 {
		if allBufferedDualMode && len(terminals) == 1 && terminals[0].Protocol == ProtocolAI {
			return nil
		}
		left := plan.bufferedBindings[0]
		identity, provenance := terminals[0].Identity, terminals[0].Provenance
		return fmt.Errorf(
			"buffered response identity=%q resource=%s/%s conflicts with %q resource=%s/%s",
			left.factoryKey, left.Provenance.Kind, left.Provenance.ID,
			identity, provenance.Kind, provenance.ID,
		)
	}
	return nil
}

func responseBindingsAreDualMode(bindings []ResponseBinding) bool {
	if len(bindings) == 0 {
		return false
	}
	for _, binding := range bindings {
		capability, err := responseCapabilityForBinding(Binding{
			Plugin: binding.Plugin, Scope: binding.Scope, Provenance: binding.Provenance,
			factoryName: binding.factoryKey,
		})
		if err != nil || !isDualModeResponseBinding(Binding{Plugin: binding.Plugin}, capability) {
			return false
		}
	}
	return true
}

func isDualModeResponseBinding(binding Binding, capability ResponseCapability) bool {
	if binding.Plugin == nil || !capability.BufferedBodyFilter || !capability.StreamingBodyFilter {
		return false
	}
	_, ok := binding.Plugin.(base.RequestResponseModeSelector)
	return ok
}

func compatibleBoundedAdapter(binding Binding, capability ResponseCapability) bool {
	if capability.StreamingResponseOwner || capability.ExclusiveProtocol != ProtocolNone {
		return false
	}
	switch binding.factoryName {
	case "gzip", "brotli", "cors":
		return true
	default:
		return false
	}
}

// Materialize resolves a request-local effective binding set while retaining
// the generation's route-owned streaming and terminal declarations.
func (p ResponsePlan) Materialize(effective EffectiveBindingSet) (ResponsePlan, error) {
	dynamic, err := BuildResponsePlan(ResponsePlanInput{
		StaticBindings: effective.all(), BufferedConfig: p.bufferedConfig,
		RouteTerminals: p.terminalCandidates,
	})
	if err != nil {
		return ResponsePlan{}, err
	}
	return dynamic, nil
}

// PostResolutionHook performs the immutable per-request compatibility check.
// The request itself is returned unchanged; execution state remains owned by
// RequestPipeline/BufferedResponseExecutor.
func (p ResponsePlan) PostResolutionHook(r *http.Request, effective EffectiveBindingSet) (*http.Request, error) {
	if _, err := p.Materialize(effective); err != nil {
		return r, err
	}
	return r, nil
}

func (p ResponsePlan) Install(pipeline RequestPipeline, terminal http.Handler) http.Handler {
	streaming, err := NewStreamingResponseExecutor(p.streamingBindings)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
		})
	}
	streaming, err = streaming.WithRouteTerminals(p.terminalCandidates)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
		})
	}
	pipeline = pipeline.WithStreamingResponseExecutor(streaming)
	if len(p.bufferedBindings) == 0 {
		return pipeline.Then(terminal)
	}
	executor, err := NewBufferedResponseExecutor(
		p.staticBindings,
		TerminalDescriptor{Owner: TerminalOwnerOrdinaryProxy},
		p.bufferedConfig,
	)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeStableResponseError(w, http.StatusInternalServerError, "Internal Server Error")
		})
	}
	return pipeline.WithBufferedResponseExecutor(executor.WithStreamingResponseExecutor(streaming)).Then(terminal)
}

type ResponsePhaseMask uint8

const (
	ResponsePhaseHeader ResponsePhaseMask = 1 << iota
	ResponsePhaseBufferedBody
	ResponsePhaseFinalStore
)

type ResponseBinding struct {
	Plugin     Plugin
	Scope      Scope
	Provenance ResourceProvenance
	Phases     ResponsePhaseMask
	factoryKey string
}

type responseFactorySpec struct {
	configAware bool
	mask        ResponsePhaseMask
	allowHeader bool
	allowBody   bool
}

var responseFactoryRegistry = map[string]responseFactorySpec{
	"ai-aliyun-content-moderation": {mask: ResponsePhaseBufferedBody, allowBody: true},
	"ai-rate-limiting":             {mask: ResponsePhaseBufferedBody, allowBody: true},
	"api-breaker":                  {},
	"body-transformer":             {configAware: true, allowBody: true},
	"echo":                         {configAware: true, allowHeader: true, allowBody: true},
	"error-page":                   {mask: ResponsePhaseBufferedBody, allowBody: true},
	"exit-transformer":             {mask: ResponsePhaseBufferedBody, allowBody: true},
	"graphql-proxy-cache":          {mask: ResponsePhaseFinalStore},
	"grpc-transcode":               {mask: ResponsePhaseBufferedBody, allowBody: true},
	"proxy-cache":                  {mask: ResponsePhaseFinalStore},
	"response-rewrite":             {mask: ResponsePhaseBufferedBody, allowBody: true},
	"serverless-pre-function":      {configAware: true, allowHeader: true, allowBody: true},
	"serverless-post-function":     {configAware: true, allowHeader: true, allowBody: true},
}

func responseFactoryAllowsDescriptor(factoryKey string, descriptor base.BindingPhaseDescriptor) bool {
	spec, ok := responseFactoryRegistry[factoryKey]
	if !ok {
		return false
	}
	if descriptor.Header && !spec.allowHeader {
		return false
	}
	if descriptor.BufferedBody && !spec.allowBody {
		return false
	}
	if !spec.configAware && (descriptor.Header || descriptor.BufferedBody) {
		return false
	}
	stage, err := parseRequestStage(descriptor.RequestStage)
	if err != nil {
		return false
	}
	if descriptor.RequestStage == "" {
		stage = requestStageRegistry[factoryKey].Stage
	}
	switch factoryKey {
	case "echo":
		return stage == RequestStageNone && (descriptor.Header || descriptor.BufferedBody)
	case "body-transformer":
		return (stage == RequestStageRewrite && !descriptor.Header) ||
			(stage == RequestStageNone && !descriptor.Header && descriptor.BufferedBody)
	case "serverless-pre-function", "serverless-post-function":
		if descriptor.Header || descriptor.BufferedBody {
			return stage == RequestStageNone && descriptor.Header != descriptor.BufferedBody
		}
		return stage == RequestStageRewrite || stage == RequestStageAccess ||
			stage == RequestStageBeforeProxy || stage == RequestStageLegacy
	}
	return true
}

func MaterializeResponseBindings(effective EffectiveBindingSet) ([]ResponseBinding, error) {
	result := make([]ResponseBinding, 0, len(effective.global)+len(effective.merged))
	for _, partition := range [][]Binding{effective.global, effective.merged} {
		ordered := append([]Binding(nil), partition...)
		slices.SortStableFunc(ordered, func(a, b Binding) int {
			if a.Plugin == nil || b.Plugin == nil {
				return 0
			}
			if priority := b.Plugin.GetPriority() - a.Plugin.GetPriority(); priority != 0 {
				return priority
			}
			return 0
		})
		for _, binding := range ordered {
			if binding.Plugin == nil {
				return nil, fmt.Errorf(
					"response binding has nil plugin (factory=%q resource=%s/%s)",
					binding.factoryName,
					binding.Provenance.Kind,
					binding.Provenance.ID,
				)
			}
			responseSpec, known := responseFactoryRegistry[binding.factoryName]
			if !known {
				if hasResponseCallbacks(binding.Plugin) {
					return nil, fmt.Errorf(
						"response callback is undeclared (factory=%q resource=%s/%s)",
						binding.factoryName,
						binding.Provenance.Kind,
						binding.Provenance.ID,
					)
				}
				continue
			}
			phases := responseSpec.mask
			if responseSpec.configAware {
				describer, ok := binding.Plugin.Config().(base.BindingPhaseDescriber)
				if !ok {
					return nil, fmt.Errorf(
						"factory %q requires a binding phase descriptor (resource=%s/%s)",
						binding.factoryName,
						binding.Provenance.Kind,
						binding.Provenance.ID,
					)
				}
				descriptor, err := describer.DescribeBindingPhases()
				if err != nil {
					return nil, fmt.Errorf("factory %q descriptor: %w", binding.factoryName, err)
				}
				if err := validateBindingPhaseDescriptor(binding.factoryName, descriptor); err != nil {
					return nil, err
				}
				resolved := requestStageRegistry[binding.factoryName]
				if descriptor.RequestStage != "" {
					resolved.Stage, err = parseRequestStage(descriptor.RequestStage)
					if err != nil {
						return nil, err
					}
				}
				if resolved.Stage != binding.Stage {
					return nil, fmt.Errorf(
						"factory %q stage disagreement: binding=%d descriptor=%d (resource=%s/%s)",
						binding.factoryName,
						binding.Stage,
						resolved.Stage,
						binding.Provenance.Kind,
						binding.Provenance.ID,
					)
				}
				if descriptor.Header {
					phases |= ResponsePhaseHeader
				}
				if descriptor.BufferedBody {
					phases |= ResponsePhaseBufferedBody
				}
			}
			if err := validateResponseCallbacks(binding, phases); err != nil {
				return nil, err
			}
			if phases == 0 {
				continue
			}
			result = append(result, ResponseBinding{
				Plugin: binding.Plugin, Scope: binding.Scope, Provenance: binding.Provenance,
				Phases: phases, factoryKey: binding.factoryName,
			})
		}
	}
	return result, nil
}

func hasResponseCallbacks(plugin Plugin) bool {
	_, header := plugin.(base.HeaderFilterPlugin)
	_, body := plugin.(base.BufferedBodyFilterPlugin)
	_, store := plugin.(base.FinalResponseStorePlugin)
	return header || body || store
}

func validateResponseCallbacks(binding Binding, phases ResponsePhaseMask) error {
	if phases&ResponsePhaseHeader != 0 {
		if _, ok := binding.Plugin.(base.HeaderFilterPlugin); !ok {
			return fmt.Errorf(
				"factory %q declares header filter without callback (resource=%s/%s)",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	if phases&ResponsePhaseBufferedBody != 0 {
		if _, ok := binding.Plugin.(base.BufferedBodyFilterPlugin); !ok {
			return fmt.Errorf(
				"factory %q declares buffered body filter without callback (resource=%s/%s)",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	if phases&ResponsePhaseFinalStore != 0 {
		if _, ok := binding.Plugin.(base.FinalResponseStorePlugin); !ok {
			return fmt.Errorf(
				"factory %q declares final response store without callback (resource=%s/%s)",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	return nil
}
