package mqtt_proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type readDeadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *readDeadlineConn) SetReadDeadline(deadline time.Time) error {
	c.deadlines = append(c.deadlines, deadline)
	return c.Conn.SetReadDeadline(deadline)
}

type writeDeadlineConn struct {
	net.Conn
	deadlines []time.Time
	clearErr  error
	writes    bytes.Buffer
}

func (c *writeDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlines = append(c.deadlines, deadline)
	if deadline.IsZero() && c.clearErr != nil {
		return c.clearErr
	}
	return nil
}

func (c *writeDeadlineConn) Write(payload []byte) (int, error) {
	return c.writes.Write(payload)
}

type deadlineTrackingConn struct {
	net.Conn
	mu             sync.Mutex
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (c *deadlineTrackingConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadlines = append(c.readDeadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(deadline)
}

func (c *deadlineTrackingConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *deadlineTrackingConn) writeDeadlineCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writeDeadlines)
}

func (c *deadlineTrackingConn) lastWriteDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.writeDeadlines) == 0 {
		return time.Time{}
	}
	return c.writeDeadlines[len(c.writeDeadlines)-1]
}

func TestWriteStreamBytesClearsWriteDeadline(t *testing.T) {
	client, gateway := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = gateway.Close()
	})
	conn := &writeDeadlineConn{Conn: gateway}
	payload := []byte("mqtt-connect-replay")

	if err := writeStreamBytes(context.Background(), conn, payload); err != nil {
		t.Fatalf("writeStreamBytes() error = %v", err)
	}
	if len(conn.deadlines) != 2 {
		t.Fatalf("recorded write deadlines = %d, want 2", len(conn.deadlines))
	}
	if conn.deadlines[0].IsZero() {
		t.Fatal("replay write deadline was not set")
	}
	if !conn.deadlines[1].IsZero() {
		t.Fatalf("cleared write deadline = %s, want zero", conn.deadlines[1])
	}
	if !bytes.Equal(conn.writes.Bytes(), payload) {
		t.Fatalf("written payload = %q, want %q", conn.writes.Bytes(), payload)
	}
}

func TestWriteStreamBytesReturnsClearDeadlineError(t *testing.T) {
	client, gateway := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = gateway.Close()
	})
	clearErr := errors.New("clear write deadline failed")
	conn := &writeDeadlineConn{Conn: gateway, clearErr: clearErr}

	err := writeStreamBytes(context.Background(), conn, []byte("mqtt-connect-replay"))
	if !errors.Is(err, clearErr) {
		t.Fatalf("writeStreamBytes() error = %v, want clear deadline error", err)
	}
	if len(conn.deadlines) != 2 || !conn.deadlines[1].IsZero() {
		t.Fatalf("recorded write deadlines = %#v, want set then clear", conn.deadlines)
	}
}

func TestReadConnectClearsPrereadDeadline(t *testing.T) {
	client, gateway := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = gateway.Close()
	})
	conn := &readDeadlineConn{Conn: gateway}
	packet := mqttConnectPacket(4, 0x02, "deadline-client", nil, nil)
	go func() { _, _ = client.Write(packet) }()

	if _, _, err := readConnectFromStream(context.Background(), conn, "MQTT", 4); err != nil {
		t.Fatalf("readConnectFromStream() error = %v", err)
	}
	if len(conn.deadlines) != 2 {
		t.Fatalf("recorded read deadlines = %d, want 2", len(conn.deadlines))
	}
	if conn.deadlines[0].IsZero() {
		t.Fatal("preread deadline was not set")
	}
	if !conn.deadlines[1].IsZero() {
		t.Fatalf("cleared read deadline = %s, want zero", conn.deadlines[1])
	}
}

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

	_ = upstreamClient.Close()
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

func TestServeStreamPreservesHalfClose(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	broker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen broker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	gateway, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen gateway: %v", err)
	}
	t.Cleanup(func() { _ = gateway.Close() })

	packet := mqttConnectPacket(4, 0x02, "half-close-client", nil, nil)
	payload := []byte("publish-before-half-close")
	request := append(append([]byte(nil), packet...), payload...)
	response := []byte("delayed-broker-response")
	brokerDone := make(chan error, 1)
	go func() {
		conn, acceptErr := broker.Accept()
		if acceptErr != nil {
			brokerDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		got := make([]byte, len(request))
		if _, readErr := io.ReadFull(conn, got); readErr != nil {
			brokerDone <- readErr
			return
		}
		if !bytes.Equal(got, request) {
			brokerDone <- errors.New("broker received a different CONNECT/payload sequence")
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var probe [1]byte
		if _, readErr := conn.Read(probe[:]); !errors.Is(readErr, io.EOF) {
			brokerDone <- fmt.Errorf("broker read after client half-close = %v, want EOF", readErr)
			return
		}
		time.Sleep(100 * time.Millisecond)
		if _, writeErr := conn.Write(response); writeErr != nil {
			brokerDone <- writeErr
			return
		}
		if closeErr, ok := conn.(interface{ CloseWrite() error }); ok {
			if closeErr := closeErr.CloseWrite(); closeErr != nil {
				brokerDone <- closeErr
				return
			}
		}
		brokerDone <- nil
	}()

	client, err := net.Dial("tcp", gateway.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverConn, err := gateway.Accept()
	if err != nil {
		t.Fatalf("accept gateway: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() {
		_, serveErr := plugin.ServeStreamWithIdle(
			context.Background(),
			serverConn,
			"198.51.100.10:1883",
			func(context.Context, string) (net.Conn, error) {
				return net.Dial("tcp", broker.Addr().String())
			},
			time.Second,
		)
		serveDone <- serveErr
	}()

	if _, err := client.Write(request); err != nil {
		t.Fatalf("write CONNECT/payload: %v", err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close client write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	gotResponse := make([]byte, len(response))
	if _, err := io.ReadFull(client, gotResponse); err != nil {
		t.Fatalf("read delayed broker response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("response = %q, want %q", gotResponse, response)
	}

	if err := <-brokerDone; err != nil {
		t.Fatalf("broker error: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ServeStreamWithIdle() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeStreamWithIdle() did not stop after both half-closes")
	}
}

func TestServeStreamWithIdlePropagatesIdleDeadline(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	client, gateway := net.Pipe()
	upstreamClient, upstreamGateway := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = gateway.Close()
		_ = upstreamClient.Close()
		_ = upstreamGateway.Close()
	})
	trackedGateway := &deadlineTrackingConn{Conn: gateway}
	trackedUpstream := &deadlineTrackingConn{Conn: upstreamGateway}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	const idle = 2 * time.Second
	go func() {
		_, err := plugin.ServeStreamWithIdle(
			ctx,
			trackedGateway,
			"198.51.100.20:1883",
			func(context.Context, string) (net.Conn, error) {
				return trackedUpstream, nil
			},
			idle,
		)
		result <- err
	}()

	packet := mqttConnectPacket(4, 0x02, "idle-client", nil, nil)
	go func() { _, _ = client.Write(packet) }()
	replayed := make([]byte, len(packet))
	if _, err := io.ReadFull(upstreamClient, replayed); err != nil {
		t.Fatalf("read replayed CONNECT packet: %v", err)
	}
	postReplay := []byte("post-replay")
	go func() { _, _ = client.Write(postReplay) }()
	forwarded := make([]byte, len(postReplay))
	if _, err := io.ReadFull(upstreamClient, forwarded); err != nil {
		t.Fatalf("read post-replay payload: %v", err)
	}
	if !bytes.Equal(forwarded, postReplay) {
		t.Fatalf("forwarded post-replay payload = %q, want %q", forwarded, postReplay)
	}
	deadlineWait := time.Now().Add(time.Second)
	for trackedUpstream.writeDeadlineCount() < 3 && time.Now().Before(deadlineWait) {
		time.Sleep(time.Millisecond)
	}
	if trackedUpstream.writeDeadlineCount() < 3 {
		t.Fatalf(
			"upstream write deadlines = %d, want replay set/clear plus idle deadline",
			trackedUpstream.writeDeadlineCount(),
		)
	}
	deadline := trackedUpstream.lastWriteDeadline()
	remaining := time.Until(deadline)
	if remaining < idle/2 || remaining > idle+100*time.Millisecond {
		t.Fatalf("upstream idle write deadline remaining = %s, want about %s", remaining, idle)
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ServeStreamWithIdle() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeStreamWithIdle() did not stop after cancellation")
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
