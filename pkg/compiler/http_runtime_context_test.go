package compiler

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestHTTPRuntimeContextUsesImmutableResolversAndCompilerOwnedClusters(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	checks := map[string]any{"passive": map[string]any{"healthy": true}}
	plan := &httpPreparationPlan{
		resources: httpResourceSet{
			upstreams: map[string]resource.Upstream{"u1": {
				Scheme: "http",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 9080, Weight: 1}},
				Checks: checks,
			}},
			ssls: map[string]resource.SSL{},
		},
		enabledFactories:  []string{"traffic-split"},
		publicAPIRegistry: public_api.NewRegistry(),
	}
	runtimeContext, err := prepared.httpRuntimeContextForRoute(
		context.Background(), resource.Route{ID: "r1"}, plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := runtimeContext.upstreamResolver("u1")
	if err != nil {
		t.Fatal(err)
	}
	resolved.Checks["passive"].(map[string]any)["healthy"] = false
	again, err := runtimeContext.upstreamResolver("u1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Checks["passive"].(map[string]any)["healthy"] != true {
		t.Fatal("immutable upstream resolver observed returned-value mutation")
	}

	upstream := &traffic_split.Upstream{
		Scheme: "http",
		Nodes:  []traffic_split.Node{{Host: "127.0.0.1", Port: 9080, Weight: 1}},
	}
	runtimeValue, err := runtimeContext.runtimeAcquirer.Acquire(
		upstream,
		map[string]int{"http://127.0.0.1:9080": 1},
		map[string]int{"http://127.0.0.1:9080": 0},
	)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeValue == nil || runtimeValue.LoadBalancer == nil || runtimeValue.RoundTripper == nil ||
		fixture.registry.Len() != 1 {
		t.Fatalf("traffic-split runtime/registry = %#v/%d", runtimeValue, fixture.registry.Len())
	}
}
