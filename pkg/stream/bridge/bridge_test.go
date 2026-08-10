package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type controlledReader struct {
	started chan struct{}
	release chan struct{}
	data    []byte
}

func (r *controlledReader) Read(p []byte) (int, error) {
	close(r.started)
	<-r.release
	return copy(p, r.data), io.EOF
}

type deadlineRecordingConn struct {
	net.Conn
	writeDeadline chan time.Time
	written       bytes.Buffer
}

func (c *deadlineRecordingConn) SetWriteDeadline(deadline time.Time) error {
	c.writeDeadline <- deadline
	return nil
}

func (c *deadlineRecordingConn) Write(p []byte) (int, error) {
	return c.written.Write(p)
}

func TestCopyRefreshesWriteDeadlineAfterReadProgress(t *testing.T) {
	source, sourcePeer := net.Pipe()
	destination, destinationPeer := net.Pipe()
	t.Cleanup(func() { _ = source.Close() })
	t.Cleanup(func() { _ = sourcePeer.Close() })
	t.Cleanup(func() { _ = destination.Close() })
	t.Cleanup(func() { _ = destinationPeer.Close() })

	reader := &controlledReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    []byte("progress"),
	}
	recorder := &deadlineRecordingConn{
		Conn:          destination,
		writeDeadline: make(chan time.Time, 1),
	}
	done := make(chan directionResult, 1)
	go func() {
		done <- copyWithIdleDeadline(source, recorder, reader, time.Second)
	}()

	<-reader.started
	deadlineSetBeforeProgress := false
	select {
	case <-recorder.writeDeadline:
		deadlineSetBeforeProgress = true
	default:
	}
	close(reader.release)
	result := <-done
	if deadlineSetBeforeProgress {
		t.Fatal("destination write deadline was set before source read made progress")
	}
	select {
	case deadline := <-recorder.writeDeadline:
		if deadline.IsZero() {
			t.Fatal("destination write deadline was not bounded after read progress")
		}
	default:
		t.Fatal("destination write deadline was not refreshed after read progress")
	}
	if !result.eof || result.err != nil {
		t.Fatalf("copy result = %#v, want clean EOF", result)
	}
	if got := recorder.written.String(); got != "progress" {
		t.Fatalf("written payload = %q, want progress", got)
	}
}

func TestPumpPreservesHalfClose(t *testing.T) {
	left, leftPeer := newTCPPair(t)
	right, rightPeer := newTCPPair(t)

	request := []byte("pump-half-close-request")
	response := []byte("pump-delayed-response")
	pumpDone := make(chan error, 1)
	go func() { pumpDone <- Pump(context.Background(), left, right, nil, time.Second) }()

	if _, err := leftPeer.Write(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := leftPeer.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close left write: %v", err)
	}
	gotRequest := make([]byte, len(request))
	if _, err := io.ReadFull(rightPeer, gotRequest); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if !bytes.Equal(gotRequest, request) {
		t.Fatalf("request = %q, want %q", gotRequest, request)
	}
	_ = rightPeer.SetReadDeadline(time.Now().Add(time.Second))
	var probe [1]byte
	if _, err := rightPeer.Read(probe[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("read after left half-close = %v, want EOF", err)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := rightPeer.Write(response); err != nil {
		t.Fatalf("write delayed response: %v", err)
	}
	if err := rightPeer.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close right write: %v", err)
	}
	gotResponse := make([]byte, len(response))
	if _, err := io.ReadFull(leftPeer, gotResponse); err != nil {
		t.Fatalf("read delayed response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("response = %q, want %q", gotResponse, response)
	}
	_ = leftPeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := leftPeer.Read(probe[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("read after right half-close = %v, want EOF", err)
	}

	select {
	case err := <-pumpDone:
		if err != nil {
			t.Fatalf("Pump() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pump() did not wait for both directions")
	}
}

func TestPumpPreservesReverseHalfClose(t *testing.T) {
	left, leftPeer := newTCPPair(t)
	right, rightPeer := newTCPPair(t)

	request := []byte("pump-reverse-half-close-request")
	response := []byte("pump-reverse-delayed-response")
	pumpDone := make(chan error, 1)
	go func() { pumpDone <- Pump(context.Background(), left, right, nil, time.Second) }()

	if _, err := rightPeer.Write(request); err != nil {
		t.Fatalf("write reverse request: %v", err)
	}
	if err := rightPeer.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close right write: %v", err)
	}
	gotRequest := make([]byte, len(request))
	if _, err := io.ReadFull(leftPeer, gotRequest); err != nil {
		t.Fatalf("read reverse request: %v", err)
	}
	if !bytes.Equal(gotRequest, request) {
		t.Fatalf("reverse request = %q, want %q", gotRequest, request)
	}
	_ = leftPeer.SetReadDeadline(time.Now().Add(time.Second))
	var probe [1]byte
	if _, err := leftPeer.Read(probe[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("read after right half-close = %v, want EOF", err)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := leftPeer.Write(response); err != nil {
		t.Fatalf("write reverse delayed response: %v", err)
	}
	if err := leftPeer.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close left write: %v", err)
	}
	gotResponse := make([]byte, len(response))
	if _, err := io.ReadFull(rightPeer, gotResponse); err != nil {
		t.Fatalf("read reverse delayed response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatalf("reverse response = %q, want %q", gotResponse, response)
	}
	_ = rightPeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := rightPeer.Read(probe[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("read after left half-close = %v, want EOF", err)
	}

	select {
	case err := <-pumpDone:
		if err != nil {
			t.Fatalf("Pump() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pump() did not wait for reverse directions")
	}
}

func TestPumpCancellationUnblocks(t *testing.T) {
	left, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()
	t.Cleanup(func() { _ = left.Close() })
	t.Cleanup(func() { _ = leftPeer.Close() })
	t.Cleanup(func() { _ = right.Close() })
	t.Cleanup(func() { _ = rightPeer.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pumpDone := make(chan error, 1)
	go func() { pumpDone <- Pump(ctx, left, right, nil, time.Minute) }()
	cancel()

	select {
	case err := <-pumpDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Pump() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pump() did not stop after context cancellation")
	}
}

func newTCPPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TCP pair: %v", err)
	}
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatalf("dial TCP pair: %v", err)
	}
	server, err := listener.Accept()
	_ = listener.Close()
	if err != nil {
		_ = client.Close()
		t.Fatalf("accept TCP pair: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	return server, client
}
