package plugin

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type capabilityPhaseFixture struct {
	Phase      string
	descriptor base.BindingPhaseDescriptor
}

func (f capabilityPhaseFixture) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	return f.descriptor, nil
}

func TestCapabilityRegistryCompletenessFromManifest(t *testing.T) {
	central, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	expectedFactories := make(map[string]struct{})
	expectedIdentities := make(map[string]struct{})
	for _, entry := range central.Plugins {
		if len(entry.Factories) == 0 {
			continue
		}
		expectedIdentities[entry.Name] = struct{}{}
		for _, factory := range entry.Factories {
			expectedFactories[factory.Key] = struct{}{}
		}
	}
	registeredFactories := make(map[string]struct{}, len(pluginRegistry))
	for factory := range pluginRegistry {
		registeredFactories[factory] = struct{}{}
	}
	assertExactStringSet(t, "factory inventory", expectedFactories, registeredFactories)

	registeredIdentities := make(map[string]struct{}, len(capabilityRegistry))
	for identity := range capabilityRegistry {
		registeredIdentities[identity] = struct{}{}
	}
	assertExactStringSet(t, "capability identity inventory", expectedIdentities, registeredIdentities)

	manifest := generatedCapabilityManifestEntries()
	generatedIdentities := make(map[string]struct{}, len(manifest))
	for identity := range manifest {
		generatedIdentities[identity] = struct{}{}
	}
	assertExactStringSet(t, "generated manifest identity inventory", expectedIdentities, generatedIdentities)

	for factory := range pluginRegistry {
		spec, ok := CapabilitySpecForFactory(factory)
		if !ok {
			t.Fatalf("factory %q has no capability spec", factory)
		}
		if spec.Capabilities == 0 || spec.Identity == "" {
			t.Fatalf("factory %q has incomplete spec %#v", factory, spec)
		}
		if _, ok := manifest[spec.Identity]; !ok {
			t.Fatalf(
				"factory %q identity %q is not in the explicit manifest",
				factory,
				spec.Identity,
			)
		}
		instance := pluginRegistry[factory]()
		if instance == nil {
			t.Fatalf("factory %q returned nil", factory)
		}
		if err := instance.Init(); err != nil {
			t.Fatalf("factory %q Init() error = %v", factory, err)
		}
		wantName := factory
		if renamed, ok := pluginNameAliases[factory]; ok {
			wantName = renamed
		}
		if instance.GetName() != wantName {
			t.Fatalf("factory %q GetName() = %q, want %q", factory, instance.GetName(), wantName)
		}
	}
}

func assertExactStringSet(t *testing.T, label string, expected, actual map[string]struct{}) {
	t.Helper()
	var missing, unexpected []string
	for key := range expected {
		if _, ok := actual[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range actual {
		if _, ok := expected[key]; !ok {
			unexpected = append(unexpected, key)
		}
	}
	slices.Sort(missing)
	slices.Sort(unexpected)
	if len(missing) != 0 || len(unexpected) != 0 {
		t.Fatalf("%s mismatch: missing=%v unexpected=%v", label, missing, unexpected)
	}
}

func TestCapabilityRegistryAliasAndImplementationNameExceptions(t *testing.T) {
	otel, ok := CapabilitySpecForFactory("otel")
	if !ok || otel.Identity != "opentelemetry" || otel.ImplementationName != "opentelemetry" {
		t.Fatalf("otel spec = %#v/%v", otel, ok)
	}
	requestContext, ok := CapabilitySpecForFactory("request-context")
	if !ok || requestContext.Identity != "request-context" ||
		requestContext.ImplementationName != "request_context" {
		t.Fatalf("request-context spec = %#v/%v", requestContext, ok)
	}
	if _, ok := CapabilitySpecForIdentity("otel"); ok {
		t.Fatal("otel unexpectedly accepted as canonical identity")
	}
	if requestContext.Capabilities&CapabilityFinalizer != 0 || requestContext.Finalizer != FinalizerNone {
		t.Fatalf("request-context unexpectedly owns a finalizer: %#v", requestContext)
	}
	prometheusSpec, ok := CapabilitySpecForFactory("prometheus")
	if !ok || prometheusSpec.Capabilities&CapabilityLog == 0 {
		t.Fatalf("prometheus spec = %#v/%v, want log ownership", prometheusSpec, ok)
	}
}

func TestCapabilityRegistryUsesGeneratedManifestFactsAndCapabilities(t *testing.T) {
	manifest := generatedCapabilityManifestEntries()
	tests := []struct {
		factory string
		bits    Capability
	}{
		{
			factory: "limit-conn",
			bits:    CapabilityRequestAccess | CapabilityConditionalTerminal | CapabilityFinalizer,
		},
		{
			factory: "request-id",
			bits:    CapabilityRequestRewrite | CapabilityConditionalTerminal,
		},
		{
			factory: "key-auth",
			bits:    CapabilityRequestAccess | CapabilityConditionalTerminal | CapabilityLogSanitizer,
		},
		{
			factory: "proxy-cache",
			bits:    CapabilityRequestAccess | CapabilityConditionalTerminal | CapabilityFinalResponseStore,
		},
		{
			factory: "ai-proxy",
			bits:    CapabilityRequestAccess | CapabilityConditionalTerminal | CapabilityBeforeProxy | CapabilityExclusiveProtocolOwner | CapabilityStreamingResponseOwner,
		},
		{factory: "http-logger", bits: CapabilityLog},
	}
	for _, test := range tests {
		t.Run(test.factory, func(t *testing.T) {
			spec, ok := CapabilitySpecForFactory(test.factory)
			if !ok {
				t.Fatalf("CapabilitySpecForFactory(%q) missing", test.factory)
			}
			entry, ok := manifest[spec.Identity]
			if !ok || len(entry.phases) == 0 && len(entry.scopes) == 0 {
				t.Fatalf("generated manifest facts for %q = %#v/%v", spec.Identity, entry, ok)
			}
			if spec.Capabilities&test.bits != test.bits {
				t.Fatalf("Capabilities = %#x, want bits %#x", spec.Capabilities, test.bits)
			}
		})
	}
}

func TestKeyAuthAndDataMaskSanitizerCapabilitiesStayExact(t *testing.T) {
	keyAuth, ok := CapabilitySpecForFactory("key-auth")
	if !ok {
		t.Fatal("key-auth capability is missing")
	}
	if keyAuth.Capabilities&CapabilityLogSanitizer == 0 {
		t.Fatalf("key-auth capabilities = %#x, want log sanitizer ownership", keyAuth.Capabilities)
	}
	dataMask, ok := CapabilitySpecForFactory("data-mask")
	if !ok || dataMask.Capabilities != CapabilityLogSanitizer {
		t.Fatalf("data-mask spec = %#v/%v, want only log sanitizer", dataMask, ok)
	}
}

func TestCapabilityRegistryDoesNotDefaultUnclassifiedIdentityToSystem(t *testing.T) {
	const identity = "test-unclassified-capability"
	pluginRegistry[identity] = nil
	defer delete(pluginRegistry, identity)

	spec, ok := buildCapabilityRegistry()[identity]
	if !ok {
		t.Fatal("unclassified identity missing from built registry")
	}
	if spec.Capabilities != 0 {
		t.Fatalf("unclassified identity silently classified: %#v", spec)
	}
}

func TestCapabilityPhaseKindsKeepResponseOwnerDistinctions(t *testing.T) {
	cache, ok := CapabilitySpecForFactory("proxy-cache")
	if !ok || cache.Capabilities&CapabilityFinalResponseStore == 0 ||
		cache.Capabilities&CapabilityBufferedBodyFilter != 0 {
		t.Fatalf("proxy-cache spec = %#v/%v", cache, ok)
	}
	gzip, ok := CapabilitySpecForFactory("gzip")
	if !ok || gzip.Capabilities&CapabilityCompressionOffer == 0 ||
		gzip.Capabilities&CapabilityStreamingResponseOwner != 0 {
		t.Fatalf("gzip spec = %#v/%v", gzip, ok)
	}
	mqtt, ok := CapabilitySpecForFactory("mqtt-proxy")
	if !ok || mqtt.Capabilities&CapabilityProtocolOwner == 0 ||
		mqtt.Capabilities&CapabilityExclusiveProtocolOwner != 0 {
		t.Fatalf("mqtt spec = %#v/%v", mqtt, ok)
	}
	for _, factory := range []string{"public-api", "dubbo-proxy", "http-dubbo", "kafka-proxy"} {
		spec, ok := CapabilitySpecForFactory(factory)
		if !ok || spec.Capabilities&CapabilityProtocolOwner != 0 ||
			slices.Contains(spec.ResponseOwners, ResponseOwnerSeparateSubsystemProtocol) {
			t.Fatalf("%s unexpectedly classified as separate protocol owner: %#v/%v", factory, spec, ok)
		}
	}
}

func TestCapabilityPhaseKindBijection(t *testing.T) {
	requestBit := map[RequestStage]Capability{
		RequestStageRewrite:         CapabilityRequestRewrite,
		RequestStageConsumerRewrite: CapabilityConsumerRewrite,
		RequestStageAccess:          CapabilityRequestAccess,
		RequestStageBeforeProxy:     CapabilityBeforeProxy,
	}
	responseBit := map[ResponseOwnerKind]Capability{
		ResponseOwnerHeaderFilter:              CapabilityHeaderFilter,
		ResponseOwnerBufferedBodyFilter:        CapabilityBufferedBodyFilter,
		ResponseOwnerFinalStore:                CapabilityFinalResponseStore,
		ResponseOwnerStreamingHeaderFilter:     CapabilityHeaderFilter,
		ResponseOwnerStreamingBodyFilter:       CapabilityStreamingBodyFilter,
		ResponseOwnerCompressionOffer:          CapabilityCompressionOffer,
		ResponseOwnerStreamingProducer:         CapabilityStreamingResponseOwner,
		ResponseOwnerExclusiveProtocol:         CapabilityExclusiveProtocolOwner,
		ResponseOwnerSeparateSubsystemProtocol: CapabilityProtocolOwner,
	}
	for identity, spec := range capabilityRegistry {
		t.Run(identity, func(t *testing.T) {
			for _, owner := range spec.RequestOwners {
				if owner == RequestOwnerNone || owner == RequestOwnerLegacyAdapter {
					t.Fatalf("request owner = %v, want explicit production owner", owner)
				}
			}
			if stage, ok := RequestStageFor(identity); ok && !stage.ConfigAware &&
				stage.Stage != RequestStageNone && stage.Stage != RequestStageLegacy {
				bit := requestBit[stage.Stage]
				if bit == 0 || spec.Capabilities&bit == 0 ||
					!slices.Contains(spec.RequestOwners, RequestOwnerInheritedStage) {
					t.Fatalf("stage %v is not in capability spec %#v", stage.Stage, spec)
				}
			}
			for _, owner := range spec.ResponseOwners {
				bit := responseBit[owner]
				if owner == ResponseOwnerNone || bit == 0 || spec.Capabilities&bit == 0 {
					t.Fatalf("response owner %v is not backed by spec %#v", owner, spec)
				}
			}
			if spec.Capabilities&CapabilityFinalizer != 0 == (spec.Finalizer == FinalizerNone) {
				t.Fatalf("finalizer capability/kind disagree: %#v", spec)
			}
			if spec.Capabilities&CapabilityGenerationOwner != 0 ==
				(spec.GenerationOwner == GenerationOwnerNone) {
				t.Fatalf("generation capability/kind disagree: %#v", spec)
			}
			instance := New(identity, base.Dependencies{})
			if instance == nil {
				t.Fatalf("canonical factory %q is not registered", identity)
			}
			if spec.Capabilities&CapabilityLog != 0 {
				if _, ok := instance.(base.LogPhasePlugin); !ok {
					t.Fatalf("log capability has no log-phase interface: %#v", spec)
				}
			}
			if spec.Capabilities&CapabilityLogSanitizer != 0 {
				if _, ok := instance.(base.LogSnapshotSanitizerPlugin); !ok {
					t.Fatalf("sanitizer capability has no sanitizer interface: %#v", spec)
				}
			}
		})
	}
	requestCapabilityOwner := map[Capability]RequestOwnerKind{
		CapabilityRequestRewrite:  RequestOwnerInheritedStage,
		CapabilityConsumerRewrite: RequestOwnerInheritedStage,
		CapabilityRequestAccess:   RequestOwnerInheritedStage,
	}
	responseCapabilityOwner := map[Capability]ResponseOwnerKind{
		CapabilityHeaderFilter:           ResponseOwnerHeaderFilter,
		CapabilityBufferedBodyFilter:     ResponseOwnerBufferedBodyFilter,
		CapabilityFinalResponseStore:     ResponseOwnerFinalStore,
		CapabilityStreamingBodyFilter:    ResponseOwnerStreamingBodyFilter,
		CapabilityCompressionOffer:       ResponseOwnerCompressionOffer,
		CapabilityStreamingResponseOwner: ResponseOwnerStreamingProducer,
		CapabilityExclusiveProtocolOwner: ResponseOwnerExclusiveProtocol,
	}
	for identity, spec := range capabilityRegistry {
		_, configAware := requestStageRegistry[identity]
		configAware = configAware && requestStageRegistry[identity].ConfigAware
		for bit, owner := range requestCapabilityOwner {
			if spec.Capabilities&bit != 0 && !slices.Contains(spec.RequestOwners, owner) &&
				!configAware {
				t.Errorf("%s capability %#x has no request owner %v: %#v", identity, bit, owner, spec)
			}
		}
		for bit, owner := range responseCapabilityOwner {
			if spec.Capabilities&bit != 0 && !slices.Contains(spec.ResponseOwners, owner) &&
				!configAware {
				t.Errorf("%s capability %#x has no response owner %v: %#v", identity, bit, owner, spec)
			}
		}
	}

	serverlessConfig := func(phase string) capabilityPhaseFixture {
		stage := phase
		if phase == "header_filter" || phase == "body_filter" || phase == "log" {
			stage = "none"
		}
		return capabilityPhaseFixture{
			Phase: phase,
			descriptor: base.BindingPhaseDescriptor{
				RequestStage: stage,
				Header:       phase == "header_filter",
				BufferedBody: phase == "body_filter",
				Log:          phase == "log",
			},
		}
	}
	for _, factory := range []string{"serverless-pre-function", "serverless-post-function"} {
		for _, fixture := range []struct {
			phase  string
			stage  RequestStage
			owners []ResponseOwnerKind
			before bool
		}{
			{phase: "rewrite", stage: RequestStageRewrite},
			{phase: "access", stage: RequestStageAccess},
			{phase: "before_proxy", stage: RequestStageBeforeProxy, before: true},
			{phase: "header_filter", stage: RequestStageNone, owners: []ResponseOwnerKind{ResponseOwnerHeaderFilter}},
			{phase: "body_filter", stage: RequestStageNone, owners: []ResponseOwnerKind{ResponseOwnerBufferedBodyFilter}},
			{phase: "log", stage: RequestStageNone},
		} {
			t.Run(factory+"/"+fixture.phase, func(t *testing.T) {
				config := serverlessConfig(fixture.phase)
				stage, ok, err := ResolveRequestStage(factory, config)
				if err != nil || !ok || stage.Stage != fixture.stage {
					t.Fatalf("request stage = %#v/%v/%v, want %v", stage, ok, err, fixture.stage)
				}
				response, err := ResolveResponsePhases(factory, config)
				if err != nil || !reflect.DeepEqual(response.Owners, fixture.owners) {
					t.Fatalf("response owners = %v/%v, want %v", response.Owners, err, fixture.owners)
				}
				owner, ok, err := ResolveBeforeProxyOwner(factory, config)
				if err != nil || ok != fixture.before || ok && owner != RequestOwnerBeforeProxyConsumer {
					t.Fatalf("before-proxy owner = %v/%v/%v", owner, ok, err)
				}
			})
		}
	}
	for factory, want := range map[string]CompressionSet{
		"gzip":   CompressionOfferGzip | CompressionOfferDeflate,
		"brotli": CompressionOfferBrotli,
	} {
		phases, err := ResolveResponsePhases(factory, nil)
		if err != nil || phases.CompressionOffers != want {
			t.Errorf("%s compression offers = %v/%v, want %v", factory, phases.CompressionOffers, err, want)
		}
	}
	for factory, kind := range map[string]GenerationOwnerKind{
		"error-log-logger": GenerationOwnerProcess,
		"server-info":      GenerationOwnerProcess,
		"log-rotate":       GenerationOwnerRoute,
	} {
		spec, _ := CapabilitySpecForFactory(factory)
		if spec.GenerationOwner != kind {
			t.Errorf("%s generation owner = %v, want %v", factory, spec.GenerationOwner, kind)
		}
	}
}

func TestResolveResponsePhasesIsConfigAware(t *testing.T) {
	bounded := responseModeTestConfig{modes: base.ResponseModeBounded}
	streaming := responseModeTestConfig{modes: base.ResponseModeStreaming}
	boundedPhases, err := ResolveResponsePhases("ai-rate-limiting", bounded)
	if err != nil {
		t.Fatalf("bounded response phases: %v", err)
	}
	streamingPhases, err := ResolveResponsePhases("ai-rate-limiting", streaming)
	if err != nil {
		t.Fatalf("streaming response phases: %v", err)
	}
	if !reflect.DeepEqual(
		boundedPhases.Owners,
		[]ResponseOwnerKind{ResponseOwnerBufferedBodyFilter},
	) {
		t.Fatalf("bounded owners = %v", boundedPhases.Owners)
	}
	if !reflect.DeepEqual(
		streamingPhases.Owners,
		[]ResponseOwnerKind{ResponseOwnerStreamingBodyFilter},
	) {
		t.Fatalf("streaming owners = %v", streamingPhases.Owners)
	}
}

func TestCapabilityPhaseFixturesCoverPlan15And16Owners(t *testing.T) {
	fixtures := []struct {
		factory  string
		config   any
		owners   []ResponseOwnerKind
		protocol ProtocolKind
	}{
		{factory: "proxy-cache", owners: []ResponseOwnerKind{ResponseOwnerFinalStore}},
		{factory: "graphql-proxy-cache", owners: []ResponseOwnerKind{ResponseOwnerFinalStore}},
		{
			factory: "proxy-buffering",
			config:  responseModeTestConfig{modes: base.ResponseModeStreaming},
			owners:  []ResponseOwnerKind{ResponseOwnerStreamingBodyFilter},
		},
		{
			factory: "ai-aliyun-content-moderation",
			config:  responseModeTestConfig{modes: base.ResponseModeBounded},
			owners:  []ResponseOwnerKind{ResponseOwnerBufferedBodyFilter},
		},
		{
			factory: "ai-aliyun-content-moderation",
			config:  responseModeTestConfig{modes: base.ResponseModeStreaming},
			owners:  []ResponseOwnerKind{ResponseOwnerStreamingBodyFilter},
		},
		{factory: "proxy-buffering", config: responseModeTestConfig{modes: base.ResponseModeBounded}},
		{
			factory:  "ai-proxy",
			owners:   []ResponseOwnerKind{ResponseOwnerStreamingProducer, ResponseOwnerExclusiveProtocol},
			protocol: ProtocolAI,
		},
		{
			factory:  "ai-proxy-multi",
			owners:   []ResponseOwnerKind{ResponseOwnerStreamingProducer, ResponseOwnerExclusiveProtocol},
			protocol: ProtocolAI,
		},
		{
			factory: "grpc-transcode",
			config:  responseModeTestConfig{modes: base.ResponseModeBounded},
			owners:  []ResponseOwnerKind{ResponseOwnerBufferedBodyFilter},
		},
		{
			factory: "grpc-transcode",
			config:  responseModeTestConfig{modes: base.ResponseModeStreaming},
		},
		{
			factory: "grpc-web",
			owners: []ResponseOwnerKind{
				ResponseOwnerHeaderFilter,
				ResponseOwnerStreamingHeaderFilter,
				ResponseOwnerStreamingBodyFilter,
				ResponseOwnerExclusiveProtocol,
			},
			protocol: ProtocolGRPCWeb,
		},
		{factory: "dubbo-proxy", owners: []ResponseOwnerKind{ResponseOwnerExclusiveProtocol}, protocol: ProtocolDubbo},
		{
			factory:  "http-dubbo",
			owners:   []ResponseOwnerKind{ResponseOwnerExclusiveProtocol},
			protocol: ProtocolHTTPDubbo,
		},
		{
			factory:  "kafka-proxy",
			owners:   []ResponseOwnerKind{ResponseOwnerStreamingProducer, ResponseOwnerExclusiveProtocol},
			protocol: ProtocolKafka,
		},
		{
			factory:  "mqtt-proxy",
			owners:   []ResponseOwnerKind{ResponseOwnerStreamingProducer, ResponseOwnerSeparateSubsystemProtocol},
			protocol: ProtocolMQTT,
		},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.factory+fmt.Sprint(fixture.config), func(t *testing.T) {
			phases, err := ResolveResponsePhases(fixture.factory, fixture.config)
			if err != nil {
				t.Fatalf("ResolveResponsePhases() error = %v", err)
			}
			if !slices.Equal(phases.Owners, fixture.owners) || phases.ExclusiveProtocol != fixture.protocol {
				t.Fatalf("phases = %#v, want owners=%v protocol=%v", phases, fixture.owners, fixture.protocol)
			}
		})
	}
}

func TestCapabilityCacheFixturesHaveLookupAndFinalStoreOwners(t *testing.T) {
	for _, factory := range []string{"proxy-cache", "graphql-proxy-cache"} {
		t.Run(factory, func(t *testing.T) {
			stage, ok, err := ResolveRequestStage(factory, nil)
			if err != nil || !ok || stage.Stage != RequestStageAccess {
				t.Fatalf("lookup stage = %#v/%v/%v, want access", stage, ok, err)
			}
			phases, err := ResolveResponsePhases(factory, nil)
			if err != nil || !slices.Equal(phases.Owners, []ResponseOwnerKind{ResponseOwnerFinalStore}) {
				t.Fatalf("store owners = %#v/%v, want final store", phases.Owners, err)
			}
		})
	}
}

func TestCapabilityLogFactoriesMaterializeExactlyOneBinding(t *testing.T) {
	for factory := range pluginRegistry {
		spec, ok := CapabilitySpecForFactory(factory)
		if !ok || spec.Capabilities&CapabilityLog == 0 {
			continue
		}
		t.Run(factory, func(t *testing.T) {
			instance := New(factory, base.Dependencies{})
			if instance == nil {
				t.Fatalf("New(%q) returned nil", factory)
			}
			if err := instance.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if isServerlessIdentity(factory) {
				if err := json.Unmarshal([]byte(`{"phase":"log"}`), instance.Config()); err != nil {
					t.Fatalf("configure log phase: %v", err)
				}
			}
			binding, err := BindPluginChecked(
				factory,
				instance,
				ScopeRoute,
				ResourceProvenance{Kind: ResourceRoute, ID: "log-fixture"},
			)
			if err != nil {
				t.Fatalf("BindPluginChecked() error = %v", err)
			}
			executor, err := NewLogExecutorFromBindings([]Binding{binding})
			if err != nil {
				t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
			}
			if got := len(executor.Bindings()); got != 1 {
				t.Fatalf("materialized log bindings = %d, want 1", got)
			}
		})
	}
}

func TestResolveBeforeProxyOwnerExactSet(t *testing.T) {
	for _, factory := range []string{"ai-proxy", "ai-proxy-multi", "dubbo-proxy", "http-dubbo", "kafka-proxy"} {
		owner, ok, err := ResolveBeforeProxyOwner(factory, nil)
		if err != nil || !ok || owner != RequestOwnerBeforeProxyConsumer {
			t.Fatalf("ResolveBeforeProxyOwner(%q) = %v/%v/%v", factory, owner, ok, err)
		}
	}
	owner, ok, err := ResolveBeforeProxyOwner("proxy-mirror", nil)
	if err != nil || !ok || owner != RequestOwnerBeforeProxyHookRegistration {
		t.Fatalf("proxy-mirror owner = %v/%v/%v", owner, ok, err)
	}
	if _, ok, err := ResolveBeforeProxyOwner("cors", nil); err != nil || ok {
		t.Fatalf("cors owner = %v/%v", ok, err)
	}
}
