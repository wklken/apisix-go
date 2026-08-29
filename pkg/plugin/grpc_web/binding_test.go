package grpc_web_test

import "github.com/wklken/apisix-go/pkg/plugin"

func bindPluginForTest(
	factoryName string,
	p plugin.Plugin,
	scope plugin.Scope,
	provenance plugin.ResourceProvenance,
) plugin.Binding {
	binding, err := plugin.BindPluginChecked(
		factoryName,
		p,
		scope,
		provenance,
	)
	if err != nil {
		panic(err)
	}
	return binding
}
