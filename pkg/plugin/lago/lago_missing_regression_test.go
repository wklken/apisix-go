package lago

import (
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestMissingBarePropertyVariablePreservesTemplate(t *testing.T) {
	p := &Plugin{now: time.Now, config: Config{
		EventTransactionID:  "tx",
		EventSubscriptionID: "sub",
		EventCode:           "requests",
		EventProperties:     map[string]string{"optional": "prefix-$missing"},
	}}
	got := p.buildEvent(map[string]any{}).Properties["optional"]
	if got != "prefix-$missing" {
		t.Fatalf("missing property variable = %q, APISIX keeps the configured template when none resolve", got)
	}
}

func TestSnapshotMissingPropertyVariablePreservesTemplate(t *testing.T) {
	p := &Plugin{
		now: time.Now,
		config: Config{
			EventTransactionID:  "tx",
			EventSubscriptionID: "sub",
			EventCode:           "requests",
			EventProperties:     map[string]string{"optional": "prefix-$missing"},
		},
	}
	got := p.buildEvent(p.lagoSnapshotFields(base.LogSnapshot{})).Properties["optional"]
	if got != "prefix-$missing" {
		t.Fatalf("detached missing property=%q", got)
	}
}
