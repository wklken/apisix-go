package route

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/error_log_logger"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
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

func TestBuildRoutePluginChainOrdersGlobalAndLocalPluginsByPriority(t *testing.T) {
	order := []string{}
	local := []pluginpkg.Plugin{&recordingPlugin{name: "local-auth", priority: 2500, order: &order}}
	global := []pluginpkg.Plugin{&recordingPlugin{name: "global-label", priority: 2399, order: &order}}
	handler := assembleRoutePluginChain(local, global).Then(http.HandlerFunc(
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

func TestBuildSystemPluginConfigsUsesGlobalBodyLimitUnlessRouteOverridesIt(t *testing.T) {
	previous := appconfig.GlobalConfig
	t.Cleanup(func() { appconfig.GlobalConfig = previous })
	appconfig.GlobalConfig = &appconfig.Config{NginxConfig: appconfig.NginxConfig{
		HTTP: appconfig.NginxHTTP{ClientMaxBodySize: 30},
	}}

	plugins := buildSystemPluginConfigs(resource.Route{ID: "global-limit"}, resource.Service{}, nil)
	clientControl, ok := plugins["client-control"].(map[string]any)
	if !ok {
		t.Fatalf("client-control config = %#v, want generated config", plugins["client-control"])
	}
	if got := clientControl["max_body_size"]; got != int64(30) {
		t.Fatalf("client-control max_body_size = %#v, want 30", got)
	}

	plugins = buildSystemPluginConfigs(
		resource.Route{ID: "route-limit"},
		resource.Service{},
		map[string]resource.PluginConfig{"client-control": map[string]any{"max_body_size": 50}},
	)
	if _, ok := plugins["client-control"]; ok {
		t.Fatalf("system client-control = %#v, want route override only", plugins["client-control"])
	}
}

func TestBuilderStopFlushesLoggerBatches(t *testing.T) {
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

func TestBuilderRejectsSnapshotContainingUndecodableRoute(t *testing.T) {
	ensureRouteStore(t)

	put := func(id string, value []byte) {
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/routes/" + id)
		event.Value = value
		routeStoreEvents <- event
	}
	remove := func(id string) {
		event := store.NewEvent()
		event.Type = store.EventTypeDelete
		event.Key = []byte("/apisix/routes/" + id)
		routeStoreEvents <- event
	}

	put("strict-valid", []byte(`{"id":"strict-valid","uri":"/strict-valid"}`))
	put("strict-invalid", []byte(`{"id":"strict-invalid","uri":"/strict-invalid","plugins":[]}`))
	routeStore.Sync()
	t.Cleanup(func() {
		remove("strict-valid")
		remove("strict-invalid")
		routeStore.Sync()
	})

	builder := NewBuilder(nil)
	defer builder.Stop()
	if handler := builder.Build(); handler != nil {
		t.Fatal("Build() returned a partial handler, want nil for an undecodable route snapshot")
	}
}
