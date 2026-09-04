package stream

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	streambridge "github.com/wklken/apisix-go/pkg/stream/bridge"
)

func compileTestRouter(
	t *testing.T,
	routes []resource.StreamRoute,
	onResult func(Result),
) (*Router, error) {
	t.Helper()
	prepared := make([]PreparedRoute, len(routes))
	for index := range routes {
		prepared[index].Route = routes[index]
		config, ok := routes[index].Plugins["mqtt-proxy"]
		if !ok || len(routes[index].Plugins) != 1 {
			continue
		}
		configMap, ok := config.(map[string]any)
		if !ok {
			t.Fatalf("mqtt-proxy test config type = %T, want map[string]any", config)
		}
		prepared[index].Protocol = preparedStreamMQTTBinding(
			t,
			plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routes[index].ID},
			configMap,
		)
	}
	return CompileRouter(context.Background(), CompileInput{
		Revision: 1,
		Routes:   prepared,
		OnResult: onResult,
	})
}

func TestRouterLeasePinsImmutableCompiledRouter(t *testing.T) {
	routes := []PreparedRoute{{Route: resource.StreamRoute{
		ID: "generation-81",
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}}}
	router, err := CompileRouter(context.Background(), CompileInput{
		Revision: 81,
		Routes:   routes,
	})
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	routes[0].Route.ID = "mutated"

	lease := RouterLease{Router: router, Release: func() {}}
	if got := lease.Router.RouteIDs(); len(got) != 1 || got[0] != "generation-81" {
		t.Fatalf("leased route IDs = %v, want [generation-81]", got)
	}
}

func TestRouteEntryPreservesFractionalStreamIdleTimeout(t *testing.T) {
	entry := routeEntry{route: resource.StreamRoute{Upstream: resource.Upstream{
		Timeout: resource.Timeout{Read: 0.5},
	}}}
	if got := entry.streamIdleTimeout(); got != 500*time.Millisecond {
		t.Fatalf("stream idle timeout = %s, want 500ms", got)
	}
}

func TestStreamBridgeIdleDeadlineExits(t *testing.T) {
	client, clientPeer := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = clientPeer.Close() }()
	upstream, upstreamPeer := net.Pipe()
	defer func() { _ = upstream.Close() }()
	defer func() { _ = upstreamPeer.Close() }()

	done := make(chan error, 1)
	go func() { done <- streambridge.Pump(context.Background(), client, upstream, nil, 50*time.Millisecond) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Pump() error = %v, want a clean idle exit", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Pump() did not exit after the configured idle deadline")
	}
}

func TestStreamBridgePreservesHalfClose(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })

	request := []byte("half-close-request")
	response := []byte("delayed-half-close-response")
	upstreamDone := make(chan error, 1)
	go func() {
		conn, acceptErr := upstream.Accept()
		if acceptErr != nil {
			upstreamDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		got := make([]byte, len(request))
		if _, readErr := io.ReadFull(conn, got); readErr != nil {
			upstreamDone <- fmt.Errorf("read request: %w", readErr)
			return
		}
		if !bytes.Equal(got, request) {
			upstreamDone <- fmt.Errorf("request = %q, want %q", got, request)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var probe [1]byte
		if _, readErr := conn.Read(probe[:]); !errors.Is(readErr, io.EOF) {
			upstreamDone <- fmt.Errorf("read after client half-close = %v, want EOF", readErr)
			return
		}
		time.Sleep(100 * time.Millisecond)
		if _, writeErr := conn.Write(response); writeErr != nil {
			upstreamDone <- fmt.Errorf("write delayed response: %w", writeErr)
			return
		}
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			if closeErr := tcpConn.CloseWrite(); closeErr != nil {
				upstreamDone <- fmt.Errorf("close upstream write: %w", closeErr)
				return
			}
		}
		upstreamDone <- nil
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen route: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	upstreamHost, upstreamPort, err := net.SplitHostPort(upstream.Addr().String())
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	upstreamPortNumber, err := strconv.Atoi(upstreamPort)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "half-close-route",
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial route: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept route: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- router.Serve(context.Background(), listener, serverConn) }()

	if _, err := client.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if tcpConn, ok := client.(*net.TCPConn); ok {
		if err := tcpConn.CloseWrite(); err != nil {
			t.Fatalf("close client write: %v", err)
		}
	} else {
		t.Fatal("client connection is not TCP")
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	gotResponse := make([]byte, len(response))
	if _, err := io.ReadFull(client, gotResponse); err != nil {
		t.Fatalf("read delayed response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("response = %q, want %q", gotResponse, response)
	}

	select {
	case err := <-upstreamDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not finish half-close exchange")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not finish half-close exchange")
	}
}

func TestRouterForwardsMatchingRouteAndPublishesResult(t *testing.T) {
	upstream, upstreamAddr := startStreamUpstream(t, []byte("stream-response"))
	defer func() { _ = upstream.Close() }()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stream route: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	upstreamHost, upstreamPort, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	var routePort int
	_, _, _ = net.SplitHostPort(listener.Addr().String())
	if _, err := fmt.Sscanf(listener.Addr().String(), "127.0.0.1:%d", &routePort); err != nil {
		t.Fatalf("parse route port: %v", err)
	}
	upstreamPortNumber, err := strconv.Atoi(upstreamPort)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	results := make(chan Result, 1)
	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID:         "tcp-route",
		ServerAddr: "127.0.0.1",
		ServerPort: routePort,
		RemoteAddr: "127.0.0.1/32",
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, func(result Result) { results <- result })
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial stream route: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept stream route: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- router.Serve(context.Background(), listener, serverConn) }()

	if _, err := client.Write([]byte("stream-request")); err != nil {
		t.Fatalf("write stream request: %v", err)
	}
	response := make([]byte, len("stream-response"))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatalf("read stream response: %v", err)
	}
	if string(response) != "stream-response" {
		t.Fatalf("response = %q, want stream-response", response)
	}
	_ = client.Close()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop after client close")
	}

	select {
	case result := <-results:
		if result.RouteID != "tcp-route" || result.Protocol != "tcp" {
			t.Fatalf("result = %#v", result)
		}
		if result.Err != nil {
			t.Fatalf("result error = %v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing stream result")
	}
}

func TestRouterRejectsNonMatchingRouteWithoutDialing(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stream route: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID:         "other-port",
		ServerPort: 1,
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial stream route: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept stream route: %v", err)
	}

	serveErr := router.Serve(context.Background(), listener, serverConn)
	if !errors.Is(serveErr, ErrNoStreamRoute) {
		t.Fatalf("Serve() error = %v, want %v", serveErr, ErrNoStreamRoute)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client read succeeded after unmatched route was closed")
	}
}

func TestRouterMatchesClientLocalAddrInsteadOfWildcardListener(t *testing.T) {
	upstream, upstreamAddr := startStreamUpstream(t, []byte("stream-response"))
	defer func() { _ = upstream.Close() }()

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen wildcard stream route: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	routePort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse route port: %v", err)
	}
	upstreamHost, upstreamPort, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	upstreamPortNumber, err := strconv.Atoi(upstreamPort)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID:         "local-addr-route",
		ServerAddr: "127.0.0.1",
		ServerPort: routePort,
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", portText))
	if err != nil {
		t.Fatalf("dial stream route: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept stream route: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- router.Serve(context.Background(), listener, serverConn) }()

	if _, err := client.Write([]byte("stream-request")); err != nil {
		t.Fatalf("write stream request: %v", err)
	}
	response := make([]byte, len("stream-response"))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatalf("read stream response: %v", err)
	}
	if string(response) != "stream-response" {
		t.Fatalf("response = %q, want stream-response", response)
	}
	_ = client.Close()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop after client close")
	}
}

func TestCompileRouterRejectsUnsupportedUpstreamScheme(t *testing.T) {
	_, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "tls-route",
		Upstream: resource.Upstream{
			Scheme: "tls",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 443, Weight: 1}},
		},
	}}, nil)

	if err == nil || !strings.Contains(err.Error(), "unsupported stream upstream scheme") {
		t.Fatalf("CompileRouter() error = %v, want unsupported scheme error", err)
	}
}

func TestCompileRouterRejectsDynamicDiscoveryWithStaticNodes(t *testing.T) {
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
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}},
			}
			test.set(&upstream)
			_, err := compileTestRouter(t, []resource.StreamRoute{{
				ID:       "dynamic-discovery-stream",
				Upstream: upstream,
			}}, nil)

			if err == nil {
				t.Fatal("CompileRouter() error = nil, want unsupported discovery error")
			}
			message := err.Error()
			if !strings.Contains(message, "dynamic-discovery-stream") || !strings.Contains(message, test.field) {
				t.Fatalf("CompileRouter() error = %q, want route and field provenance", message)
			}
		})
	}
}

func TestCompileRouterReportsReferencedUpstreamDiscoveryProvenance(t *testing.T) {
	_, err := compileTestRouter(t, []resource.StreamRoute{{
		ID:         "referenced-discovery-stream",
		UpstreamID: "stream-upstream-42",
		Upstream: resource.Upstream{
			Scheme:        "tcp",
			DiscoveryType: "dns",
			Nodes:         []resource.Node{{Host: "127.0.0.1", Port: 8080, Weight: 1}},
		},
	}}, nil)

	if err == nil {
		t.Fatal("CompileRouter() error = nil, want unsupported discovery error")
	}
	message := err.Error()
	if !strings.Contains(message, `upstream "stream-upstream-42"`) ||
		strings.Contains(message, "referenced-discovery-stream") {
		t.Fatalf("CompileRouter() error = %q, want upstream provenance without route fallback", message)
	}
}

func TestRouterMQTTForwardsAndPublishesClientID(t *testing.T) {
	packet := streamMQTTConnectPacket("route-client")
	payload := []byte("publish-before-connect-ack")
	response := []byte("broker-response")
	upstream, upstreamAddr := startStreamMQTTUpstream(t, append(packet, payload...), response)
	defer func() { _ = upstream.Close() }()
	upstreamHost, upstreamPort, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	upstreamPortNumber, err := strconv.Atoi(upstreamPort)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stream route: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, listenerPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	listenerPortNumber, err := strconv.Atoi(listenerPort)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	results := make(chan Result, 1)
	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID:         "mqtt-route",
		ServerPort: listenerPortNumber,
		Plugins:    map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{"protocol_level": 4}},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, func(result Result) { results <- result })
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial stream route: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept stream route: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- router.Serve(context.Background(), listener, serverConn) }()

	if _, err := client.Write(append(append([]byte(nil), packet...), payload...)); err != nil {
		t.Fatalf("write MQTT request: %v", err)
	}
	gotResponse := make([]byte, len(response))
	if _, err := io.ReadFull(client, gotResponse); err != nil {
		t.Fatalf("read MQTT response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("response = %q, want %q", gotResponse, response)
	}
	_ = client.Close()

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop after MQTT client close")
	}
	select {
	case result := <-results:
		if result.RouteID != "mqtt-route" || result.Protocol != "mqtt" || result.ClientID != "route-client" {
			t.Fatalf("result = %#v", result)
		}
		if result.Err != nil {
			t.Fatalf("result error = %v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing MQTT stream result")
	}
}

func TestRouterRejectsMalformedMQTTBeforeDial(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	upstreamAddr := upstream.Addr().String()
	upstreamHost, upstreamPort, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	upstreamPortNumber, err := strconv.Atoi(upstreamPort)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stream route: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, listenerPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	listenerPortNumber, err := strconv.Atoi(listenerPort)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID:         "mqtt-route",
		ServerPort: listenerPortNumber,
		Plugins:    map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{"protocol_level": 4}},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial stream route: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept stream route: %v", err)
	}
	if _, err := client.Write([]byte{0x20, 0x00}); err != nil {
		t.Fatalf("write malformed MQTT packet: %v", err)
	}
	serveErr := router.Serve(context.Background(), listener, serverConn)
	if !errors.Is(serveErr, mqtt_proxy.ErrMalformedConnect) {
		t.Fatalf("Serve() error = %v, want malformed CONNECT", serveErr)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client read succeeded after malformed CONNECT")
	}
}

func TestCompileRouterRejectsUnknownStreamPlugin(t *testing.T) {
	_, err := compileTestRouter(t, []resource.StreamRoute{{
		ID:      "unknown-plugin",
		Plugins: map[string]resource.PluginConfig{"limit-conn": map[string]any{}},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}}, nil)

	if err == nil || !strings.Contains(err.Error(), "not supported by the detached Go stream owner") {
		t.Fatalf("CompileRouter() error = %v, want unsupported plugin error", err)
	}
}

func TestCompileRouterDefaultsOmittedStreamNodeWeightToOne(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "omitted-weight",
		Upstream: resource.Upstream{
			Type:  "chash",
			Nodes: []resource.Node{{Host: "omitted.example", Port: 1883}},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	entry := router.routes[0]
	if len(entry.hashNodes) != 1 {
		t.Fatalf("hash nodes = %#v, want one omitted-weight node", entry.hashNodes)
	}
	if got := entry.hashNodes[0].weight; got != 1 {
		t.Fatalf("omitted stream node weight = %d, want 1", got)
	}
}

func TestCompileRouterDisablesExplicitZeroStreamNodeWeightForRRAndChash(t *testing.T) {
	for _, upstreamType := range []string{"roundrobin", "chash"} {
		t.Run(upstreamType, func(t *testing.T) {
			var route resource.StreamRoute
			if err := json.Unmarshal(fmt.Appendf(nil, `{
				"id": "zero-weight-%s",
				"upstream": {
					"scheme": "tcp",
					"type": %q,
					"nodes": [
						{"host": "disabled.example", "port": 1883, "weight": 0},
						{"host": "enabled.example", "port": 1884, "weight": 1}
					]
				}
			}`, upstreamType, upstreamType), &route); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			router, err := compileTestRouter(t, []resource.StreamRoute{route}, nil)
			if err != nil {
				t.Fatalf("CompileRouter() error = %v", err)
			}

			entry := router.routes[0]
			for _, node := range entry.hashNodes {
				if node.target == "tcp://disabled.example:1883" {
					t.Fatalf("explicit zero-weight node remained in hash nodes: %#v", entry.hashNodes)
				}
			}
			for i := range 32 {
				if got := entry.selectTarget(fmt.Sprintf("client-%d", i)); got == "tcp://disabled.example:1883" {
					t.Fatalf("selection %d chose disabled zero-weight node", i)
				}
			}
		})
	}
}

func TestCompileRouterRejectsNegativeStreamNodeWeight(t *testing.T) {
	_, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "negative-weight",
		Upstream: resource.Upstream{
			Nodes: []resource.Node{{Host: "negative.example", Port: 1883, Weight: -1}},
		},
	}}, nil)

	if err == nil || !strings.Contains(err.Error(), "weight must be non-negative") {
		t.Fatalf("CompileRouter() error = %v, want negative-weight rejection", err)
	}
}

func TestCompileRouterRejectsAllZeroStreamUpstreamWeightsForRRAndChash(t *testing.T) {
	for _, upstreamType := range []string{"roundrobin", "chash"} {
		t.Run(upstreamType, func(t *testing.T) {
			var route resource.StreamRoute
			if err := json.Unmarshal(fmt.Appendf(nil, `{
				"id": "all-zero-%s",
				"upstream": {
					"scheme": "tcp",
					"type": %q,
					"nodes": [
						{"host": "zero-a.example", "port": 1883, "weight": 0},
						{"host": "zero-b.example", "port": 1884, "weight": 0}
					]
				}
			}`, upstreamType, upstreamType), &route); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if _, err := compileTestRouter(t, []resource.StreamRoute{route}, nil); err == nil ||
				!strings.Contains(err.Error(), "at least one upstream node must have a positive weight") {
				t.Fatalf("CompileRouter() error = %v, want all-zero rejection", err)
			}
		})
	}
}

func TestRouterSelectsOnlyHighestPriorityStreamGroup(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "priority-selection",
		Upstream: resource.Upstream{
			Type: "roundrobin",
			Nodes: []resource.Node{
				{Host: "high.example", Port: 1883, Weight: 1, Priority: 10},
				{Host: "low.example", Port: 1883, Weight: 1, Priority: 0},
			},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	entry := router.routes[0]
	for range 20 {
		if got := entry.selectTarget(""); got != "tcp://high.example:1883" {
			t.Fatalf("selected target = %q, want only highest-priority target", got)
		}
	}
}

func TestRouterRetriesFromClosedHighPriorityStreamNode(t *testing.T) {
	high, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen high-priority target: %v", err)
	}
	highAddr := high.Addr().String()
	highHost, highPortText, err := net.SplitHostPort(highAddr)
	if err != nil {
		t.Fatalf("split high-priority target: %v", err)
	}
	highPort, err := strconv.Atoi(highPortText)
	if err != nil {
		t.Fatalf("parse high-priority target port: %v", err)
	}

	low, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen low-priority target: %v", err)
	}
	t.Cleanup(func() { _ = low.Close() })
	if err := high.Close(); err != nil {
		t.Fatalf("close high-priority target: %v", err)
	}
	lowHost, lowPortText, err := net.SplitHostPort(low.Addr().String())
	if err != nil {
		t.Fatalf("split low-priority target: %v", err)
	}
	lowPort, err := strconv.Atoi(lowPortText)
	if err != nil {
		t.Fatalf("parse low-priority target port: %v", err)
	}

	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "priority-retry",
		Upstream: resource.Upstream{
			Retries: 1,
			Nodes: []resource.Node{
				{Host: highHost, Port: highPort, Weight: 1, Priority: 10},
				{Host: lowHost, Port: lowPort, Weight: 1, Priority: 0},
			},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	conn, err := router.routes[0].dial(context.Background(), "")
	if err != nil {
		t.Fatalf("dial() error = %v, want lower-priority fallback", err)
	}
	if got, want := conn.RemoteAddr().String(), low.Addr().String(); got != want {
		_ = conn.Close()
		t.Fatalf("dial() connected to %q, want lower-priority target %q", got, want)
	}
	_ = conn.Close()
}

func TestRouterDoesNotRetryWhenStreamRetriesIsZero(t *testing.T) {
	high, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen high-priority target: %v", err)
	}
	highAddr := high.Addr().String()
	highHost, highPortText, err := net.SplitHostPort(highAddr)
	if err != nil {
		t.Fatalf("split high-priority target: %v", err)
	}
	highPort, err := strconv.Atoi(highPortText)
	if err != nil {
		t.Fatalf("parse high-priority target port: %v", err)
	}

	low, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen low-priority target: %v", err)
	}
	t.Cleanup(func() { _ = low.Close() })
	if err := high.Close(); err != nil {
		t.Fatalf("close high-priority target: %v", err)
	}
	lowHost, lowPortText, err := net.SplitHostPort(low.Addr().String())
	if err != nil {
		t.Fatalf("split low-priority target: %v", err)
	}
	lowPort, err := strconv.Atoi(lowPortText)
	if err != nil {
		t.Fatalf("parse low-priority target port: %v", err)
	}

	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "priority-no-retry",
		Upstream: resource.Upstream{
			Retries: 0,
			Nodes: []resource.Node{
				{Host: highHost, Port: highPort, Weight: 1, Priority: 10},
				{Host: lowHost, Port: lowPort, Weight: 1, Priority: 0},
			},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	if _, err := router.routes[0].dial(context.Background(), ""); err == nil {
		t.Fatal("dial() error = nil, want closed high-priority target failure")
	}
	tcpLow := low.(*net.TCPListener)
	if err := tcpLow.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set low-priority listener deadline: %v", err)
	}
	conn, acceptErr := tcpLow.Accept()
	if acceptErr == nil {
		_ = conn.Close()
		t.Fatal("dial() fell back to low-priority target with retries=0")
	}
	var networkError net.Error
	if !errors.As(acceptErr, &networkError) || !networkError.Timeout() {
		t.Fatalf("low-priority Accept() error = %v, want timeout without a connection", acceptErr)
	}
}

func TestRouterMatchesExactRemoteAddressAndCIDR(t *testing.T) {
	for _, remote := range []string{"127.0.0.1", "127.0.0.1/32"} {
		router, err := compileTestRouter(t, []resource.StreamRoute{{
			RemoteAddr: remote,
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		}}, nil)
		if err != nil {
			t.Fatalf("CompileRouter(%q) error = %v", remote, err)
		}
		if !router.routeMatches(resource.StreamRoute{RemoteAddr: remote}, "127.0.0.1:1234", "127.0.0.1:1883") {
			t.Fatalf("route with remote_addr %q did not match loopback peer", remote)
		}
	}
}

func TestRouterUsesMostSpecificMatchingResource(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{
		{
			ID: "wildcard",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		{
			ID:         "specific",
			ServerPort: 1883,
			RemoteAddr: "127.0.0.1",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	entry, ok := router.matchEntry("127.0.0.1:1883", "127.0.0.1:1000")
	if !ok || entry.route.ID != "specific" {
		t.Fatalf("matched route = %#v, want specific resource", entry.route)
	}
}

func TestRouterUsesLongestRemotePrefixIndependentOfResourceOrder(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{
		{
			ID:         "broad",
			RemoteAddr: "10.0.0.0/8",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		{
			ID:         "disjoint",
			RemoteAddr: "192.0.2.1",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		{
			ID:         "narrow",
			RemoteAddr: "10.20.0.0/16",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	entry, ok := router.matchEntry("127.0.0.1:1883", "10.20.30.40:1000")
	if !ok || entry.route.ID != "narrow" {
		t.Fatalf("matched route = %#v, want narrow resource", entry.route)
	}
}

func TestRouterUsesExactRemoteAddressBeforeCIDR(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{
		{
			ID:         "cidr",
			RemoteAddr: "192.0.2.0/24",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		{
			ID:         "exact",
			RemoteAddr: "192.0.2.10",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 2, Weight: 1}},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	entry, ok := router.matchEntry("127.0.0.1:1883", "192.0.2.10:1000")
	if !ok || entry.route.ID != "exact" {
		t.Fatalf("matched route = %#v, want exact resource", entry.route)
	}
}

func TestCompileRouterRejectsOverlappingIncomparablePredicates(t *testing.T) {
	_, err := compileTestRouter(t, []resource.StreamRoute{
		{
			ID:         "listener-specific",
			ServerAddr: "127.0.0.1",
			RemoteAddr: "10.0.0.0/8",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		{
			ID:         "remote-specific",
			ServerPort: 1883,
			RemoteAddr: "10.20.0.0/16",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 2, Weight: 1}},
			},
		},
	}, nil)

	if err == nil {
		t.Fatal("CompileRouter() error = nil, want ambiguous overlap")
	}
	if !strings.Contains(err.Error(), "overlapping stream route predicates") ||
		!strings.Contains(err.Error(), "listener-specific") ||
		!strings.Contains(err.Error(), "remote-specific") {
		t.Fatalf("CompileRouter() error = %q, want ambiguity and both route IDs", err)
	}
}

func TestCompileRouterRejectsConflictingListenAddresses(t *testing.T) {
	_, err := compileTestRouter(t, []resource.StreamRoute{
		{
			ID:         "alpha",
			ServerAddr: "127.0.0.1",
			ServerPort: 1883,
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		{
			ID:         "beta",
			ServerAddr: "127.0.0.1",
			ServerPort: 1883,
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 2, Weight: 1}},
			},
		},
	}, nil)

	if err == nil {
		t.Fatal("CompileRouter() error = nil, want conflicting listen address")
	}
	if !strings.Contains(err.Error(), "conflicting stream listen address") ||
		!strings.Contains(err.Error(), "alpha") ||
		!strings.Contains(err.Error(), "beta") {
		t.Fatalf("CompileRouter() error = %q, want both route IDs", err)
	}
}

func TestCompileRouterAllowsSharedListenForDifferentRemoteAddresses(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{
		{
			ID:         "private-network",
			ServerAddr: "127.0.0.1",
			ServerPort: 1883,
			RemoteAddr: "10.0.0.0/8",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		},
		{
			ID:         "site-network",
			ServerAddr: "127.0.0.1",
			ServerPort: 1883,
			RemoteAddr: "192.168.0.0/16",
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 2, Weight: 1}},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v, want shared listen with distinct remote_addr", err)
	}
	if len(router.routes) != 2 {
		t.Fatalf("compiled routes = %d, want 2", len(router.routes))
	}
}

func TestRouterUsesDeterministicClientIDHashForChashUpstream(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "mqtt-hash",
		Upstream: resource.Upstream{
			Type:   "chash",
			HashOn: "vars",
			Key:    "mqtt_client_id",
			Nodes: []resource.Node{
				{Host: "broker-a", Port: 1883, Weight: 1},
				{Host: "broker-b", Port: 1883, Weight: 1},
			},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	entry, ok := router.matchEntry("127.0.0.1:1883", "127.0.0.1:1000")
	if !ok {
		t.Fatal("chash route did not match")
	}
	first := entry.selectTarget("client-1")
	if first == "" || first != entry.selectTarget("client-1") {
		t.Fatalf("same client ID selected different targets: first=%q second=%q", first, entry.selectTarget("client-1"))
	}
	if first == entry.selectTarget("client-2") && first == entry.selectTarget("client-3") {
		t.Fatalf("different client IDs all selected %q; expected deterministic distribution", first)
	}
}

func TestRouterChashPreservesConfiguredNodeOrderWithinPriority(t *testing.T) {
	router, err := compileTestRouter(t, []resource.StreamRoute{{
		ID: "mqtt-hash-order",
		Upstream: resource.Upstream{
			Type:   "chash",
			HashOn: "vars",
			Key:    "mqtt_client_id",
			Nodes: []resource.Node{
				{Host: "broker-z", Port: 1883, Weight: 1, Priority: 10},
				{Host: "broker-a", Port: 1883, Weight: 1, Priority: 10},
			},
		},
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	entry := router.routes[0]
	if got := entry.groups[0].hashNodes[0].target; got != "tcp://broker-z:1883" {
		t.Fatalf("first chash target = %q, want configured first target", got)
	}
	if got := entry.selectTarget("client-1"); got != "tcp://broker-z:1883" {
		t.Fatalf("client-1 chash target = %q, want mapping from configured node order", got)
	}
}

func startStreamUpstream(t *testing.T, response []byte) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		request := make([]byte, len("stream-request"))
		if _, readErr := io.ReadFull(conn, request); readErr != nil {
			return
		}
		_, _ = conn.Write(response)
	}()
	return listener, listener.Addr().String()
}

func startStreamMQTTUpstream(t *testing.T, request, response []byte) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen MQTT upstream: %v", err)
	}
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		got := make([]byte, len(request))
		if _, readErr := io.ReadFull(conn, got); readErr != nil || !bytes.Equal(got, request) {
			return
		}
		_, _ = conn.Write(response)
	}()
	return listener, listener.Addr().String()
}

func streamMQTTConnectPacket(clientID string) []byte {
	body := make([]byte, 0, 16+len(clientID))
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], 4)
	body = append(body, length[:]...)
	body = append(body, []byte("MQTT")...)
	body = append(body, 4, 0x02, 0, 60)
	binary.BigEndian.PutUint16(length[:], uint16(len(clientID)))
	body = append(body, length[:]...)
	body = append(body, clientID...)
	return append([]byte{0x10, byte(len(body))}, body...)
}
