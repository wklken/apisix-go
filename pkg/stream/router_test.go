package stream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestRouterForwardsMatchingRouteAndPublishesResult(t *testing.T) {
	upstream, upstreamAddr := startStreamUpstream(t, []byte("stream-response"))
	defer upstream.Close()

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
	router, err := NewRouter([]resource.StreamRoute{{
		ID:         "tcp-route",
		ServerAddr: "127.0.0.1",
		ServerPort: routePort,
		RemoteAddr: "127.0.0.1/32",
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, nil, func(result Result) { results <- result })
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
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

	router, err := NewRouter([]resource.StreamRoute{{
		ID:         "other-port",
		ServerPort: 1,
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}}, nil, nil)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
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

func TestNewRouterRejectsUnsupportedUpstreamScheme(t *testing.T) {
	_, err := NewRouter([]resource.StreamRoute{{
		ID: "tls-route",
		Upstream: resource.Upstream{
			Scheme: "tls",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 443, Weight: 1}},
		},
	}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported stream upstream scheme") {
		t.Fatalf("NewRouter() error = %v, want unsupported scheme error", err)
	}
}

func TestRouterMQTTForwardsAndPublishesClientID(t *testing.T) {
	packet := streamMQTTConnectPacket("route-client")
	payload := []byte("publish-before-connect-ack")
	response := []byte("broker-response")
	upstream, upstreamAddr := startStreamMQTTUpstream(t, append(packet, payload...), response)
	defer upstream.Close()
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
	router, err := NewRouter([]resource.StreamRoute{{
		ID:         "mqtt-route",
		ServerPort: listenerPortNumber,
		Plugins:    map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{"protocol_level": 4}},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, []string{"mqtt-proxy"}, func(result Result) { results <- result })
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
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

	router, err := NewRouter([]resource.StreamRoute{{
		ID:         "mqtt-route",
		ServerPort: listenerPortNumber,
		Plugins:    map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{"protocol_level": 4}},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPortNumber, Weight: 1}},
		},
	}}, []string{"mqtt-proxy"}, nil)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
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

func TestNewRouterRejectsUnknownStreamPlugin(t *testing.T) {
	_, err := NewRouter([]resource.StreamRoute{{
		ID:      "unknown-plugin",
		Plugins: map[string]resource.PluginConfig{"limit-conn": map[string]any{}},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}}, []string{"limit-conn"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not supported by the Go stream owner") {
		t.Fatalf("NewRouter() error = %v, want unsupported plugin error", err)
	}
}

func TestRouterMatchesExactRemoteAddressAndCIDR(t *testing.T) {
	for _, remote := range []string{"127.0.0.1", "127.0.0.1/32"} {
		router, err := NewRouter([]resource.StreamRoute{{
			RemoteAddr: remote,
			Upstream: resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
			},
		}}, nil, nil)
		if err != nil {
			t.Fatalf("NewRouter(%q) error = %v", remote, err)
		}
		if !router.routeMatches(resource.StreamRoute{RemoteAddr: remote}, "127.0.0.1:1234", "127.0.0.1:1883") {
			t.Fatalf("route with remote_addr %q did not match loopback peer", remote)
		}
	}
}

func TestRouterPrefersSpecificRouteOverWildcard(t *testing.T) {
	router, err := NewRouter([]resource.StreamRoute{
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
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	entry, ok := router.matchEntry("127.0.0.1:1883", "127.0.0.1:1000")
	if !ok || entry.route.ID != "specific" {
		t.Fatalf("matched route = %#v, want specific", entry.route)
	}
}

func TestRouterUsesDeterministicClientIDHashForChashUpstream(t *testing.T) {
	router, err := NewRouter([]resource.StreamRoute{{
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
	}}, []string{"mqtt-proxy"}, nil)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
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
		defer conn.Close()
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
		defer conn.Close()
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
