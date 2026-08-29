package server

import "github.com/wklken/apisix-go/pkg/plugin"

func bindPluginForTest(
	factoryName string,
	p plugin.Plugin,
	scope plugin.Scope,
	provenance plugin.ResourceProvenance,
) plugin.Binding {
	if binding, err := plugin.BindPluginChecked(factoryName, p, scope, provenance); err == nil {
		binding.Priority = p.GetPriority()
		return binding
	}
	priority := 0
	if p != nil {
		priority = p.GetPriority()
	}
	return plugin.Binding{
		Plugin:     p,
		Descriptor: plugin.Descriptor{Factory: factoryName, Priority: priority},
		Priority:   priority,
		Scope:      scope,
		Provenance: provenance,
	}
}
