package route

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/error_log_logger"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/resource"
)

type recordingPlugin struct {
	name     string
	priority int
	order    *[]string
}

func (p *recordingPlugin) Init() error               { return nil }
func (p *recordingPlugin) PostInit() error           { return nil }
func (p *recordingPlugin) Config() any               { return nil }
func (p *recordingPlugin) GetSchema() string         { return "" }
func (p *recordingPlugin) GetMetadataSchema() string { return "" }
func (p *recordingPlugin) GetPriority() int          { return p.priority }
func (p *recordingPlugin) GetName() string           { return p.name }
func (p *recordingPlugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*p.order = append(*p.order, p.name)
		next.ServeHTTP(w, r)
	})
}

var _ pluginpkg.Plugin = (*recordingPlugin)(nil)

func TestBeforeProxyHookRunsOnceAfterTransformsAndBeforeFallback(t *testing.T) {
	var order []string
	fallback := withRequestPipeline(
		pluginpkg.BuildPluginChain(),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "fallback:"+r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}),
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = apisixctx.WithBeforeProxyHook(r, func(r *http.Request) error {
			order = append(order, "hook:"+r.URL.Path)
			return nil
		})
		r.URL.Path = "/final"
		fallback.ServeHTTP(w, r)
		if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
			t.Fatalf("run before-proxy hook: %v", err)
		}
	})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/original", nil))

	if got, want := strings.Join(order, ","), "hook:/final,fallback:/final"; got != want {
		t.Fatalf("execution order = %q, want %q", got, want)
	}
}

func TestBuildRoutePluginChainOrdersGlobalAndLocalPluginsByPriority(t *testing.T) {
	order := []string{}
	local := []pluginpkg.Binding{pluginpkg.BindPlugin(
		"local-auth",
		&recordingPlugin{name: "local-auth", priority: 2500, order: &order},
		pluginpkg.ScopeRoute,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "route-order"},
	)}
	global := []pluginpkg.Binding{pluginpkg.BindPlugin(
		"global-label",
		&recordingPlugin{name: "global-label", priority: 2399, order: &order},
		pluginpkg.ScopeGlobal,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceGlobalRule, ID: "global-order"},
	)}
	handler := assembleRouteExecutor(local, global, nil).Then(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			order = append(order, "upstream")
			w.WriteHeader(http.StatusNoContent)
		},
	))

	response := performRouteTestRequest(t, handler, "/priority")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if got := strings.Join(order, ","); got != "local-auth,global-label,upstream" {
		t.Fatalf("execution order = %q, want local-auth,global-label,upstream", got)
	}
}

func TestBuildGlobalNotFoundHandlerRunsGlobalPlugins(t *testing.T) {
	globalRule := resource.GlobalRule{
		ID: "global-transform",
		Plugins: map[string]resource.PluginConfig{
			"exit-transformer": map[string]any{
				"functions": []any{
					"return (function(code, body, header) if code == 404 then return 405 end return code, body, header end)(...)",
				},
			},
		},
	}
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		GlobalRules:    []resource.GlobalRule{globalRule},
		EnabledPlugins: []string{"exit-transformer"},
		Profiles:       testEffectiveConfig().Profiles,
	})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.Global) != 1 {
		t.Fatalf("global plans = %d, want 1", len(plan.Global))
	}
	binding := testPluginBindingForSource(
		t, plan.Global[0].Factory, plan.Global[0].Config,
		plan.Global[0].Scope, plan.Global[0].Provenance,
		resource.Route{}, resource.Service{}, "127.0.0.1:9080",
	)
	binding, err = plan.Global[0].Apply(binding)
	if err != nil {
		t.Fatalf("PluginPlan.Apply() error = %v", err)
	}
	handler, err := BuildPreparedNotFoundHandler([]pluginpkg.Binding{binding})
	if err != nil {
		t.Fatalf("BuildPreparedNotFoundHandler() error = %v", err)
	}

	response := performRouteTestRequest(t, handler, "/missing")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestBuildSystemPluginConfigsDoesNotGenerateGlobalClientControl(t *testing.T) {
	plugins := buildSystemPluginConfigs(
		resource.Route{ID: "global-limit"},
		resource.Service{},
		pluginpkg.NewEnabledSet(nil),
	)
	if _, ok := plugins["client-control"]; ok {
		t.Fatalf("system client-control = %#v, want server-owned global streaming limit", plugins["client-control"])
	}
}

func TestInlineUpstreamConfiguredIncludesDiscoveryFields(t *testing.T) {
	for _, test := range []struct {
		name     string
		upstream resource.Upstream
	}{
		{name: "discovery type", upstream: resource.Upstream{DiscoveryType: "dns"}},
		{name: "service name", upstream: resource.Upstream{ServiceName: "orders.default.svc"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !inlineUpstreamConfigured(test.upstream) {
				t.Fatalf("inlineUpstreamConfigured(%#v) = false, want true", test.upstream)
			}
		})
	}
}

func TestBuildHandlerRejectsDynamicDiscoveryWithStaticNodes(t *testing.T) {
	for _, test := range []struct {
		name  string
		field string
		set   func(*resource.Upstream)
	}{
		{name: "discovery type", field: "discovery_type", set: func(upstream *resource.Upstream) {
			upstream.DiscoveryType = "dns"
		}},
		{name: "service name", field: "service_name", set: func(upstream *resource.Upstream) {
			upstream.ServiceName = "orders.default.svc"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := resource.Upstream{
				Nodes: []resource.Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}},
			}
			test.set(&upstream)
			_, err := PlanRouteUpstream(resource.Route{
				ID:       "dynamic-discovery-route",
				Uri:      "/dynamic-discovery",
				Upstream: upstream,
			}, resource.Service{}, nil, nil, &testEffectiveConfig().Config)
			if err == nil {
				t.Fatal("PlanRouteUpstream() error = nil, want unsupported discovery error")
			}
			message := err.Error()
			if !strings.Contains(message, "dynamic-discovery-route") ||
				!strings.Contains(message, test.field) {
				t.Fatalf("buildHandlerStrict() error = %q, want route and field provenance", message)
			}
		})
	}
}

func TestResolveRouteUpstreamUsesReferencedUpstreamProvenance(t *testing.T) {
	const upstreamID = "referenced-discovery-upstream"
	plan, err := PlanRouteUpstream(
		resource.Route{ID: "referenced-discovery-route", UpstreamID: upstreamID},
		resource.Service{},
		map[string]resource.Upstream{upstreamID: {
			Nodes: []resource.Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}}, DiscoveryType: "dns",
		}},
		nil,
		&testEffectiveConfig().Config,
	)
	if err == nil {
		t.Fatal("PlanRouteUpstream() error = nil, want unsupported referenced discovery")
	}
	want := pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceUpstream, ID: upstreamID}
	if plan.Provenance != (pluginpkg.ResourceProvenance{}) && plan.Provenance != want {
		t.Fatalf("upstream provenance = %#v, want empty-on-error or %#v", plan.Provenance, want)
	}
	if !strings.Contains(err.Error(), `upstream "`+upstreamID+`"`) {
		t.Fatalf("PlanRouteUpstream() error = %q, want referenced upstream provenance", err)
	}
}

func TestPreparedLoggerOwnerStopFlushesBatches(t *testing.T) {
	delivered := make(chan struct{}, 1)
	logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logServer.Close)

	binding := testPluginBinding(
		t,
		"http-logger",
		map[string]any{
			"uri":              logServer.URL,
			"batch_max_size":   10,
			"buffer_duration":  60,
			"inactive_timeout": 60,
		},
		resource.Route{ID: "route-a"},
	)
	httpLogger, ok := binding.Plugin.(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", binding.Plugin)
	}
	if err := httpLogger.Fire(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("Fire() error = %v", err)
	}

	httpLogger.BatchProcessor.Stop()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for prepared logger owner to flush batch")
	}
}

func TestPreparedProxyCacheOwnersKeepConfiguredZoneAlive(t *testing.T) {
	zones := []appconfig.Zone{{Name: "route-refresh-memory", MemorySize: "1M"}}
	if err := proxy_cache.RefreshConfiguredZones(zones); err != nil {
		t.Fatalf("RefreshConfiguredZones() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy_cache.RefreshConfiguredZones(nil) })

	firstConfig := testEffectiveConfig()
	firstConfig.Config.Apisix.ProxyCache.Zones = zones
	pluginConfig := map[string]any{
		"cache_strategy": "memory",
		"cache_zone":     "route-refresh-memory",
		"cache_ttl":      60,
	}
	firstBinding := testPluginBindingForSourceWithDependencies(
		t, "proxy-cache", pluginConfig, pluginpkg.ScopeRoute,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "route-refresh"},
		resource.Route{ID: "route-refresh"}, resource.Service{}, "127.0.0.1:9080",
		base.Dependencies{Config: firstConfig},
	)
	firstPlugin, ok := firstBinding.Plugin.(*proxy_cache.Plugin)
	if !ok {
		t.Fatalf("first plugin type = %T, want *proxy_cache.Plugin", firstBinding.Plugin)
	}

	secondConfig := testEffectiveConfig()
	secondConfig.Config.Apisix.ProxyCache.Zones = zones
	secondBinding := testPluginBindingForSourceWithDependencies(
		t, "proxy-cache", pluginConfig, pluginpkg.ScopeRoute,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "route-refresh"},
		resource.Route{ID: "route-refresh"}, resource.Service{}, "127.0.0.1:9080",
		base.Dependencies{Config: secondConfig},
	)
	secondPlugin, ok := secondBinding.Plugin.(*proxy_cache.Plugin)
	if !ok {
		t.Fatalf("second plugin type = %T, want *proxy_cache.Plugin", secondBinding.Plugin)
	}

	t.Cleanup(firstPlugin.Stop)
	t.Cleanup(secondPlugin.Stop)
	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte("route-refresh-response"))
	})
	firstResponse := performRouteTestRequest(t, firstPlugin.Handler(upstream), "/refresh")
	if got := firstResponse.Header().Get("Apisix-Cache-Status"); got != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", got)
	}

	firstPlugin.Stop()
	secondResponse := performRouteTestRequest(t, secondPlugin.Handler(upstream), "/refresh")
	if got := secondResponse.Header().Get("Apisix-Cache-Status"); got != "HIT" {
		t.Fatalf("cache status after old owner stop = %q, want HIT", got)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func performRouteTestRequest(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

type priorityRouteFixture struct {
	id       string
	uri      string
	priority int
	upstream string
}

func buildPriorityRouter(t *testing.T, fixtures []priorityRouteFixture) http.Handler {
	t.Helper()
	prepared := make([]PreparedRoute, 0, len(fixtures))
	for _, fixture := range fixtures {
		request := httptest.NewRequest(http.MethodGet, "http://"+fixture.upstream, nil)
		port, err := strconv.Atoi(request.URL.Port())
		if err != nil {
			t.Fatalf("parse priority upstream port: %v", err)
		}
		routeResource := resource.Route{
			ID: fixture.id, Uri: fixture.uri, Priority: fixture.priority,
			Upstream: resource.Upstream{Type: "roundrobin", Scheme: "http", Nodes: []resource.Node{{
				Host: request.URL.Hostname(), Port: port, Weight: 1,
			}}},
		}
		prepared = append(prepared, PreparedRoute{
			Route: routeResource,
			Handler: testPreparedProxyHandler(
				t, routeResource, resource.Service{}, testEffectiveConfig(),
			),
		})
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: prepared})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}
	return snapshot.Handler()
}

func routePriorityNode(t *testing.T, rawURL string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, rawURL, nil)
	return net.JoinHostPort(request.URL.Hostname(), request.URL.Port())
}

func assertHigherPriorityRouteWins(t *testing.T, uri string, path string) {
	t.Helper()
	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer low.Close()
	high := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer high.Close()

	lowNode := routePriorityNode(t, low.URL)
	highNode := routePriorityNode(t, high.URL)

	for _, lowFirst := range []bool{true, false} {
		order := "high-first"
		if lowFirst {
			order = "low-first"
		}
		t.Run(order, func(t *testing.T) {
			lowFixture := priorityRouteFixture{uri: uri, priority: 1, upstream: lowNode}
			highFixture := priorityRouteFixture{uri: uri, priority: 10, upstream: highNode}
			var fixtures []priorityRouteFixture
			if lowFirst {
				lowFixture.id = "aaa-prio-low"
				highFixture.id = "zzz-prio-high"
				fixtures = []priorityRouteFixture{lowFixture, highFixture}
			} else {
				highFixture.id = "aaa-prio-high"
				lowFixture.id = "zzz-prio-low"
				fixtures = []priorityRouteFixture{highFixture, lowFixture}
			}
			response := performRouteTestRequest(t, buildPriorityRouter(t, fixtures), path)
			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d from the priority-10 route", response.Code, http.StatusAccepted)
			}
		})
	}
}

func TestRoutePriorityExactWinsRegardlessOfOrder(t *testing.T) {
	assertHigherPriorityRouteWins(t, "/api/v1/items", "/api/v1/items")
}

func TestRoutePriorityParameterWinsRegardlessOfOrder(t *testing.T) {
	assertHigherPriorityRouteWins(t, "/api/v1/items/:id", "/api/v1/items/1")
}

func TestRoutePriorityWildcardWinsRegardlessOfOrder(t *testing.T) {
	assertHigherPriorityRouteWins(t, "/api/v1/*", "/api/v1/items/1")
}

func TestRoutePriorityEqualKeepsLaterRegistration(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer second.Close()

	fixtures := []priorityRouteFixture{
		{id: "aaa-equal-first", uri: "/api/v1/items/:id", priority: 5, upstream: routePriorityNode(t, first.URL)},
		{id: "zzz-equal-second", uri: "/api/v1/items/:id", priority: 5, upstream: routePriorityNode(t, second.URL)},
	}
	response := performRouteTestRequest(t, buildPriorityRouter(t, fixtures), "/api/v1/items/1")
	if response.Code != http.StatusAccepted {
		t.Fatalf(
			"status = %d, want %d from the later-registered equal-priority route",
			response.Code,
			http.StatusAccepted,
		)
	}
}

func TestPreparedErrorLogOwnerStopFlushesBatch(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	received := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	binding := testPluginBindingForSource(
		t,
		"error-log-logger",
		map[string]any{
			"tcp": map[string]any{
				"host": host,
				"port": port,
			},
			"level":            "INFO",
			"batch_max_size":   10,
			"buffer_duration":  60,
			"inactive_timeout": 60,
		},
		pluginpkg.ScopeSystem,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceSystem, ID: "error-log-logger"},
		resource.Route{}, resource.Service{}, "127.0.0.1:9080",
	)
	errorLogger, ok := binding.Plugin.(*error_log_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *error_log_logger.Plugin", binding.Plugin)
	}
	errorLogger.Send(map[string]any{"message": "shutdown error"})
	errorLogger.Stop()

	select {
	case payload := <-received:
		if !strings.Contains(payload, "shutdown error") {
			t.Fatalf("payload = %q, want shutdown error", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for prepared owner to flush error-log-logger")
	}
}

func TestInitPluginsStrictRejectsPluginWhenPostInitFails(t *testing.T) {
	err := testPluginInitializationError(
		"limit-count",
		map[string]any{
			"rules": []any{
				map[string]any{"count": 1, "time_window": 60, "key": "$http_x_user"},
				map[string]any{"count": 2, "time_window": 60, "key": "$http_x_user"},
			},
		},
	)

	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want invalid plugin rejection")
	}
}

func TestInitPluginsStrictRejectsInvalidProxyBufferingConfig(t *testing.T) {
	err := testPluginInitializationError(
		"proxy-buffering",
		map[string]any{
			"disable_proxy_buffering": "yes",
		},
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want invalid config rejection")
	}
}

func TestInitPluginsStrictRejectsInvalidProxyControlConfig(t *testing.T) {
	err := testPluginInitializationError(
		"proxy-control",
		map[string]any{
			"request_buffering": "yes",
		},
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want invalid config rejection")
	}
}

func TestClonePluginConfigsAllocatesForInheritedOnlyRoute(t *testing.T) {
	cloned := clonePluginConfigs(nil)
	if cloned == nil {
		t.Fatal("clonePluginConfigs(nil) returned nil map")
	}
	cloned["key-auth"] = map[string]any{}
	if len(cloned) != 1 {
		t.Fatalf("cloned plugin count = %d, want 1 inherited-only plugin", len(cloned))
	}
	original := map[string]resource.PluginConfig{"route-plugin": map[string]any{}}
	copied := clonePluginConfigs(original)
	copied["inherited-plugin"] = map[string]any{}
	if len(original) != 1 {
		t.Fatalf("original plugin count = %d, want unchanged route plugin map", len(original))
	}
}

func TestInitPluginsStrictAppliesMetaDisable(t *testing.T) {
	plans, err := planPluginSources(
		materializedPluginSources(
			map[string]resource.PluginConfig{
				"request-id": map[string]any{
					"_meta": map[string]any{"disable": true},
				},
			},
			pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "meta-disabled"},
		),
		pluginpkg.NewEnabledSet([]string{"request-id"}),
		false,
	)
	if err != nil {
		t.Fatalf("planPluginSources() error = %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("plans len = %d, want disabled plugin omitted", len(plans))
	}
}

func TestInitPluginsStrictAppliesMetaPriority(t *testing.T) {
	binding := testPlannedPluginBinding(
		t,
		"request-id",
		map[string]any{"_meta": map[string]any{"priority": 3210}},
		resource.Route{ID: "meta-priority"},
	)
	if got := binding.Priority; got != 3210 {
		t.Fatalf("binding priority = %d, want 3210", got)
	}
}

func TestInitPluginsStrictAppliesMetaFilter(t *testing.T) {
	binding := testPlannedPluginBinding(
		t,
		"request-id",
		map[string]any{
			"_meta": map[string]any{
				"filter": []any{[]any{"arg_enable_request_id", "==", "yes"}},
			},
		},
		resource.Route{ID: "meta-filter"},
	)

	handler := binding.Plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	withoutMatch := httptest.NewRecorder()
	withoutMatchRequest := httptest.NewRequest(http.MethodGet, "/meta", nil)
	withoutMatchRequest.URL.RawQuery = "enable_request_id=no"
	handler.ServeHTTP(withoutMatch, withoutMatchRequest)
	if got := withoutMatch.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("filtered request id = %q, want no request id", got)
	}

	withMatch := httptest.NewRecorder()
	withMatchRequest := httptest.NewRequest(http.MethodGet, "/meta?enable_request_id=yes", nil)
	handler.ServeHTTP(withMatch, withMatchRequest)
	if got := withMatch.Header().Get("X-Request-Id"); got == "" {
		t.Fatal("matching request did not receive request id")
	}
}

func TestInitPluginsStrictAppliesNegatedNumericMetaFilterForMissingAge(t *testing.T) {
	binding := testPlannedPluginBinding(
		t,
		"request-id",
		map[string]any{
			"_meta": map[string]any{
				"filter": []any{[]any{"arg_age", "!", ">=", 18}},
			},
		},
		resource.Route{ID: "meta-age-filter"},
	)

	handler := binding.Plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		name        string
		target      string
		wantApplied bool
	}{
		{name: "missing age", target: "/meta", wantApplied: true},
		{name: "malformed age", target: "/meta?age=abc", wantApplied: true},
		{name: "numeric age", target: "/meta?age=21", wantApplied: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.target, nil))
			gotApplied := response.Header().Get("X-Request-Id") != ""
			if gotApplied != test.wantApplied {
				t.Fatalf("request-id applied = %v, want %v", gotApplied, test.wantApplied)
			}
		})
	}
}

func TestInitPluginsStrictRejectsInvalidMetaFilter(t *testing.T) {
	plans, err := planPluginSources(
		materializedPluginSources(
			map[string]resource.PluginConfig{
				"request-id": map[string]any{
					"_meta": map[string]any{"filter": []any{"not-an-expression"}},
				},
			},
			pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "meta-invalid-filter"},
		),
		pluginpkg.NewEnabledSet([]string{"request-id"}),
		false,
	)
	if err == nil {
		t.Fatal("planPluginSources() error = nil, want invalid metadata filter rejection")
	}
	if len(plans) != 0 {
		t.Fatalf("plans len = %d, want no partially planned plugins", len(plans))
	}
}

func TestInitPluginsStrictAppliesMetaErrorResponse(t *testing.T) {
	binding := testPlannedPluginBinding(
		t,
		"jwt-auth",
		map[string]any{
			"_meta": map[string]any{
				"error_response": map[string]any{"message": "custom auth failure"},
			},
		},
		resource.Route{ID: "meta-error-response"},
	)

	handler := binding.Plugin.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called after jwt-auth rejected request")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/meta", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("custom error response removed jwt-auth challenge header")
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"message":"custom auth failure"}` {
		t.Fatalf("body = %q, want custom JSON response", got)
	}
}

func TestPlanHTTPPluginsKeepsKafkaServiceOwnershipAndRouteIndependence(t *testing.T) {
	kafkaConfig := func(topic string) resource.PluginConfig {
		return map[string]any{
			"broker_list":   map[string]any{"127.0.0.1": 9092},
			"kafka_topic":   topic,
			"producer_type": "sync",
		}
	}
	service := resource.Service{
		ID: "shared-kafka-service",
		Plugins: map[string]resource.PluginConfig{
			"kafka-logger": kafkaConfig("integration"),
		},
	}
	input := PlanningInput{
		Routes: []resource.Route{
			{ID: "route-one", Uri: "/one", ServiceID: service.ID},
			{ID: "route-two", Uri: "/two", ServiceID: service.ID},
		},
		Services:       map[string]resource.Service{service.ID: service},
		EnabledPlugins: []string{"kafka-logger"},
	}
	plan, err := PlanHTTPPlugins(context.Background(), input)
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.Routes) != 2 || len(plan.Routes[0].ServicePlans) != 1 || len(plan.Routes[1].ServicePlans) != 1 {
		t.Fatalf("service plans = %#v, want one kafka plan per route", plan.Routes)
	}
	first := plan.Routes[0].ServicePlans[0]
	second := plan.Routes[1].ServicePlans[0]
	wantOwner := pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceService, ID: service.ID}
	if first.Provenance != wantOwner || second.Provenance != wantOwner ||
		first.Source != second.Source || fmt.Sprint(first.Config) != fmt.Sprint(second.Config) {
		t.Fatalf("shared service plans = %#v/%#v, want identical service ownership", first, second)
	}

	service.Plugins["kafka-logger"] = kafkaConfig("integration-v2")
	input.Services[service.ID] = service
	changed, err := PlanHTTPPlugins(context.Background(), input)
	if err != nil {
		t.Fatalf("PlanHTTPPlugins(changed service) error = %v", err)
	}
	if fmt.Sprint(changed.Routes[0].ServicePlans[0].Config) == fmt.Sprint(first.Config) {
		t.Fatal("changed service kafka-logger config retained the old plan identity input")
	}

	input.Services = nil
	input.Routes = []resource.Route{
		{
			ID: "route-one", Uri: "/one",
			Plugins: map[string]resource.PluginConfig{"kafka-logger": kafkaConfig("integration")},
		},
		{
			ID: "route-two", Uri: "/two",
			Plugins: map[string]resource.PluginConfig{"kafka-logger": kafkaConfig("integration")},
		},
	}
	routePlan, err := PlanHTTPPlugins(context.Background(), input)
	if err != nil {
		t.Fatalf("PlanHTTPPlugins(route-local) error = %v", err)
	}
	if routePlan.Routes[0].Local[0].Provenance == routePlan.Routes[1].Local[0].Provenance {
		t.Fatal("route-level kafka-logger plans share ownership, want independent route provenance")
	}
}

func TestServiceNonLoggerInstancesKeepPerRouteResourceContext(t *testing.T) {
	receivedRoutes := make(chan string, 2)
	opaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := apisixjson.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode OPA request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		input, _ := body["input"].(map[string]any)
		route, _ := input["route"].(map[string]any)
		routeID, _ := route["id"].(string)
		receivedRoutes <- routeID
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"allow":true}}`))
	}))
	t.Cleanup(opaServer.Close)

	config := map[string]any{
		"host":       opaServer.URL,
		"policy":     "http/authz",
		"with_route": true,
	}
	service := resource.Service{ID: "shared-opa-service"}
	provenance := pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceService, ID: service.ID}
	first := testPluginBindingForSource(
		t, "opa", config, pluginpkg.ScopeRoute, provenance,
		resource.Route{ID: "route-one"}, service, "127.0.0.1:9080",
	)
	second := testPluginBindingForSource(
		t, "opa", config, pluginpkg.ScopeRoute, provenance,
		resource.Route{ID: "route-two"}, service, "127.0.0.1:9080",
	)
	if first.Plugin == second.Plugin {
		t.Fatal("service-level OPA instances are shared, want per-route resource context")
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	first.Plugin.Handler(next).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "http://gateway.test/one", nil),
	)
	second.Plugin.Handler(next).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "http://gateway.test/two", nil),
	)

	for i, want := range []string{"route-one", "route-two"} {
		if got := <-receivedRoutes; got != want {
			t.Fatalf("OPA request %d route id = %q, want %q", i+1, got, want)
		}
	}
}

func TestInitPluginsStrictRejectsUnknownPlugin(t *testing.T) {
	plans, err := planPluginSources(
		materializedPluginSources(
			map[string]resource.PluginConfig{"not-a-plugin": map[string]any{}},
			pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "unknown-plugin"},
		),
		pluginpkg.NewEnabledSet([]string{"request-id"}),
		false,
	)
	if err == nil {
		t.Fatal("planPluginSources() error = nil, want unknown plugin rejection")
	}
	if len(plans) != 0 {
		t.Fatalf("plans len = %d, want no partially planned plugins", len(plans))
	}
}

func TestInitPluginsStrictRejectsProxyCacheConfigFailure(t *testing.T) {
	zones := []appconfig.Zone{{Name: "strict-disk-only", DiskPath: t.TempDir()}}
	if err := proxy_cache.RefreshConfiguredZones(zones); err != nil {
		t.Fatalf("RefreshConfiguredZones() error = %v", err)
	}
	t.Cleanup(func() { _ = proxy_cache.RefreshConfiguredZones(nil) })

	err := testPluginInitializationError(
		"proxy-cache",
		map[string]any{
			"cache_strategy": "memory",
			"cache_zone":     "strict-disk-only",
		},
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want strict proxy-cache failure")
	}
}

func TestConfiguredProxyCacheZonesRejectInvalidUnusedZone(t *testing.T) {
	zones := []appconfig.Zone{{Name: "unused-invalid-refresh", MemorySize: "zero"}}
	if err := proxy_cache.RefreshConfiguredZones(zones); err == nil {
		t.Fatal("RefreshConfiguredZones() error = nil, want invalid static proxy-cache zone rejection")
	}
}

func TestPlanRoutePluginsReturnsRouteContext(t *testing.T) {
	routeResource := testRouteFromJSON(t,
		`{"id":"strict-invalid","uri":"/strict-invalid","plugins":{"not-a-plugin":{}}}`,
	)
	_, err := planRoutePlugins(
		routeResource,
		PlanningInput{},
		pluginpkg.NewEnabledSet(nil),
	)
	if err == nil || !strings.Contains(err.Error(), "strict-invalid") {
		t.Fatalf("planRoutePlugins() error = %v, want route-scoped error", err)
	}
}

func TestPlannedQuarantinePublishesValidRoutesAndOmitsInvalidRoutes(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes: []resource.Route{
			testRouteFromJSON(t, `{"id":"quarantine-valid","uri":"/quarantine-valid"}`),
			testRouteFromJSON(
				t,
				`{"id":"quarantine-invalid","uri":"/quarantine-invalid","plugins":{"not-a-plugin":{}}}`,
			),
		},
	})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.Quarantined) != 1 || len(plan.Routes) != 1 {
		t.Fatalf("planned routes/quarantine = %d/%v, want 1/1", len(plan.Routes), plan.Quarantined)
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes:   testPreparedRoutes(plan.Routes[0].Route),
	})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}

	valid := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(valid, httptest.NewRequest(http.MethodGet, "/quarantine-valid", nil))
	if valid.Code == http.StatusNotFound {
		t.Fatalf("valid route status = %d, want registered handler", valid.Code)
	}

	invalid := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/quarantine-invalid", nil))
	if invalid.Code != http.StatusNotFound {
		t.Fatalf("invalid route status = %d, want 404", invalid.Code)
	}
}

func TestPlannedQuarantineDoesNotPartiallyPublishMultiURIRoute(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{Routes: []resource.Route{
		testRouteFromJSON(t,
			`{"id":"quarantine-multi-uri","uris":["/quarantine-first","/quarantine/:id/:id"]}`,
		),
	}})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.Quarantined) != 1 || len(plan.Routes) != 0 {
		t.Fatalf("planned routes/quarantine = %d/%v, want 0/1", len(plan.Routes), plan.Quarantined)
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}

	for _, path := range []string{"/quarantine-first", "/quarantine/value/value"} {
		response := httptest.NewRecorder()
		snapshot.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("quarantined route path %q status = %d, want 404", path, response.Code)
		}
	}
}

func TestPlannedQuarantineRejectsUnsupportedMethodWithoutPanicking(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{Routes: []resource.Route{
		testRouteFromJSON(t,
			`{"id":"quarantine-method-invalid","uri":"/quarantine-method/:id","methods":["BOGUS"]}`,
		),
		testRouteFromJSON(t,
			`{"id":"quarantine-method-valid","uri":"/quarantine-method-valid","methods":["GET"]}`,
		),
	}})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.Quarantined) != 1 || len(plan.Routes) != 1 {
		t.Fatalf("planned routes/quarantine = %d/%v, want 1/1", len(plan.Routes), plan.Quarantined)
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes:   testPreparedRoutes(plan.Routes[0].Route),
	})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}

	valid := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(valid, httptest.NewRequest(http.MethodGet, "/quarantine-method-valid", nil))
	if valid.Code == http.StatusNotFound {
		t.Fatalf("valid route status = %d, want registered handler", valid.Code)
	}
}

func TestPlannedQuarantineRejectsAPISIXInvalidMethodFormsAndDuplicates(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{Routes: []resource.Route{
		testRouteFromJSON(
			t,
			`{"id":"quarantine-method-lowercase","uri":"/quarantine-method-lowercase","methods":["get"]}`,
		),
		testRouteFromJSON(t, `{"id":"quarantine-method-query","uri":"/quarantine-method-query","methods":["QUERY"]}`),
		testRouteFromJSON(
			t,
			`{"id":"quarantine-method-duplicate","uri":"/quarantine-method-duplicate","methods":["GET","GET"]}`,
		),
		testRouteFromJSON(
			t,
			`{"id":"quarantine-uri-duplicate","uris":["/quarantine-uri-duplicate/:id","/quarantine-uri-duplicate/:name"]}`,
		),
		testRouteFromJSON(t, `{"id":"quarantine-method-sibling","uri":"/quarantine-method-sibling","methods":["GET"]}`),
	}})
	if err != nil {
		t.Fatalf("PlanHTTPPlugins() error = %v", err)
	}
	if len(plan.Quarantined) != 4 || len(plan.Routes) != 1 {
		t.Fatalf("planned routes/quarantine = %d/%v, want 1/4", len(plan.Routes), plan.Quarantined)
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes:   testPreparedRoutes(plan.Routes[0].Route),
	})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}

	valid := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(valid, httptest.NewRequest(http.MethodGet, "/quarantine-method-sibling", nil))
	if valid.Code == http.StatusNotFound {
		t.Fatalf("valid sibling status = %d, want registered handler", valid.Code)
	}
}

func TestBuildPreparedHandlerAllowsPluginOnlyRouteWithoutUpstreamNodes(t *testing.T) {
	routeResource := resource.Route{ID: "plugin-only"}
	upstream, err := PlanRouteUpstream(
		routeResource, resource.Service{}, nil, nil, &testEffectiveConfig().Config,
	)
	if err != nil {
		t.Fatalf("PlanRouteUpstream() error = %v", err)
	}
	_, err = BuildPreparedHandler(PreparedHandlerInput{
		Route:        routeResource,
		Upstream:     upstream,
		Runtime:      PreparedUpstreamRuntime{RoundTripper: http.DefaultTransport},
		StaticConfig: testEffectiveConfig().Config,
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v, want plugin-only route support", err)
	}
}

func TestUnownedSecretReferenceRejectsRoutePluginBeforePostInit(t *testing.T) {
	err := testPluginInitializationError(
		"basic-auth",
		map[string]any{"realm": "$ENV://ROUTE_REALM"},
	)

	if err == nil ||
		!strings.Contains(err.Error(), "unowned secret reference") ||
		!strings.Contains(err.Error(), "realm") {
		t.Fatalf("plugin initialization error = %v, want unowned route secret rejection", err)
	}
}

func TestUnownedSecretReferenceRejectsRoutePluginBeforePostInitLowercaseEnvironmentPrefix(t *testing.T) {
	err := testPluginInitializationError(
		"basic-auth",
		map[string]any{"realm": "$env://ROUTE_REALM"},
	)

	if err == nil ||
		!strings.Contains(err.Error(), "unowned secret reference") ||
		!strings.Contains(err.Error(), "realm") {
		t.Fatalf("plugin initialization error = %v, want lowercase unowned route secret rejection", err)
	}
}
