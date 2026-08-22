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
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
)

func TestBuildUsesDynamicHTTPPluginAllowlistAndFallsBackAfterDelete(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t, "request-id")
	putDynamicHTTPPluginList(t, `[ {"name":"gzip"}, {"name":"mqtt-proxy","stream":true} ]`)

	const routeID = "dynamic-http-plugin-route"
	route := `{"id":"dynamic-http-plugin-route","uri":"/dynamic-http-plugin","plugins":{"request-id":{}}}`
	putRouteResource(t, routeID, []byte(route))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil || handler != nil ||
		!strings.Contains(err.Error(), `plugin "request-id" is disabled`) {
		t.Fatalf("dynamic allowlist BuildStrict() = (%T, %v), want request-id rejection", handler, err)
	}

	deleteDynamicHTTPPluginList(t)
	if handler, err = builder.BuildStrict(); err != nil || handler == nil {
		t.Fatalf("startup allowlist fallback BuildStrict() = (%T, %v), want success", handler, err)
	}
}

func TestBuildInheritsServiceHosts(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)

	const serviceID = "route-service-hosts"
	serviceJSON := []byte(`{"id":"route-service-hosts","hosts":["service.example.com"]}`)
	putHTTPAllowlistResource(t, "services", serviceID, serviceJSON)
	const routeTemplate = `{"id":"service-host-route","uri":"/service-host",` +
		`"service_id":%q,"upstream":{"type":"roundrobin","nodes":{%q:1}}}`
	routeBody := fmt.Appendf(
		nil,
		routeTemplate,
		serviceID,
		routePriorityNode(t, backend.URL),
	)
	putRouteResource(t, "service-host-route", routeBody)

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil {
		t.Fatalf("BuildStrict() error = %v", err)
	}

	for _, test := range []struct {
		name string
		host string
		want int
	}{
		{name: "service host", host: "service.example.com", want: http.StatusNoContent},
		{name: "other host", host: "other.example.com", want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/service-host", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("host %q status = %d, want %d", test.host, response.Code, test.want)
			}
		})
	}
}

func TestMaterializeRouteUsesOneServiceVersionForHostsAndHandler(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(backend.Close)

	const serviceID = "materialized-service-version"
	serviceBody := fmt.Appendf(nil,
		`{"id":%q,"hosts":["v1.example.com"],"upstream":{"type":"roundrobin","nodes":{%q:1}}}`,
		serviceID,
		routePriorityNode(t, backend.URL),
	)
	putHTTPAllowlistResource(t, "services", serviceID, serviceBody)

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, hosts, err := builder.materializeRouteStrict(resource.Route{
		ID:        "materialized-route-version",
		ServiceID: serviceID,
	})
	if err != nil {
		t.Fatalf("materializeRouteStrict() error = %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "v1.example.com" {
		t.Fatalf("materialized hosts = %v, want service v1 hosts", hosts)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://v1.example.com/", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("materialized handler status = %d, want service v1 upstream status", response.Code)
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
		priorities,
	)
	if err != nil {
		t.Fatalf("buildClusterConfigWithTransport() error = %v", err)
	}
	if !mapsEqual(config.Priorities, priorities) {
		t.Fatalf("cluster priorities = %#v, want %#v", config.Priorities, priorities)
	}
}

func TestBuildReverseHandlerSelectsHigherPriorityNode(t *testing.T) {
	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(low.Close)
	high := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(high.Close)

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	lowNode := websocketNode(t, low)
	highNode := websocketNode(t, high)
	handler, err := builder.buildReverseHandler(resource.Route{
		Upstream: resource.Upstream{
			Scheme: "http",
			Nodes: []resource.Node{
				{Host: lowNode.Host, Port: lowNode.Port, Weight: 1, Priority: 1},
				{Host: highNode.Host, Port: highNode.Port, Weight: 1, Priority: 10},
			},
		},
	}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}
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

func TestExplicitFalseRouteWebsocketOverridesService(t *testing.T) {
	ensureRouteStore(t)
	backend := newWebsocketBackend(t)
	const serviceID = "route-websocket-inherit-service"
	serviceBody, err := apisixjson.Marshal(resource.Service{
		ID:              serviceID,
		EnableWebsocket: true,
		Upstream:        backend.upstream(t),
	})
	if err != nil {
		t.Fatalf("marshal websocket service: %v", err)
	}
	putHTTPAllowlistResource(t, "services", serviceID, serviceBody)

	var route resource.Route
	if err := apisixjson.Unmarshal(fmt.Appendf(nil,
		`{"id":"route-websocket-explicit-false","service_id":%q,"enable_websocket":false}`,
		serviceID,
	), &route); err != nil {
		t.Fatalf("unmarshal explicit false route: %v", err)
	}
	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(route)
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("explicit false websocket status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestUpstreamStatusResponseHeaderFollowsConfiguration(t *testing.T) {
	previous := appconfig.GlobalConfig
	t.Cleanup(func() { appconfig.GlobalConfig = previous })
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
			appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{
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
			if err := newModifyResponse()(response); err != nil {
				t.Fatalf("newModifyResponse() error = %v", err)
			}
			if got := response.Header.Get("X-APISIX-Upstream-Status"); got != test.want {
				t.Fatalf("upstream status header = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUpstreamTransportFailuresExposeRetryStatusChain(t *testing.T) {
	previous := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{}
	t.Cleanup(func() { appconfig.GlobalConfig = previous })

	transport := proxy.NewRetryTransport(routeRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}))
	request := httptest.NewRequest(http.MethodGet, "http://upstream.test/", nil)
	request = apisixctx.WithRequestVars(request)
	request = proxy.WithRetries(request, 2, func(*http.Request) bool { return true })
	_, err := transport.RoundTrip(request)
	if err == nil {
		t.Fatal("RoundTrip() error = nil, want transport failure")
	}

	response := httptest.NewRecorder()
	newErrorHandler()(response, request, err)
	if got := response.Header().Get("X-APISIX-Upstream-Status"); got != "502, 502, 502" {
		t.Fatalf("upstream status header = %q, want retry chain", got)
	}
	if got := apisixctx.GetRequestVar(request, "$upstream_status"); got != "502, 502, 502" {
		t.Fatalf("$upstream_status = %#v, want retry chain", got)
	}
}

func TestDirectorFailureDoesNotExposeSyntheticUpstreamStatus(t *testing.T) {
	previous := appconfig.GlobalConfig
	t.Cleanup(func() { appconfig.GlobalConfig = previous })

	for _, show := range []bool{false, true} {
		t.Run(fmt.Sprintf("show=%t", show), func(t *testing.T) {
			appconfig.GlobalConfig = &appconfig.Config{Apisix: appconfig.Apisix{
				ShowUpstreamStatusInResponseHeader: show,
			}}
			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
			request = apisixctx.WithRequestVars(request)
			request = withDirectorError(request, errors.New("no upstream target"))
			proxy.RecordUpstreamStatus(request, http.StatusBadGateway)

			response := httptest.NewRecorder()
			response.Header().Set("X-APISIX-Upstream-Status", "spoofed-by-upstream")
			newErrorHandler()(response, request, errors.New("unsupported protocol scheme"))
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

func putDynamicHTTPPluginList(t *testing.T, value string) {
	t.Helper()
	ensureRouteStore(t)
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/plugins")
	event.Value = []byte(value)
	routeStoreEvents <- event
	if err := routeStore.Sync(); err != nil {
		t.Fatalf("put dynamic plugin list: %v", err)
	}
	t.Cleanup(func() { deleteDynamicHTTPPluginList(t) })
}

func deleteDynamicHTTPPluginList(t *testing.T) {
	t.Helper()
	if routeStore == nil {
		return
	}
	event := store.NewEvent()
	event.Type = store.EventTypeDelete
	event.Key = []byte("/apisix/plugins")
	routeStoreEvents <- event
	if err := routeStore.Sync(); err != nil {
		t.Errorf("delete dynamic plugin list: %v", err)
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
