package route

import (
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type matchedRouteRequestContext struct {
	plugin.Plugin
	requestPhase base.RequestPhasePlugin
}

func (p matchedRouteRequestContext) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	result := p.requestPhase.RunRequestPhase(w, r)
	request := result.Request
	if request == nil {
		request = r
	}
	if uri, host, ok := apisixctx.MatchedRoute(request); ok {
		apisixctx.RegisterApisixVar(request, "$matched_uri", uri)
		apisixctx.RegisterApisixVar(request, "$matched_host", host)
	}
	result.Request = request
	return result
}

func bindMatchedRouteRequestContext(bindings []plugin.Binding) []plugin.Binding {
	bound := append([]plugin.Binding(nil), bindings...)
	for index := range bound {
		binding := &bound[index]
		if binding.Descriptor.Factory != "request-context" || binding.Plugin == nil {
			continue
		}
		requestPhase, ok := binding.Plugin.(base.RequestPhasePlugin)
		if !ok {
			continue
		}
		binding.Plugin = matchedRouteRequestContext{
			Plugin: binding.Plugin, requestPhase: requestPhase,
		}
	}
	return bound
}
