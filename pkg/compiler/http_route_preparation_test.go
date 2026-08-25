package compiler

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

func TestPrepareHTTPRoutesQuarantinesInvalidRouteWithoutLosingPreparedCluster(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	plan := &httpPreparationPlan{
		plugins: &routepkg.HTTPPluginPlan{Routes: []routepkg.PlannedRoute{
			{Route: resource.Route{ID: "good", Upstream: resource.Upstream{
				Scheme: "http", Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9080, Weight: 1}},
			}}},
			{Route: resource.Route{ID: "bad", Upstream: resource.Upstream{
				Scheme: "http", Nodes: []resource.Node{{Host: "", Port: 9081, Weight: 1}},
			}}},
		}},
		publicAPIRegistry: public_api.NewRegistry(),
	}

	routes, err := prepared.prepareHTTPRoutes(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes.routes) != 1 || routes.routes[0].planned.Route.ID != "good" || routes.routes[0].cluster == nil {
		t.Fatalf("prepared routes = %#v, want good route with cluster", routes.routes)
	}
	wantQuarantine := generation.ResourceKey{Kind: "routes", ID: "bad"}
	if len(routes.quarantined) != 1 || routes.quarantined[0] != wantQuarantine {
		t.Fatalf("quarantined = %#v, want %#v", routes.quarantined, wantQuarantine)
	}
	if fixture.registry.Len() != 1 {
		t.Fatalf("registry len = %d, want one live good-route cluster", fixture.registry.Len())
	}
}

func TestPrepareHTTPRoutesRollsBackClusterWhenLaterConsumerPreparationFails(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	plan := &httpPreparationPlan{
		plugins: &routepkg.HTTPPluginPlan{
			Routes: []routepkg.PlannedRoute{{Route: resource.Route{ID: "rollback", Upstream: resource.Upstream{
				Scheme: "http", Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9082, Weight: 1}},
			}}}},
			Consumers: map[string][]routepkg.PluginPlan{"missing": nil},
		},
		publicAPIRegistry: public_api.NewRegistry(),
	}

	routes, err := prepared.prepareHTTPRoutes(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	wantQuarantine := generation.ResourceKey{Kind: "routes", ID: "rollback"}
	if len(routes.routes) != 0 || len(routes.quarantined) != 1 || routes.quarantined[0] != wantQuarantine {
		t.Fatalf("prepared/quarantined = %#v/%#v", routes.routes, routes.quarantined)
	}
	if fixture.registry.Len() != 0 {
		t.Fatalf("registry len = %d, want tentative cluster released", fixture.registry.Len())
	}
}
