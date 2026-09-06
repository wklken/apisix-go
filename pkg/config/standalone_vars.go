package config

// lyaml represents YAML null as an empty table, while APISIX JSON resources
// retain ngx.null. Preserve this distinction for route expression constants
// before normalizing the YAML resource document into JSON.
func normalizeStandaloneYAMLRouteVars(document any) {
	root, _ := document.(map[string]any)
	routes, _ := root["routes"].([]any)
	for _, raw := range routes {
		route, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if rules, ok := route["vars"].([]any); ok {
			route["vars"] = normalizeYAMLExpressionConstants(rules)
		}
	}
}

func normalizeYAMLExpressionConstants(value any) any {
	switch typed := value.(type) {
	case nil:
		return map[string]any{}
	case []any:
		for i, item := range typed {
			typed[i] = normalizeYAMLExpressionConstants(item)
		}
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeYAMLExpressionConstants(item)
		}
	}
	return value
}
