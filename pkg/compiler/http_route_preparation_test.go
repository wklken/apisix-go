package compiler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	graphql_proxy_cache "github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_transcode"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func newScopedWorkerTestFactory(t *testing.T) *WorkerCompilerFactory {
	t.Helper()
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewWorkerCompilerFactory(
		workerTestEffective(),
		testutil.NewSecretMaterializer(&countingScopedBroker{}, catalog),
		workerTestRuntimeObservers(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

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

func TestWorkerFactoryPreparesAIProxyMultiTerminalRoute(t *testing.T) {
	factory := newScopedWorkerTestFactory(t)
	factory.effective.Config.Plugins = []string{"ai-proxy-multi"}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	desired := mustGenerationSnapshot(t, 94, []generation.Resource{
		resourceValue("routes", "ai-proxy-multi", `{
"id":"ai-proxy-multi","uri":"/v1/messages","plugins":{"ai-proxy-multi":{
"instances":[{"name":"openai-backend","provider":"openai-compatible","weight":1,
"auth":{"header":{"Authorization":"Bearer test-token"}},
"options":{"model":"gpt-4o"},"override":{"endpoint":"http://127.0.0.1:1"}}],
"ssl_verify":false}}}`),
	}, nil)
	var trace []string
	factory.checkpoint = func(stage string, _ workerFactoryCheckpointState) error {
		trace = append(trace, stage)
		return nil
	}
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	registered, err := factory.generations.prepareGenerationSecrets(
		context.Background(),
		ticket,
		desired,
		nil,
	)
	if err != nil {
		t.Fatalf("prepareGenerationSecrets() error = %v", err)
	}
	prepared, err := factory.transferPreparedGeneration(context.Background(), registered, nil)
	if err != nil {
		t.Fatalf("transferPreparedGeneration() error = %v, completed stages = %v", err, trace)
	}
	if prepared.HTTP() == nil {
		t.Fatal("prepared HTTP snapshot = nil")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerFactoryPreparesAIProxyWithResponseRewrite(t *testing.T) {
	factory := newScopedWorkerTestFactory(t)
	factory.effective.Config.Plugins = []string{"ai-proxy", "response-rewrite"}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	desired := mustGenerationSnapshot(t, 95, []generation.Resource{
		resourceValue("routes", "ai-proxy-response-rewrite", `{
"id":"ai-proxy-response-rewrite","uri":"/v1/messages","plugins":{
"ai-proxy":{"provider":"anthropic","auth":{"header":{"x-api-key":"test-key"}},
"options":{"model":"claude"},"override":{"endpoint":"http://127.0.0.1:1/v1/messages"}},
"response-rewrite":{"headers":{"set":{"X-LLM-Tool-Count":"$llm_tool_count"}}}}}`),
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
	defer func() {
		if err := prepared.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if got := prepared.HTTP().Quarantined(); len(got) != 0 {
		t.Fatalf("AI proxy + response-rewrite route quarantined = %#v", got)
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
		purgeRegistry:     graphql_proxy_cache.NewRegistry(),
	}

	routes, err := prepared.prepareHTTPRoutes(context.Background(), plan, "node-1")
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
		purgeRegistry:     graphql_proxy_cache.NewRegistry(),
	}

	routes, err := prepared.prepareHTTPRoutes(context.Background(), plan, "node-1")
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

func TestWorkerFactoryKeepsRouteWithRedisLimitCountConsumer(t *testing.T) {
	factory := newScopedWorkerTestFactory(t)
	factory.effective.Config.Plugins = []string{"key-auth", "limit-count"}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	desired := mustGenerationSnapshot(t, 196, []generation.Resource{
		resourceValue("consumers", "jack1", `{
"username":"jack1","plugins":{
"key-auth":{"key":"jack1"},
"limit-count":{"count":2,"key":"remote_addr","policy":"redis",
"redis_host":"127.0.0.1","redis_port":6379,"redis_timeout":1001,
"rejected_code":503,"show_limit_quota_header":true,"time_window":60}
}}`),
		resourceValue("routes", "consumer-limit-count", `{
"id":"consumer-limit-count","uri":"/consumer-limit-count",
"plugins":{"key-auth":{}},"upstream":{"nodes":{"127.0.0.1:9080":1},"type":"roundrobin"}}`),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatalf("PrepareGeneration() error = %v", err)
	}
	if got := prepared.HTTP().Quarantined(); len(got) != 0 {
		t.Fatalf("quarantined routes = %#v, want none", got)
	}
}
