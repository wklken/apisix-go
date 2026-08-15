package ai

import (
	"strings"
	"testing"
)

func TestInitFailsClosedForUnsupportedControlPlane(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	if err == nil {
		t.Fatal("PostInit() error = nil, want unsupported control-plane error")
	}
	if !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "control-plane") {
		t.Fatalf("PostInit() error = %q, want unsupported control-plane diagnostic", err)
	}
}
