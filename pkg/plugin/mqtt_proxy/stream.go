package mqtt_proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/wklken/apisix-go/pkg/runtime"
	streambridge "github.com/wklken/apisix-go/pkg/stream/bridge"
)

const (
	defaultMQTTStreamPrereadTimeout = 5 * time.Second
	defaultMQTTStreamWriteTimeout   = 5 * time.Second
	defaultMQTTStreamIdleTimeout    = 60 * time.Second
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
	tasks := runtime.NewRequestTaskGroup(ctx, "connection/mqtt-proxy")
	finish := func(acceptErr error, accepted net.Conn) error {
		acceptedClose := mqttPanicState{}
		if accepted != nil {
			acceptedClose = mqttCapturePanic(func() { _ = accepted.Close() })
		}
		listenerStop := mqttCapturePanic(stopListener)
		var waitErr error
		waitPanic := mqttCapturePanic(func() { waitErr = tasks.Wait() })
		if waitPanic.panicked {
			panic(waitPanic.value)
		}
		if acceptedClose.panicked {
			panic(acceptedClose.value)
		}
		if listenerStop.panicked {
			panic(listenerStop.value)
		}
		if waitErr != nil {
			return waitErr
		}
		if acceptErr == nil {
			return nil
		}
		return acceptErr
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return finish(nil, nil)
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return finish(err, nil)
		}

		accepted := conn
		if err := tasks.Go(func(taskCtx context.Context) error {
			defer func() {
				primary := mqttPanicState{}
				if recovered := recover(); recovered != nil {
					primary = mqttPanicState{panicked: true, value: recovered}
				}
				acceptedClose := mqttCapturePanic(func() { _ = accepted.Close() })
				if primary.panicked {
					panic(primary.value)
				}
				if acceptedClose.panicked {
					panic(acceptedClose.value)
				}
			}()
			peer := ""
			if accepted.RemoteAddr() != nil {
				peer = accepted.RemoteAddr().String()
			}
			info, streamErr := p.ServeStream(taskCtx, accepted, peer, dial)
			if onResult != nil {
				onResult(info, streamErr)
			}
			return nil
		}); err != nil {
			return finish(err, accepted)
		}
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
	return p.ServeStreamWithIdle(ctx, client, peer, dial, defaultMQTTStreamIdleTimeout)
}

// ServeStreamWithIdle is the route-owned MQTT stream entry point. It uses the
// supplied per-direction idle bound after CONNECT preread and replay.
func (p *Plugin) ServeStreamWithIdle(
	ctx context.Context,
	client net.Conn,
	peer string,
	dial StreamDialer,
	idle time.Duration,
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
	cleanupFns := []func(){stopClientCancel}
	defer func() {
		primary := mqttPanicState{}
		if recovered := recover(); recovered != nil {
			primary = mqttPanicState{panicked: true, value: recovered}
		}
		cleanup := mqttCaptureCleanupPanics(cleanupFns)
		if primary.panicked {
			panic(primary.value)
		}
		if cleanup.panicked {
			panic(cleanup.value)
		}
	}()
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
	cleanupFns = append(cleanupFns, func() { _ = upstream.Close() })
	// Install ownership of the returned upstream before joining the preread
	// watcher: that join can replay a client-close panic after cancellation.
	stopClientCancel()
	stopBothCancel := closeStreamOnContextDone(ctx, client, upstream)
	cleanupFns = append(cleanupFns, stopBothCancel)

	if err := writeStreamBytes(ctx, upstream, preread); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return StreamInfo{}, ctxErr
		}
		return StreamInfo{}, fmt.Errorf("mqtt CONNECT replay: %w", err)
	}

	info := StreamInfo{ConnectInfo: connectInfo, ClientID: clientID, Peer: peer}
	// Any bytes already buffered after CONNECT are forwarded exactly once
	// through the buffered reader.
	return info, streambridge.Pump(ctx, client, upstream, bufio.NewReader(client), idle)
}

func readConnectFromStream(
	ctx context.Context,
	conn net.Conn,
	protocolName string,
	protocolLevel int,
) ([]byte, ConnectInfo, error) {
	deadline := time.Now().Add(defaultMQTTStreamPrereadTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, ConnectInfo{}, fmt.Errorf("mqtt CONNECT read deadline: %w", err)
	}
	info, raw, err := decodeConnect(conn, protocolName, protocolLevel)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ConnectInfo{}, ctxErr
		}
		return nil, ConnectInfo{}, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, ConnectInfo{}, fmt.Errorf("mqtt CONNECT clear read deadline: %w", err)
	}
	return raw, info, nil
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
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear write deadline: %w", err)
	}
	return nil
}

func closeStreamOnContextDone(ctx context.Context, conns ...net.Conn) func() {
	closeFns := make([]func(), 0, len(conns))
	for _, conn := range conns {
		accepted := conn
		closeFns = append(closeFns, func() { _ = accepted.Close() })
	}
	return newMQTTContextStopper(ctx, closeFns...)
}

func closeListenerOnContextDone(ctx context.Context, listener net.Listener) func() {
	return newMQTTContextStopper(ctx, func() { _ = listener.Close() })
}

type mqttPanicState struct {
	panicked bool
	value    any
}

func newMQTTContextStopper(ctx context.Context, closeFns ...func()) func() {
	stop := make(chan struct{})
	tasks := runtime.NewRequestTaskGroup(ctx, "connection/mqtt-proxy")
	admissionErr := tasks.Go(func(taskCtx context.Context) error {
		canceled := false
		if taskCtx.Err() != nil {
			canceled = true
		} else {
			select {
			case <-taskCtx.Done():
				canceled = true
			case <-stop:
				canceled = taskCtx.Err() != nil
			}
		}
		if canceled {
			closeMQTTConnections(closeFns...)
		}
		return nil
	})

	var stopOnce sync.Once
	var result mqttPanicState
	return func() {
		stopOnce.Do(func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					result = mqttPanicState{panicked: true, value: recovered}
				}
			}()
			if admissionErr != nil {
				panic(admissionErr)
			}
			close(stop)
			_ = tasks.Wait()
		})
		if result.panicked {
			panic(result.value)
		}
	}
}

func closeMQTTConnections(closeFns ...func()) {
	var first mqttPanicState
	for _, closeFn := range closeFns {
		state := mqttCapturePanic(closeFn)
		if state.panicked && !first.panicked {
			first = state
		}
	}
	if first.panicked {
		panic(first.value)
	}
}

func mqttCapturePanic(fn func()) (state mqttPanicState) {
	defer func() {
		if recovered := recover(); recovered != nil {
			state = mqttPanicState{panicked: true, value: recovered}
		}
	}()
	fn()
	return state
}

func mqttCaptureCleanupPanics(cleanupFns []func()) mqttPanicState {
	first := mqttPanicState{}
	for _, cleanupFn := range slices.Backward(cleanupFns) {
		state := mqttCapturePanic(cleanupFn)
		if state.panicked && !first.panicked {
			first = state
		}
	}
	return first
}
