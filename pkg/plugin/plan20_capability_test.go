package plugin

import "testing"

func TestDataMaskLogSnapshotSanitizerCapabilityIsExact(t *testing.T) {
	spec, ok := CapabilitySpecForFactory("data-mask")
	if !ok {
		t.Fatal("data-mask capability is missing")
	}
	if spec.PrimaryPlan != "Plan 20" {
		t.Fatalf("data-mask primary plan = %q, want Plan 20", spec.PrimaryPlan)
	}
	if spec.Capabilities != CapabilityLogSanitizer {
		t.Fatalf("data-mask capabilities = %#x, want only log sanitizer", spec.Capabilities)
	}
	stage, ok := RequestStageFor("data-mask")
	if !ok || stage != (RequestStageSpec{Stage: RequestStageNone}) {
		t.Fatalf("data-mask request stage = %#v/%v, want none", stage, ok)
	}
	if isConditionalTerminalIdentity("data-mask") {
		t.Fatal("data-mask remains a conditional request terminal")
	}
}
