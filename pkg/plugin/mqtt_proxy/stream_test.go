package mqtt_proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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

var mqttTask5RawPanic = &struct{ marker string }{marker: "mqtt-task5-raw-panic"}

var (
	mqttTask5StopBothPanic = &struct{ marker string }{marker: "mqtt-task5-stop-both-panic"}
	mqttTask5UpstreamPanic = &struct{ marker string }{marker: "mqtt-task5-upstream-panic"}
	mqttTask5AcceptedPanic = &struct{ marker string }{marker: "mqtt-task5-accepted-panic"}
)

func TestCloseStreamOnContextDoneJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	conn := &mqttTask5BlockingCloseConn{
		closeStarted: make(chan struct{}),
		closeRelease: release,
	}
	stop := closeStreamOnContextDone(ctx, conn)
	cancel()

	select {
	case <-conn.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not start stream close")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stream stopper returned before connection close completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stream stopper did not join connection close")
	}

	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			stop()
		})
	}
	callers.Wait()

	panicCtx, panicCancel := context.WithCancel(context.Background())
	panicConn := &mqttTask5PanicCloseConn{
		panicValue:   mqttTask5RawPanic,
		closeStarted: make(chan struct{}),
	}
	other := &mqttTask5CountingConn{}
	panicStop := closeStreamOnContextDone(panicCtx, panicConn, other)
	panicCancel()
	select {
	case <-panicConn.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("panic close did not start")
	}
	panicResults := make(chan any, 8)
	for range 8 {
		go func() {
			defer func() { panicResults <- recover() }()
			panicStop()
		}()
	}
	for range 8 {
		if recovered := <-panicResults; recovered != mqttTask5RawPanic {
			t.Fatalf("stopper panic = %#v, want exact %#v", recovered, mqttTask5RawPanic)
		}
	}
	if got := other.closeCount(); got != 1 {
		t.Fatalf("close callbacks after panic = %d, want 1", got)
	}

	normal := &mqttTask5CountingConn{}
	normalStop := closeStreamOnContextDone(context.Background(), normal)
	normalStop()
	normalStop()
	if got := normal.closeCount(); got != 0 {
		t.Fatalf("normal stop closed %d connections, want 0", got)
	}
}

func TestCloseListenerOnContextDoneJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	listener := &mqttTask5BlockingListener{
		closeStarted: make(chan struct{}),
		closeRelease: release,
	}
	stop := closeListenerOnContextDone(ctx, listener)
	cancel()

	select {
	case <-listener.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not start listener close")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("listener stopper returned before listener close completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("listener stopper did not join listener close")
	}

	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			stop()
		})
	}
	callers.Wait()

	normal := &mqttTask5CountingListener{}
	normalStop := closeListenerOnContextDone(context.Background(), normal)
	normalStop()
	normalStop()
	if got := normal.closeCount(); got != 0 {
		t.Fatalf("normal stop closed listener %d times, want 0", got)
	}
}

func TestServeListenerWaitsForAcceptedStreams(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	accepted := newMQTTTask5ReadUntilCloseConn()
	listener := newMQTTTask5SequenceListener(accepted)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	dialCalled := make(chan struct{})
	listenerDone := make(chan error, 1)
	go func() {
		listenerDone <- plugin.ServeListener(
			ctx,
			listener,
			func(context.Context, string) (net.Conn, error) {
				close(dialCalled)
				return nil, nil
			},
			func(StreamInfo, error) {
				close(callbackStarted)
				<-callbackRelease
			},
		)
	}()
	select {
	case <-dialCalled:
		t.Fatal("dialer called before accepted stream cancellation")
	default:
	}

	select {
	case <-accepted.readStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted stream did not start")
	}
	cancel()
	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted stream result callback did not start")
	}
	select {
	case err := <-listenerDone:
		t.Fatalf("ServeListener returned before accepted callback joined: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(callbackRelease)
	select {
	case err := <-listenerDone:
		if err != nil {
			t.Fatalf("ServeListener() error = %v, want nil on cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeListener did not return after accepted callback completed")
	}
}

func TestServeListenerRawPanicReturnsFromOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeListenerRawPanicHelper$")
	cmd.Env = append(os.Environ(), "APISIX_GO_MQTT_PANIC_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper exited before owner recovery (ctx err %v): %v\n%s", ctx.Err(), err, out)
	}
	if !bytes.Contains(out, []byte("mqtt-owner-recovered")) {
		t.Fatalf("missing owner recovery marker: %s", out)
	}
}

func TestServeListenerTaskPanicWinsAfterAllConnectionCleanup(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	ctx := newMQTTTask5StopRaceContext()
	client := newMQTTTask5ConnectThenPanicConn(
		mqttConnectPacket(4, 0x02, "panic-precedence-client", nil, nil),
		ctx.markCanceled,
	)
	upstream := newMQTTTask5PanicUpstreamConn()
	listener := newMQTTTask5AcceptErrorListener(client, errors.New("listener drain"))

	type outcome struct {
		err        error
		panicValue any
	}
	done := make(chan outcome, 1)
	go func() {
		result := outcome{}
		defer func() {
			result.panicValue = recover()
			done <- result
		}()
		result.err = plugin.ServeListener(
			ctx,
			listener,
			func(context.Context, string) (net.Conn, error) { return upstream, nil },
			nil,
		)
	}()

	select {
	case result := <-done:
		if result.err != nil || result.panicValue != mqttTask5RawPanic {
			t.Fatalf(
				"ServeListener() outcome = (%v, %#v), want exact task panic %#v",
				result.err,
				result.panicValue,
				mqttTask5RawPanic,
			)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeListener did not finish panic cleanup")
	}
	if got := client.closeCount(); got < 3 {
		t.Fatalf("client cleanup attempts = %d, want stop-both plus bridge and accepted cleanup", got)
	}
	if got := upstream.closeCount(); got < 3 {
		t.Fatalf("upstream cleanup attempts = %d, want bridge, stop-both, and owner cleanup", got)
	}
}

func TestServeStreamInstallsUpstreamCleanupBeforeStoppingPrereadWatcher(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	ctx, cancel := context.WithCancel(context.Background())
	client := newMQTTTask5CancelPanicConn(
		mqttConnectPacket(4, 0x02, "preread-stop-panic-client", nil, nil),
	)
	upstream := &mqttTask5CountingConn{}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = plugin.ServeStreamWithIdle(
			ctx,
			client,
			"198.51.100.5:1883",
			func(context.Context, string) (net.Conn, error) {
				cancel()
				select {
				case <-client.closeStarted:
				case <-time.After(time.Second):
					t.Fatal("preread watcher did not start client cleanup")
				}
				return upstream, nil
			},
			time.Second,
		)
	}()
	if recovered != mqttTask5StopBothPanic {
		t.Fatalf(
			"ServeStreamWithIdle() panic = %#v, want exact preread-stop panic %#v",
			recovered,
			mqttTask5StopBothPanic,
		)
	}
	if got := upstream.closeCount(); got != 1 {
		t.Fatalf("upstream cleanup attempts = %d, want 1 after preread stopper panic", got)
	}
}

func TestServeListenerPreservesAcceptErrorWhenContextCancelsDuringDrain(t *testing.T) {
	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	accepted := newMQTTTask5ReadUntilCloseConn()
	acceptErr := errors.New("mqtt accept failed")
	listener := newMQTTTask5AcceptErrorListener(accepted, acceptErr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- plugin.ServeListener(ctx, listener, func(context.Context, string) (net.Conn, error) {
			return nil, errors.New("dialer must not run")
		}, nil)
	}()
	select {
	case <-accepted.readStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted stream did not start before accept failure")
	}
	select {
	case <-listener.errorReturned:
	case <-time.After(time.Second):
		t.Fatal("listener did not return the ordinary accept error")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, acceptErr) {
			t.Fatalf("ServeListener() error = %v, want original accept error %v", err, acceptErr)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeListener did not finish draining after accept failure")
	}
}

func TestContextStopperRechecksCancellationWhenStopBranchWins(t *testing.T) {
	ctx := newMQTTTask5StopRaceContext()
	conn := &mqttTask5CountingConn{}
	stop := closeStreamOnContextDone(ctx, conn)
	select {
	case <-ctx.watcherReady:
	case <-time.After(time.Second):
		t.Fatal("context watcher did not reach its cancellation select")
	}

	ctx.markCanceled()
	stop()
	if got := conn.closeCount(); got != 1 {
		t.Fatalf("connection cleanup attempts = %d, want 1 when cancellation races with stop", got)
	}
}

func TestServeListenerRawPanicHelper(t *testing.T) {
	if os.Getenv("APISIX_GO_MQTT_PANIC_HELPER") != "1" {
		return
	}

	plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
	conn := &mqttTask5PanicReadConn{readReady: make(chan struct{})}
	listener := newMQTTTask5SequenceListener(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-conn.readReady
		cancel()
	}()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		if err := plugin.ServeListener(ctx, listener, func(context.Context, string) (net.Conn, error) {
			return nil, errors.New("dialer must not run after CONNECT panic")
		}, nil); err != nil {
			fmt.Fprintf(os.Stderr, "ServeListener() error = %v\n", err)
			os.Exit(1)
		}
	}()
	if recovered != mqttTask5RawPanic {
		fmt.Fprintf(os.Stderr, "recovered panic = %#v, want %#v\n", recovered, mqttTask5RawPanic)
		os.Exit(1)
	}
	fmt.Println("mqtt-owner-recovered")
}

func TestServeStreamCancellationJoinsOwnedWork(t *testing.T) {
	t.Run("preread", func(t *testing.T) {
		plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
		client := newMQTTTask5ReadUntilCloseConn()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := plugin.ServeStreamWithIdle(
				ctx,
				client,
				"198.51.100.1:1883",
				func(context.Context, string) (net.Conn, error) {
					return nil, errors.New("dialer must not run during preread")
				},
				time.Second,
			)
			result <- err
		}()
		select {
		case <-client.readStarted:
		case <-time.After(time.Second):
			t.Fatal("preread did not start")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ServeStreamWithIdle() error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("ServeStreamWithIdle did not join preread cancellation")
		}
		if !client.isClosed() {
			t.Fatal("preread client was not closed before return")
		}
	})

	t.Run("dial", func(t *testing.T) {
		plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
		clientPeer, client := net.Pipe()
		defer func() { _ = clientPeer.Close() }()
		defer func() { _ = client.Close() }()
		trackedClient := &mqttTask5TrackedConn{Conn: client, closed: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		dialStarted := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			_, err := plugin.ServeStreamWithIdle(
				ctx,
				trackedClient,
				"198.51.100.2:1883",
				func(dialCtx context.Context, _ string) (net.Conn, error) {
					close(dialStarted)
					<-dialCtx.Done()
					return nil, dialCtx.Err()
				},
				time.Second,
			)
			result <- err
		}()
		packet := mqttConnectPacket(4, 0x02, "dial-cancel-client", nil, nil)
		if _, err := clientPeer.Write(packet); err != nil {
			t.Fatalf("write CONNECT: %v", err)
		}
		select {
		case <-dialStarted:
		case <-time.After(time.Second):
			t.Fatal("upstream dial did not start")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ServeStreamWithIdle() error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("ServeStreamWithIdle did not join dial cancellation")
		}
		if !trackedClient.isClosed() {
			t.Fatal("dial-phase client was not closed before return")
		}
	})

	t.Run("connect-replay", func(t *testing.T) {
		plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
		clientPeer, client := net.Pipe()
		defer func() { _ = clientPeer.Close() }()
		defer func() { _ = client.Close() }()
		upstream := newMQTTTask5WriteUntilCloseConn()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := plugin.ServeStreamWithIdle(
				ctx,
				client,
				"198.51.100.3:1883",
				func(context.Context, string) (net.Conn, error) {
					return upstream, nil
				},
				time.Second,
			)
			result <- err
		}()
		packet := mqttConnectPacket(4, 0x02, "replay-cancel-client", nil, nil)
		if _, err := clientPeer.Write(packet); err != nil {
			t.Fatalf("write CONNECT: %v", err)
		}
		select {
		case <-upstream.writeStarted:
		case <-time.After(time.Second):
			t.Fatal("CONNECT replay did not start")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ServeStreamWithIdle() error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("ServeStreamWithIdle did not join replay cancellation")
		}
		if !upstream.isClosed() {
			t.Fatal("replay upstream was not closed before return")
		}
	})

	t.Run("bridge-copy", func(t *testing.T) {
		plugin := newMQTTStreamPlugin(t, Config{ProtocolLevel: 4})
		clientPeer, clientBase := net.Pipe()
		upstreamPeer, upstreamBase := net.Pipe()
		defer func() { _ = clientPeer.Close() }()
		defer func() { _ = clientBase.Close() }()
		defer func() { _ = upstreamPeer.Close() }()
		defer func() { _ = upstreamBase.Close() }()
		client := &mqttTask5TrackedConn{Conn: clientBase, readStarted: make(chan struct{}), closed: make(chan struct{})}
		upstream := &mqttTask5TrackedConn{
			Conn:        upstreamBase,
			readStarted: make(chan struct{}),
			closed:      make(chan struct{}),
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := plugin.ServeStreamWithIdle(
				ctx,
				client,
				"198.51.100.4:1883",
				func(context.Context, string) (net.Conn, error) {
					return upstream, nil
				},
				time.Second,
			)
			result <- err
		}()
		packet := mqttConnectPacket(4, 0x02, "bridge-cancel-client", nil, nil)
		go func() { _, _ = clientPeer.Write(packet) }()
		replayed := make([]byte, len(packet))
		if _, err := io.ReadFull(upstreamPeer, replayed); err != nil {
			t.Fatalf("read replayed CONNECT: %v", err)
		}
		select {
		case <-client.readStarted:
		case <-time.After(time.Second):
			t.Fatal("client bridge read did not start")
		}
		select {
		case <-upstream.readStarted:
		case <-time.After(time.Second):
			t.Fatal("upstream bridge read did not start")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ServeStreamWithIdle() error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("ServeStreamWithIdle did not join bridge cancellation")
		}
		if !client.isClosed() || !upstream.isClosed() {
			t.Fatal("bridge endpoints were not closed before return")
		}
	})
}

type mqttTask5BlockingCloseConn struct {
	net.Conn
	closeStarted chan struct{}
	closeRelease <-chan struct{}
	closeOnce    sync.Once
}

func (c *mqttTask5BlockingCloseConn) Close() error {
	c.closeOnce.Do(func() { close(c.closeStarted) })
	if c.closeRelease != nil {
		<-c.closeRelease
	}
	return nil
}

type mqttTask5CountingConn struct {
	net.Conn
	mu     sync.Mutex
	closes int
}

type mqttTask5PanicCloseConn struct {
	net.Conn
	panicValue   any
	closeStarted chan struct{}
	closeOnce    sync.Once
}

func (c *mqttTask5PanicCloseConn) Close() error {
	c.closeOnce.Do(func() { close(c.closeStarted) })
	panic(c.panicValue)
}

func (c *mqttTask5CountingConn) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

func (c *mqttTask5CountingConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

type mqttTask5BlockingListener struct {
	closeStarted chan struct{}
	closeRelease <-chan struct{}
	closeOnce    sync.Once
}

func (l *mqttTask5BlockingListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }

func (l *mqttTask5BlockingListener) Close() error {
	l.closeOnce.Do(func() { close(l.closeStarted) })
	if l.closeRelease != nil {
		<-l.closeRelease
	}
	return nil
}

func (l *mqttTask5BlockingListener) Addr() net.Addr { return mqttTask5Addr("mqtt-task5") }

type mqttTask5CountingListener struct {
	mu     sync.Mutex
	closes int
}

func (l *mqttTask5CountingListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }

func (l *mqttTask5CountingListener) Close() error {
	l.mu.Lock()
	l.closes++
	l.mu.Unlock()
	return nil
}

func (l *mqttTask5CountingListener) Addr() net.Addr { return mqttTask5Addr("mqtt-task5") }

func (l *mqttTask5CountingListener) closeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closes
}

type mqttTask5Addr string

func (a mqttTask5Addr) Network() string { return "tcp" }
func (a mqttTask5Addr) String() string  { return string(a) }

type mqttTask5SequenceListener struct {
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func newMQTTTask5SequenceListener(conn net.Conn) *mqttTask5SequenceListener {
	return &mqttTask5SequenceListener{conn: conn, closed: make(chan struct{})}
}

func (l *mqttTask5SequenceListener) Accept() (net.Conn, error) {
	if l.conn != nil {
		conn := l.conn
		l.conn = nil
		return conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *mqttTask5SequenceListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *mqttTask5SequenceListener) Addr() net.Addr { return mqttTask5Addr("mqtt-task5") }

type mqttTask5ReadUntilCloseConn struct {
	net.Conn
	readStarted chan struct{}
	readOnce    sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
}

func newMQTTTask5ReadUntilCloseConn() *mqttTask5ReadUntilCloseConn {
	return &mqttTask5ReadUntilCloseConn{
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (c *mqttTask5ReadUntilCloseConn) Read([]byte) (int, error) {
	c.readOnce.Do(func() { close(c.readStarted) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *mqttTask5ReadUntilCloseConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *mqttTask5ReadUntilCloseConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mqttTask5ReadUntilCloseConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mqttTask5ReadUntilCloseConn) LocalAddr() net.Addr              { return mqttTask5Addr("local") }
func (c *mqttTask5ReadUntilCloseConn) RemoteAddr() net.Addr             { return mqttTask5Addr("remote") }

func (c *mqttTask5ReadUntilCloseConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

type mqttTask5PanicReadConn struct {
	net.Conn
	readReady chan struct{}
	readOnce  sync.Once
}

func (c *mqttTask5PanicReadConn) Read([]byte) (int, error) {
	c.readOnce.Do(func() { close(c.readReady) })
	panic(mqttTask5RawPanic)
}

func (c *mqttTask5PanicReadConn) Close() error                     { return nil }
func (c *mqttTask5PanicReadConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mqttTask5PanicReadConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mqttTask5PanicReadConn) LocalAddr() net.Addr              { return mqttTask5Addr("local") }
func (c *mqttTask5PanicReadConn) RemoteAddr() net.Addr             { return mqttTask5Addr("remote") }

type mqttTask5TrackedConn struct {
	net.Conn
	readStarted chan struct{}
	readOnce    sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
}

func (c *mqttTask5TrackedConn) Read(p []byte) (int, error) {
	if c.readStarted != nil {
		c.readOnce.Do(func() { close(c.readStarted) })
	}
	return c.Conn.Read(p)
}

func (c *mqttTask5TrackedConn) Close() error {
	if c.closed != nil {
		c.closeOnce.Do(func() { close(c.closed) })
	}
	return c.Conn.Close()
}

func (c *mqttTask5TrackedConn) isClosed() bool {
	if c.closed == nil {
		return false
	}
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

type mqttTask5WriteUntilCloseConn struct {
	net.Conn
	writeStarted chan struct{}
	closed       chan struct{}
	closeOnce    sync.Once
}

func newMQTTTask5WriteUntilCloseConn() *mqttTask5WriteUntilCloseConn {
	return &mqttTask5WriteUntilCloseConn{
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *mqttTask5WriteUntilCloseConn) Write([]byte) (int, error) {
	select {
	case <-c.writeStarted:
	default:
		close(c.writeStarted)
	}
	<-c.closed
	return 0, net.ErrClosed
}

func (c *mqttTask5WriteUntilCloseConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *mqttTask5WriteUntilCloseConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mqttTask5WriteUntilCloseConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mqttTask5WriteUntilCloseConn) LocalAddr() net.Addr              { return mqttTask5Addr("local") }
func (c *mqttTask5WriteUntilCloseConn) RemoteAddr() net.Addr             { return mqttTask5Addr("remote") }

func (c *mqttTask5WriteUntilCloseConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

type mqttTask5ConnectThenPanicConn struct {
	net.Conn
	mu          sync.Mutex
	payload     []byte
	offset      int
	cancel      func()
	closes      int
	readPanic   sync.Once
	readStarted chan struct{}
}

func newMQTTTask5ConnectThenPanicConn(payload []byte, cancel func()) *mqttTask5ConnectThenPanicConn {
	return &mqttTask5ConnectThenPanicConn{
		payload:     append([]byte(nil), payload...),
		cancel:      cancel,
		readStarted: make(chan struct{}),
	}
}

func (c *mqttTask5ConnectThenPanicConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.offset < len(c.payload) {
		n := copy(p, c.payload[c.offset:])
		c.offset += n
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	c.readPanic.Do(func() {
		close(c.readStarted)
		c.cancel()
	})
	panic(mqttTask5RawPanic)
}

func (c *mqttTask5ConnectThenPanicConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *mqttTask5ConnectThenPanicConn) Close() error {
	c.mu.Lock()
	c.closes++
	closes := c.closes
	c.mu.Unlock()
	if closes >= 3 {
		panic(mqttTask5AcceptedPanic)
	}
	panic(mqttTask5StopBothPanic)
}

func (c *mqttTask5ConnectThenPanicConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *mqttTask5ConnectThenPanicConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mqttTask5ConnectThenPanicConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mqttTask5ConnectThenPanicConn) LocalAddr() net.Addr              { return mqttTask5Addr("local") }
func (c *mqttTask5ConnectThenPanicConn) RemoteAddr() net.Addr             { return mqttTask5Addr("remote") }

type mqttTask5PanicUpstreamConn struct {
	net.Conn
	mu        sync.Mutex
	closes    int
	closed    chan struct{}
	closeOnce sync.Once
}

func newMQTTTask5PanicUpstreamConn() *mqttTask5PanicUpstreamConn {
	return &mqttTask5PanicUpstreamConn{closed: make(chan struct{})}
}

func (c *mqttTask5PanicUpstreamConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}

func (c *mqttTask5PanicUpstreamConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *mqttTask5PanicUpstreamConn) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closed) })
	panic(mqttTask5UpstreamPanic)
}

func (c *mqttTask5PanicUpstreamConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *mqttTask5PanicUpstreamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mqttTask5PanicUpstreamConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mqttTask5PanicUpstreamConn) LocalAddr() net.Addr              { return mqttTask5Addr("local") }
func (c *mqttTask5PanicUpstreamConn) RemoteAddr() net.Addr             { return mqttTask5Addr("remote") }

type mqttTask5CancelPanicConn struct {
	net.Conn
	mu           sync.Mutex
	payload      []byte
	offset       int
	closeStarted chan struct{}
	closeOnce    sync.Once
}

func newMQTTTask5CancelPanicConn(payload []byte) *mqttTask5CancelPanicConn {
	return &mqttTask5CancelPanicConn{
		payload:      append([]byte(nil), payload...),
		closeStarted: make(chan struct{}),
	}
}

func (c *mqttTask5CancelPanicConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.offset >= len(c.payload) {
		return 0, io.EOF
	}
	n := copy(p, c.payload[c.offset:])
	c.offset += n
	return n, nil
}

func (c *mqttTask5CancelPanicConn) Close() error {
	c.closeOnce.Do(func() { close(c.closeStarted) })
	panic(mqttTask5StopBothPanic)
}

func (c *mqttTask5CancelPanicConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mqttTask5CancelPanicConn) SetWriteDeadline(time.Time) error { return nil }
func (c *mqttTask5CancelPanicConn) LocalAddr() net.Addr              { return mqttTask5Addr("local") }
func (c *mqttTask5CancelPanicConn) RemoteAddr() net.Addr             { return mqttTask5Addr("remote") }

type mqttTask5AcceptErrorListener struct {
	mu            sync.Mutex
	conn          net.Conn
	err           error
	errorReturned chan struct{}
	errorOnce     sync.Once
}

func newMQTTTask5AcceptErrorListener(conn net.Conn, err error) *mqttTask5AcceptErrorListener {
	return &mqttTask5AcceptErrorListener{
		conn:          conn,
		err:           err,
		errorReturned: make(chan struct{}),
	}
}

func (l *mqttTask5AcceptErrorListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.conn != nil {
		conn := l.conn
		l.conn = nil
		l.mu.Unlock()
		return conn, nil
	}
	l.mu.Unlock()
	l.errorOnce.Do(func() { close(l.errorReturned) })
	return nil, l.err
}

func (l *mqttTask5AcceptErrorListener) Close() error   { return nil }
func (l *mqttTask5AcceptErrorListener) Addr() net.Addr { return mqttTask5Addr("mqtt-task5") }

type mqttTask5StopRaceContext struct {
	mu           sync.Mutex
	canceled     bool
	done         chan struct{}
	watcherReady chan struct{}
	readyOnce    sync.Once
}

func newMQTTTask5StopRaceContext() *mqttTask5StopRaceContext {
	return &mqttTask5StopRaceContext{
		done:         make(chan struct{}),
		watcherReady: make(chan struct{}),
	}
}

func (c *mqttTask5StopRaceContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *mqttTask5StopRaceContext) Done() <-chan struct{}       { return c.done }
func (c *mqttTask5StopRaceContext) Value(any) any               { return nil }

func (c *mqttTask5StopRaceContext) Err() error {
	c.readyOnce.Do(func() { close(c.watcherReady) })
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canceled {
		return context.Canceled
	}
	return nil
}

func (c *mqttTask5StopRaceContext) markCanceled() {
	c.mu.Lock()
	c.canceled = true
	c.mu.Unlock()
}
