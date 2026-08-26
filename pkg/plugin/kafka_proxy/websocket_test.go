package kafka_proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var (
	websocketRawPanic   = &struct{ marker string }{marker: "kafka-websocket-raw-panic"}
	websocketClosePanic = &struct{ marker string }{marker: "kafka-websocket-close-panic"}
)

type websocketPanicConn struct {
	net.Conn
	panicEnabled atomic.Bool
	closePanic   any
	peerExited   chan struct{}
	peerExitOnce sync.Once
}

func (c *websocketPanicConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.peerExitOnce.Do(func() { close(c.peerExited) })
	}
	return n, err
}

func (c *websocketPanicConn) Write(p []byte) (int, error) {
	if c.panicEnabled.Load() {
		panic(websocketRawPanic)
	}
	return c.Conn.Write(p)
}

func (c *websocketPanicConn) Close() error {
	err := c.Conn.Close()
	if c.closePanic != nil {
		panic(c.closePanic)
	}
	return err
}

type websocketPanicListener struct {
	net.Listener
	accepted chan<- *websocketPanicConn
}

func (l *websocketPanicListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &websocketPanicConn{
		Conn:       conn,
		peerExited: make(chan struct{}),
	}
	l.accepted <- wrapped
	return wrapped, nil
}

func validWebSocketUpgradeRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/kafka", nil)
	request.Header.Set("Connection", "keep-alive, Upgrade")
	request.Header.Set("Upgrade", "WebSocket")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Version", "13")
	return request
}

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name  string
		build func() *http.Request
		want  bool
	}{
		{name: "valid mixed-case comma-separated", build: validWebSocketUpgradeRequest, want: true},
		{name: "nil request", build: func() *http.Request { return nil }},
		{name: "wrong method", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Method = http.MethodPost
			return request
		}},
		{name: "missing connection token", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Set("Connection", "keep-alive")
			return request
		}},
		{name: "missing upgrade header", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Del("Upgrade")
			return request
		}},
		{name: "missing key", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Del("Sec-WebSocket-Key")
			return request
		}},
		{name: "wrong version", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Set("Sec-WebSocket-Version", "12")
			return request
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsWebSocketUpgrade(test.build()); got != test.want {
				t.Fatalf("IsWebSocketUpgrade() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWebSocketProxyErrorWrapsAndFlagsHijack(t *testing.T) {
	cause := errors.New("kafka frame failed")
	wrapped := &websocketProxyError{hijacked: true, err: cause}

	if wrapped.Error() != cause.Error() {
		t.Fatalf("Error() = %q, want %q", wrapped.Error(), cause.Error())
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("errors.Is() = false, want unwrapped cause")
	}
	if !WebSocketWasHijacked(wrapped) {
		t.Fatal("WebSocketWasHijacked() = false for a hijacked error")
	}
	if WebSocketWasHijacked(cause) {
		t.Fatal("WebSocketWasHijacked() = true for a plain error")
	}
	if WebSocketWasHijacked(&websocketProxyError{err: cause}) {
		t.Fatal("WebSocketWasHijacked() = true for a non-hijacked error")
	}
}

func TestWriteKafkaPayloadWritesCompleteFrames(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	payload := append(kafkaTestFrame([]byte("one")), kafkaTestFrame([]byte("two"))...)
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(payload))
		_, _ = io.ReadFull(server, buf)
		received <- buf
	}()
	if err := writeKafkaPayload(t.Context(), client, payload, 1024, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := <-received; !bytes.Equal(got, payload) {
		t.Fatalf("received = %x, want %x", got, payload)
	}
}

func TestWriteKafkaPayloadRejectsMalformedFrames(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{name: "empty payload", payload: nil, want: "empty Kafka WebSocket message"},
		{name: "incomplete header", payload: []byte{0x00, 0x00}, want: "incomplete frame header"},
		{
			name:    "oversized declared frame",
			payload: []byte{0x00, 0x00, 0x08, 0x00, 0x01, 0x02, 0x03, 0x04},
			want:    "exceeds max frame size",
		},
		{
			name:    "incomplete body",
			payload: []byte{0x00, 0x00, 0x00, 0x08, 0x01, 0x02},
			want:    "incomplete frame payload",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			err := writeKafkaPayload(t.Context(), client, test.payload, 1024, time.Second)
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte(test.want)) {
				t.Fatalf("writeKafkaPayload() error = %v, want fragment %q", err, test.want)
			}
		})
	}
}

func TestWriteKafkaPayloadHonorsExpiredContextDeadline(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := writeKafkaPayload(ctx, client, kafkaTestFrame([]byte("data")), 1024, time.Second); err == nil {
		t.Fatal("writeKafkaPayload() error = nil with an expired context deadline")
	}
}

func TestWriteKafkaPayloadFailsOnPeerClosure(t *testing.T) {
	client, server := net.Pipe()
	_ = server.Close()
	t.Cleanup(func() { _ = client.Close() })

	if err := writeKafkaPayload(t.Context(), client, kafkaTestFrame([]byte("data")), 1024, time.Second); err == nil {
		t.Fatal("writeKafkaPayload() error = nil after peer closed")
	}
}

func TestWebSocketBridgeNormalClose(t *testing.T) {
	plain := context.Background()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "nil error", ctx: plain, want: true},
		{name: "eof", ctx: plain, err: io.EOF, want: true},
		{name: "closed network", ctx: plain, err: net.ErrClosed, want: true},
		{name: "canceled with canceled context", ctx: canceled, err: context.Canceled, want: true},
		{
			name: "closed connection with canceled context",
			ctx:  canceled,
			err:  errors.New("use of closed network connection"),
			want: true,
		},
		{name: "canceled without canceled context", ctx: plain, err: context.Canceled},
		{name: "ordinary error without cancellation", ctx: plain, err: errors.New("boom")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := websocketBridgeNormalClose(test.ctx, test.err); got != test.want {
				t.Fatalf("websocketBridgeNormalClose() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestServeWebSocketRejectsNonUpgradeRequests(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/kafka", nil)
	response := httptest.NewRecorder()
	err := ServeWebSocket(response, request, "kafka://127.0.0.1:9092", TransportOptions{})
	if !errors.Is(err, ErrWebSocketUpgradeRequired) {
		t.Fatalf("ServeWebSocket() error = %v, want ErrWebSocketUpgradeRequired", err)
	}

	request = httptest.NewRequest(http.MethodGet, "http://example.test/kafka", nil)
	err = ServePubSubWebSocket(response, request, []string{"kafka://127.0.0.1:9092"}, TransportOptions{}, nil)
	if !errors.Is(err, ErrWebSocketUpgradeRequired) {
		t.Fatalf("ServePubSubWebSocket() error = %v, want ErrWebSocketUpgradeRequired", err)
	}
}

func TestServePubSubWebSocketFullLoop(t *testing.T) {
	served := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "APISIX/test-version")
		served <- ServePubSubWebSocket(w, r, []string{"kafka://127.0.0.1:9092"}, TransportOptions{}, func(
			context.Context,
			[]string,
			ConsumerOptions,
		) (KafkaConsumer, error) {
			return &fakeKafkaConsumer{messages: []KafkaMessage{{Offset: 7, Value: []byte("value")}}}, nil
		})
	}))
	defer server.Close()

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/pubsub", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := response.Header.Get("Server"); got != "APISIX/test-version" {
		t.Fatalf("websocket Server header = %q, want preconfigured token", got)
	}

	pingWire := mustMarshalPubSubRequest(t, PubSubRequest{Sequence: 1, Command: CmdPing, State: []byte("s")})
	if err := conn.WriteMessage(websocket.BinaryMessage, pingWire); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	pong, err := ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("parse pong: %v", err)
	}
	if pong.Kind != RespPong || pong.Sequence != 1 || !bytes.Equal(pong.State, []byte("s")) {
		t.Fatalf("pong = %#v, want echoed ping state", pong)
	}

	fetchWire := mustMarshalPubSubRequest(t, PubSubRequest{
		Sequence: 2, Command: CmdKafkaFetch, Topic: "orders", Partition: 0, Position: 0,
	})
	if err := conn.WriteMessage(websocket.BinaryMessage, fetchWire); err != nil {
		t.Fatalf("write fetch: %v", err)
	}
	_, payload, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read fetch response: %v", err)
	}
	fetchResponse, err := ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("parse fetch response: %v", err)
	}
	if fetchResponse.Kind != RespKafkaFetch || len(fetchResponse.Messages) != 1 ||
		fetchResponse.Messages[0].Offset != 7 {
		t.Fatalf("fetch response = %#v, want one message at offset 7", fetchResponse)
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{0x08}); err != nil {
		t.Fatalf("write malformed request: %v", err)
	}
	_, payload, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read malformed-request response: %v", err)
	}
	malformedResponse, err := ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("parse malformed-request response: %v", err)
	}
	if malformedResponse.Sequence != 0 || malformedResponse.Kind != RespError ||
		malformedResponse.Code != 0 || malformedResponse.Message != "wrong command" {
		t.Fatalf("malformed-request response = %#v, want sequence 0 wrong command", malformedResponse)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte("ignored")); err != nil {
		t.Fatalf("write text message: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, pingWire); err != nil {
		t.Fatalf("write ping after malformed/text messages: %v", err)
	}
	_, payload, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read pong after malformed/text messages: %v", err)
	}
	postMalformed, err := ParsePubSubResponse(payload)
	if err != nil {
		t.Fatalf("parse pong after malformed/text messages: %v", err)
	}
	if postMalformed.Kind != RespPong || postMalformed.Sequence != 1 ||
		!bytes.Equal(postMalformed.State, []byte("s")) {
		t.Fatalf("post-malformed response = %#v, want pong sequence 1", postMalformed)
	}
	_ = conn.Close()
	if err := <-served; err != nil {
		t.Fatalf("ServePubSubWebSocket() error = %v after normal close", err)
	}
}

func echoKafkaFrame(conn net.Conn) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return
	}
	frame := append(header[:], payload...)
	_, _ = conn.Write(frame)
}

func TestServeWebSocketBridgesRawFrames(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer func() { _ = backend.Close() }()
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		echoKafkaFrame(conn)
	}()

	served := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- ServeWebSocket(w, r, "kafka://"+backend.Addr().String(), TransportOptions{})
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/kafka", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	frame := kafkaTestFrame([]byte("echo-this"))
	if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read echoed frame: %v", err)
	}
	if !bytes.Equal(payload, frame) {
		t.Fatalf("echoed payload = %x, want %x", payload, frame)
	}
	_ = conn.Close()
	if err := <-served; err != nil {
		t.Fatalf("ServeWebSocket() error = %v", err)
	}
}

func TestServeWebSocketRawDirectionPanicReturnsAfterPeerCleanup(t *testing.T) {
	runServeWebSocketPanicHelper(t, false)
}

func TestServeWebSocketDirectionPanicPrecedesWatcherClosePanic(t *testing.T) {
	runServeWebSocketPanicHelper(t, true)
}

func runServeWebSocketPanicHelper(t *testing.T, closePanic bool) {
	t.Helper()
	helperCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(helperCtx, os.Args[0], "-test.run=^TestServeWebSocketRawDirectionPanicHelper$")
	command.Env = append(os.Environ(), "APISIX_GO_KAFKA_WEBSOCKET_PANIC_HELPER=1")
	if closePanic {
		command.Env = append(command.Env, "APISIX_GO_KAFKA_WEBSOCKET_CLOSE_PANIC=1")
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helper exited before WebSocket owner recovery (ctx err %v): %v\n%s", helperCtx.Err(), err, output)
	}
	peerIndex := bytes.Index(output, []byte("kafka-websocket-peer-exited"))
	ownerIndex := bytes.Index(output, []byte("kafka-websocket-owner-recovered"))
	if peerIndex < 0 || ownerIndex < 0 {
		t.Fatalf("missing WebSocket ownership markers: %s", output)
	}
	if peerIndex > ownerIndex {
		t.Fatalf("WebSocket owner recovered before peer cleanup: %s", output)
	}
}

func TestServeWebSocketRawDirectionPanicHelper(t *testing.T) {
	if os.Getenv("APISIX_GO_KAFKA_WEBSOCKET_PANIC_HELPER") != "1" {
		return
	}

	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer func() { _ = backendListener.Close() }()
	sendFrame := make(chan struct{})
	go func() {
		conn, acceptErr := backendListener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-sendFrame
		_, _ = conn.Write(kafkaTestFrame([]byte("panic-direction")))
		_, _ = io.Copy(io.Discard, conn)
	}()

	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("HTTP listen: %v", err)
	}
	accepted := make(chan *websocketPanicConn, 1)
	wrappedListener := &websocketPanicListener{Listener: serverListener, accepted: accepted}
	ownerRecovered := make(chan struct{})
	var websocketConn *websocketPanicConn
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered != websocketRawPanic {
				fmt.Fprintf(os.Stderr, "recovered panic = %#v, want %#v\n", recovered, websocketRawPanic)
				os.Exit(1)
			}
			select {
			case <-websocketConn.peerExited:
				fmt.Println("kafka-websocket-peer-exited")
			default:
				fmt.Fprintln(os.Stderr, "kafka-websocket-owner-recovered-before-peer-exit")
				os.Exit(1)
			}
			fmt.Println("kafka-websocket-owner-recovered")
			close(ownerRecovered)
		}()
		if err := ServeWebSocket(w, r, "kafka://"+backendListener.Addr().String(), TransportOptions{}); err != nil {
			fmt.Fprintf(os.Stderr, "ServeWebSocket() error = %v\n", err)
			os.Exit(1)
		}
	})}
	go func() { _ = server.Serve(wrappedListener) }()
	defer func() { _ = server.Close() }()

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+serverListener.Addr().String()+"/kafka", nil)
	if err != nil {
		t.Fatalf("WebSocket dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	websocketConn = <-accepted
	if os.Getenv("APISIX_GO_KAFKA_WEBSOCKET_CLOSE_PANIC") == "1" {
		websocketConn.closePanic = websocketClosePanic
	}
	websocketConn.panicEnabled.Store(true)
	close(sendFrame)
	select {
	case <-ownerRecovered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WebSocket owner recovery")
	}
}
