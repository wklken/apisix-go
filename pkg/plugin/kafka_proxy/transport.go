package kafka_proxy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	defaultKafkaConnectTimeout = 5 * time.Second
	defaultKafkaReadTimeout    = 10 * time.Second
	defaultKafkaWriteTimeout   = 10 * time.Second
	defaultKafkaMaxFrameSize   = 16 << 20
)

// TransportOptions bounds a plugin-owned Kafka request/response exchange.
// The transport deliberately forwards raw length-prefixed frames; Kafka API
// decoding and stream-route ownership remain outside this HTTP plugin package.
type TransportOptions struct {
	Dialer         *net.Dialer
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	MaxFrameSize   int
	TLSConfig      *tls.Config
}

// Transport performs one bounded raw Kafka frame exchange.
type Transport struct {
	dialer         *net.Dialer
	connectTimeout time.Duration
	readTimeout    time.Duration
	writeTimeout   time.Duration
	maxFrameSize   int
}

// NewTransport creates a bounded raw Kafka transport without decoding Kafka
// API messages or inventing an HTTP-to-Kafka REST contract.
func NewTransport(options TransportOptions) *Transport {
	dialer := net.Dialer{}
	if options.Dialer != nil {
		dialer = *options.Dialer
	}
	if options.ConnectTimeout <= 0 {
		options.ConnectTimeout = defaultKafkaConnectTimeout
	}
	if options.ReadTimeout <= 0 {
		options.ReadTimeout = defaultKafkaReadTimeout
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = defaultKafkaWriteTimeout
	}
	if options.MaxFrameSize <= 0 {
		options.MaxFrameSize = defaultKafkaMaxFrameSize
	}
	return &Transport{
		dialer:         &dialer,
		connectTimeout: options.ConnectTimeout,
		readTimeout:    options.ReadTimeout,
		writeTimeout:   options.WriteTimeout,
		maxFrameSize:   options.MaxFrameSize,
	}
}

// RoundTrip writes one Kafka length-prefixed request frame and reads one
// length-prefixed response frame from target. Cancellation closes the socket
// so a blocked read or write cannot outlive the request context.
func (t *Transport) RoundTrip(ctx context.Context, target string, request []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kafka round trip: %w", err)
	}
	if err := validateKafkaFrame(request, t.maxFrameSize); err != nil {
		return nil, fmt.Errorf("kafka request frame: %w", err)
	}

	address, err := kafkaTargetAddress(target)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, t.connectTimeout)
	defer cancel()
	conn, err := t.dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("kafka dial: %w", ctxErr)
		}
		if dialCtxErr := dialCtx.Err(); dialCtxErr != nil {
			return nil, fmt.Errorf("kafka dial: %w", dialCtxErr)
		}
		return nil, fmt.Errorf("kafka dial %s: %w", address, err)
	}
	stopCloseOnCancel := closeOnContextDone(ctx, conn)
	defer stopCloseOnCancel()
	defer func() { _ = conn.Close() }()

	if err := setDeadline(ctx, conn, t.writeTimeout, conn.SetWriteDeadline); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("kafka write deadline: %w", ctxErr)
		}
		return nil, fmt.Errorf("kafka write deadline: %w", err)
	}
	if err := writeAll(conn, request); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("kafka write: %w", ctxErr)
		}
		return nil, fmt.Errorf("kafka write: %w", err)
	}

	if err := setDeadline(ctx, conn, t.readTimeout, conn.SetReadDeadline); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("kafka read deadline: %w", ctxErr)
		}
		return nil, fmt.Errorf("kafka read deadline: %w", err)
	}
	response, err := readKafkaFrame(conn, t.maxFrameSize)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("kafka read: %w", ctxErr)
		}
		return nil, fmt.Errorf("kafka read: %w", err)
	}
	return response, nil
}

func kafkaTargetAddress(target string) (string, error) {
	if !strings.Contains(target, "://") {
		if target == "" {
			return "", fmt.Errorf("kafka target is empty")
		}
		return target, nil
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("parse kafka target %q: %w", target, err)
	}
	if parsed.Scheme != "kafka" {
		return "", fmt.Errorf("unsupported kafka target scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid kafka target %q", target)
	}
	return parsed.Host, nil
}

func validateKafkaFrame(frame []byte, maxFrameSize int) error {
	if len(frame) < 4 {
		return fmt.Errorf("frame length %d is less than the Kafka header", len(frame))
	}
	size := binary.BigEndian.Uint32(frame[:4])
	if uint64(size) > uint64(maxFrameSize) {
		return fmt.Errorf("frame size %d exceeds max frame size %d", size, maxFrameSize)
	}
	if int64(size) != int64(len(frame)-4) {
		return fmt.Errorf("declared payload length %d does not match %d", size, len(frame)-4)
	}
	return nil
}

func readKafkaFrame(reader io.Reader, maxFrameSize int) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if uint64(size) > uint64(maxFrameSize) {
		return nil, fmt.Errorf("frame size %d exceeds max frame size %d", size, maxFrameSize)
	}
	frame := make([]byte, 4+int(size))
	copy(frame, header[:])
	if _, err := io.ReadFull(reader, frame[4:]); err != nil {
		return nil, err
	}
	return frame, nil
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

func setDeadline(
	ctx context.Context,
	conn net.Conn,
	timeout time.Duration,
	setter func(time.Time) error,
) error {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return setter(deadline)
}

func closeOnContextDone(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}
