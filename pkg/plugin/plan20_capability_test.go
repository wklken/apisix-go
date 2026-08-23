package plugin

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestDataMaskLogSnapshotSanitizerCapabilityIsExact(t *testing.T) {
	spec, ok := CapabilitySpecForFactory("data-mask")
	if !ok {
		t.Fatal("data-mask capability is missing")
	}
	if spec.Capabilities != CapabilityLogSanitizer {
		t.Fatalf("data-mask capabilities = %#x, want only log sanitizer", spec.Capabilities)
	}
	if _, ok := New("data-mask", base.Dependencies{}).(base.LogSnapshotSanitizerPlugin); !ok {
		t.Fatal("data-mask does not implement the log snapshot sanitizer interface")
	}
	keyAuth, ok := CapabilitySpecForFactory("key-auth")
	if !ok || keyAuth.Capabilities&CapabilityLogSanitizer == 0 {
		t.Fatalf("key-auth capability = %#v/%v, want log sanitizer", keyAuth, ok)
	}
	if _, ok := New("key-auth", base.Dependencies{}).(base.LogSnapshotSanitizerPlugin); !ok {
		t.Fatal("key-auth does not implement the log snapshot sanitizer interface")
	}
	stage, ok := RequestStageFor("data-mask")
	if !ok || stage != (RequestStageSpec{Stage: RequestStageNone}) {
		t.Fatalf("data-mask request stage = %#v/%v, want none", stage, ok)
	}
	if isConditionalTerminalIdentity("data-mask") {
		t.Fatal("data-mask remains a conditional request terminal")
	}
}
