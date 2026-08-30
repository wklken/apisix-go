package pluginintegration

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDifferentialMQTTDriverProjectsStreamPortAndPreservesHTTPRuntime(t *testing.T) {
	spec := differentialCasesForPlugin("mqtt-proxy")[0]
	projected, err := projectDifferentialConfig(spec.Config, "127.0.0.1:39001")
	if err != nil {
		t.Fatal(err)
	}
	if err := projectDifferentialMQTTListenPort(projected, 31985); err != nil {
		t.Fatal(err)
	}
	projectedSpec := spec
	projectedSpec.Config = projected
	if endpoint, err := differentialMQTTListenEndpoint(projectedSpec); err != nil || endpoint != "127.0.0.1:31985" {
		t.Fatalf("MQTT endpoint = %q, %v", endpoint, err)
	}
	if _, err := differentialMQTTListenEndpoint(spec); err == nil {
		t.Fatal("unprojected MQTT case unexpectedly exposed a listener endpoint")
	}

	base, err := renderDifferentialCandidateRuntimeWithOverlay(
		19080, 19081, 19082, t.TempDir(), []string{"mqtt-proxy"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderDifferentialMQTTRuntime(base, "127.0.0.1:31985")
	if err != nil {
		t.Fatal(err)
	}
	var runtime map[string]any
	if err := yaml.Unmarshal(rendered, &runtime); err != nil {
		t.Fatal(err)
	}
	apisix := runtime["apisix"].(map[string]any)
	if apisix["proxy_mode"] != "http&stream" || apisix["node_listen"] == nil ||
		apisix["status"] == nil || apisix["control"] == nil {
		t.Fatalf("MQTT runtime lost HTTP harness fields: %#v", apisix)
	}
	plugins := runtime["stream_plugins"].([]any)
	if len(plugins) != 1 || plugins[0] != "mqtt-proxy" {
		t.Fatalf("stream_plugins = %#v", plugins)
	}
}

func TestDifferentialMQTTCandidateDriverRejectsInvalidThenForwardsCONNECT(t *testing.T) {
	spec := differentialCasesForPlugin("mqtt-proxy")[0]
	fixture, err := startDifferentialMQTTFixture(spec.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()
	if err := probeDifferentialMQTTFixture(fixture); err != nil {
		t.Fatal(err)
	}
	fixtureEndpoint := net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port()))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	gatewayErrors := make(chan error, 1)
	go func() {
		invalid, acceptErr := listener.Accept()
		if acceptErr != nil {
			gatewayErrors <- acceptErr
			return
		}
		invalidPayload := make([]byte, len(differentialMQTTProxyInvalidPacket))
		_, readErr := io.ReadFull(invalid, invalidPayload)
		_ = invalid.Close()
		if readErr != nil || string(invalidPayload) != differentialMQTTProxyInvalidPacket {
			gatewayErrors <- readErr
			return
		}

		client, acceptErr := listener.Accept()
		if acceptErr != nil {
			gatewayErrors <- acceptErr
			return
		}
		defer func() { _ = client.Close() }()
		packet, readErr := readDifferentialMQTTCONNECTFrame(bufio.NewReader(client))
		if readErr != nil {
			gatewayErrors <- readErr
			return
		}
		upstream, dialErr := net.Dial("tcp", fixtureEndpoint)
		if dialErr != nil {
			gatewayErrors <- dialErr
			return
		}
		if _, writeErr := upstream.Write(packet); writeErr != nil {
			_ = upstream.Close()
			gatewayErrors <- writeErr
			return
		}
		response, readErr := io.ReadAll(upstream)
		_ = upstream.Close()
		if readErr != nil {
			gatewayErrors <- readErr
			return
		}
		_, writeErr := client.Write(response)
		gatewayErrors <- writeErr
	}()

	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := projectDifferentialConfig(spec.Config, fixtureEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := projectDifferentialMQTTListenPort(projected, port); err != nil {
		t.Fatal(err)
	}
	projectedSpec := spec
	projectedSpec.Config = projected
	observation, err := observeDifferentialMQTTProxyCandidate(
		fixture, projectedSpec, fixtureEndpoint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-gatewayErrors; err != nil {
		t.Fatal(err)
	}
	if len(observation.Steps) != 2 || observation.Steps[0].Body != "" ||
		observation.Steps[1].Body != differentialMQTTProxyFixtureResponseBody ||
		!observation.Upstream.Received {
		t.Fatalf("MQTT observation = %#v", observation)
	}
}

func TestDifferentialMQTTExchangeTreatsPeerResetAsRejectedWithoutResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		tcpConnection, ok := connection.(*net.TCPConn)
		if !ok {
			_ = connection.Close()
			serverDone <- io.ErrUnexpectedEOF
			return
		}
		payload := make([]byte, len(differentialMQTTProxyInvalidPacket))
		_, readErr := io.ReadFull(tcpConnection, payload)
		if readErr == nil {
			readErr = tcpConnection.SetLinger(0)
		}
		_ = tcpConnection.Close()
		serverDone <- readErr
	}()

	response, err := executeDifferentialMQTTExchange(
		listener.Addr().String(), []byte(differentialMQTTProxyInvalidPacket), true,
	)
	if err != nil {
		t.Fatalf("peer reset should represent rejected MQTT input: %v", err)
	}
	if len(response) != 0 {
		t.Fatalf("peer reset response = %x, want empty", response)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDifferentialMQTTExchangeDoesNotMaskResetWhenResponseIsRequired(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		tcpConnection, ok := connection.(*net.TCPConn)
		if !ok {
			_ = connection.Close()
			serverDone <- io.ErrUnexpectedEOF
			return
		}
		payload := make([]byte, len(differentialMQTTProxyInvalidPacket))
		_, readErr := io.ReadFull(tcpConnection, payload)
		if readErr == nil {
			readErr = tcpConnection.SetLinger(0)
		}
		_ = tcpConnection.Close()
		serverDone <- readErr
	}()

	_, err = executeDifferentialMQTTExchange(
		listener.Addr().String(), []byte(differentialMQTTProxyInvalidPacket), false,
	)
	if err == nil {
		t.Fatal("required MQTT response unexpectedly treated peer reset as success")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
