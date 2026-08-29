package route

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

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

func requestPipelineForTest(plugins ...plugin.Plugin) plugin.RequestPipeline {
	bindings := make([]plugin.Binding, 0, len(plugins))
	for _, p := range plugins {
		bindings = append(bindings, bindPluginForTest(
			p.GetName(),
			p,
			plugin.ScopeRoute,
			plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: p.GetName()},
		))
	}
	return plugin.NewRequestPipeline(bindings, nil)
}

type pluginChainForTest []plugin.Plugin

func withRequestPipelineForTest(plugins pluginChainForTest, terminal http.Handler) http.Handler {
	pipeline := plugin.NewRequestPipeline(nil, nil)
	return plugins.Then(pipeline.Then(terminal))
}

func (plugins pluginChainForTest) Then(terminal http.Handler) http.Handler {
	ordered := append([]plugin.Plugin(nil), plugins...)
	slices.SortFunc(ordered, func(a, b plugin.Plugin) int {
		return cmp.Compare(b.GetPriority(), a.GetPriority())
	})
	transformCount := 0
	for _, p := range ordered {
		if isResponseTransformForTest(p.GetName()) {
			transformCount++
		}
	}
	handler := terminal
	for _, p := range slices.Backward(ordered) {
		handler = p.Handler(handler)
	}
	return base.WithTransformPipeline(transformCount)(handler)
}

func isResponseTransformForTest(name string) bool {
	switch name {
	case "proxy-cache", "echo", "response-rewrite", "serverless-pre-function", "serverless-post-function",
		"brotli", "ai-rate-limiting", "grpc-transcode", "exit-transformer", "body-transformer",
		"error-page", "graphql-proxy-cache":
		return true
	default:
		return false
	}
}
