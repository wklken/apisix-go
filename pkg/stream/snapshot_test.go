package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestCompileRouterOwnsInputAndPreparedMQTTBinding(t *testing.T) {
	packet := streamMQTTConnectPacket("detached-client")
	payload := []byte("detached-payload")
	response := []byte("detached-response")
	upstream, upstreamAddr := startStreamMQTTUpstream(t, append(packet, payload...), response)
	t.Cleanup(func() { _ = upstream.Close() })
	upstreamHost, upstreamPort := splitStreamTestAddress(t, upstreamAddr)

	pluginConfig := map[string]any{"protocol_level": 4}
	route := resource.StreamRoute{
		ID:      "detached-mqtt",
		Plugins: map[string]resource.PluginConfig{"mqtt-proxy": pluginConfig},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: upstreamHost, Port: upstreamPort, Weight: 1}},
			Checks: map[string]any{"nested": map[string]any{"state": "original"}},
		},
	}
	binding := preparedStreamMQTTBinding(
		t,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
		pluginConfig,
	)
	input := CompileInput{
		Revision: 41,
		Routes:   []PreparedRoute{{Route: route, Protocol: binding}},
	}
	router, err := CompileRouter(context.Background(), input)
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	input.Routes[0].Route.ID = "mutated-route"
	input.Routes[0].Route.Upstream.Nodes[0].Host = "mutated.invalid"
	input.Routes[0].Route.Upstream.Checks["nested"].(map[string]any)["state"] = "mutated"
	pluginConfig["protocol_level"] = 5
	input.Routes[0].Protocol.Plugin = nil
	input.Routes[0].Protocol.Descriptor.Phases[0] = plugin.PhaseAccess
	input.Routes[0].Protocol.InstanceKey.Owner.ID = "mutated-owner"

	if got, want := router.RouteIDs(), []string{"detached-mqtt"}; !slices.Equal(got, want) {
		t.Fatalf("RouteIDs() = %v, want %v", got, want)
	}
	entry := router.routes[0]
	if entry.route.ID != "detached-mqtt" || entry.route.Upstream.Nodes[0].Host != upstreamHost {
		t.Fatalf("compiled route changed with input mutation: %#v", entry.route)
	}
	if got := entry.route.Upstream.Checks["nested"].(map[string]any)["state"]; got != "original" {
		t.Fatalf("compiled nested upstream checks = %v, want original", got)
	}
	if entry.route.Plugins != nil {
		t.Fatalf("compiled route retained raw plugin documents: %#v", entry.route.Plugins)
	}

	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	served := make(chan struct {
		clientID string
		protocol string
		err      error
	}, 1)
	go func() {
		clientID, protocol, serveErr := entry.serve(context.Background(), client, "192.0.2.10:1234")
		served <- struct {
			clientID string
			protocol string
			err      error
		}{clientID: clientID, protocol: protocol, err: serveErr}
	}()
	if _, err := peer.Write(append(append([]byte(nil), packet...), payload...)); err != nil {
		t.Fatalf("write detached MQTT request: %v", err)
	}
	gotResponse := make([]byte, len(response))
	if _, err := io.ReadFull(peer, gotResponse); err != nil {
		t.Fatalf("read detached MQTT response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("detached MQTT response = %q, want %q", gotResponse, response)
	}
	_ = peer.Close()
	select {
	case got := <-served:
		if got.err != nil || got.clientID != "detached-client" || got.protocol != "mqtt" {
			t.Fatalf("detached MQTT result = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("detached MQTT route did not stop")
	}
}

func TestCompileRouterAcceptsOnlyCompleteEffectiveProtocolBinding(t *testing.T) {
	mqttConfig := map[string]any{"protocol_level": 4}
	baseRoute := resource.StreamRoute{
		ID:      "binding-route",
		Plugins: map[string]resource.PluginConfig{"mqtt-proxy": mqttConfig},
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1883, Weight: 1}},
		},
	}
	valid := preparedStreamMQTTBinding(
		t,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: baseRoute.ID},
		mqttConfig,
	)

	tests := []struct {
		name     string
		route    resource.StreamRoute
		binding  plugin.Binding
		wantPart string
	}{
		{name: "missing", route: baseRoute, wantPart: "requires one prepared mqtt-proxy binding"},
		{
			name:     "incomplete",
			route:    baseRoute,
			binding:  plugin.Binding{Plugin: valid.Plugin},
			wantPart: "descriptor factory",
		},
		{
			name:  "wrong factory",
			route: baseRoute,
			binding: func() plugin.Binding {
				binding := valid
				binding.Descriptor.Factory = "kafka-proxy"
				return binding
			}(),
			wantPart: "descriptor factory",
		},
		{
			name:  "wrong owner",
			route: baseRoute,
			binding: func() plugin.Binding {
				binding := valid
				binding.Provenance.ID = "another-route"
				return binding
			}(),
			wantPart: "provenance",
		},
		{
			name: "more than one effective plugin",
			route: func() resource.StreamRoute {
				route := baseRoute
				route.Plugins = map[string]resource.PluginConfig{
					"mqtt-proxy": mqttConfig,
					"other":      map[string]any{},
				}
				return route
			}(),
			binding:  valid,
			wantPart: "exactly one effective stream plugin",
		},
		{
			name: "binding on raw TCP",
			route: func() resource.StreamRoute {
				route := baseRoute
				route.Plugins = nil
				return route
			}(),
			binding:  valid,
			wantPart: "raw TCP route cannot carry a protocol binding",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileRouter(context.Background(), CompileInput{
				Revision: 42,
				Routes:   []PreparedRoute{{Route: test.route, Protocol: test.binding}},
			})
			if err == nil || !strings.Contains(err.Error(), test.wantPart) {
				t.Fatalf("CompileRouter() error = %v, want %q", err, test.wantPart)
			}
		})
	}

	serviceRoute := baseRoute
	serviceRoute.ServiceID = "service-1"
	serviceBinding := preparedStreamMQTTBinding(
		t,
		plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: serviceRoute.ServiceID},
		mqttConfig,
	)
	if _, err := CompileRouter(context.Background(), CompileInput{
		Revision: 43,
		Routes:   []PreparedRoute{{Route: serviceRoute, Protocol: serviceBinding}},
	}); err != nil {
		t.Fatalf("CompileRouter() service-owned binding error = %v", err)
	}

	rawRoute := baseRoute
	rawRoute.ID = "raw-route"
	rawRoute.Plugins = nil
	if _, err := CompileRouter(context.Background(), CompileInput{
		Revision: 44,
		Routes:   []PreparedRoute{{Route: rawRoute}},
	}); err != nil {
		t.Fatalf("CompileRouter() raw TCP error = %v", err)
	}
}

func TestCompileRouterRejectsZeroRevision(t *testing.T) {
	_, err := CompileRouter(context.Background(), CompileInput{
		Routes: []PreparedRoute{{Route: detachedRawRoute("missing-revision", 1883)}},
	})
	if err == nil || !strings.Contains(err.Error(), "revision is required") {
		t.Fatalf("CompileRouter() error = %v, want required revision", err)
	}
}

func TestCompiledRouterConcurrentRouteIDsAndServe(t *testing.T) {
	router, err := CompileRouter(context.Background(), CompileInput{
		Revision: 46,
		Routes:   []PreparedRoute{{Route: detachedRawRoute("frozen", 1883)}},
	})
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}

	const attempts = 32
	errs := make(chan error, attempts*2)
	var workers sync.WaitGroup
	for range attempts {
		workers.Go(func() {
			if got, want := router.RouteIDs(), []string{"frozen"}; !slices.Equal(got, want) {
				errs <- errors.New("concurrent RouteIDs changed the compiled route")
			}
		})
		workers.Go(func() {
			client, peer := net.Pipe()
			defer func() { _ = peer.Close() }()
			if serveErr := router.Serve(context.Background(), nil, client); !errors.Is(serveErr, ErrNoStreamRoute) {
				errs <- serveErr
			}
		})
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("concurrent compiled-router operation returned nil error")
		}
		t.Fatal(err)
	}
}

func preparedStreamMQTTBinding(
	t *testing.T,
	provenance plugin.ResourceProvenance,
	config map[string]any,
) plugin.Binding {
	t.Helper()
	p := &mqtt_proxy.Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("mqtt Init() error = %v", err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		t.Fatalf("parse mqtt config: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("mqtt PostInit() error = %v", err)
	}
	descriptor, err := plugin.ResolveDescriptorForFactory("mqtt-proxy", p)
	if err != nil {
		t.Fatalf("ResolveDescriptorForFactory() error = %v", err)
	}
	binding, err := plugin.BindAttemptResolvedPlugin(
		secret.AttemptID{1},
		descriptor,
		p,
		plugin.ScopeRoute,
		provenance,
		plugin.InstanceIdentityInput{PluginConfig: p.Config()},
	)
	if err != nil {
		t.Fatalf("BindAttemptResolvedPlugin() error = %v", err)
	}
	return binding
}

func detachedRawRoute(id string, port int) resource.StreamRoute {
	return resource.StreamRoute{
		ID:         id,
		ServerPort: port,
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}
}

func splitStreamTestAddress(t *testing.T, address string) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split stream test address: %v", err)
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		t.Fatalf("parse stream test port: %v", err)
	}
	return host, port
}
