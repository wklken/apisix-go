package mqtt_proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	defaultMQTTStreamPrereadTimeout = 5 * time.Second
	defaultMQTTStreamWriteTimeout   = 5 * time.Second
)

// StreamDialer selects a stream upstream using the parsed MQTT client ID. The
// peer fallback is passed when CONNECT does not carry a client ID.
type StreamDialer func(context.Context, string) (net.Conn, error)

// StreamInfo is the bounded metadata extracted during MQTT CONNECT preread.
type StreamInfo struct {
	ConnectInfo
	ClientID string
	Peer     string
}

// StreamResultHandler observes one accepted stream after it stops. It is the
// integration point for a future runtime stream log/load-balancer context.
type StreamResultHandler func(StreamInfo, error)

// ServeListener owns a TCP listener and delegates each accepted connection to
// ServeStream. It is intentionally plugin-owned; the main HTTP server does
// not call it until a stream-route configuration contract exists.
func (p *Plugin) ServeListener(
	ctx context.Context,
	listener net.Listener,
	dial StreamDialer,
	onResult StreamResultHandler,
) error {
	if listener == nil {
		return fmt.Errorf("mqtt stream listener is nil")
	}
	stopListener := closeListenerOnContextDone(ctx, listener)
	defer stopListener()

	var active sync.WaitGroup
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				active.Wait()
				return nil
			}
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				continue
			}
			active.Wait()
			return err
		}

		active.Go(func() {
			peer := ""
			if conn.RemoteAddr() != nil {
				peer = conn.RemoteAddr().String()
			}
			info, streamErr := p.ServeStream(ctx, conn, peer, dial)
			if onResult != nil {
				onResult(info, streamErr)
			}
			_ = conn.Close()
		})
	}
}

// ServeStream owns one MQTT client/upstream connection pair. It prereads and
// validates CONNECT, replays every inspected byte unchanged, exposes the
// client ID to the upstream selector, and then forwards both directions until
// close or cancellation. HTTP handlers are deliberately not involved.
func (p *Plugin) ServeStream(
	ctx context.Context,
	client net.Conn,
	peer string,
	dial StreamDialer,
) (StreamInfo, error) {
	if client == nil {
		return StreamInfo{}, fmt.Errorf("mqtt client connection is nil")
	}
	if dial == nil {
		_ = client.Close()
		return StreamInfo{}, fmt.Errorf("mqtt stream dialer is nil")
	}
	if err := ctx.Err(); err != nil {
		_ = client.Close()
		return StreamInfo{}, err
	}

	stopClientCancel := closeStreamOnContextDone(ctx, client)
	defer stopClientCancel()
	preread, connectInfo, err := readConnectFromStream(
		ctx,
		client,
		p.config.ProtocolName,
		p.config.ProtocolLevel,
	)
	if err != nil {
		_ = client.Close()
		return StreamInfo{}, err
	}

	clientID := ClientIDOrPeer(connectInfo, peer)
	upstream, err := dial(ctx, clientID)
	if err != nil {
		_ = client.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return StreamInfo{}, ctxErr
		}
		return StreamInfo{}, fmt.Errorf("mqtt upstream dial: %w", err)
	}
	if upstream == nil {
		_ = client.Close()
		return StreamInfo{}, fmt.Errorf("mqtt upstream dial returned nil connection")
	}
	defer upstream.Close()
	stopBothCancel := closeStreamOnContextDone(ctx, client, upstream)
	defer stopBothCancel()

	if err := writeStreamBytes(ctx, upstream, preread); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return StreamInfo{}, ctxErr
		}
		return StreamInfo{}, fmt.Errorf("mqtt CONNECT replay: %w", err)
	}

	info := StreamInfo{ConnectInfo: connectInfo, ClientID: clientID, Peer: peer}
	return info, copyMQTTStream(ctx, client, upstream)
}

func readConnectFromStream(
	ctx context.Context,
	conn net.Conn,
	protocolName string,
	protocolLevel int,
) ([]byte, ConnectInfo, error) {
	buffer := make([]byte, 0, 1024)
	chunk := make([]byte, 1024)
	for {
		if len(buffer) >= DefaultMaxConnectPacketSize {
			return nil, ConnectInfo{}, fmt.Errorf(
				"%w: CONNECT packet exceeds %d bytes",
				ErrMalformedConnect,
				DefaultMaxConnectPacketSize,
			)
		}
		deadline := time.Now().Add(defaultMQTTStreamPrereadTimeout)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, ConnectInfo{}, fmt.Errorf("mqtt CONNECT read deadline: %w", err)
		}
		read, readErr := conn.Read(chunk)
		if read > 0 {
			if len(buffer)+read > DefaultMaxConnectPacketSize {
				return nil, ConnectInfo{}, fmt.Errorf(
					"%w: CONNECT packet exceeds %d bytes",
					ErrMalformedConnect,
					DefaultMaxConnectPacketSize,
				)
			}
			buffer = append(buffer, chunk[:read]...)
			info, parseErr := ParseConnectPacket(buffer, protocolName, protocolLevel)
			if parseErr == nil {
				return buffer, info, nil
			}
			if !errors.Is(parseErr, ErrNeedMoreData) {
				return nil, ConnectInfo{}, parseErr
			}
		}
		if readErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ConnectInfo{}, ctxErr
			}
			return nil, ConnectInfo{}, readErr
		}
	}
}

func writeStreamBytes(ctx context.Context, conn net.Conn, payload []byte) error {
	deadline := time.Now().Add(defaultMQTTStreamWriteTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	for len(payload) > 0 {
		written, err := conn.Write(payload)
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

func copyMQTTStream(ctx context.Context, client net.Conn, upstream net.Conn) error {
	results := make(chan error, 2)
	go copyMQTTDirection(upstream, client, results)
	go copyMQTTDirection(client, upstream, results)

	var first error
	select {
	case first = <-results:
	case <-ctx.Done():
		first = ctx.Err()
	}
	_ = client.Close()
	_ = upstream.Close()
	<-results
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return normalizeMQTTCopyError(first)
}

func copyMQTTDirection(dst net.Conn, src net.Conn, results chan<- error) {
	_, err := io.Copy(dst, src)
	results <- err
}

func normalizeMQTTCopyError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "closed pipe") || strings.Contains(message, "use of closed network connection") {
		return nil
	}
	return err
}

func closeStreamOnContextDone(ctx context.Context, conns ...net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			for _, conn := range conns {
				_ = conn.Close()
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

func closeListenerOnContextDone(ctx context.Context, listener net.Listener) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}
