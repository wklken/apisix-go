package kafka_proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/segmentio/kafka-go"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var (
	ErrWebSocketUpgradeRequired = errors.New("kafka-proxy requires a WebSocket upgrade")
	ErrWebSocketProtocol        = errors.New("invalid WebSocket frame")
)

type websocketProxyError struct {
	hijacked bool
	err      error
}

func (e *websocketProxyError) Error() string { return e.err.Error() }

func (e *websocketProxyError) Unwrap() error { return e.err }

// WebSocketWasHijacked reports whether an error occurred after the HTTP
// response was replaced by the WebSocket connection and therefore cannot be
// rendered as another HTTP response.
func WebSocketWasHijacked(err error) bool {
	var proxyErr *websocketProxyError
	return errors.As(err, &proxyErr) && proxyErr.hijacked
}

// IsWebSocketUpgrade reports whether the request satisfies the RFC 6455
// opening handshake required by the Kafka route owner.
func IsWebSocketUpgrade(r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet {
		return false
	}
	return headerContainsToken(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") &&
		r.Header.Get("Sec-WebSocket-Key") != "" &&
		r.Header.Get("Sec-WebSocket-Version") == "13"
}

func headerContainsToken(value, token string) bool {
	for item := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(item), token) {
			return true
		}
	}
	return false
}

// ServeWebSocket owns the bounded WebSocket-to-Kafka raw-frame bridge. The
// request must be upgraded by a real HTTP server; no HTTP-to-Kafka REST shape
// is inferred from the WebSocket payload.
func ServeWebSocket(w http.ResponseWriter, r *http.Request, target string, options TransportOptions) error {
	if !IsWebSocketUpgrade(r) {
		return ErrWebSocketUpgradeRequired
	}
	address, err := kafkaTargetAddress(target)
	if err != nil {
		return err
	}
	transport := NewTransport(options)
	dialCtx, cancelDial := context.WithTimeout(r.Context(), transport.connectTimeout)
	defer cancelDial()
	backend, err := transport.dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		if ctxErr := r.Context().Err(); ctxErr != nil {
			return fmt.Errorf("kafka dial: %w", ctxErr)
		}
		return fmt.Errorf("kafka dial %s: %w", address, err)
	}

	conn, err := upgradeKafkaWebSocket(w, r, transport)
	if err != nil {
		_ = backend.Close()
		return fmt.Errorf("upgrade Kafka WebSocket: %w", err)
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	closeOnCancel := closeConnectionsOnCancel(ctx, conn.UnderlyingConn(), backend)
	watcherStopped := false
	stopWatcher := func() {
		if watcherStopped {
			return
		}
		watcherStopped = true
		closeOnCancel()
	}

	bridge := &websocketBridge{
		conn:         conn,
		maxFrameSize: transport.maxFrameSize,
		readTimeout:  transport.readTimeout,
		writeTimeout: transport.writeTimeout,
	}
	tasks := runtime.NewRequestTaskGroup(ctx, "connection/kafka-proxy")
	results := make(chan directionResult, 2)
	admitted := 0
	finished := false
	finish := func(haveFirst bool) websocketFinishResult {
		finished = true
		outcome := websocketFinishResult{}
		cancel()
		if panicked, value := recoverWebSocketPanic(func() {
			closeConnectionsSafely(conn.UnderlyingConn(), backend)
		}); panicked {
			outcome.cleanupPanicked = true
			outcome.cleanupPanic = value
		}

		remaining := admitted
		if haveFirst {
			remaining--
		}
		for index := 0; index < remaining; index++ {
			result := <-results
			if haveFirst && index == 0 {
				outcome.second = result
			}
		}

		if panicked, value := recoverWebSocketPanic(stopWatcher); panicked {
			outcome.stopPanicked = true
			outcome.stopPanic = value
		}
		if panicked, value := recoverWebSocketPanic(func() {
			outcome.waitErr = tasks.Wait()
		}); panicked {
			outcome.waitPanicked = true
			outcome.waitPanic = value
		}
		return outcome
	}
	defer func() {
		if !finished {
			_ = finish(false)
		}
	}()
	for _, direction := range []func(context.Context, net.Conn) error{
		bridge.clientToKafka,
		bridge.kafkaToClient,
	} {
		run := direction
		if err := tasks.Go(func(taskCtx context.Context) error {
			result := directionResult{}
			defer func() { results <- result }()
			result.err = run(taskCtx, backend)
			result.completed = true
			return result.err
		}); err != nil {
			panic(err)
		}
		admitted++
	}

	first := <-results
	outcome := finish(true)
	if outcome.waitPanicked {
		panic(outcome.waitPanic)
	}
	if outcome.cleanupPanicked {
		panic(outcome.cleanupPanic)
	}
	if outcome.stopPanicked {
		panic(outcome.stopPanic)
	}
	if !first.completed {
		return outcome.waitErr
	}
	if websocketBridgeNormalClose(ctx, first.err) ||
		(errors.Is(first.err, context.Canceled) && websocketBridgeNormalClose(ctx, outcome.second.err)) {
		return nil
	}
	return &websocketProxyError{hijacked: true, err: first.err}
}

// ServePubSubWebSocket owns the APISIX 3.17 Kafka PubSub protocol. Each
// binary WebSocket message contains one PubSubReq and receives one PubSubResp;
// Kafka's native length-prefixed frames never cross this owner boundary.
func ServePubSubWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	brokers []string,
	options TransportOptions,
	factory KafkaConsumerFactory,
) error {
	if !IsWebSocketUpgrade(r) {
		return ErrWebSocketUpgradeRequired
	}
	if factory == nil {
		factory = newKafkaConsumer
	}
	transport := NewTransport(options)

	conn, err := upgradeKafkaWebSocket(w, r, transport)
	if err != nil {
		return fmt.Errorf("upgrade Kafka PubSub WebSocket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	closeOnCancel := closeConnectionsOnCancel(ctx, conn.UnderlyingConn())
	defer closeOnCancel()
	bridge := &websocketBridge{
		conn:         conn,
		maxFrameSize: transport.maxFrameSize,
		readTimeout:  transport.readTimeout,
		writeTimeout: transport.writeTimeout,
	}
	consumer, err := factory(ctx, brokers, ConsumerOptions{
		ConnectTimeout: transport.connectTimeout,
		ReadTimeout:    transport.readTimeout,
		MaxFetchBytes:  transport.maxFrameSize,
		TLSConfig:      options.TLSConfig,
		SASLEnabled:    SASLEnabled(r),
		SASLUsername:   SASLUsername(r),
		SASLPassword:   SASLPassword(r),
	})
	if err != nil {
		_ = bridge.writeClose(1011, "Kafka consumer unavailable")
		return &websocketProxyError{hijacked: true, err: err}
	}

	for {
		messageType, payload, err := bridge.readFrame()
		if err != nil {
			if websocketBridgeNormalClose(ctx, err) {
				return nil
			}
			return &websocketProxyError{hijacked: true, err: err}
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		request, err := ParsePubSubRequest(payload)
		if err != nil {
			response := PubSubResponse{
				Kind:    RespError,
				Message: "wrong command",
			}
			encoded, encodeErr := MarshalPubSubResponse(response)
			if encodeErr != nil {
				return &websocketProxyError{hijacked: true, err: encodeErr}
			}
			if err := bridge.writeBinary(encoded); err != nil {
				return &websocketProxyError{hijacked: true, err: err}
			}
			continue
		}
		response, err := dispatchPubSubRequest(ctx, consumer, request)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			response = PubSubResponse{
				Sequence: request.Sequence,
				Kind:     RespError,
				Code:     pubSubErrorCode(err),
				Message:  pubSubErrorMessage(request.Command, err),
			}
		}
		encoded, err := MarshalPubSubResponse(response)
		if err != nil {
			return &websocketProxyError{hijacked: true, err: err}
		}
		if len(encoded) > transport.maxFrameSize {
			_ = bridge.writeClose(1009, "Kafka PubSub response is too large")
			return &websocketProxyError{
				hijacked: true,
				err:      fmt.Errorf("kafka PubSub response exceeds max frame size %d", transport.maxFrameSize),
			}
		}
		if err := bridge.writeBinary(encoded); err != nil {
			return &websocketProxyError{hijacked: true, err: err}
		}
	}
}

// readMessage returns one complete binary WebSocket message.
func (b *websocketBridge) readMessage() ([]byte, error) {
	messageType, payload, err := b.readFrame()
	if err != nil {
		return nil, err
	}
	if messageType != websocket.BinaryMessage {
		_ = b.writeClose(1003, "Kafka WebSocket messages must be binary")
		return nil, fmt.Errorf("%w: Kafka WebSocket messages must be binary", ErrWebSocketProtocol)
	}
	return payload, nil
}

func (b *websocketBridge) readFrame() (int, []byte, error) {
	if err := b.conn.SetReadDeadline(time.Now().Add(b.readTimeout)); err != nil {
		return 0, nil, err
	}
	messageType, payload, err := b.conn.ReadMessage()
	if err != nil {
		var closeErr *websocket.CloseError
		if errors.As(err, &closeErr) {
			return 0, nil, io.EOF
		}
		if errors.Is(err, websocket.ErrReadLimit) {
			return 0, nil, fmt.Errorf("%w: Kafka WebSocket message is too large", ErrWebSocketProtocol)
		}
		return 0, nil, err
	}
	return messageType, payload, nil
}

func dispatchPubSubRequest(
	ctx context.Context,
	consumer KafkaConsumer,
	request PubSubRequest,
) (PubSubResponse, error) {
	switch request.Command {
	case CmdPing:
		return PubSubResponse{Sequence: request.Sequence, Kind: RespPong, State: request.State}, nil
	case CmdKafkaListOffset:
		offset, err := consumer.ListOffset(ctx, request.Topic, request.Partition, request.Position)
		if err != nil {
			return PubSubResponse{}, err
		}
		return PubSubResponse{Sequence: request.Sequence, Kind: RespKafkaListOffset, Offset: offset}, nil
	case CmdKafkaFetch:
		messages, err := consumer.Fetch(ctx, request.Topic, request.Partition, request.Position)
		if err != nil {
			return PubSubResponse{}, err
		}
		return PubSubResponse{Sequence: request.Sequence, Kind: RespKafkaFetch, Messages: messages}, nil
	case CmdEmpty:
		return PubSubResponse{}, fmt.Errorf("empty Kafka PubSub command is unsupported")
	default:
		return PubSubResponse{}, fmt.Errorf("unsupported Kafka PubSub command %d", request.Command)
	}
}

func pubSubErrorCode(err error) int32 {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return 504
	}
	return 502
}

func pubSubErrorMessage(command PubSubCommand, err error) string {
	if isKafkaAuthError(err) {
		return "Kafka authentication failed"
	}
	if command == CmdKafkaListOffset {
		return "Kafka list offset failed"
	}
	if command == CmdKafkaFetch {
		return "Kafka fetch failed"
	}
	return "Kafka PubSub command failed"
}

func isKafkaAuthError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, kafka.SASLAuthenticationFailed) ||
		errors.Is(err, kafka.UnsupportedSASLMechanism) ||
		errors.Is(err, kafka.IllegalSASLState) ||
		strings.Contains(strings.ToLower(err.Error()), "sasl authentication")
}

func upgradeKafkaWebSocket(w http.ResponseWriter, r *http.Request, transport *Transport) (*websocket.Conn, error) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	responseHeader := make(http.Header)
	if values := w.Header().Values("Server"); len(values) > 0 {
		responseHeader["Server"] = append([]string(nil), values...)
	}
	conn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(int64(transport.maxFrameSize + 4))
	return conn, nil
}

type websocketBridge struct {
	conn         *websocket.Conn
	writeMu      sync.Mutex
	maxFrameSize int
	readTimeout  time.Duration
	writeTimeout time.Duration
}

type directionResult struct {
	err       error
	completed bool
}

type websocketFinishResult struct {
	second          directionResult
	waitErr         error
	cleanupPanicked bool
	cleanupPanic    any
	stopPanicked    bool
	stopPanic       any
	waitPanicked    bool
	waitPanic       any
}

func recoverWebSocketPanic(run func()) (panicked bool, value any) {
	completed := false
	defer func() {
		if !completed {
			panicked = true
			value = recover()
		}
	}()
	run()
	completed = true
	return false, nil
}

func (b *websocketBridge) clientToKafka(ctx context.Context, backend net.Conn) error {
	for {
		payload, err := b.readMessage()
		if err != nil {
			return err
		}
		if err := writeKafkaPayload(ctx, backend, payload, b.maxFrameSize, b.writeTimeout); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			_ = b.writeClose(1002, "invalid Kafka frame")
			return fmt.Errorf("write Kafka frame: %w", err)
		}
	}
}

func (b *websocketBridge) kafkaToClient(ctx context.Context, backend net.Conn) error {
	for {
		if err := setDeadline(ctx, backend, b.readTimeout, backend.SetReadDeadline); err != nil {
			return err
		}
		frame, err := readKafkaFrame(backend, b.maxFrameSize)
		if err != nil {
			return err
		}
		if err := b.writeBinary(frame); err != nil {
			return err
		}
	}
}

func writeKafkaPayload(
	ctx context.Context,
	conn net.Conn,
	payload []byte,
	maxFrameSize int,
	timeout time.Duration,
) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty Kafka WebSocket message")
	}
	for len(payload) > 0 {
		if len(payload) < 4 {
			return fmt.Errorf("kafka WebSocket message has an incomplete frame header")
		}
		size := binary.BigEndian.Uint32(payload[:4])
		if uint64(size) > uint64(maxFrameSize) {
			return fmt.Errorf("kafka frame size %d exceeds max frame size %d", size, maxFrameSize)
		}
		frameSize := 4 + int(size)
		if frameSize > len(payload) {
			return fmt.Errorf("kafka WebSocket message has an incomplete frame payload")
		}
		if err := setDeadline(ctx, conn, timeout, conn.SetWriteDeadline); err != nil {
			return err
		}
		if err := writeAll(conn, payload[:frameSize]); err != nil {
			return err
		}
		payload = payload[frameSize:]
	}
	return nil
}

func (b *websocketBridge) writeBinary(payload []byte) error {
	if len(payload) > b.maxFrameSize+4 {
		return fmt.Errorf("WebSocket response frame is too large")
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if err := b.conn.SetWriteDeadline(time.Now().Add(b.writeTimeout)); err != nil {
		return err
	}
	return b.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (b *websocketBridge) writeClose(code uint16, reason string) error {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	return b.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(int(code), reason),
		time.Now().Add(b.writeTimeout),
	)
}

func closeConnectionsOnCancel(ctx context.Context, connections ...net.Conn) func() {
	return newConnectionCancellationWatcher(ctx, connections...)
}

func websocketBridgeNormalClose(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	return ctx.Err() != nil &&
		(errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "use of closed network connection"))
}
