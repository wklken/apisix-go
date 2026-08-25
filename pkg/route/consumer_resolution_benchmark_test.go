package route

import (
	"context"
	"runtime"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func BenchmarkPlanHTTPPluginsConsumer(b *testing.B) {
	input := PlanningInput{
		EnabledPlugins: []string{"limit-count"},
		Consumers: map[string]resource.Consumer{
			"benchmark-consumer": {
				Username: "benchmark-consumer",
				Plugins: map[string]resource.PluginConfig{
					"limit-count": map[string]any{
						"count": 1, "time_window": 60, "key": "remote_addr",
					},
				},
			},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink *HTTPPluginPlan
	for b.Loop() {
		plan, err := PlanHTTPPlugins(context.Background(), input)
		if err != nil {
			b.Fatal(err)
		}
		sink = plan
	}
	b.StopTimer()
	runtime.KeepAlive(sink)
}
