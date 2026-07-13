package kafka_proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

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
	defer func() { _ = backend.Close() }()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return fmt.Errorf("kafka WebSocket server does not support connection hijacking")
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijack Kafka WebSocket: %w", err)
	}
	defer func() { _ = client.Close() }()
	if err := writeWebSocketHandshake(rw, r.Header.Get("Sec-WebSocket-Key")); err != nil {
		return &websocketProxyError{hijacked: true, err: fmt.Errorf("write Kafka WebSocket handshake: %w", err)}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	closeOnCancel := closeConnectionsOnCancel(ctx, client, backend)
	defer closeOnCancel()

	bridge := &websocketBridge{
		client:       client,
		rw:           rw,
		maxFrameSize: transport.maxFrameSize,
		readTimeout:  transport.readTimeout,
		writeTimeout: transport.writeTimeout,
	}
	results := make(chan error, 2)
	go func() { results <- bridge.clientToKafka(ctx, backend) }()
	go func() { results <- bridge.kafkaToClient(ctx, backend) }()

	first := <-results
	cancel()
	_ = client.Close()
	_ = backend.Close()
	second := <-results
	if websocketBridgeNormalClose(ctx, first) ||
		(errors.Is(first, context.Canceled) && websocketBridgeNormalClose(ctx, second)) {
		return nil
	}
	return &websocketProxyError{hijacked: true, err: first}
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

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return fmt.Errorf("kafka PubSub WebSocket server does not support connection hijacking")
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijack Kafka PubSub WebSocket: %w", err)
	}
	defer func() { _ = client.Close() }()
	if err := writeWebSocketHandshake(rw, r.Header.Get("Sec-WebSocket-Key")); err != nil {
		return &websocketProxyError{hijacked: true, err: fmt.Errorf("write Kafka PubSub handshake: %w", err)}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	closeOnCancel := closeConnectionsOnCancel(ctx, client)
	defer closeOnCancel()
	bridge := &websocketBridge{
		client:       client,
		rw:           rw,
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
		payload, err := bridge.readMessage()
		if err != nil {
			if websocketBridgeNormalClose(ctx, err) {
				return nil
			}
			return &websocketProxyError{hijacked: true, err: err}
		}
		request, err := ParsePubSubRequest(payload)
		if err != nil {
			_ = bridge.writeClose(1002, "malformed PubSub request")
			return &websocketProxyError{hijacked: true, err: err}
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
		if err := setDeadline(ctx, client, transport.writeTimeout, client.SetWriteDeadline); err != nil {
			return &websocketProxyError{hijacked: true, err: err}
		}
		if err := bridge.writeFrame(0x2, encoded); err != nil {
			return &websocketProxyError{hijacked: true, err: err}
		}
	}
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
	if err != nil {
		return "Kafka PubSub command failed"
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

type websocketBridge struct {
	client       net.Conn
	rw           *bufio.ReadWriter
	writeMu      sync.Mutex
	maxFrameSize int
	readTimeout  time.Duration
	writeTimeout time.Duration
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
		if err := setDeadline(ctx, b.client, b.writeTimeout, b.client.SetWriteDeadline); err != nil {
			return err
		}
		if err := b.writeFrame(0x2, frame); err != nil {
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

func writeWebSocketHandshake(rw *bufio.ReadWriter, key string) error {
	hash := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(hash[:])
	if _, err := fmt.Fprintf(
		rw,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		accept,
	); err != nil {
		return err
	}
	return rw.Flush()
}

type websocketFrame struct {
	fin     bool
	opcode  byte
	payload []byte
}

func (b *websocketBridge) readMessage() ([]byte, error) {
	var message bytes.Buffer
	var dataOpcode byte
	for {
		frame, err := b.readFrame()
		if err != nil {
			return nil, err
		}
		switch frame.opcode {
		case 0x8:
			_ = b.writeFrame(0x8, frame.payload)
			return nil, io.EOF
		case 0x9:
			if err := b.writeFrame(0xa, frame.payload); err != nil {
				return nil, err
			}
			continue
		case 0xa:
			continue
		case 0x1:
			if dataOpcode != 0 {
				return nil, b.protocolError(1002, "nested WebSocket message")
			}
			dataOpcode = frame.opcode
		case 0x2:
			if dataOpcode != 0 {
				return nil, b.protocolError(1002, "nested WebSocket message")
			}
			dataOpcode = frame.opcode
		case 0x0:
			if dataOpcode == 0 {
				return nil, b.protocolError(1002, "unexpected WebSocket continuation")
			}
		default:
			return nil, b.protocolError(1002, "unsupported WebSocket opcode")
		}
		if dataOpcode == 0x1 {
			return nil, b.protocolError(1003, "Kafka WebSocket messages must be binary")
		}
		if message.Len()+len(frame.payload) > b.maxFrameSize+4 {
			return nil, b.protocolError(1009, "Kafka WebSocket message is too large")
		}
		_, _ = message.Write(frame.payload)
		if frame.fin {
			return message.Bytes(), nil
		}
	}
}

func (b *websocketBridge) readFrame() (websocketFrame, error) {
	if err := setDeadline(context.Background(), b.client, b.readTimeout, b.client.SetReadDeadline); err != nil {
		return websocketFrame{}, err
	}
	var header [2]byte
	if _, err := io.ReadFull(b.client, header[:]); err != nil {
		return websocketFrame{}, err
	}
	if header[0]&0x70 != 0 {
		return websocketFrame{}, b.protocolError(1002, "reserved WebSocket bits are set")
	}
	frame := websocketFrame{fin: header[0]&0x80 != 0, opcode: header[0] & 0x0f}
	masked := header[1]&0x80 != 0
	if !masked {
		return websocketFrame{}, b.protocolError(1002, "client WebSocket frame is not masked")
	}
	payloadLength := uint64(header[1] & 0x7f)
	switch payloadLength {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(b.client, extended[:]); err != nil {
			return websocketFrame{}, err
		}
		payloadLength = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(b.client, extended[:]); err != nil {
			return websocketFrame{}, err
		}
		payloadLength = binary.BigEndian.Uint64(extended[:])
		if payloadLength&(1<<63) != 0 {
			return websocketFrame{}, b.protocolError(1002, "invalid WebSocket payload length")
		}
	}
	if payloadLength > uint64(b.maxFrameSize+4) {
		return websocketFrame{}, b.protocolError(1009, "Kafka WebSocket frame is too large")
	}
	if (frame.opcode&0x8) != 0 && (!frame.fin || payloadLength > 125) {
		return websocketFrame{}, b.protocolError(1002, "invalid WebSocket control frame")
	}
	var mask [4]byte
	if _, err := io.ReadFull(b.client, mask[:]); err != nil {
		return websocketFrame{}, err
	}
	frame.payload = make([]byte, int(payloadLength))
	if _, err := io.ReadFull(b.client, frame.payload); err != nil {
		return websocketFrame{}, err
	}
	for index := range frame.payload {
		frame.payload[index] ^= mask[index%4]
	}
	return frame, nil
}

func (b *websocketBridge) writeFrame(opcode byte, payload []byte) error {
	if len(payload) > b.maxFrameSize+4 {
		return fmt.Errorf("WebSocket response frame is too large")
	}
	header := make([]byte, 2)
	header[0] = 0x80 | opcode
	switch {
	case len(payload) < 126:
		header[1] = byte(len(payload))
	case uint64(len(payload)) <= 0xffff:
		header[1] = 126
		var extended [2]byte
		binary.BigEndian.PutUint16(extended[:], uint16(len(payload)))
		header = append(header, extended[:]...)
	default:
		header[1] = 127
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(len(payload)))
		header = append(header, extended[:]...)
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if _, err := b.rw.Write(header); err != nil {
		return err
	}
	if _, err := b.rw.Write(payload); err != nil {
		return err
	}
	return b.rw.Flush()
}

func (b *websocketBridge) protocolError(code uint16, reason string) error {
	_ = b.writeClose(code, reason)
	return fmt.Errorf("%w: %s", ErrWebSocketProtocol, reason)
}

func (b *websocketBridge) writeClose(code uint16, reason string) error {
	var closePayload [2]byte
	binary.BigEndian.PutUint16(closePayload[:], code)
	if len(reason) > 123 {
		reason = reason[:123]
	}
	return b.writeFrame(0x8, append(closePayload[:], []byte(reason)...))
}

func closeConnectionsOnCancel(ctx context.Context, connections ...net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			for _, conn := range connections {
				_ = conn.Close()
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

func websocketBridgeNormalClose(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	return ctx.Err() != nil &&
		(errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "use of closed network connection"))
}
