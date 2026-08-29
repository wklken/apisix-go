package plugin

func bindPluginForTest(
	factoryName string,
	p Plugin,
	scope Scope,
	provenance ResourceProvenance,
) Binding {
	if binding, err := BindPluginChecked(factoryName, p, scope, provenance); err == nil {
		binding.Priority = p.GetPriority()
		return binding
	}
	priority := 0
	if p != nil {
		priority = p.GetPriority()
	}
	return Binding{
		Plugin:     p,
		Descriptor: Descriptor{Factory: factoryName, Priority: priority},
		Priority:   priority,
		Scope:      scope,
		Provenance: provenance,
	}
}
