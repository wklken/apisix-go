package route

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin"
)

type pluginExecutor interface {
	Then(http.Handler) http.Handler
}

func withRequestPipeline(chain pluginExecutor, fallback http.Handler) http.Handler {
	pipeline := plugin.NewRequestPipeline(nil, nil)
	return chain.Then(pipeline.Then(fallback))
}

func assembleRouteExecutor(
	routeBindings []plugin.Binding,
	globalBindings []plugin.Binding,
	systemBindings []plugin.Binding,
) plugin.Executor {
	bindings := make([]plugin.Binding, 0, len(routeBindings)+len(globalBindings)+len(systemBindings))
	bindings = append(bindings, systemBindings...)
	bindings = append(bindings, globalBindings...)
	bindings = append(bindings, routeBindings...)
	return plugin.NewScopedExecutor(bindings...)
}
