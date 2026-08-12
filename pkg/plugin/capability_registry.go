package plugin

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Capability uint32

const (
	CapabilitySystem Capability = 1 << iota
	CapabilityRequestRewrite
	CapabilityConsumerRewrite
	CapabilityRequestAccess
	CapabilityBeforeProxy
	CapabilityConditionalTerminal
	CapabilityHeaderFilter
	CapabilityBufferedBodyFilter
	CapabilityFinalResponseStore
	CapabilityStreamingBodyFilter
	CapabilityCompressionOffer
	CapabilityStreamingResponseOwner
	CapabilityExclusiveProtocolOwner
	CapabilityProtocolOwner
	CapabilityLog
	CapabilityFinalizer
	CapabilityGenerationOwner
	CapabilitySeparateSubsystem
)

type RequestOwnerKind uint8

const (
	RequestOwnerNone RequestOwnerKind = iota
	RequestOwnerInheritedStage
	RequestOwnerBeforeProxyConsumer
	RequestOwnerBeforeProxyHookRegistration
	RequestOwnerRouteOwned
	RequestOwnerSeparateSubsystem
	RequestOwnerLegacyAdapter
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

type CapabilitySpec struct {
	Identity           string
	ImplementationName string
	Capabilities       Capability
	RequestOwners      []RequestOwnerKind
	ResponseOwners     []ResponseOwnerKind
	Finalizer          FinalizerKind
	GenerationOwner    GenerationOwnerKind
	PrimaryPlan        string
}

type capabilityManifestEntry struct {
	primaryPlan  string
	capabilities Capability
}

// CompressionSet records structural content-coding offers without coupling
// the capability registry to a request-local compression.State.
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

var capabilityRegistry map[string]CapabilitySpec

func init() {
	capabilityRegistry = buildCapabilityRegistry()
}

func canonicalCapabilityFactory(factory string) string {
	if factory == "otel" {
		return "opentelemetry"
	}
	return factory
}

func CapabilitySpecForFactory(factory string) (CapabilitySpec, bool) {
	spec, ok := capabilityRegistry[canonicalCapabilityFactory(factory)]
	return cloneCapabilitySpec(spec), ok
}

func CapabilitySpecForIdentity(identity string) (CapabilitySpec, bool) {
	if identity == "request_context" {
		identity = "request-context"
	}
	spec, ok := capabilityRegistry[identity]
	return cloneCapabilitySpec(spec), ok
}

func cloneCapabilitySpec(spec CapabilitySpec) CapabilitySpec {
	spec.RequestOwners = append([]RequestOwnerKind(nil), spec.RequestOwners...)
	spec.ResponseOwners = append([]ResponseOwnerKind(nil), spec.ResponseOwners...)
	return spec
}

func buildCapabilityRegistry() map[string]CapabilitySpec {
	manifest := capabilityManifestEntries()
	registry := make(map[string]CapabilitySpec, len(pluginRegistry)-1)
	for factory := range pluginRegistry {
		identity := canonicalCapabilityFactory(factory)
		if _, exists := registry[identity]; exists {
			continue
		}
		implementation := identity
		if identity == "request-context" {
			implementation = "request_context"
		}
		spec := CapabilitySpec{Identity: identity, ImplementationName: implementation}
		if entry, classified := manifest[identity]; classified {
			spec.PrimaryPlan = entry.primaryPlan
			spec.Capabilities = entry.capabilities
		}
		registry[identity] = spec
	}

	for identity, spec := range registry {
		if stage, ok := RequestStageFor(identity); ok {
			spec.Capabilities |= capabilityForRequestStage(stage.Stage)
			if stage.Stage != RequestStageLegacy && stage.Stage != RequestStageNone {
				spec.RequestOwners = append(spec.RequestOwners, RequestOwnerInheritedStage)
			}
		}
		if capability, ok := ResponseCapabilityFor(identity); ok {
			spec.Capabilities |= capabilityBits(capability)
			spec.ResponseOwners = append(spec.ResponseOwners, responseOwnerKinds(capability)...)
		}
		if responseSpec, ok := responseFactoryRegistry[identity]; ok {
			if responseSpec.mask&ResponsePhaseHeader != 0 {
				spec.Capabilities |= CapabilityHeaderFilter
				spec.ResponseOwners = append(spec.ResponseOwners, ResponseOwnerHeaderFilter)
			}
			if responseSpec.mask&ResponsePhaseBufferedBody != 0 {
				spec.Capabilities |= CapabilityBufferedBodyFilter
				spec.ResponseOwners = append(spec.ResponseOwners, ResponseOwnerBufferedBodyFilter)
			}
			if responseSpec.mask&ResponsePhaseFinalStore != 0 {
				spec.Capabilities |= CapabilityFinalResponseStore
				spec.ResponseOwners = append(spec.ResponseOwners, ResponseOwnerFinalStore)
			}
		}
		if isLogIdentity(identity) {
			spec.Capabilities |= CapabilityLog
		}
		if kind, ok := finalizerForIdentity(identity); ok {
			spec.Capabilities |= CapabilityFinalizer
			spec.Finalizer = kind
		}
		if generation, ok := generationOwnerForIdentity(identity); ok {
			spec.Capabilities |= CapabilityGenerationOwner
			spec.GenerationOwner = generation
		}
		if isConditionalTerminalIdentity(identity) {
			spec.Capabilities |= CapabilityConditionalTerminal
		}
		if identity == "proxy-mirror" {
			spec.Capabilities |= CapabilityBeforeProxy
			spec.RequestOwners = append(spec.RequestOwners, RequestOwnerBeforeProxyHookRegistration)
		}
		if beforeProxyOwnerForFactory(identity) == RequestOwnerBeforeProxyConsumer {
			spec.Capabilities |= CapabilityBeforeProxy
			spec.RequestOwners = append(spec.RequestOwners, RequestOwnerBeforeProxyConsumer)
		}
		if isSeparateSubsystemIdentity(identity) {
			spec.Capabilities |= CapabilitySeparateSubsystem
			if identity == "mqtt-proxy" {
				spec.Capabilities |= CapabilityProtocolOwner
			}
			spec.RequestOwners = append(spec.RequestOwners, RequestOwnerSeparateSubsystem)
		}
		if identity == "request-context" {
			spec.Finalizer = FinalizerSnapshot
		}
		if isServerlessIdentity(identity) {
			spec.Capabilities |= CapabilityRequestRewrite | CapabilityRequestAccess |
				CapabilityBeforeProxy | CapabilityHeaderFilter | CapabilityBufferedBodyFilter |
				CapabilityConditionalTerminal
			spec.RequestOwners = append(
				spec.RequestOwners,
				RequestOwnerInheritedStage,
				RequestOwnerBeforeProxyConsumer,
			)
			spec.ResponseOwners = append(
				spec.ResponseOwners,
				ResponseOwnerHeaderFilter,
				ResponseOwnerBufferedBodyFilter,
			)
		}
		spec.RequestOwners = uniqueRequestOwners(spec.RequestOwners)
		spec.ResponseOwners = uniqueResponseOwners(spec.ResponseOwners)
		registry[identity] = spec
	}
	return registry
}

// capabilityManifestEntries is the production copy of the capability
// manifest.  Every registered identity is listed in exactly one primary-plan
// group; an unknown identity remains unclassified so completeness checks fail
// instead of silently assigning the system capability.
func capabilityManifestEntries() map[string]capabilityManifestEntry {
	entries := make(map[string]capabilityManifestEntry, 114)
	add := func(plan string, capabilities Capability, identities ...string) {
		for _, identity := range identities {
			entries[identity] = capabilityManifestEntry{
				primaryPlan:  plan,
				capabilities: capabilities,
			}
		}
	}
	add(
		"Plan 12",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityFinalizer,
		"limit-conn",
	)
	add("Plan 12", CapabilitySystem|CapabilityRequestRewrite|CapabilityFinalizer, "request-context")
	add(
		"Plan 13",
		CapabilityRequestRewrite|CapabilityConditionalTerminal,
		"ai-prompt-decorator",
		"ai-prompt-template",
		"ai-rag",
		"ai-request-rewrite",
		"data-mask",
		"degraphql",
		"jwe-decrypt",
		"request-id",
		"traffic-split",
	)
	add(
		"Plan 13",
		CapabilitySystem|CapabilityRequestRewrite|CapabilitySeparateSubsystem,
		"example-plugin",
	)
	add(
		"Plan 13",
		CapabilityRequestRewrite,
		"proxy-control",
		"proxy-mirror",
		"proxy-rewrite",
		"real-ip",
		"traffic-label",
	)
	add("Plan 14", CapabilityConsumerRewrite, "attach-consumer-label")
	add(
		"Plan 14",
		CapabilityRequestAccess|CapabilityConditionalTerminal,
		"acl",
		"ai-aws-content-moderation",
		"ai-prompt-guard",
		"authz-casbin",
		"authz-casdoor",
		"authz-keycloak",
		"basic-auth",
		"cas-auth",
		"chaitin-waf",
		"client-control",
		"consumer-restriction",
		"csrf",
		"dingtalk-auth",
		"feishu-auth",
		"forward-auth",
		"graphql-limit-count",
		"hmac-auth",
		"ip-restriction",
		"jwt-auth",
		"key-auth",
		"ldap-auth",
		"limit-count",
		"limit-req",
		"multi-auth",
		"oas-validator",
		"opa",
		"openid-connect",
		"referer-restriction",
		"request-validation",
		"saml-auth",
		"ua-restriction",
		"uri-blocker",
		"wolf-rbac",
		"workflow",
	)
	add(
		"Plan 15",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityFinalizer,
		"api-breaker",
	)
	add("Plan 15", CapabilityRequestRewrite|CapabilityBufferedBodyFilter, "body-transformer")
	add("Plan 15", CapabilityHeaderFilter|CapabilityBufferedBodyFilter, "echo")
	add(
		"Plan 15",
		CapabilityBufferedBodyFilter,
		"error-page",
		"exit-transformer",
		"response-rewrite",
	)
	add(
		"Plan 15",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityFinalResponseStore,
		"graphql-proxy-cache",
		"proxy-cache",
	)
	add(
		"Plan 15",
		CapabilityRequestRewrite|CapabilityRequestAccess|CapabilityBeforeProxy|CapabilityConditionalTerminal|CapabilityHeaderFilter|CapabilityBufferedBodyFilter|CapabilityLog,
		"serverless-pre-function",
		"serverless-post-function",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityBufferedBodyFilter|CapabilityStreamingBodyFilter,
		"ai-aliyun-content-moderation",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityBeforeProxy|CapabilityExclusiveProtocolOwner|CapabilityStreamingResponseOwner,
		"ai-proxy",
		"ai-proxy-multi",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityBufferedBodyFilter|CapabilityStreamingBodyFilter|CapabilityFinalizer,
		"ai-rate-limiting",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal,
		"aws-lambda",
		"azure-functions",
		"openfunction",
		"openwhisk",
	)
	add(
		"Plan 16",
		CapabilityHeaderFilter|CapabilityCompressionOffer|CapabilityStreamingBodyFilter,
		"brotli",
		"gzip",
	)
	add(
		"Plan 16",
		CapabilityRequestRewrite|CapabilityConditionalTerminal|CapabilityHeaderFilter,
		"cors",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityBeforeProxy|CapabilityExclusiveProtocolOwner|CapabilitySeparateSubsystem,
		"dubbo-proxy",
	)
	add(
		"Plan 16",
		CapabilityRequestRewrite|CapabilityConditionalTerminal,
		"fault-injection",
		"redirect",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityBufferedBodyFilter,
		"grpc-transcode",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityHeaderFilter|CapabilityStreamingBodyFilter|CapabilityExclusiveProtocolOwner,
		"grpc-web",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityBeforeProxy|CapabilityExclusiveProtocolOwner|CapabilitySeparateSubsystem|CapabilityStreamingResponseOwner,
		"kafka-proxy",
	)
	add(
		"Plan 16",
		CapabilityBeforeProxy|CapabilityExclusiveProtocolOwner|CapabilitySeparateSubsystem,
		"http-dubbo",
	)
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilityStreamingResponseOwner,
		"mcp-bridge",
	)
	add("Plan 16", CapabilityRequestAccess|CapabilityConditionalTerminal, "mocking")
	add("Plan 16", CapabilityProtocolOwner|CapabilitySeparateSubsystem, "mqtt-proxy")
	add("Plan 16", CapabilityRequestRewrite|CapabilityStreamingBodyFilter, "proxy-buffering")
	add(
		"Plan 16",
		CapabilityRequestAccess|CapabilityConditionalTerminal|CapabilitySeparateSubsystem,
		"public-api",
	)
	add("Plan 17", CapabilitySystem, "ai", "gm")
	add(
		"Plan 17",
		CapabilitySystem|CapabilitySeparateSubsystem,
		"batch-requests",
		"node-status",
		"prometheus",
	)
	add(
		"Plan 17",
		CapabilitySystem|CapabilityGenerationOwner|CapabilitySeparateSubsystem,
		"error-log-logger",
		"server-info",
	)
	add(
		"Plan 17",
		CapabilityRequestAccess|CapabilitySystem|CapabilityGenerationOwner,
		"log-rotate",
	)
	add(
		"Plan 17",
		CapabilityLog,
		"clickhouse-logger",
		"datadog",
		"elasticsearch-logger",
		"file-logger",
		"google-cloud-logging",
		"http-logger",
		"kafka-logger",
		"lago",
		"loggly",
		"loki-logger",
		"rocketmq-logger",
		"skywalking-logger",
		"sls-logger",
		"splunk-hec-logging",
		"syslog",
		"tcp-logger",
		"tencent-cloud-cls",
		"udp-logger",
	)
	add(
		"Plan 17",
		CapabilityRequestRewrite|CapabilityFinalizer,
		"opentelemetry",
		"skywalking",
		"zipkin",
	)
	return entries
}

func beforeProxyOwnerForFactory(identity string) RequestOwnerKind {
	if slices.Contains([]string{
		"ai-proxy", "ai-proxy-multi", "dubbo-proxy", "http-dubbo", "kafka-proxy",
	}, identity) {
		return RequestOwnerBeforeProxyConsumer
	}
	return RequestOwnerNone
}

func capabilityForRequestStage(stage RequestStage) Capability {
	switch stage {
	case RequestStageRewrite:
		return CapabilityRequestRewrite
	case RequestStageConsumerRewrite:
		return CapabilityConsumerRewrite
	case RequestStageAccess:
		return CapabilityRequestAccess
	case RequestStageBeforeProxy:
		return CapabilityBeforeProxy
	default:
		return 0
	}
}

func capabilityBits(capability ResponseCapability) Capability {
	var bits Capability
	if capability.HeaderFilter {
		bits |= CapabilityHeaderFilter
	}
	if capability.BufferedBodyFilter {
		bits |= CapabilityBufferedBodyFilter
	}
	if capability.StreamingBodyFilter {
		bits |= CapabilityStreamingBodyFilter
	}
	if capability.StreamingResponseOwner {
		bits |= CapabilityStreamingResponseOwner
	}
	if capability.CompressionOffer {
		bits |= CapabilityCompressionOffer
	}
	if capability.ExclusiveProtocol != ProtocolNone &&
		capability.ExclusiveProtocol != ProtocolMQTT {
		bits |= CapabilityExclusiveProtocolOwner
	}
	if capability.SeparateSubsystem {
		bits |= CapabilitySeparateSubsystem
		if capability.ExclusiveProtocol == ProtocolMQTT {
			bits |= CapabilityProtocolOwner
		}
	}
	return bits
}

func responseOwnerKinds(capability ResponseCapability) []ResponseOwnerKind {
	owners := make([]ResponseOwnerKind, 0, 5)
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
	if capability.ExclusiveProtocol != ProtocolNone &&
		capability.ExclusiveProtocol != ProtocolMQTT {
		owners = append(owners, ResponseOwnerExclusiveProtocol)
	}
	if capability.SeparateSubsystem && capability.ExclusiveProtocol == ProtocolMQTT {
		owners = append(owners, ResponseOwnerSeparateSubsystemProtocol)
	}
	return owners
}

func uniqueRequestOwners(owners []RequestOwnerKind) []RequestOwnerKind {
	seen := make(map[RequestOwnerKind]struct{}, len(owners))
	result := make([]RequestOwnerKind, 0, len(owners))
	for _, owner := range owners {
		if _, ok := seen[owner]; ok {
			continue
		}
		seen[owner] = struct{}{}
		result = append(result, owner)
	}
	return result
}

func uniqueResponseOwners(owners []ResponseOwnerKind) []ResponseOwnerKind {
	seen := make(map[ResponseOwnerKind]struct{}, len(owners))
	result := make([]ResponseOwnerKind, 0, len(owners))
	for _, owner := range owners {
		if _, ok := seen[owner]; ok {
			continue
		}
		seen[owner] = struct{}{}
		result = append(result, owner)
	}
	return result
}

func isLogIdentity(identity string) bool {
	if identity == "serverless-pre-function" || identity == "serverless-post-function" {
		return true
	}
	return slices.Contains([]string{
		"clickhouse-logger", "datadog", "elasticsearch-logger", "file-logger", "google-cloud-logging",
		"http-logger", "kafka-logger", "lago", "loggly", "loki-logger", "rocketmq-logger",
		"skywalking-logger", "sls-logger", "splunk-hec-logging", "syslog", "tcp-logger",
		"tencent-cloud-cls", "udp-logger",
	}, identity)
}

func finalizerForIdentity(identity string) (FinalizerKind, bool) {
	if identity == "request-context" {
		return FinalizerSnapshot, true
	}
	if slices.Contains([]string{
		"limit-conn", "api-breaker", "ai-rate-limiting", "opentelemetry", "skywalking", "zipkin",
	}, identity) {
		return FinalizerDynamic, true
	}
	return FinalizerNone, false
}

func generationOwnerForIdentity(identity string) (GenerationOwnerKind, bool) {
	if identity == "log-rotate" {
		return GenerationOwnerRoute, true
	}
	if slices.Contains([]string{"error-log-logger", "server-info"}, identity) {
		return GenerationOwnerProcess, true
	}
	return GenerationOwnerNone, false
}

func isConditionalTerminalIdentity(identity string) bool {
	return slices.Contains([]string{
		"limit-conn", "ai-aliyun-content-moderation", "ai-prompt-decorator", "ai-prompt-template", "ai-rag",
		"ai-request-rewrite", "ai-proxy", "ai-proxy-multi", "ai-rate-limiting", "aws-lambda",
		"azure-functions", "acl", "ai-aws-content-moderation", "ai-prompt-guard", "authz-casbin",
		"authz-casdoor", "authz-keycloak", "basic-auth", "cas-auth", "chaitin-waf", "client-control",
		"consumer-restriction", "csrf", "dingtalk-auth", "feishu-auth", "forward-auth",
		"graphql-limit-count", "hmac-auth", "ip-restriction", "jwt-auth", "key-auth", "ldap-auth",
		"limit-count", "limit-req", "multi-auth", "oas-validator", "opa", "openid-connect",
		"referer-restriction", "request-validation", "saml-auth", "ua-restriction", "uri-blocker",
		"wolf-rbac", "workflow", "api-breaker", "graphql-proxy-cache", "proxy-cache", "serverless-pre-function",
		"serverless-post-function", "cors", "fault-injection", "grpc-transcode", "grpc-web", "jwe-decrypt",
		"mcp-bridge", "mocking", "openfunction", "openwhisk", "public-api", "redirect", "request-id",
		"data-mask", "degraphql", "traffic-split",
	}, identity)
}

func isSeparateSubsystemIdentity(identity string) bool {
	return slices.Contains([]string{
		"batch-requests", "dubbo-proxy", "error-log-logger", "example-plugin", "http-dubbo", "kafka-proxy",
		"mqtt-proxy", "node-status", "prometheus", "public-api", "server-info",
	}, identity)
}

func ResolveResponsePhases(factory string, config any) (ResolvedResponsePhases, error) {
	identity := canonicalCapabilityFactory(factory)
	spec, ok := capabilityRegistry[identity]
	if !ok {
		return ResolvedResponsePhases{}, fmt.Errorf("unknown capability factory %q", factory)
	}
	if isServerlessIdentity(identity) {
		phase, err := configuredPhase(config)
		if err != nil {
			return ResolvedResponsePhases{}, err
		}
		switch phase {
		case "", "access", "rewrite", "before_proxy", "log":
			return ResolvedResponsePhases{}, nil
		case "header_filter":
			return ResolvedResponsePhases{
				Owners: []ResponseOwnerKind{ResponseOwnerHeaderFilter},
			}, nil
		case "body_filter":
			return ResolvedResponsePhases{
				Owners: []ResponseOwnerKind{ResponseOwnerBufferedBodyFilter},
			}, nil
		default:
			return ResolvedResponsePhases{}, fmt.Errorf(
				"factory %q has unsupported phase %q",
				factory,
				phase,
			)
		}
	}

	owners := append([]ResponseOwnerKind(nil), spec.ResponseOwners...)
	if descriptor, ok := config.(base.ResponseModeDescriber); ok {
		mode, err := descriptor.DescribeResponseMode()
		if err != nil {
			return ResolvedResponsePhases{}, fmt.Errorf(
				"factory %q response mode: %w",
				factory,
				err,
			)
		}
		if mode.Modes&^(base.ResponseModeBounded|base.ResponseModeStreaming|base.ResponseModeHijack) != 0 {
			return ResolvedResponsePhases{}, fmt.Errorf(
				"factory %q response mode has unsupported mask %d", factory, mode.Modes,
			)
		}
		owners = resolveResponseModeOwners(owners, mode.Modes)
	}
	compressionOffers := compressionOffersFor(identity)
	exclusive := ProtocolNone
	if capability, ok := ResponseCapabilityFor(identity); ok {
		exclusive = capability.ExclusiveProtocol
	}
	return ResolvedResponsePhases{
		Owners:            uniqueResponseOwners(owners),
		CompressionOffers: compressionOffers,
		ExclusiveProtocol: exclusive,
	}, nil
}

func ResolveBeforeProxyOwner(factory string, config any) (RequestOwnerKind, bool, error) {
	identity := canonicalCapabilityFactory(factory)
	if _, ok := capabilityRegistry[identity]; !ok {
		return RequestOwnerNone, false, nil
	}
	if slices.Contains(
		[]string{"ai-proxy", "ai-proxy-multi", "dubbo-proxy", "http-dubbo", "kafka-proxy"},
		identity,
	) {
		return RequestOwnerBeforeProxyConsumer, true, nil
	}
	if identity == "proxy-mirror" {
		return RequestOwnerBeforeProxyHookRegistration, true, nil
	}
	if isServerlessIdentity(identity) {
		phase, err := configuredPhase(config)
		if err != nil {
			return RequestOwnerNone, false, err
		}
		if phase == "before_proxy" {
			return RequestOwnerBeforeProxyConsumer, true, nil
		}
	}
	return RequestOwnerNone, false, nil
}

func resolveResponseModeOwners(
	owners []ResponseOwnerKind,
	modes base.ResponseModeMask,
) []ResponseOwnerKind {
	result := make([]ResponseOwnerKind, 0, len(owners)+2)
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

func compressionOffersFor(identity string) CompressionSet {
	switch identity {
	case "gzip":
		return CompressionOfferGzip | CompressionOfferDeflate
	case "brotli":
		return CompressionOfferBrotli
	default:
		return 0
	}
}

func isServerlessIdentity(identity string) bool {
	return identity == "serverless-pre-function" || identity == "serverless-post-function"
}

func configuredPhase(config any) (string, error) {
	if config == nil {
		return "", nil
	}
	value := reflect.ValueOf(config)
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return "", nil
		}
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		field := value.FieldByName("Phase")
		if field.IsValid() && field.Kind() == reflect.String {
			return field.String(), nil
		}
	}
	if descriptor, ok := config.(base.BindingPhaseDescriber); ok {
		phase, err := descriptor.DescribeBindingPhases()
		if err != nil {
			return "", err
		}
		return phase.RequestStage, nil
	}
	return "", nil
}
