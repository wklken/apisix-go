package route

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestUpstreamKeepalivePoolControlsTransport(t *testing.T) {
	for _, test := range []struct {
		name, pool string
		size       int
		idle       time.Duration
	}{
		{"default", "", 320, 60 * time.Second},
		{"empty", `,"keepalive_pool":{}`, 320, 60 * time.Second},
		{"configured", `,"keepalive_pool":{"size":3,"idle_timeout":0.05}`, 3, 50 * time.Millisecond},
		{"no-idle-expiry", `,"keepalive_pool":{"idle_timeout":0}`, 320, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstream resource.Upstream
			if err := json.Unmarshal([]byte(`{"nodes":{"127.0.0.1:1":1}`+test.pool+`}`), &upstream); err != nil {
				t.Fatal(err)
			}
			option, err := buildTransportOptionWithSSLResolver(resource.Route{}, upstream, nil)
			if err != nil {
				t.Fatal(err)
			}
			transport := proxy.NewTransport(option)
			defer transport.CloseIdleConnections()
			if transport.IdleConnTimeout != test.idle || transport.MaxIdleConnsPerHost != test.size {
				t.Fatalf(
					"idle=%s size=%d want %s/%d",
					transport.IdleConnTimeout,
					transport.MaxIdleConnsPerHost,
					test.idle,
					test.size,
				)
			}
		})
	}
}

func TestUpstreamKeepaliveStaticDefaultsAndResourceOverride(t *testing.T) {
	cfg := &appconfig.Config{
		NginxConfig: appconfig.NginxConfig{
			HTTP: appconfig.NginxHTTP{
				Upstream: &appconfig.NginxHTTPUpstream{Keepalive: 7, KeepaliveTimeout: 2 * time.Second},
			},
		},
	}
	upstream := resource.Upstream{Nodes: []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}}}
	first, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		upstream,
		map[string]int{"http://127.0.0.1:1": 1},
		nil,
		cfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	tr := proxy.NewTransport(first.Transport)
	defer tr.CloseIdleConnections()
	if tr.MaxIdleConnsPerHost != 7 || tr.IdleConnTimeout != 2*time.Second {
		t.Fatalf("static pool size=%d idle=%s", tr.MaxIdleConnsPerHost, tr.IdleConnTimeout)
	}
	upstream.KeepalivePool = &resource.UpstreamKeepalivePool{Size: 3, IdleTimeout: 0}
	second, err := buildClusterConfigWithSSLResolver(
		resource.Route{},
		upstream,
		map[string]int{"http://127.0.0.1:1": 1},
		nil,
		cfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	tr2 := proxy.NewTransport(second.Transport)
	defer tr2.CloseIdleConnections()
	if tr2.MaxIdleConnsPerHost != 3 || tr2.IdleConnTimeout != 0 {
		t.Fatalf("resource pool size=%d idle=%s", tr2.MaxIdleConnsPerHost, tr2.IdleConnTimeout)
	}
	firstKey, err := first.Key()
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := second.Key()
	if err != nil {
		t.Fatal(err)
	}
	if firstKey == secondKey {
		t.Fatal("pool configuration change reused cluster identity")
	}
	cloned := cloneCompileUpstream(upstream)
	upstream.KeepalivePool.Size = 99
	if cloned.KeepalivePool.Size != 3 {
		t.Fatal("snapshot retains mutable pool config")
	}
}

func TestUpstreamKeepaliveIdleTimeoutClosesRealConnection(t *testing.T) {
	closed := make(chan struct{}, 4)
	upstream := httptest.NewUnstartedServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, r.RemoteAddr) }),
	)
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	upstream.Start()
	defer upstream.Close()
	route := testRouteFromJSON(
		t,
		fmt.Sprintf(
			`{"id":"idle-pool","uri":"/","upstream":{"keepalive_pool":{"idle_timeout":0.05},"nodes":{%q:1}}}`,
			upstream.Listener.Addr().String(),
		),
	)
	handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if first.Code != 200 {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("configured idle timeout did not close upstream connection")
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if second.Code != 200 || first.Body.String() == second.Body.String() {
		t.Fatalf("second status=%d connections=%q/%q", second.Code, first.Body.String(), second.Body.String())
	}
}

func TestUpstreamKeepaliveRequestLimitClosesConnection(t *testing.T) {
	for _, test := range []struct{ method, scheme string }{{http.MethodGet, "http"}, {http.MethodPost, "http"}, {http.MethodGet, "https"}, {http.MethodPost, "https"}} {
		t.Run(test.scheme+"/"+test.method, func(t *testing.T) {
			addresses := make(chan string, 5)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				addresses <- r.RemoteAddr
				_, _ = io.WriteString(w, "ok")
			}))
			if test.scheme == "https" {
				upstream.StartTLS()
			} else {
				upstream.Start()
			}
			defer upstream.Close()
			route := testRouteFromJSON(
				t,
				fmt.Sprintf(
					`{"id":"requests-cap","uri":"/","upstream":{"scheme":%q,"nodes":{%q:1},"keepalive_pool":{"requests":2}}}`,
					test.scheme,
					upstream.Listener.Addr().String(),
				),
			)
			handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
			for range 5 {
				response := httptest.NewRecorder()
				handler.ServeHTTP(
					response,
					httptest.NewRequest(test.method, "http://gateway/", strings.NewReader("payload")),
				)
				if response.Code != 200 {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			}
			seen := make([]string, 5)
			for i := range seen {
				seen[i] = <-addresses
			}
			if seen[0] != seen[1] || seen[1] == seen[2] || seen[2] != seen[3] || seen[3] == seen[4] {
				t.Fatalf("connection sequence=%v, want pairs on separate connections", seen)
			}
		})
	}
}

func TestUpstreamKeepaliveRejectedUpgradeRetiresConnection(t *testing.T) {
	seen := make(chan string, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.RemoteAddr
		_, _ = io.WriteString(w, "ordinary response")
	}))
	defer upstream.Close()
	route := testRouteFromJSON(
		t,
		fmt.Sprintf(
			`{"id":"cap-upgrade","uri":"/","enable_websocket":true,"upstream":{"nodes":{%q:1},"keepalive_pool":{"requests":1}}}`,
			upstream.Listener.Addr().String(),
		),
	)
	handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "http://gateway/", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	a, b, c := <-seen, <-seen, <-seen
	if a == b || b == c || a == c {
		t.Fatalf("rejected upgrades reused connection: %s %s %s", a, b, c)
	}
}

func TestUpstreamKeepaliveStaticZeroDisablesReuse(t *testing.T) {
	seen := make(chan string, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.RemoteAddr
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	route := testRouteFromJSON(
		t,
		fmt.Sprintf(`{"id":"zero-cap","uri":"/","upstream":{"nodes":{%q:1}}}`, upstream.Listener.Addr().String()),
	)
	cfg := testEffectiveConfig()
	cfg.Config.NginxConfig.HTTP.Upstream = &appconfig.NginxHTTPUpstream{
		Keepalive:         320,
		KeepaliveTimeout:  time.Minute,
		KeepaliveRequests: 0,
	}
	handler := testPreparedProxyHandler(t, route, resource.Service{}, cfg)
	for range 3 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway/", nil))
		if response.Code != 200 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	a, b, c := <-seen, <-seen, <-seen
	if a == b || b == c || a == c {
		t.Fatalf("zero cap reused connection: %s %s %s", a, b, c)
	}
}

func TestUpstreamKeepaliveRequestLimitPreservesSuccessfulUpgrade(t *testing.T) {
	backend := newWebsocketBackend(t)
	route := testRouteFromJSON(
		t,
		fmt.Sprintf(
			`{"id":"cap-upgrade","uri":"/","enable_websocket":true,"upstream":{"nodes":{%q:1},"keepalive_pool":{"requests":1}}}`,
			backend.server.Listener.Addr().String(),
		),
	)
	handler := testPreparedProxyHandler(t, route, resource.Service{}, testEffectiveConfig())
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, response, err := dialWebsocket(t, server.URL)
	if err != nil {
		t.Fatalf("upgrade: %v, response=%v", err, response)
	}
	defer func() { _ = conn.Close() }()
	_, message, err := conn.ReadMessage()
	if err != nil || string(message) != "upstream-websocket" {
		t.Fatalf("message=%q err=%v", message, err)
	}
}
