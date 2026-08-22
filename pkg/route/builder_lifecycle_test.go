package route

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/error_log_logger"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	bolt "go.etcd.io/bbolt"
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
	builder := NewBuilder(nil)
	handler, err := builder.buildGlobalNotFoundHandler([]resource.GlobalRule{{
		ID: "global-transform",
		Plugins: map[string]resource.PluginConfig{
			"exit-transformer": map[string]any{
				"functions": []any{
					"return (function(code, body, header) if code == 404 then return 405 end return code, body, header end)(...)",
				},
			},
		},
	}})
	if err != nil {
		t.Fatalf("buildGlobalNotFoundHandler() error = %v", err)
	}

	response := performRouteTestRequest(t, handler, "/missing")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestBuildSystemPluginConfigsDoesNotGenerateGlobalClientControl(t *testing.T) {
	previous := appconfig.GlobalConfig
	t.Cleanup(func() { appconfig.GlobalConfig = previous })
	appconfig.GlobalConfig = &appconfig.Config{NginxConfig: appconfig.NginxConfig{
		HTTP: appconfig.NginxHTTP{ClientMaxBodySize: 30},
	}}

	plugins := buildSystemPluginConfigs(resource.Route{ID: "global-limit"}, resource.Service{})
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
	ensureRouteStore(t)
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
			builder := NewBuilder(nil)
			t.Cleanup(builder.Stop)
			_, err := builder.buildHandlerStrict(resource.Route{
				ID:       "dynamic-discovery-route",
				Uri:      "/dynamic-discovery",
				Upstream: upstream,
			})
			if err == nil {
				t.Fatal("buildHandlerStrict() error = nil, want unsupported discovery error")
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
	putHTTPAllowlistResource(t, "upstreams", upstreamID, []byte(`{
		"nodes": {"127.0.0.1:8080": 1},
		"discovery_type": "dns"
	}`))

	upstream, provenance, err := resolveRouteUpstream(
		resource.Route{ID: "referenced-discovery-route", UpstreamID: upstreamID},
		resource.Service{},
	)
	if err != nil {
		t.Fatalf("resolveRouteUpstream() error = %v", err)
	}
	if upstream.DiscoveryType != "dns" {
		t.Fatalf("resolved discovery_type = %q, want dns", upstream.DiscoveryType)
	}
	want := pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceUpstream, ID: upstreamID}
	if provenance != want {
		t.Fatalf("upstream provenance = %#v, want %#v", provenance, want)
	}
}

func TestBuilderStopFlushesLoggerBatches(t *testing.T) {
	ensureRouteStore(t)

	delivered := make(chan struct{}, 1)
	logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logServer.Close)

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080")
	plugins := builder.initPlugins(
		map[string]resource.PluginConfig{
			"http-logger": map[string]any{
				"uri":              logServer.URL,
				"batch_max_size":   10,
				"buffer_duration":  60,
				"inactive_timeout": 60,
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-a"}),
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	httpLogger, ok := plugins[0].(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", plugins[0])
	}
	if err := httpLogger.Fire(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("Fire() error = %v", err)
	}

	builder.Stop()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Builder.Stop to flush logger batch")
	}
}

func TestBuilderRefreshKeepsConfiguredProxyCacheZoneAlive(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "route-refresh-memory", MemorySize: "1M"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	firstBuilder := NewBuilder(nil)
	firstPlugins := firstBuilder.initPlugins(
		map[string]resource.PluginConfig{
			"proxy-cache": map[string]any{
				"cache_strategy": "memory",
				"cache_zone":     "route-refresh-memory",
				"cache_ttl":      60,
			},
		},
		firstBuilder.pluginRouteContext(resource.Route{ID: "route-refresh"}),
	)
	if len(firstPlugins) != 1 {
		t.Fatalf("first plugins len = %d, want 1", len(firstPlugins))
	}
	firstPlugin, ok := firstPlugins[0].(*proxy_cache.Plugin)
	if !ok {
		t.Fatalf("first plugin type = %T, want *proxy_cache.Plugin", firstPlugins[0])
	}

	secondBuilder := NewBuilder(nil)
	secondPlugins := secondBuilder.initPlugins(
		map[string]resource.PluginConfig{
			"proxy-cache": map[string]any{
				"cache_strategy": "memory",
				"cache_zone":     "route-refresh-memory",
				"cache_ttl":      60,
			},
		},
		secondBuilder.pluginRouteContext(resource.Route{ID: "route-refresh"}),
	)
	if len(secondPlugins) != 1 {
		t.Fatalf("second plugins len = %d, want 1", len(secondPlugins))
	}
	secondPlugin, ok := secondPlugins[0].(*proxy_cache.Plugin)
	if !ok {
		t.Fatalf("second plugin type = %T, want *proxy_cache.Plugin", secondPlugins[0])
	}

	t.Cleanup(firstBuilder.Stop)
	t.Cleanup(secondBuilder.Stop)
	calls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte("route-refresh-response"))
	})
	firstResponse := performRouteTestRequest(t, firstPlugin.Handler(upstream), "/refresh")
	if got := firstResponse.Header().Get("Apisix-Cache-Status"); got != "MISS" {
		t.Fatalf("first cache status = %q, want MISS", got)
	}

	firstBuilder.Stop()
	secondResponse := performRouteTestRequest(t, secondPlugin.Handler(upstream), "/refresh")
	if got := secondResponse.Header().Get("Apisix-Cache-Status"); got != "HIT" {
		t.Fatalf("cache status after old builder stop = %q, want HIT", got)
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
	ensureRouteStore(t)
	for _, fixture := range fixtures {
		value := fmt.Sprintf(
			`{"id":%q,"uri":%q,"priority":%d,"upstream":{"type":"roundrobin","nodes":{%q:1}}}`,
			fixture.id,
			fixture.uri,
			fixture.priority,
			fixture.upstream,
		)
		putRouteResource(t, fixture.id, []byte(value))
	}
	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	return builder.Build()
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

func TestBuilderStopFlushesErrorLogLoggerBatch(t *testing.T) {
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

	builder := NewBuilder(nil)
	plugins := builder.initPlugins(
		map[string]resource.PluginConfig{
			"error-log-logger": map[string]any{
				"tcp": map[string]any{
					"host": host,
					"port": port,
				},
				"level":            "INFO",
				"batch_max_size":   10,
				"buffer_duration":  60,
				"inactive_timeout": 60,
			},
		},
		pluginRouteContext{},
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	errorLogger, ok := plugins[0].(*error_log_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *error_log_logger.Plugin", plugins[0])
	}
	errorLogger.Send(map[string]any{"message": "shutdown error"})
	builder.Stop()

	select {
	case payload := <-received:
		if !strings.Contains(payload, "shutdown error") {
			t.Fatalf("payload = %q, want shutdown error", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Builder.Stop to flush error-log-logger")
	}
}

func TestBuilderStartsOneGlobalErrorLogObserverFromMetadata(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	builder := NewBuilder(nil)
	if err := builder.startGlobalErrorLogObserver(map[string]any{
		"tcp": map[string]any{
			"host": host,
			"port": port,
		},
		"level":            "WARN",
		"batch_max_size":   10,
		"buffer_duration":  60,
		"inactive_timeout": 60,
	}); err != nil {
		t.Fatalf("start global error-log observer: %v", err)
	}

	logger.Warn("global builder error-log marker")
	builder.Stop()

	received := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		body := make([]byte, 1024)
		n, _ := conn.Read(body)
		received <- string(body[:n])
	}()
	select {
	case payload := <-received:
		if !strings.Contains(payload, "[warn] global builder error-log marker") {
			t.Fatalf("payload = %q, want global warning", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for global error-log observer")
	}
}

func TestInitPluginsStrictRejectsPluginWhenPostInitFails(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"limit-count": map[string]any{
				"rules": []any{
					map[string]any{"count": 1, "time_window": 60, "key": "$http_x_user"},
					map[string]any{"count": 2, "time_window": 60, "key": "$http_x_user"},
				},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-a"}),
	)

	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want invalid plugin rejection")
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
	}
}

func TestInitPluginsStrictRejectsInvalidProxyBufferingConfig(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"proxy-buffering": map[string]any{
				"disable_proxy_buffering": "yes",
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "invalid-proxy-buffering"}),
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want invalid config rejection")
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
	}
}

func TestInitPluginsStrictRejectsInvalidProxyControlConfig(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"proxy-control": map[string]any{
				"request_buffering": "yes",
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "invalid-proxy-control"}),
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want invalid config rejection")
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
	}
}

func TestInitPluginsStrictRejectsInvalidPluginMetadata(t *testing.T) {
	ensureRouteStore(t)

	metadata := map[string]any{"allow_origins": map[string]any{"key": "*a"}}
	body, err := apisixjson.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	routeStoreEvents <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/plugin_metadata/cors"),
		Value: body,
	}

	deadline := time.Now().Add(time.Second)
	storedMetadata := false
	for time.Now().Before(deadline) {
		var stored map[string]any
		if err := store.GetPluginMetadata("cors", &stored); err == nil {
			origins, ok := stored["allow_origins"].(map[string]any)
			if ok && origins["key"] == "*a" {
				storedMetadata = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !storedMetadata {
		t.Fatal("timed out waiting for CORS metadata")
	}

	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"cors": map[string]any{"allow_origins_by_metadata": []any{"key"}},
		},
		builder.pluginRouteContext(resource.Route{ID: "invalid-cors-metadata"}),
	)
	if err == nil || !strings.Contains(err.Error(), "validate plugin cors metadata") {
		t.Fatalf("initPluginsStrict() error = %v, want invalid CORS metadata rejection", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
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
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"request-id": map[string]any{
				"_meta": map[string]any{"disable": true},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "meta-disabled"}),
	)
	if err != nil {
		t.Fatalf("initPluginsStrict() error = %v", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want disabled plugin omitted", len(plugins))
	}
}

func TestInitPluginsStrictAppliesMetaPriority(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"request-id": map[string]any{
				"_meta": map[string]any{"priority": 3210},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "meta-priority"}),
	)
	if err != nil {
		t.Fatalf("initPluginsStrict() error = %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}
	if got := plugins[0].GetPriority(); got != 3210 {
		t.Fatalf("plugin priority = %d, want 3210", got)
	}
}

func TestInitPluginsStrictAppliesMetaFilter(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"request-id": map[string]any{
				"_meta": map[string]any{
					"filter": []any{[]any{"arg_enable_request_id", "==", "yes"}},
				},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "meta-filter"}),
	)
	if err != nil {
		t.Fatalf("initPluginsStrict() error = %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	handler := plugins[0].Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"request-id": map[string]any{
				"_meta": map[string]any{
					"filter": []any{[]any{"arg_age", "!", ">=", 18}},
				},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "meta-age-filter"}),
	)
	if err != nil {
		t.Fatalf("initPluginsStrict() error = %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	handler := plugins[0].Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"request-id": map[string]any{
				"_meta": map[string]any{"filter": []any{"not-an-expression"}},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "meta-invalid-filter"}),
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want invalid metadata filter rejection")
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
	}
}

func TestInitPluginsStrictAppliesMetaErrorResponse(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"jwt-auth": map[string]any{
				"_meta": map[string]any{
					"error_response": map[string]any{"message": "custom auth failure"},
				},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "meta-error-response"}),
	)
	if err != nil {
		t.Fatalf("initPluginsStrict() error = %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	handler := plugins[0].Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
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

func TestServiceKafkaLoggerInstanceIsSharedWhileRouteInstancesRemainIndependent(t *testing.T) {
	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	config := map[string]resource.PluginConfig{
		"kafka-logger": map[string]any{
			"broker_list":   map[string]any{"127.0.0.1": 9092},
			"kafka_topic":   "integration",
			"producer_type": "sync",
		},
	}
	service := resource.Service{ID: "shared-kafka-service", Plugins: config}

	first, err := builder.initServicePluginsStrict(config, pluginRouteContext{
		routeID: "route-one",
		route:   resource.Route{ID: "route-one"},
		service: service,
	})
	if err != nil {
		t.Fatalf("initialize first service plugins: %v", err)
	}
	second, err := builder.initServicePluginsStrict(config, pluginRouteContext{
		routeID: "route-two",
		route:   resource.Route{ID: "route-two"},
		service: service,
	})
	if err != nil {
		t.Fatalf("initialize second service plugins: %v", err)
	}
	if first[0] != second[0] {
		t.Fatal("service-level kafka-logger instances differ, want one shared processor")
	}
	if got := len(builder.stoppers); got != 1 {
		t.Fatalf("stoppers after shared service config = %d, want 1", got)
	}

	changedConfig := map[string]resource.PluginConfig{
		"kafka-logger": map[string]any{
			"broker_list":   map[string]any{"127.0.0.1": 9092},
			"kafka_topic":   "integration-v2",
			"producer_type": "sync",
		},
	}
	changed, err := builder.initServicePluginsStrict(changedConfig, pluginRouteContext{
		routeID: "route-three",
		route:   resource.Route{ID: "route-three"},
		service: resource.Service{ID: service.ID, Plugins: changedConfig},
	})
	if err != nil {
		t.Fatalf("initialize changed service plugins: %v", err)
	}
	if first[0] == changed[0] {
		t.Fatal("changed service kafka-logger config reused the old sender")
	}
	if got := len(builder.stoppers); got != 2 {
		t.Fatalf("stoppers after service config change = %d, want old and new instances", got)
	}

	routeContext := builder.pluginRouteContext(resource.Route{ID: "route-local"})
	routeFirst, err := builder.initPluginsStrict(config, routeContext)
	if err != nil {
		t.Fatalf("initialize first route plugins: %v", err)
	}
	routeSecond, err := builder.initPluginsStrict(config, routeContext)
	if err != nil {
		t.Fatalf("initialize second route plugins: %v", err)
	}
	if routeFirst[0] == routeSecond[0] {
		t.Fatal("route-level kafka-logger instances are shared, want independent route ownership")
	}
	builder.Stop()
	builder.Stop()
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

	builder := NewBuilder(nil)
	config := map[string]resource.PluginConfig{
		"opa": map[string]any{
			"host":       opaServer.URL,
			"policy":     "http/authz",
			"with_route": true,
		},
	}
	service := resource.Service{ID: "shared-opa-service", Plugins: config}

	first, err := builder.initServicePluginsStrict(config, pluginRouteContext{
		routeID: "route-one",
		route:   resource.Route{ID: "route-one"},
		service: service,
	})
	if err != nil {
		t.Fatalf("initialize first service plugins: %v", err)
	}
	second, err := builder.initServicePluginsStrict(config, pluginRouteContext{
		routeID: "route-two",
		route:   resource.Route{ID: "route-two"},
		service: service,
	})
	if err != nil {
		t.Fatalf("initialize second service plugins: %v", err)
	}
	if first[0] == second[0] {
		t.Fatal("service-level OPA instances are shared, want per-route resource context")
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	first[0].Handler(next).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "http://gateway.test/one", nil),
	)
	second[0].Handler(next).ServeHTTP(
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
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{"not-a-plugin": map[string]any{}},
		builder.pluginRouteContext(resource.Route{ID: "unknown-plugin"}),
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want unknown plugin rejection")
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
	}
}

func TestInitPluginsStrictRejectsProxyCacheConfigFailure(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "strict-disk-only", DiskPath: t.TempDir()}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"proxy-cache": map[string]any{
				"cache_strategy": "memory",
				"cache_zone":     "strict-disk-only",
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "strict-cache-route"}),
	)
	if err == nil {
		t.Fatal("initPluginsStrict() error = nil, want strict proxy-cache failure")
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized strict plugin", len(plugins))
	}
	handler, buildErr := builder.buildHandlerStrict(resource.Route{
		ID: "strict-cache-route",
		Plugins: map[string]resource.PluginConfig{
			"proxy-cache": map[string]any{
				"cache_strategy": "memory",
				"cache_zone":     "strict-disk-only",
			},
		},
	})
	if buildErr == nil || handler != nil {
		t.Fatalf("buildHandlerStrict() = (%v, %v), want nil handler and strict error", handler, buildErr)
	}
	builder.Stop()
}

func TestBuilderRejectsInvalidUnusedProxyCacheZoneBeforeRefresh(t *testing.T) {
	oldConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{ProxyCache: appconfig.ProxyCache{
		Zones: []appconfig.Zone{{Name: "unused-invalid-refresh", MemorySize: "zero"}},
	}}}
	t.Cleanup(func() { appconfig.GlobalConfig = oldConfig })

	builder := NewBuilder(nil)
	if handler := builder.Build(); handler != nil {
		t.Fatal("Build() returned a handler, want nil for invalid static proxy-cache zone registry")
	}
	builder.Stop()
}

func TestBuilderPublishesValidRouteWhenLegacySnapshotRowIsUndecodable(t *testing.T) {
	storage := openLegacyRouteStore(t, map[string]map[string][]byte{
		"routes": {
			"strict-valid":   []byte(`{"id":"strict-valid","uri":"/strict-valid"}`),
			"strict-invalid": []byte(`{"id":"strict-invalid","uri":"/strict-invalid","plugins":[]}`),
		},
	})
	builder := NewBuilder(storage)
	defer builder.Stop()
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want valid handler with legacy row quarantine", handler, err)
	}
}

func TestBuildStrictReturnsRouteContext(t *testing.T) {
	ensureRouteStore(t)
	putRouteResource(t, "strict-invalid", []byte(
		`{"id":"strict-invalid","uri":"/strict-invalid","plugins":{"not-a-plugin":{}}}`,
	))
	builder := NewBuilder(nil)
	defer builder.Stop()
	handler, err := builder.BuildStrict()
	if err == nil || !strings.Contains(err.Error(), "strict-invalid") {
		t.Fatalf("BuildStrict() handler/error = %T/%v, want route-scoped error", handler, err)
	}
	if handler != nil {
		t.Fatalf("BuildStrict() handler = %T, want nil", handler)
	}
}

func TestBuildWithRouteQuarantinePublishesValidRoutesAndOmitsInvalidRoutes(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "quarantine-valid", []byte(
		`{"id":"quarantine-valid","uri":"/quarantine-valid"}`,
	))
	putRouteResource(t, "quarantine-invalid", []byte(
		`{"id":"quarantine-invalid","uri":"/quarantine-invalid","plugins":{"not-a-plugin":{}}}`,
	))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil || handler == nil {
		t.Fatalf("BuildWithRouteQuarantine() = (%T, %v), want valid generation", handler, err)
	}

	valid := httptest.NewRecorder()
	handler.ServeHTTP(valid, httptest.NewRequest(http.MethodGet, "/quarantine-valid", nil))
	if valid.Code == http.StatusNotFound {
		t.Fatalf("valid route status = %d, want registered handler", valid.Code)
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/quarantine-invalid", nil))
	if invalid.Code != http.StatusNotFound {
		t.Fatalf("invalid route status = %d, want 404", invalid.Code)
	}
	if got, want := builder.QuarantinedResourceCount(), 1; got != want {
		t.Fatalf("QuarantinedResourceCount() = %d, want %d", got, want)
	}
}

func TestBuildWithRouteQuarantineDoesNotPartiallyPublishMultiURIRoute(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "quarantine-multi-uri", []byte(
		`{"id":"quarantine-multi-uri","uris":["/quarantine-first","/quarantine/:id/:id"]}`,
	))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil || handler == nil {
		t.Fatalf("BuildWithRouteQuarantine() = (%T, %v), want valid generation", handler, err)
	}

	for _, path := range []string{"/quarantine-first", "/quarantine/value/value"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("quarantined route path %q status = %d, want 404", path, response.Code)
		}
	}
	if got, want := builder.QuarantinedResourceCount(), 1; got != want {
		t.Fatalf("QuarantinedResourceCount() = %d, want %d", got, want)
	}
}

func TestBuildWithRouteQuarantineRejectsUnsupportedMethodWithoutPanicking(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "quarantine-method-invalid", []byte(
		`{"id":"quarantine-method-invalid","uri":"/quarantine-method/:id","methods":["BOGUS"]}`,
	))
	putRouteResource(t, "quarantine-method-valid", []byte(
		`{"id":"quarantine-method-valid","uri":"/quarantine-method-valid","methods":["GET"]}`,
	))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil || handler == nil {
		t.Fatalf("BuildWithRouteQuarantine() = (%T, %v), want valid generation", handler, err)
	}

	valid := httptest.NewRecorder()
	handler.ServeHTTP(valid, httptest.NewRequest(http.MethodGet, "/quarantine-method-valid", nil))
	if valid.Code == http.StatusNotFound {
		t.Fatalf("valid route status = %d, want registered handler", valid.Code)
	}
	if got, want := builder.QuarantinedResourceCount(), 1; got != want {
		t.Fatalf("QuarantinedResourceCount() = %d, want %d", got, want)
	}
}

func TestBuildWithRouteQuarantineRejectsAPISIXInvalidMethodFormsAndDuplicates(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "quarantine-method-lowercase", []byte(
		`{"id":"quarantine-method-lowercase","uri":"/quarantine-method-lowercase","methods":["get"]}`,
	))
	putRouteResource(t, "quarantine-method-query", []byte(
		`{"id":"quarantine-method-query","uri":"/quarantine-method-query","methods":["QUERY"]}`,
	))
	putRouteResource(t, "quarantine-method-duplicate", []byte(
		`{"id":"quarantine-method-duplicate","uri":"/quarantine-method-duplicate","methods":["GET","GET"]}`,
	))
	putRouteResource(t, "quarantine-uri-duplicate", []byte(
		`{"id":"quarantine-uri-duplicate","uris":["/quarantine-uri-duplicate/:id","/quarantine-uri-duplicate/:name"]}`,
	))
	putRouteResource(t, "quarantine-method-sibling", []byte(
		`{"id":"quarantine-method-sibling","uri":"/quarantine-method-sibling","methods":["GET"]}`,
	))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil || handler == nil {
		t.Fatalf("BuildWithRouteQuarantine() = (%T, %v), want valid generation", handler, err)
	}

	valid := httptest.NewRecorder()
	handler.ServeHTTP(valid, httptest.NewRequest(http.MethodGet, "/quarantine-method-sibling", nil))
	if valid.Code == http.StatusNotFound {
		t.Fatalf("valid sibling status = %d, want registered handler", valid.Code)
	}
	if got, want := builder.QuarantinedResourceCount(), 4; got != want {
		t.Fatalf("QuarantinedResourceCount() = %d, want %d", got, want)
	}
}

func TestBuildWithRouteQuarantineRollsBackRouteLifecycleResources(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t, "kafka-logger")
	putHTTPAllowlistResource(t, "services", "quarantine-lifecycle-service", []byte(
		`{"id":"quarantine-lifecycle-service","plugins":{"kafka-logger":{"broker_list":{"127.0.0.1":9092},"kafka_topic":"quarantine","producer_type":"sync"}}}`,
	))
	putRouteResource(t, "quarantine-lifecycle", []byte(
		`{"id":"quarantine-lifecycle","uri":"/quarantine-lifecycle","service_id":"quarantine-lifecycle-service","upstream":{"nodes":{"127.0.0.1:1":0}}}`,
	))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil || handler == nil {
		t.Fatalf("BuildWithRouteQuarantine() = (%T, %v), want valid generation", handler, err)
	}
	builder.stopperMu.Lock()
	stopperCount := len(builder.stoppers)
	builder.stopperMu.Unlock()
	if stopperCount != 0 {
		t.Fatalf("quarantined route retained %d lifecycle stopper(s), want zero", stopperCount)
	}
	if got := len(builder.servicePlugins); got != 0 {
		t.Fatalf("quarantined route retained %d service plugin(s), want zero", got)
	}
}

func TestBuilderPublishesValidRouteWhenLegacyGlobalRuleRowIsUndecodable(t *testing.T) {
	storage := openLegacyRouteStore(t, map[string]map[string][]byte{
		"routes": {
			"strict-global-route": []byte(`{"id":"strict-global-route","uri":"/strict-global"}`),
		},
		"global_rules": {
			"strict-valid-global":   []byte(`{"id":"strict-valid-global","plugins":{}}`),
			"strict-invalid-global": []byte(`{"id":"strict-invalid-global","plugins":[]}`),
		},
	})

	rules, err := store.ListGlobalRules()
	if err == nil {
		t.Fatal("ListGlobalRules() error = nil, want malformed global-rule error")
	}
	if rules != nil {
		t.Fatalf("ListGlobalRules() rules = %#v, want no partial snapshot", rules)
	}
	if !strings.Contains(err.Error(), "strict-invalid-global") {
		t.Fatalf("ListGlobalRules() error = %q, want global-rule ID", err)
	}

	builder := NewBuilder(storage)
	defer builder.Stop()
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want valid handler with legacy global-rule quarantine", handler, err)
	}
}

func TestBuilderBuildStrictUsesPassedStoreSnapshot(t *testing.T) {
	globalStore, _ := openBuildSnapshotStore(t, map[string]map[string][]byte{
		"routes": {
			"global-only": []byte(`{"id":"global-only","uri":"/global-only","service_id":"missing-service"}`),
		},
	})
	passedStore, _ := openBuildSnapshotStore(t, map[string]map[string][]byte{
		"routes": {
			"passed-store": []byte(
				`{"id":"passed-store","uri":"/passed-store","service_id":"passed-service","plugin_config_id":"passed-config"}`,
			),
		},
		"services": {
			"passed-service": []byte(`{"id":"passed-service","upstream_id":"passed-upstream"}`),
		},
		"upstreams": {
			"passed-upstream": []byte(`{"scheme":"http","nodes":{"127.0.0.1:8080":1}}`),
		},
		"plugin_configs": {
			"passed-config": []byte(`{"plugins":{}}`),
		},
	})
	previous := store.ReplaceGlobalStoreForTest(globalStore)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previous) })

	builder := NewBuilder(passedStore)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want handler from passed Store", handler, err)
	}
}

func TestBuilderTrafficSplitUsesPassedStoreSnapshot(t *testing.T) {
	previousConfig := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Plugins: []string{"traffic-split"}}
	t.Cleanup(func() { appconfig.GlobalConfig = previousConfig })
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	if err != nil {
		t.Fatalf("split target address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("split target port: %v", err)
	}

	globalStore, _ := openBuildSnapshotStore(t, nil)
	passedStore, _ := openBuildSnapshotStore(t, map[string]map[string][]byte{
		"routes": {
			"passed-split": []byte(`{
				"id":"passed-split",
				"uri":"/passed-split",
				"plugins":{"traffic-split":{"rules":[{"weighted_upstreams":[{"upstream_id":"split","weight":1}]}]}},
				"upstream":{"scheme":"http","nodes":{"127.0.0.1:8080":1}}
			}`),
		},
		"upstreams": {
			"split": fmt.Appendf(nil,
				`{"scheme":"http","nodes":[{"host":%q,"port":%d,"weight":1}]}`,
				host,
				port,
			),
		},
	})
	previousStore := store.ReplaceGlobalStoreForTest(globalStore)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	if _, err := store.GetUpstream("split"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("global Store split upstream error = %v, want ErrNotFound", err)
	}

	builder := NewBuilder(passedStore)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want traffic-split upstream_id from passed Store", handler, err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/passed-split", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf(
			"traffic-split response status = %d, want %d; body=%q",
			response.Code,
			http.StatusNoContent,
			response.Body,
		)
	}
}

func openLegacyRouteStore(t *testing.T, seed map[string]map[string][]byte) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	initial, err := store.Open(path, make(chan *store.Event, 1))
	if err != nil {
		t.Fatalf("open initial legacy store: %v", err)
	}
	if err := initial.Stop(); err != nil {
		t.Fatalf("stop initial legacy store: %v", err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for bucketName, entries := range seed {
			bucket := tx.Bucket([]byte(bucketName))
			for id, value := range entries {
				if err := bucket.Put([]byte(id), value); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		t.Fatalf("seed legacy database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
	events := make(chan *store.Event, 1)
	storage, err := store.Open(path, events)
	if err != nil {
		t.Fatalf("reopen legacy store: %v", err)
	}
	storage.Start()
	previous := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previous)
		if err := storage.Stop(); err != nil {
			t.Errorf("stop legacy store: %v", err)
		}
	})
	return storage
}

func openBuildSnapshotStore(t *testing.T, seed map[string]map[string][]byte) (*store.Store, chan *store.Event) {
	t.Helper()
	events := make(chan *store.Event, 16)
	storage, err := store.Open(filepath.Join(t.TempDir(), "builder-snapshot.db"), events)
	if err != nil {
		t.Fatalf("open builder snapshot store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() {
		if err := storage.Stop(); err != nil {
			t.Errorf("stop builder snapshot store: %v", err)
		}
	})
	for bucket, entries := range seed {
		for id, value := range entries {
			event := store.NewEvent()
			event.Type = store.EventTypePut
			event.Key = []byte("/apisix/" + bucket + "/" + id)
			event.Value = append([]byte(nil), value...)
			events <- event
			if err := storage.Sync(); err != nil {
				t.Fatalf("seed %s/%s: %v", bucket, id, err)
			}
		}
	}
	return storage, events
}

func TestBuildReverseHandlerAllowsPluginOnlyRouteWithoutUpstreamNodes(t *testing.T) {
	_, err := (&Builder{}).buildReverseHandler(resource.Route{}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v, want plugin-only route support", err)
	}
}

func TestBuilderClusterRegistrySharesIdenticalUpstreamUntilFinalStop(t *testing.T) {
	ensureRouteStore(t)
	putRouteResource(
		t,
		"cluster-shared",
		[]byte(
			`{"id":"cluster-shared","uri":"/cluster-shared","upstream":{"scheme":"http","nodes":[{"host":"127.0.0.1","port":18081,"weight":1}]}}`,
		),
	)

	registry := pxy.NewClusterRegistry(pxy.NopClusterObserver{})
	t.Cleanup(registry.Close)

	firstBuilder := NewBuilderWithClusterRegistry(routeStore, "127.0.0.1:9080", registry)
	defer firstBuilder.Stop()
	firstHandler, err := firstBuilder.BuildStrict()
	if err != nil {
		t.Fatalf("first BuildStrict() error = %v", err)
	}
	if firstHandler == nil {
		t.Fatal("first BuildStrict() returned nil handler")
	}
	if got := registry.Len(); got != 1 {
		t.Fatalf("registry.Len() after first build = %d, want 1", got)
	}

	secondBuilder := NewBuilderWithClusterRegistry(routeStore, "127.0.0.1:9080", registry)
	defer secondBuilder.Stop()
	secondHandler, err := secondBuilder.BuildStrict()
	if err != nil {
		t.Fatalf("second BuildStrict() error = %v", err)
	}
	if secondHandler == nil {
		t.Fatal("second BuildStrict() returned nil handler")
	}
	if got := registry.Len(); got != 1 {
		t.Fatalf("registry.Len() after second build = %d, want shared cluster 1", got)
	}

	firstBuilder.Stop()
	if got := registry.Len(); got != 1 {
		t.Fatalf("registry.Len() after first builder stop = %d, want shared cluster retained", got)
	}

	secondBuilder.Stop()
	if got := registry.Len(); got != 0 {
		t.Fatalf("registry.Len() after second builder stop = %d, want 0", got)
	}
}

func TestBuilderClusterRegistrySeparatesChangedUpstreamTimeout(t *testing.T) {
	ensureRouteStore(t)
	putRouteResource(
		t,
		"cluster-timeout-a",
		[]byte(
			`{"id":"cluster-timeout-a","uri":"/cluster-timeout-a","upstream":{"scheme":"http","timeout":{"read":1},"nodes":[{"host":"127.0.0.1","port":18082,"weight":1}]}}`,
		),
	)
	putRouteResource(
		t,
		"cluster-timeout-b",
		[]byte(
			`{"id":"cluster-timeout-b","uri":"/cluster-timeout-b","upstream":{"scheme":"http","timeout":{"read":2},"nodes":[{"host":"127.0.0.1","port":18082,"weight":1}]}}`,
		),
	)

	registry := pxy.NewClusterRegistry(pxy.NopClusterObserver{})
	t.Cleanup(registry.Close)

	builder := NewBuilderWithClusterRegistry(routeStore, "127.0.0.1:9080", registry)
	defer builder.Stop()
	handler, err := builder.BuildStrict()
	if err != nil {
		t.Fatalf("BuildStrict() error = %v", err)
	}
	if handler == nil {
		t.Fatal("BuildStrict() returned nil handler")
	}
	if got := registry.Len(); got != 2 {
		t.Fatalf("registry.Len() = %d, want 2 distinct clusters", got)
	}
}

func TestUnownedSecretReferenceRejectsRoutePluginBeforePostInit(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"basic-auth": map[string]any{"realm": "$ENV://ROUTE_REALM"},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-unowned-secret"}),
	)

	if err == nil ||
		!strings.Contains(err.Error(), "unowned secret reference") ||
		!strings.Contains(err.Error(), "realm") {
		t.Fatalf("initPluginsStrict() error = %v, want unowned route secret rejection", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
	}
}

func TestUnownedSecretReferenceRejectsRoutePluginBeforePostInitLowercaseEnvironmentPrefix(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"basic-auth": map[string]any{"realm": "$env://ROUTE_REALM"},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-unowned-lowercase-secret"}),
	)

	if err == nil ||
		!strings.Contains(err.Error(), "unowned secret reference") ||
		!strings.Contains(err.Error(), "realm") {
		t.Fatalf("initPluginsStrict() error = %v, want lowercase unowned route secret rejection", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no partially initialized plugins", len(plugins))
	}
}

func TestBuilderRejectsDisabledWorkflowChildBeforeSecretMaterialization(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t, "workflow")
	t.Setenv("ROUTE_DISABLED_WORKFLOW_SECRET", "")
	putRouteResource(
		t,
		"disabled-workflow-secret",
		[]byte(
			`{"id":"disabled-workflow-secret","uri":"/disabled-workflow-secret","plugins":{"workflow":{"rules":[{"actions":[["limit-count",{"count":1,"time_window":60,"key":"$ENV://ROUTE_DISABLED_WORKFLOW_SECRET"}]]}]}}}`,
		),
	)

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("BuildStrict() = (%T, %v), want disabled nested workflow rejection", handler, err)
	}
	if !strings.Contains(err.Error(), `workflow action plugin "limit-count" is disabled`) ||
		strings.Contains(err.Error(), "credential unavailable") {
		t.Fatalf("BuildStrict() error = %q, want disabled error before secret access", err)
	}
	builder.stopperMu.Lock()
	stopperCount := len(builder.stoppers)
	builder.stopperMu.Unlock()
	if stopperCount != 0 {
		t.Fatalf("builder stoppers = %d, want no retained workflow runtime state", stopperCount)
	}
}

func TestBuilderRejectsInvalidWorkflowChildBeforeSecretMaterialization(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t, "workflow", "limit-count")
	t.Setenv("ROUTE_INVALID_WORKFLOW_SECRET", "")
	putRouteResource(
		t,
		"invalid-workflow-secret",
		[]byte(
			`{"id":"invalid-workflow-secret","uri":"/invalid-workflow-secret","plugins":{"workflow":{"rules":[{"actions":[["limit-count",{"count":1,"key":"$ENV://ROUTE_INVALID_WORKFLOW_SECRET"}]]}]}}}`,
		),
	)

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("BuildStrict() = (%T, %v), want invalid nested workflow rejection", handler, err)
	}
	if !strings.Contains(err.Error(), "time_window") || strings.Contains(err.Error(), "credential unavailable") {
		t.Fatalf("BuildStrict() error = %q, want schema error before secret access", err)
	}
	builder.stopperMu.Lock()
	stopperCount := len(builder.stoppers)
	builder.stopperMu.Unlock()
	if stopperCount != 0 {
		t.Fatalf("builder stoppers = %d, want no retained workflow runtime state", stopperCount)
	}
}
