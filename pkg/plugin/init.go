package plugin

import (
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type dependencyReceiver interface {
	SetDependencies(base.Dependencies)
}

// New returns the plugin registered for name, or nil for unknown names.
func New(name string, deps base.Dependencies) Plugin {
	registered, ok := pluginRegistry[name]
	if !ok {
		return nil
	}
	p := registered.create()
	receiver, ok := p.(dependencyReceiver)
	if !ok {
		panic("registered plugin does not embed base.BasePlugin")
	}
	receiver.SetDependencies(deps)
	return p
}
