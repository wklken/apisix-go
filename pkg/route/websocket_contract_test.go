package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type websocketBackend struct {
	server       *httptest.Server
	upgradeCalls atomic.Int32
	httpCalls    atomic.Int32
}

func newWebsocketBackend(t *testing.T) *websocketBackend {
	t.Helper()
	backend := &websocketBackend{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			backend.upgradeCalls.Add(1)
			connection, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upstream websocket upgrade: %v", err)
				return
			}
			defer func() { _ = connection.Close() }()
			if err := connection.WriteMessage(websocket.TextMessage, []byte("upstream-websocket")); err != nil {
				t.Errorf("upstream websocket write: %v", err)
			}
			return
		}
		backend.httpCalls.Add(1)
		_, _ = w.Write([]byte("ordinary-http"))
	}))
	t.Cleanup(backend.server.Close)
	return backend
}

func (b *websocketBackend) upstream(t *testing.T) resource.Upstream {
	t.Helper()
	parsed, err := url.Parse(b.server.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	return resource.Upstream{
		Scheme: parsed.Scheme,
		Nodes:  []resource.Node{{Host: parsed.Hostname(), Port: port, Weight: 1}},
	}
}

func websocketNode(t *testing.T, server *httptest.Server) resource.Node {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	return resource.Node{Host: parsed.Hostname(), Port: port, Weight: 1}
}

func buildWebsocketHandler(t *testing.T, route resource.Route) http.Handler {
	t.Helper()
	ensureRouteStore(t)
	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(route)
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}
	return handler
}

func dialWebsocket(t *testing.T, serverURL string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(serverURL, "http"), nil)
}

func TestWebsocketUpgradeDisabledReturnsStableAPISIXJSON(t *testing.T) {
	backend := newWebsocketBackend(t)
	handler := buildWebsocketHandler(t, resource.Route{
		ID:       "websocket-disabled-route",
		Upstream: backend.upstream(t),
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	connection, response, err := dialWebsocket(t, gateway.URL)
	if err == nil {
		_ = connection.Close()
		t.Fatal("websocket dial error = nil, want APISIX-owned rejection")
	}
	if response == nil {
		t.Fatalf("websocket dial response = nil, want 400 response: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("websocket dial status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatalf("read rejection body: %v", readErr)
	}
	wantBody := util.BuildMessageResponse(websocketDisabledMessage)
	if string(body) != wantBody {
		t.Fatalf("rejection body = %q, want %q", body, wantBody)
	}
	if got := backend.upgradeCalls.Load(); got != 0 {
		t.Fatalf("upstream websocket calls = %d, want 0 before upstream dialing", got)
	}
}

func TestRouteEnabledWebsocketUpgradeUsesReverseProxyHijack(t *testing.T) {
	backend := newWebsocketBackend(t)
	handler := buildWebsocketHandler(t, resource.Route{
		ID:              "websocket-route-enabled",
		EnableWebsocket: true,
		Upstream:        backend.upstream(t),
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	connection, response, err := dialWebsocket(t, gateway.URL)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	messageType, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read proxied websocket message: %v", err)
	}
	if messageType != websocket.TextMessage || string(message) != "upstream-websocket" {
		t.Fatalf("proxied websocket message = (%d, %q), want text/upstream-websocket", messageType, message)
	}
	if got := backend.upgradeCalls.Load(); got != 1 {
		t.Fatalf("upstream websocket calls = %d, want 1", got)
	}
}

func TestServiceEnabledWebsocketUpgradeUsesReverseProxyHijack(t *testing.T) {
	backend := newWebsocketBackend(t)
	const serviceID = "websocket-service-enabled"
	serviceBody, err := apisixjson.Marshal(resource.Service{
		ID:              serviceID,
		EnableWebsocket: true,
		Upstream:        backend.upstream(t),
	})
	if err != nil {
		t.Fatalf("marshal websocket service: %v", err)
	}
	putHTTPAllowlistResource(t, "services", serviceID, serviceBody)
	handler := buildWebsocketHandler(t, resource.Route{
		ID:        "websocket-service-route",
		ServiceID: serviceID,
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	connection, response, err := dialWebsocket(t, gateway.URL)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	messageType, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read proxied websocket message: %v", err)
	}
	if messageType != websocket.TextMessage || string(message) != "upstream-websocket" {
		t.Fatalf("proxied websocket message = (%d, %q), want text/upstream-websocket", messageType, message)
	}
	if got := backend.upgradeCalls.Load(); got != 1 {
		t.Fatalf("upstream websocket calls = %d, want 1", got)
	}
}

func TestWebsocketUpgradeBypassesBufferedResponseExecutor(t *testing.T) {
	backend := newWebsocketBackend(t)
	handler := buildWebsocketHandler(t, resource.Route{
		ID:              "websocket-response-binding",
		EnableWebsocket: true,
		Plugins: map[string]resource.PluginConfig{
			"echo": map[string]any{"body": "response-binding"},
		},
		Upstream: backend.upstream(t),
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	connection, response, err := dialWebsocket(t, gateway.URL)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("websocket dial with response binding: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	messageType, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read proxied websocket message: %v", err)
	}
	if messageType != websocket.TextMessage || string(message) != "upstream-websocket" {
		t.Fatalf("proxied websocket message = (%d, %q), want text/upstream-websocket", messageType, message)
	}
}

func TestWebsocketUpgradeRunsRouteCorsHeaderPath(t *testing.T) {
	backend := newWebsocketBackend(t)
	handler := buildWebsocketHandler(t, resource.Route{
		ID:              "websocket-cors-route",
		EnableWebsocket: true,
		Plugins: map[string]resource.PluginConfig{
			"cors": map[string]any{"allow_origins": "https://client.test"},
		},
		Upstream: backend.upstream(t),
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	dialer := websocket.Dialer{}
	connection, response, err := dialer.Dial(
		"ws"+strings.TrimPrefix(gateway.URL, "http"),
		http.Header{"Origin": []string{"https://client.test"}},
	)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("websocket dial with route CORS: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("websocket response status = %d, want 101", response.StatusCode)
	}
}

func TestWebsocketUpgradeIgnoresDynamicConsumerResponseBinding(t *testing.T) {
	backend := newWebsocketBackend(t)
	const consumerID = "websocket-dynamic-response-consumer"
	putHTTPAllowlistResource(t, "consumers", consumerID, []byte(`{
		"username": "websocket-dynamic-response-consumer",
		"plugins": {
			"key-auth": {"key": "websocket-dynamic-key"},
			"echo": {"body": "consumer-response-binding"}
		}
	}`))
	handler := buildWebsocketHandler(t, resource.Route{
		ID:              "websocket-dynamic-response-route",
		EnableWebsocket: true,
		Plugins: map[string]resource.PluginConfig{
			"key-auth": map[string]any{},
		},
		Upstream: backend.upstream(t),
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	dialer := websocket.Dialer{}
	connection, response, err := dialer.Dial(
		"ws"+strings.TrimPrefix(gateway.URL, "http"),
		http.Header{"apikey": []string{"websocket-dynamic-key"}},
	)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("websocket dial with dynamic consumer response binding: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	messageType, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read proxied websocket message: %v", err)
	}
	if messageType != websocket.TextMessage || string(message) != "upstream-websocket" {
		t.Fatalf("proxied websocket message = (%d, %q), want text/upstream-websocket", messageType, message)
	}
}

func TestWebsocketUpgradeAdmissionRejectsSecondTunnelAcrossNodes(t *testing.T) {
	previous := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{Proxy: appconfig.Proxy{MaxInFlight: 1}}
	t.Cleanup(func() { appconfig.GlobalConfig = previous })

	release := make(chan struct{})
	connected := make(chan struct{})
	var upgradeCalls atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream websocket upgrade: %v", err)
			return
		}
		if upgradeCalls.Add(1) == 1 {
			close(connected)
		}
		<-release
		_ = connection.Close()
	})
	backendOne := httptest.NewServer(backendHandler)
	backendTwo := httptest.NewServer(backendHandler)
	t.Cleanup(func() {
		close(release)
		backendOne.Close()
		backendTwo.Close()
	})
	handler := buildWebsocketHandler(t, resource.Route{
		ID:              "websocket-admission-route",
		EnableWebsocket: true,
		Upstream: resource.Upstream{
			Scheme: "http",
			Nodes:  []resource.Node{websocketNode(t, backendOne), websocketNode(t, backendTwo)},
		},
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	first, response, err := dialWebsocket(t, gateway.URL)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("first websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("first websocket never reached an upstream node")
	}

	second, response, err := dialWebsocket(t, gateway.URL)
	if second != nil {
		_ = second.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second websocket dial = conn:%v response:%v err:%v, want fail-closed 503", second, response, err)
	}
	_ = response.Body.Close()
	if got := upgradeCalls.Load(); got != 1 {
		t.Fatalf("upstream websocket calls = %d, want one admitted tunnel", got)
	}
}

func TestOrdinaryHTTPPassesThroughWithWebsocketEnabled(t *testing.T) {
	backend := newWebsocketBackend(t)
	handler := buildWebsocketHandler(t, resource.Route{
		ID:              "ordinary-http-route",
		EnableWebsocket: true,
		Upstream:        backend.upstream(t),
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	response, err := http.Get(gateway.URL)
	if err != nil {
		t.Fatalf("ordinary HTTP request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read ordinary HTTP body: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ordinary-http" {
		t.Fatalf("ordinary HTTP response = (%d, %q), want 200/ordinary-http", response.StatusCode, body)
	}
	if got := backend.httpCalls.Load(); got != 1 {
		t.Fatalf("ordinary HTTP upstream calls = %d, want 1", got)
	}
}

func TestWebsocketUpgradeDetectionRequiresUpgradeTokenAndValue(t *testing.T) {
	tests := []struct {
		name        string
		connections []string
		upgrade     string
		status      int
	}{
		{
			name:        "multiple connection values",
			connections: []string{"keep-alive", " Upgrade "},
			upgrade:     " WebSocket ",
			status:      http.StatusBadRequest,
		},
		{
			name:        "comma separated connection value",
			connections: []string{"keep-alive, uPgRaDe"},
			upgrade:     "WEBSOCKET",
			status:      http.StatusBadRequest,
		},
		{
			name:        "ordinary HTTP",
			connections: []string{"keep-alive, not-upgrade"},
			upgrade:     "websocket",
			status:      http.StatusNoContent,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := requireWebsocketEnablement(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), false)
			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
			request.Header["Connection"] = append([]string(nil), test.connections...)
			request.Header.Set("Upgrade", test.upgrade)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
		})
	}
}
