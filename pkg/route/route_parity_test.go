package route

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestPlanRoutePluginsUsesDynamicHTTPAllowlistAndFallsBackWhenAbsent(t *testing.T) {
	routeResource := resource.Route{
		ID: "dynamic-http-plugin-route", Uri: "/dynamic-http-plugin",
		Plugins: map[string]resource.PluginConfig{"request-id": map[string]any{}},
	}
	input := PlanningInput{EnabledPlugins: []string{"request-id"}}
	_, err := planRoutePlugins(routeResource, input, plugin.NewEnabledSet([]string{"gzip"}))
	if err == nil ||
		!strings.Contains(err.Error(), `plugin "request-id"`) ||
		!strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("dynamic allowlist plan error = %v, want request-id rejection", err)
	}
	planned, err := planRoutePlugins(
		routeResource,
		input,
		plugin.NewEnabledSet(input.EnabledPlugins),
	)
	if err != nil || len(planned.Local) != 1 {
		t.Fatalf(
			"startup allowlist fallback plan = (%d local, %v), want success",
			len(planned.Local),
			err,
		)
	}
}

func TestBuildMapsUpstreamNodePriorities(t *testing.T) {
	priorities := map[string]int{
		"http://127.0.0.1:18080": 10,
		"http://127.0.0.1:18081": 1,
	}
	config, err := buildClusterConfigWithTransport(
		resource.Route{},
		resource.Upstream{Scheme: "http"},
		map[string]int{
			"http://127.0.0.1:18080": 1,
			"http://127.0.0.1:18081": 1,
		},
		proxy.TransportOption{},
		&testEffectiveConfig().Config,
		priorities,
	)
	if err != nil {
		t.Fatalf("buildClusterConfigWithTransport() error = %v", err)
	}
	if !mapsEqual(config.Priorities, priorities) {
		t.Fatalf("cluster priorities = %#v, want %#v", config.Priorities, priorities)
	}
}

func TestPreparedHandlerSelectsHigherPriorityNode(t *testing.T) {
	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(low.Close)
	high := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(high.Close)

	lowNode := websocketNode(t, low)
	highNode := websocketNode(t, high)
	handler := testPreparedProxyHandler(t, resource.Route{
		ID: "priority-route",
		Upstream: resource.Upstream{
			Scheme: "http",
			Nodes: []resource.Node{
				{Host: lowNode.Host, Port: lowNode.Port, Weight: 1, Priority: 1},
				{Host: highNode.Host, Port: highNode.Port, Weight: 1, Priority: 10},
			},
		},
	}, resource.Service{}, testEffectiveConfig())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("priority-selected status = %d, want %d", response.Code, http.StatusAccepted)
	}
}

func TestDuplicateGlobalRulePluginsAreRemovedAcrossRules(t *testing.T) {
	rules := deduplicateGlobalRules([]resource.GlobalRule{
		{ID: "first", Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{"header_name": "X-First"},
			"gzip":       map[string]any{},
		}},
		{ID: "second", Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{"header_name": "X-Second"},
			"cors":       map[string]any{},
		}},
	})
	if len(rules) != 2 {
		t.Fatalf("deduplicated rules = %d, want 2", len(rules))
	}
	if _, ok := rules[0].Plugins["request-id"]; ok {
		t.Fatal("duplicate request-id remained in first global rule")
	}
	if _, ok := rules[1].Plugins["request-id"]; ok {
		t.Fatal("duplicate request-id remained in second global rule")
	}
	if _, ok := rules[0].Plugins["gzip"]; !ok {
		t.Fatal("unique gzip was removed from first global rule")
	}
	if _, ok := rules[1].Plugins["cors"]; !ok {
		t.Fatal("unique cors was removed from second global rule")
	}
}

func TestUpstreamStatusResponseHeaderFollowsConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		show   bool
		status int
		want   string
	}{
		{name: "configured all statuses", show: true, status: http.StatusOK, want: "200"},
		{name: "default hides successful status", show: false, status: http.StatusOK},
		{name: "default exposes final 5xx", show: false, status: http.StatusBadGateway, want: "502"},
	} {
		t.Run(test.name, func(t *testing.T) {
			staticConfig := &appconfig.Config{Apisix: appconfig.Apisix{
				ShowUpstreamStatusInResponseHeader: test.show,
			}}
			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
			request = apisixctx.WithRequestVars(request)
			response := &http.Response{
				StatusCode: test.status,
				Header:     make(http.Header),
				Request:    request,
			}
			response.Header.Set("X-APISIX-Upstream-Status", "spoofed-by-upstream")
			if err := newModifyResponse(staticConfig)(response); err != nil {
				t.Fatalf("newModifyResponse() error = %v", err)
			}
			if got := response.Header.Get("X-APISIX-Upstream-Status"); got != test.want {
				t.Fatalf("upstream status header = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUpstreamTransportFailuresExposeRetryStatusChain(t *testing.T) {
	transport := proxy.NewRetryTransport(
		routeRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}),
	)
	request := httptest.NewRequest(http.MethodGet, "http://upstream.test/", nil)
	request = apisixctx.WithRequestVars(request)
	request = proxy.WithRetries(request, 2, func(*http.Request) bool { return true })
	_, err := transport.RoundTrip(request)
	if err == nil {
		t.Fatal("RoundTrip() error = nil, want transport failure")
	}

	response := httptest.NewRecorder()
	newErrorHandler(&testEffectiveConfig().Config)(response, request, err)
	if got := response.Header().Get("X-APISIX-Upstream-Status"); got != "502, 502, 502" {
		t.Fatalf("upstream status header = %q, want retry chain", got)
	}
	if got := apisixctx.GetRequestVar(request, "$upstream_status"); got != "502, 502, 502" {
		t.Fatalf("$upstream_status = %#v, want retry chain", got)
	}
}

func TestDirectorFailureDoesNotExposeSyntheticUpstreamStatus(t *testing.T) {
	for _, show := range []bool{false, true} {
		t.Run(fmt.Sprintf("show=%t", show), func(t *testing.T) {
			staticConfig := &appconfig.Config{Apisix: appconfig.Apisix{
				ShowUpstreamStatusInResponseHeader: show,
			}}
			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
			request = apisixctx.WithRequestVars(request)
			request = withDirectorError(request, errors.New("no upstream target"))
			proxy.RecordUpstreamStatus(request, http.StatusBadGateway)

			response := httptest.NewRecorder()
			response.Header().Set("X-APISIX-Upstream-Status", "spoofed-by-upstream")
			newErrorHandler(
				staticConfig,
			)(
				response,
				request,
				errors.New("unsupported protocol scheme"),
			)
			if got := response.Header().Get("X-APISIX-Upstream-Status"); got != "" {
				t.Fatalf("upstream status header = %q, want absent for local director failure", got)
			}
			if got := apisixctx.GetRequestVar(request, "$upstream_status"); got != nil {
				t.Fatalf("$upstream_status = %#v, want absent for local director failure", got)
			}
		})
	}
}

func TestWebsocketUpgradeStripsUpstreamServerHeader(t *testing.T) {
	backend := newWebsocketBackend(t)
	handler := buildWebsocketHandler(t, resource.Route{
		ID:              "websocket-server-header",
		EnableWebsocket: true,
		Upstream:        backend.upstream(t),
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	connection, response, err := dialWebsocket(t, gateway.URL)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if got := response.Header.Values("Server"); len(got) != 0 {
		t.Fatalf("websocket Server headers = %q, want upstream token stripped", got)
	}
}

func mapsEqual(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range right {
		if left[key] != value {
			return false
		}
	}
	return true
}
