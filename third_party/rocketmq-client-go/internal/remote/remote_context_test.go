package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/apache/rocketmq-client-go/v2/primitive"
)

func TestInvokeOneWayHonorsContextWhileConnectionLockIsHeld(t *testing.T) {
	client := NewRemotingClient(&RemotingClientConfig{TcpOption: TcpOption{
		ConnectionTimeout: time.Second,
		WriteTimeout:      time.Second,
	}})
	client.connectionLocker.Lock()
	defer client.connectionLocker.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.InvokeOneWay(ctx, "127.0.0.1:1", NewRemotingCommand(1, nil, nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("InvokeOneWay() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("connection lock ignored context: elapsed %s", elapsed)
	}
}

func TestDoRequestHonorsContextWhileConnectionWriteLockIsHeld(t *testing.T) {
	client := NewRemotingClient(&RemotingClientConfig{TcpOption: TcpOption{
		WriteTimeout: time.Second,
	}})
	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})
	conn := &tcpConnWrapper{Conn: local}
	conn.Lock()
	defer conn.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.doRequest(ctx, conn, NewRemotingCommand(1, nil, nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("doRequest() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("per-connection write lock ignored context: elapsed %s", elapsed)
	}
}

func TestDoRequestCancelsBlockedWrite(t *testing.T) {
	client := NewRemotingClient(&RemotingClientConfig{TcpOption: TcpOption{
		WriteTimeout: time.Minute,
	}})
	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})
	conn := &tcpConnWrapper{Conn: local}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.doRequest(ctx, conn, NewRemotingCommand(1, nil, bytes.Repeat([]byte("x"), 1<<20)))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("doRequest() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked write ignored context: elapsed %s", elapsed)
	}
}

type cancellationDeadlineConn struct {
	net.Conn
	cancel          context.CancelFunc
	cancelOnce      sync.Once
	callbackStarted chan struct{}
	callbackRelease chan struct{}
	callbackOnce    sync.Once
	deadlineMu      sync.Mutex
	lastDeadline    time.Time
}

func (c *cancellationDeadlineConn) Write(buffer []byte) (int, error) {
	c.cancelOnce.Do(c.cancel)
	return len(buffer), nil
}

func (c *cancellationDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	if deadline.Before(time.Now().Add(time.Second)) {
		c.callbackOnce.Do(func() { close(c.callbackStarted) })
		<-c.callbackRelease
	}
	c.deadlineMu.Lock()
	c.lastDeadline = deadline
	c.deadlineMu.Unlock()
	return nil
}

func TestDoRequestWaitsForCancellationDeadlineCallbackBeforeReusingConnection(t *testing.T) {
	client := NewRemotingClient(&RemotingClientConfig{TcpOption: TcpOption{
		WriteTimeout: time.Minute,
	}})
	local, peer := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	connection := &cancellationDeadlineConn{
		Conn:            local,
		cancel:          cancel,
		callbackStarted: make(chan struct{}),
		callbackRelease: make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-connection.callbackRelease:
		default:
			close(connection.callbackRelease)
		}
		_ = local.Close()
		_ = peer.Close()
	})
	conn := &tcpConnWrapper{Conn: connection}
	result := make(chan error, 1)
	go func() {
		result <- client.doRequest(ctx, conn, NewRemotingCommand(1, nil, nil))
	}()

	select {
	case <-connection.callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("cancellation deadline callback did not start")
	}
	select {
	case err := <-result:
		t.Fatalf("doRequest() returned %v before cancellation deadline callback completed", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(connection.callbackRelease)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("doRequest() error = %v, want context canceled", err)
	}
	if err := client.doRequest(context.Background(), conn, NewRemotingCommand(1, nil, nil)); err != nil {
		t.Fatalf("second doRequest() error = %v", err)
	}
	connection.deadlineMu.Lock()
	lastDeadline := connection.lastDeadline
	connection.deadlineMu.Unlock()
	if !lastDeadline.After(time.Now().Add(time.Second)) {
		t.Fatalf("reused connection deadline = %s, want future write deadline", lastDeadline)
	}
}

func TestSendRequestPassesCallerContextThroughInterceptor(t *testing.T) {
	client := NewRemotingClient(&RemotingClientConfig{TcpOption: TcpOption{
		WriteTimeout: time.Second,
	}})
	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})
	conn := &tcpConnWrapper{Conn: local}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var intercepted context.Context
	client.RegisterInterceptor(func(
		got context.Context, req, reply interface{}, next primitive.Invoker,
	) error {
		intercepted = got
		return next(got, req, reply)
	})
	go func() {
		_, _ = io.Copy(io.Discard, peer)
	}()
	if err := client.sendRequest(ctx, conn, NewRemotingCommand(1, nil, nil)); err != nil {
		t.Fatalf("sendRequest() error = %v", err)
	}
	if intercepted != ctx {
		t.Fatalf("interceptor context = %p, want caller context %p", intercepted, ctx)
	}
}
