package compiler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_transcode"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

func TestPreparedGenerationCompilesGenerationLocalGRPCProtoImports(t *testing.T) {
	compile := func(revision uint64, fieldNumber int) []byte {
		t.Helper()
		rootProto := `syntax = "proto3"; package fixture; import "common.proto"; ` +
			`service Echo { rpc Call(common.Message) returns (common.Message); }`
		commonProto := fmt.Sprintf(
			`syntax = "proto3"; package common; message Message { string value = %d; }`,
			fieldNumber,
		)
		snapshot := mustGenerationSnapshot(t, revision, []generation.Resource{
			resourceValue("routes", "grpc", `{
"id":"grpc","uri":"/grpc","plugin_config_id":"plugin-config-1"}`),
			resourceValue("plugin_configs", "plugin-config-1", `{
"plugins":{"grpc-transcode":{
"proto_id":"root.proto","service":"fixture.Echo","method":"Call"
}}}`),
			resourceValue("protos", "root.proto", fmt.Sprintf(`{"content":%q}`, rootProto)),
			resourceValue("protos", "common.proto", fmt.Sprintf(`{"content":%q}`, commonProto)),
		}, nil)
		candidate := compileDomain(t, generation.DomainHTTP, snapshot, generation.PublishedGeneration{}, false)
		prepared, _ := newEffectiveBindingMaterializerFixture(
			t,
			[]string{"grpc-transcode"},
			map[generation.Domain]generation.PublicationCandidate{generation.DomainHTTP: candidate},
		)
		prepared.effective.Config.Plugins = []string{"grpc-transcode"}
		plan, err := prepared.planHTTPPreparation(context.Background(), candidate)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.plugins.Routes) != 1 {
			t.Fatalf("planned grpc routes = %d", len(plan.plugins.Routes))
		}
		planned := plan.plugins.Routes[0]
		runtimeContext, err := prepared.httpRuntimeContextForRoute(context.Background(), planned.Route, plan)
		if err != nil {
			t.Fatal(err)
		}
		bindings, err := prepared.materializeHTTPPluginPlans(
			context.Background(),
			generation.ResourceKey{Kind: "routes", ID: planned.Route.ID},
			planned.Local,
			effectiveBindingResourceContext{kind: effectiveBindingContextHTTP, route: planned.Route},
			runtimeContext,
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(bindings) != 1 {
			t.Fatalf("prepared grpc local bindings = %d", len(bindings))
		}
		instance, ok := bindings[0].Plugin.(*grpc_transcode.Plugin)
		if !ok {
			t.Fatalf("grpc binding plugin = %T", bindings[0].Plugin)
		}
		response := httptest.NewRecorder()
		result := instance.RunRequestPhase(
			response,
			httptest.NewRequest(http.MethodGet, "/grpc?value=hello", nil),
		)
		if result.Request == nil {
			t.Fatalf("grpc request phase stopped: status=%d body=%q", response.Code, response.Body.String())
		}
		body, err := io.ReadAll(result.Request.Body)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	first := compile(91, 1)
	second := compile(92, 2)
	if bytes.Equal(first, second) {
		t.Fatalf("different generation proto bytes are equal: %x", first)
	}
	if !bytes.Equal(first, []byte{0, 0, 0, 0, 7, 0x0a, 5, 'h', 'e', 'l', 'l', 'o'}) {
		t.Fatalf("generation 91 grpc bytes = %x", first)
	}
	if !bytes.Equal(second, []byte{0, 0, 0, 0, 7, 0x12, 5, 'h', 'e', 'l', 'l', 'o'}) {
		t.Fatalf("generation 92 grpc bytes = %x", second)
	}
}

func TestWorkerFactoryCompilesGlobalGRPCTranscodeWithGenerationProtoImports(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	factory.effective.Config.Plugins = []string{"grpc-transcode"}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	rootProto := `syntax = "proto3"; package fixture; import "common.proto"; ` +
		`service Echo { rpc Call(common.Message) returns (common.Message); }`
	commonProto := `syntax = "proto3"; package common; message Message { string value = 1; }`
	desired := mustGenerationSnapshot(t, 93, []generation.Resource{
		resourceValue("global_rules", "grpc-global", `{
"id":"grpc-global","plugins":{"grpc-transcode":{
"proto_id":"root.proto","service":"fixture.Echo","method":"Call"
}}}`),
		resourceValue("protos", "root.proto", fmt.Sprintf(`{"content":%q}`, rootProto)),
		resourceValue("protos", "common.proto", fmt.Sprintf(`{"content":%q}`, commonProto)),
	}, nil)

	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.HTTP() == nil {
		t.Fatal("prepared HTTP snapshot = nil")
	}
}

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
