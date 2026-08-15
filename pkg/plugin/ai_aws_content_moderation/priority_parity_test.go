package ai_aws_content_moderation_test

import (
	"testing"

	awsmoderation "github.com/wklken/apisix-go/pkg/plugin/ai_aws_content_moderation"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy_multi"
	"github.com/wklken/apisix-go/pkg/plugin/ai_rate_limiting"
)

func TestAISelectionPriorityTableMatchesAPISIX317(t *testing.T) {
	type priorityPlugin interface {
		Init() error
		GetPriority() int
	}
	plugins := []struct {
		name string
		want int
		new  func() priorityPlugin
	}{
		{name: "ai-rate-limiting", want: 1030, new: func() priorityPlugin { return &ai_rate_limiting.Plugin{} }},
		{name: "ai-proxy", want: 1040, new: func() priorityPlugin { return &ai_proxy.Plugin{} }},
		{name: "ai-proxy-multi", want: 1041, new: func() priorityPlugin { return &ai_proxy_multi.Plugin{} }},
		{name: "ai-aws-content-moderation", want: 1050, new: func() priorityPlugin {
			return &awsmoderation.Plugin{}
		}},
	}
	for _, test := range plugins {
		plugin := test.new()
		if err := plugin.Init(); err != nil {
			t.Fatalf("%s Init() error = %v", test.name, err)
		}
		if got := plugin.GetPriority(); got != test.want {
			t.Errorf("%s priority = %d, want %d", test.name, got, test.want)
		}
	}
}
