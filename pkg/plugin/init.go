package plugin

import (
	"sort"

	"github.com/justinas/alice"
	"github.com/wklken/apisix-go/pkg/plugin/basic_auth"
	"github.com/wklken/apisix-go/pkg/plugin/file_logger"
	"github.com/wklken/apisix-go/pkg/plugin/otel"
	"github.com/wklken/apisix-go/pkg/plugin/request_id"
)

func New(name string) Plugin {
	switch name {
	case "request_id":
		return &request_id.Plugin{}
	case "basic_auth":
		return &basic_auth.Plugin{}
	case "file_logger":
		return &file_logger.Plugin{}
	case "otel":
		return &otel.Plugin{}
	}
	return nil
}

func BuildPluginChain(plugins ...Plugin) alice.Chain {
	// sort the plugin by priority
	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].GetPriority() < plugins[j].GetPriority()
	})

	// build the alice chain
	chain := alice.New()
	for _, plugin := range plugins {
		chain = chain.Append(plugin.Handler)
	}

	return chain
}
