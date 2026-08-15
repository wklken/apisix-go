package plugin

import (
	"errors"
	"net/http"
	"testing"
)

type materializingTestPlugin struct {
	materializeErr error
	called         bool
}

func (p *materializingTestPlugin) Init() error               { return nil }
func (p *materializingTestPlugin) PostInit() error           { return nil }
func (p *materializingTestPlugin) Config() any               { return nil }
func (p *materializingTestPlugin) GetSchema() string         { return "" }
func (p *materializingTestPlugin) GetMetadataSchema() string { return "" }
func (p *materializingTestPlugin) GetPriority() int          { return 0 }
func (p *materializingTestPlugin) GetName() string           { return "materializing-test" }
func (p *materializingTestPlugin) Handler(next http.Handler) http.Handler {
	return next
}

func (p *materializingTestPlugin) MaterializeSecrets() error {
	p.called = true
	return p.materializeErr
}

func TestMaterializePluginSecretsCallsOnlyDeclaredOwnerAndPropagatesError(t *testing.T) {
	want := errors.New("credential unavailable")
	p := &materializingTestPlugin{materializeErr: want}
	if err := MaterializePluginSecrets(p); !errors.Is(err, want) {
		t.Fatalf("MaterializePluginSecrets() error = %v, want %v", err, want)
	}
	if !p.called {
		t.Fatal("MaterializeSecrets() was not called")
	}
	if err := MaterializePluginSecrets(&nonMaterializingTestPlugin{}); err != nil {
		t.Fatalf("MaterializePluginSecrets(plain) error = %v", err)
	}
}

type nonMaterializingTestPlugin struct{}

func (*nonMaterializingTestPlugin) Init() error               { return nil }
func (*nonMaterializingTestPlugin) PostInit() error           { return nil }
func (*nonMaterializingTestPlugin) Config() any               { return nil }
func (*nonMaterializingTestPlugin) GetSchema() string         { return "" }
func (*nonMaterializingTestPlugin) GetMetadataSchema() string { return "" }
func (*nonMaterializingTestPlugin) GetPriority() int          { return 0 }
func (*nonMaterializingTestPlugin) GetName() string           { return "plain-test" }
func (*nonMaterializingTestPlugin) Handler(next http.Handler) http.Handler {
	return next
}
