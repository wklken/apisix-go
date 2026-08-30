package pluginintegration

func mustDifferentialCase(name string) DifferentialCase {
	for _, spec := range differentialCases() {
		if spec.Name == name {
			return spec
		}
	}
	panic("differential case is absent from catalog: " + name)
}
