package plugin

import (
	"slices"
	"sort"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestCapabilityManifestCoversEveryFactory(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, entry := range m.Plugins {
		for _, factory := range entry.Factories {
			seen[factory.Key] = entry.Name
		}
	}
	var missing, extra []string
	for key := range pluginRegistry {
		if _, ok := seen[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range seen {
		if _, ok := pluginRegistry[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("factory inventory mismatch: missing=%v extra=%v", missing, extra)
	}
}

func TestCapabilityManifestMatchesRuntimeFacts(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	for key, factory := range pluginRegistry {
		entry, ok := m.Plugin(key)
		if !ok {
			t.Fatalf("factory %s is absent from the capability manifest", key)
		}
		if factory == nil {
			t.Fatalf("factory %s is nil", key)
		}
		instance := factory()
		if instance == nil {
			t.Fatalf("factory %s returned nil", key)
		}
		if err := instance.Init(); err != nil {
			t.Fatalf("%s Init: %v", key, err)
		}
		if instance.GetName() != entry.Implementation {
			t.Fatalf(
				"%s implementation = %q, manifest = %q",
				key,
				instance.GetName(),
				entry.Implementation,
			)
		}
		if instance.GetPriority() != entry.Priority {
			t.Fatalf(
				"%s priority = %d, manifest = %d",
				key,
				instance.GetPriority(),
				entry.Priority,
			)
		}

		spec, ok := CapabilitySpecForFactory(key)
		if !ok {
			t.Fatalf("factory %s has no runtime capability spec", key)
		}
		if stage, ok := RequestStageFor(key); ok && !stage.ConfigAware {
			phase := manifestPhaseForRequestStage(stage.Stage)
			if phase != "" && !slices.Contains(entry.Phases, phase) {
				t.Fatalf("%s request stage %s is absent from manifest phases %v", key, phase, entry.Phases)
			}
		}
		wantPhases := manifestPhasesForRuntimeSpec(spec)
		if !slices.Equal(entry.Phases, wantPhases) {
			t.Fatalf("%s phases = %v, runtime = %v", key, entry.Phases, wantPhases)
		}
	}
}

func manifestPhaseForRequestStage(stage RequestStage) string {
	switch stage {
	case RequestStageRewrite:
		return "rewrite"
	case RequestStageConsumerRewrite:
		return "consumer_rewrite"
	case RequestStageAccess:
		return "access"
	case RequestStageBeforeProxy:
		return "before_proxy"
	default:
		return ""
	}
}

func manifestPhasesForRuntimeSpec(spec CapabilitySpec) []string {
	phases := make([]string, 0, 9)
	appendIf := func(phase string, capabilities Capability) {
		if spec.Capabilities&capabilities != 0 {
			phases = append(phases, phase)
		}
	}
	appendIf("rewrite", CapabilityRequestRewrite)
	appendIf("consumer_rewrite", CapabilityConsumerRewrite)
	appendIf("access", CapabilityRequestAccess)
	appendIf("before_proxy", CapabilityBeforeProxy)
	appendIf("header_filter", CapabilityHeaderFilter)
	appendIf(
		"body_filter",
		CapabilityBufferedBodyFilter|CapabilityFinalResponseStore|CapabilityStreamingBodyFilter,
	)
	appendIf("log", CapabilityLog|CapabilityLogSanitizer)
	appendIf("finalizer", CapabilityFinalizer)
	appendIf(
		"protocol",
		CapabilityStreamingResponseOwner|CapabilityExclusiveProtocolOwner|CapabilityProtocolOwner,
	)
	return phases
}
