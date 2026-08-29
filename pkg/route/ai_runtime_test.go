package route

import "github.com/wklken/apisix-go/pkg/plugin"

func assembleRouteExecutor(
	routeBindings []plugin.Binding,
	globalBindings []plugin.Binding,
	systemBindings []plugin.Binding,
) plugin.RequestPipeline {
	bindings := make([]plugin.Binding, 0, len(routeBindings)+len(globalBindings)+len(systemBindings))
	bindings = append(bindings, systemBindings...)
	bindings = append(bindings, globalBindings...)
	bindings = append(bindings, routeBindings...)
	return plugin.NewRequestPipeline(bindings, nil)
}
