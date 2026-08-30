package pluginintegration

func differentialCasesForPlugin(plugin string) []DifferentialCase {
	cases := make([]DifferentialCase, 0, 1)
	for _, spec := range differentialCases() {
		if spec.Plugin == plugin {
			cases = append(cases, spec)
		}
	}
	return cases
}
