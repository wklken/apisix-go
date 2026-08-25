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
	protos := map[string]string{"root.proto": "root-content", "common.proto": "common-content"}
	plan := &httpPreparationPlan{
		resources: httpResourceSet{
			upstreams: map[string]resource.Upstream{"u1": {
				Scheme: "http",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 9080, Weight: 1}},
				Checks: checks,
			}},
			ssls:   map[string]resource.SSL{},
			protos: protos,
		},
		enabledFactories:  []string{"traffic-split"},
		publicAPIRegistry: public_api.NewRegistry(),
		protoResolver:     newHTTPProtoResolver(protos),
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
	protos["root.proto"] = "mutated"
	resolvedProto, err := runtimeContext.protoResolver("root.proto")
	if err != nil || resolvedProto != "root-content" {
		t.Fatalf("immutable proto resolver = %q, %v", resolvedProto, err)
	}
	secondRuntimeContext, err := prepared.httpRuntimeContextForRoute(
		context.Background(), resource.Route{ID: "r2"}, plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondResolvedProto, err := secondRuntimeContext.protoResolver("root.proto")
	if err != nil || secondResolvedProto != "root-content" {
		t.Fatalf("shared plan proto resolver = %q, %v", secondResolvedProto, err)
	}
	importedProto, err := runtimeContext.protoResolver("common.proto")
	if err != nil || importedProto != "common-content" {
		t.Fatalf("imported proto resolver = %q, %v", importedProto, err)
	}
	if _, err := runtimeContext.protoResolver("missing.proto"); err == nil {
		t.Fatal("missing generation proto resolved without error")
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

func TestHTTPPreparationPlanSharesProtoResolverWithRouteAndNotFoundRuntimes(t *testing.T) {
	prepared, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	calls := 0
	plan := &httpPreparationPlan{
		publicAPIRegistry: public_api.NewRegistry(),
		protoResolver: func(id string) (string, error) {
			calls++
			return id, nil
		},
	}
	routeRuntime, err := prepared.httpRuntimeContextForRoute(
		context.Background(), resource.Route{ID: "r1"}, plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	notFoundRuntime, err := prepared.httpRuntimeContextForNotFound(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeRuntime.protoResolver("route.proto"); err != nil {
		t.Fatal(err)
	}
	if _, err := notFoundRuntime.protoResolver("global.proto"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("shared proto resolver calls = %d, want 2", calls)
	}
}
