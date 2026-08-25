package compiler

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestPlanStreamPreparationPreservesLegacyMergeAndExactProvenance(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 84, []generation.Resource{
		resourceValue(
			"stream_routes",
			"z",
			`{"id":"z","service_id":"s1","plugins":{"mqtt-proxy":{"protocol_name":"route","protocol_level":4}},"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1884":1}}}`,
		),
		resourceValue("stream_routes", "a", `{"id":"a","service_id":"s1"}`),
		resourceValue(
			"services",
			"s1",
			`{"id":"s1","plugins":{"mqtt-proxy":{"protocol_name":"service","protocol_level":4}},"upstream_id":"u1"}`,
		),
		resourceValue("upstreams", "u1", `{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}`),
	}, nil)
	candidate := compileDomain(t, generation.DomainStream, snapshot, generation.PublishedGeneration{}, false)
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t, nil, map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
	)
	prepared.effective.Config.StreamPlugins = []string{"mqtt-proxy"}

	plan, err := prepared.planStreamPreparation(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if plan.revision != 84 || !slices.Equal(plan.enabledFactories, []string{"mqtt-proxy"}) ||
		len(plan.routes) != 2 || plan.routes[0].route.ID != "a" || plan.routes[1].route.ID != "z" {
		t.Fatalf("plan identity/order = %#v", plan)
	}
	inherited := plan.routes[0]
	if inherited.service.ID != "s1" || inherited.route.UpstreamID != "u1" || len(inherited.route.Upstream.Nodes) != 1 {
		t.Fatalf("inherited route/service/upstream = %#v / %#v", inherited.route, inherited.service)
	}
	assertStreamBindingRequest(t, inherited.binding, "mqtt-proxy", "services", "s1", plugin.ResourceService)
	if got := inherited.binding.config.(map[string]any)["protocol_name"]; got != "service" {
		t.Fatalf("inherited config = %#v", inherited.binding.config)
	}
	routeWinner := plan.routes[1]
	assertStreamBindingRequest(t, routeWinner.binding, "mqtt-proxy", "stream_routes", "z", plugin.ResourceRoute)
	if got := routeWinner.binding.config.(map[string]any)["protocol_name"]; got != "route" {
		t.Fatalf("route winner config = %#v", routeWinner.binding.config)
	}
	if len(routeWinner.route.Upstream.Nodes) != 1 || routeWinner.route.Upstream.Nodes[0].Port != 1884 {
		t.Fatalf("route inline upstream lost precedence: %#v", routeWinner.route.Upstream)
	}
}

func TestPlanStreamPreparationRouteDisableSuppressesInheritedPlugin(t *testing.T) {
	resources := streamPlannerResources(
		resource.StreamRoute{
			ID: "r1", ServiceID: "s1", Plugins: map[string]resource.PluginConfig{
				"mqtt-proxy": map[string]any{"_meta": map[string]any{"disable": true}},
			},
		},
	)
	resources.services["s1"] = resource.Service{
		ID: "s1", Plugins: map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{"protocol_name": "service"}},
		Upstream: testStreamUpstream(1883),
	}

	plan, err := buildStreamPreparationPlan(
		context.Background(),
		resources,
		[]string{"mqtt-proxy"},
		newTestCompiler(t).manifest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.routes) != 1 || plan.routes[0].binding != nil || len(plan.routes[0].route.Plugins) != 0 {
		t.Fatalf("disabled winner was retained: %#v", plan.routes)
	}
}

func TestPlanStreamPreparationRouteUpstreamIDSuppressesServiceLookup(t *testing.T) {
	resources := streamPlannerResources(resource.StreamRoute{
		ID: "r1", ServiceID: "missing", UpstreamID: "u1",
		Plugins: map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{}},
	})
	resources.upstreams["u1"] = testStreamUpstream(1883)
	plan, err := buildStreamPreparationPlan(
		context.Background(),
		resources,
		[]string{"mqtt-proxy"},
		newTestCompiler(t).manifest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.routes[0].service.ID != "" || len(plan.routes[0].route.Upstream.Nodes) != 1 {
		t.Fatalf("service was read despite route upstream_id: %#v", plan.routes[0])
	}
}

func TestPlanStreamPreparationDynamicPluginContract(t *testing.T) {
	base := streamPlannerResources(resource.StreamRoute{
		ID: "r1", Plugins: map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{}},
		Upstream: testStreamUpstream(1883),
	})
	compiler := newTestCompiler(t)

	fallback, err := buildStreamPreparationPlan(context.Background(), base, []string{"mqtt-proxy"}, compiler.manifest)
	if err != nil || fallback.routes[0].binding == nil {
		t.Fatalf("static fallback plan/error = %#v / %v", fallback, err)
	}
	base.dynamicPlugins = true
	base.enabledPlugins = make([]string, 0)
	if _, err := buildStreamPreparationPlan(
		context.Background(),
		base,
		[]string{"mqtt-proxy"},
		compiler.manifest,
	); err == nil ||
		!strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("present empty dynamic set error = %v", err)
	}
	base.enabledPlugins = []string{"request-id"}
	if _, err := buildStreamPreparationPlan(
		context.Background(),
		base,
		[]string{"mqtt-proxy"},
		compiler.manifest,
	); err == nil ||
		!strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("HTTP-only dynamic entry leaked into stream: %v", err)
	}

	base.routes[0].Plugins = nil
	base.enabledPlugins = make([]string, 0)
	raw, err := buildStreamPreparationPlan(
		context.Background(),
		base,
		[]string{"mqtt-proxy"},
		compiler.manifest,
	)
	if err != nil || len(raw.routes) != 1 || raw.routes[0].binding != nil {
		t.Fatalf("present empty dynamic set rejected raw TCP route: %#v / %v", raw, err)
	}
}

func TestPlanStreamPreparationRejectsCandidateOutsideAttempt(t *testing.T) {
	first := mustGenerationSnapshot(t, 85, []generation.Resource{
		resourceValue("stream_routes", "r1", `{"id":"r1","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}}`),
	}, nil)
	second := mustGenerationSnapshot(t, 86, []generation.Resource{
		resourceValue("stream_routes", "r2", `{"id":"r2","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1884":1}}}`),
	}, nil)
	owned := compileDomain(t, generation.DomainStream, first, generation.PublishedGeneration{}, false)
	foreign := compileDomain(t, generation.DomainStream, second, generation.PublishedGeneration{}, false)
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t, nil, map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: owned},
	)
	if _, err := prepared.planStreamPreparation(context.Background(), foreign); err == nil {
		t.Fatal("foreign stream candidate error = nil")
	}
}

func TestPlanStreamPreparationRejectsDecodedAllZeroWeights(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 87, []generation.Resource{
		resourceValue("stream_routes", "r1", `{"id":"r1","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":0}}}`),
	}, nil)
	candidate := compileDomain(t, generation.DomainStream, snapshot, generation.PublishedGeneration{}, false)
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t, nil, map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
	)
	if _, err := prepared.planStreamPreparation(context.Background(), candidate); err == nil ||
		!strings.Contains(err.Error(), "positive weight") {
		t.Fatalf("all-zero upstream weights error = %v", err)
	}
}

func TestBuildStreamPreparationPlanOwnsNestedInputs(t *testing.T) {
	pluginConfig := map[string]any{"protocol_level": 4, "nested": map[string]any{"value": "before"}}
	resources := streamPlannerResources(resource.StreamRoute{
		ID:       "r1",
		Plugins:  map[string]resource.PluginConfig{"mqtt-proxy": pluginConfig},
		Upstream: testStreamUpstream(1883),
	})
	plan, err := buildStreamPreparationPlan(
		context.Background(), resources, []string{"mqtt-proxy"}, newTestCompiler(t).manifest,
	)
	if err != nil {
		t.Fatal(err)
	}
	resources.routes[0].ID = "mutated"
	resources.routes[0].Upstream.Nodes[0].Host = "mutated"
	pluginConfig["protocol_level"] = 5
	pluginConfig["nested"].(map[string]any)["value"] = "after"
	got := plan.routes[0]
	if got.route.ID != "r1" || got.route.Upstream.Nodes[0].Host != "127.0.0.1" {
		t.Fatalf("route plan aliased caller input: %#v", got.route)
	}
	config := got.binding.config.(map[string]any)
	if config["protocol_level"] != 4 || config["nested"].(map[string]any)["value"] != "before" {
		t.Fatalf("binding plan aliased caller config: %#v", config)
	}
}

func TestPlanStreamPreparationRejectsBeforeMaterialization(t *testing.T) {
	compiler := newTestCompiler(t)
	tests := []struct {
		name      string
		resources streamResourceSet
		want      string
	}{
		{
			name: "missing-service",
			resources: streamPlannerResources(
				resource.StreamRoute{ID: "r1", ServiceID: "missing", Upstream: testStreamUpstream(1883)},
			),
			want: "service",
		},
		{
			name:      "missing-upstream",
			resources: streamPlannerResources(resource.StreamRoute{ID: "r1", UpstreamID: "missing"}),
			want:      "upstream",
		},
		{
			name: "discovery",
			resources: streamPlannerResources(
				resource.StreamRoute{
					ID:       "r1",
					Upstream: resource.Upstream{Scheme: "tcp", DiscoveryType: "dns", ServiceName: "mqtt"},
				},
			),
			want: "discovery",
		},
		{
			name: "scheme",
			resources: streamPlannerResources(
				resource.StreamRoute{
					ID:       "r1",
					Upstream: resource.Upstream{Scheme: "tls", Nodes: testStreamUpstream(1883).Nodes},
				},
			),
			want: "scheme",
		},
		{
			name: "tls",
			resources: streamPlannerResources(
				resource.StreamRoute{
					ID: "r1",
					Upstream: resource.Upstream{
						Scheme: "tcp",
						TLS:    &resource.UpstreamTLS{},
						Nodes:  testStreamUpstream(1883).Nodes,
					},
				},
			),
			want: "TLS",
		},
		{
			name: "negative-weight",
			resources: streamPlannerResources(
				resource.StreamRoute{
					ID: "r1",
					Upstream: resource.Upstream{
						Scheme: "tcp",
						Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1883, Weight: -1}},
					},
				},
			),
			want: "weight",
		},
		{
			name: "unsupported-plugin",
			resources: streamPlannerResources(
				resource.StreamRoute{
					ID:       "r1",
					Upstream: testStreamUpstream(1883),
					Plugins:  map[string]resource.PluginConfig{"request-id": map[string]any{}},
				},
			),
			want: "stream factory",
		},
		{
			name: "multiple-plugins",
			resources: streamPlannerResources(
				resource.StreamRoute{
					ID:       "r1",
					Upstream: testStreamUpstream(1883),
					Plugins: map[string]resource.PluginConfig{
						"mqtt-proxy": map[string]any{},
						"request-id": map[string]any{},
					},
				},
			),
			want: "more than one",
		},
	}
	conflict := streamPlannerResources(
		resource.StreamRoute{ID: "r1", ServerAddr: "0.0.0.0", ServerPort: 9000, Upstream: testStreamUpstream(1883)},
		resource.StreamRoute{ID: "r2", ServerAddr: "0.0.0.0", ServerPort: 9000, Upstream: testStreamUpstream(1884)},
	)
	tests = append(tests, struct {
		name      string
		resources streamResourceSet
		want      string
	}{name: "listen-conflict", resources: conflict, want: "conflicting"})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildStreamPreparationPlan(
				context.Background(), test.resources, []string{"mqtt-proxy", "request-id"}, compiler.manifest,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func streamPlannerResources(routes ...resource.StreamRoute) streamResourceSet {
	return streamResourceSet{
		revision: 1, routes: routes,
		services: make(map[string]resource.Service), upstreams: make(map[string]resource.Upstream),
	}
}

func testStreamUpstream(port int) resource.Upstream {
	return resource.Upstream{Scheme: "tcp", Nodes: []resource.Node{{Host: "127.0.0.1", Port: port, Weight: 1}}}
}

func assertStreamBindingRequest(
	t *testing.T,
	request *streamBindingRequest,
	factory, kind, id string,
	provenance plugin.ResourceKind,
) {
	t.Helper()
	if request == nil || request.factory != factory ||
		request.source != (generation.ResourceKey{Kind: kind, ID: id}) ||
		request.scope != plugin.ScopeRoute ||
		request.provenance != (plugin.ResourceProvenance{Kind: provenance, ID: id}) {
		t.Fatalf("binding request = %#v", request)
	}
}
