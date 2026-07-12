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
	"github.com/wklken/apisix-go/pkg/plugin/error_log_logger"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/resource"
)

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
		defer conn.Close()
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

func TestInitPluginsSkipsPluginWhenPostInitFails(t *testing.T) {
	builder := NewBuilder(nil)
	plugins := builder.initPlugins(
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

	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want invalid plugin skipped", len(plugins))
	}
}

func TestInitPluginsSkipsInvalidProxyBufferingConfig(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"proxy-buffering": map[string]any{
				"disable_proxy_buffering": "yes",
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "invalid-proxy-buffering"}),
	)
	if err != nil {
		t.Fatalf("initPluginsStrict() error = %v, want ordinary plugin validation to be skipped", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no initialized plugins", len(plugins))
	}
}

func TestInitPluginsSkipsInvalidProxyControlConfig(t *testing.T) {
	builder := NewBuilder(nil)
	plugins, err := builder.initPluginsStrict(
		map[string]resource.PluginConfig{
			"proxy-control": map[string]any{
				"request_buffering": "yes",
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "invalid-proxy-control"}),
	)
	if err != nil {
		t.Fatalf("initPluginsStrict() error = %v, want ordinary plugin validation to be skipped", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("plugins len = %d, want no initialized plugins", len(plugins))
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
