package mqtt_proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestServeStreamReplaysPrereadAndExposesClientID(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	client, gateway := net.Pipe()
	upstreamClient, upstreamGateway := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = gateway.Close()
		_ = upstreamClient.Close()
		_ = upstreamGateway.Close()
	})

	dialed := make(chan string, 1)
	result := make(chan error, 1)
	go func() {
		_, err := plugin.ServeStream(
			context.Background(),
			gateway,
			"192.0.2.10:1883",
			func(_ context.Context, clientID string) (net.Conn, error) {
				dialed <- clientID
				return upstreamGateway, nil
			},
		)
		result <- err
	}()

	packet := mqttConnectPacket(4, 0x02, "client-1", nil, nil)
	extra := []byte("publish-before")
	go func() {
		_, _ = client.Write(append(append([]byte(nil), packet...), extra...))
	}()

	select {
	case got := <-dialed:
		if got != "client-1" {
			t.Fatalf("dial client ID = %q, want client-1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream dial")
	}

	replayed := make([]byte, len(packet)+len(extra))
	if _, err := io.ReadFull(upstreamClient, replayed); err != nil {
		t.Fatalf("read replayed CONNECT bytes: %v", err)
	}
	if !bytes.Equal(replayed, append(packet, extra...)) {
		t.Fatalf("replayed bytes = %x, want %x", replayed, append(packet, extra...))
	}

	serverPayload := []byte("broker-response")
	go func() { _, _ = upstreamClient.Write(serverPayload) }()
	received := make([]byte, len(serverPayload))
	if _, err := io.ReadFull(client, received); err != nil {
		t.Fatalf("read broker response: %v", err)
	}
	if !bytes.Equal(received, serverPayload) {
		t.Fatalf("client payload = %q, want %q", received, serverPayload)
	}

	_ = client.Close()
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("ServeStream() error = %v, want clean disconnect", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeStream() did not stop after client disconnect")
	}
}

func TestServeStreamRejectsMalformedConnectBeforeDial(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	client, gateway := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = gateway.Close()
	})
	dialCalled := false
	result := make(chan error, 1)
	go func() {
		_, err := plugin.ServeStream(
			context.Background(),
			gateway,
			"192.0.2.10:1883",
			func(context.Context, string) (net.Conn, error) {
				dialCalled = true
				return nil, nil
			},
		)
		result <- err
	}()

	_, _ = client.Write([]byte{0x20, 0x00})
	select {
	case err := <-result:
		if !errors.Is(err, ErrMalformedConnect) {
			t.Fatalf("ServeStream() error = %v, want malformed CONNECT", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeStream() did not reject malformed CONNECT")
	}
	if dialCalled {
		t.Fatal("stream dialer was called for malformed CONNECT")
	}
}

func TestServeStreamHonorsCancellation(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	client, gateway := net.Pipe()
	upstreamClient, upstreamGateway := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = gateway.Close()
		_ = upstreamClient.Close()
		_ = upstreamGateway.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := plugin.ServeStream(
			ctx,
			gateway,
			"198.51.100.8:1883",
			func(context.Context, string) (net.Conn, error) {
				return upstreamGateway, nil
			},
		)
		result <- err
	}()

	packet := mqttConnectPacket(4, 0x02, "cancel-client", nil, nil)
	go func() { _, _ = client.Write(packet) }()
	replayed := make([]byte, len(packet))
	if _, err := io.ReadFull(upstreamClient, replayed); err != nil {
		t.Fatalf("read replayed packet: %v", err)
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ServeStream() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeStream() did not stop after cancellation")
	}
}

func TestServeListenerPublishesStreamInfo(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stream route: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen broker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })

	packet := mqttConnectPacket(4, 0x02, "listener-client", nil, nil)
	extra := []byte("publish")
	response := []byte("connack")
	brokerDone := make(chan error, 1)
	go func() {
		conn, acceptErr := broker.Accept()
		if acceptErr != nil {
			brokerDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		got := make([]byte, len(packet)+len(extra))
		if _, readErr := io.ReadFull(conn, got); readErr != nil {
			brokerDone <- readErr
			return
		}
		if !bytes.Equal(got, append(packet, extra...)) {
			brokerDone <- errors.New("broker received a different CONNECT/payload sequence")
			return
		}
		_, writeErr := conn.Write(response)
		brokerDone <- writeErr
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listenerDone := make(chan error, 1)
	events := make(chan struct {
		info StreamInfo
		err  error
	}, 1)
	go func() {
		listenerDone <- plugin.ServeListener(ctx, listener, func(_ context.Context, clientID string) (net.Conn, error) {
			if clientID != "listener-client" {
				return nil, errors.New("unexpected client ID")
			}
			return net.Dial("tcp", broker.Addr().String())
		}, func(info StreamInfo, streamErr error) {
			events <- struct {
				info StreamInfo
				err  error
			}{info: info, err: streamErr}
		})
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial stream route: %v", err)
	}
	go func() { _, _ = client.Write(append(append([]byte(nil), packet...), extra...)) }()
	received := make([]byte, len(response))
	if _, err := io.ReadFull(client, received); err != nil {
		t.Fatalf("read broker response: %v", err)
	}
	if !bytes.Equal(received, response) {
		t.Fatalf("client response = %q, want %q", received, response)
	}
	_ = client.Close()

	select {
	case event := <-events:
		if event.err != nil {
			t.Fatalf("stream result error = %v", event.err)
		}
		if event.info.ClientID != "listener-client" || event.info.Peer == "" {
			t.Fatalf("stream info = %#v, want client ID and peer", event.info)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream info callback")
	}
	if err := <-brokerDone; err != nil {
		t.Fatalf("broker error: %v", err)
	}

	cancel()
	select {
	case err := <-listenerDone:
		if err != nil {
			t.Fatalf("ServeListener() error = %v, want clean cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeListener() did not stop after cancellation")
	}
}

func newMQTTStreamPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()
	plugin := &Plugin{config: config}
	if err := plugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return plugin
}
