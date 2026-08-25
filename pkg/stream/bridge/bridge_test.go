package bridge

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

var bridgeRawPanic = &struct{ marker string }{marker: "bridge-raw-panic"}

type panicReadConn struct {
	net.Conn
}

func (c *panicReadConn) Read([]byte) (int, error) {
	panic(bridgeRawPanic)
}

type peerCloseConn struct {
	net.Conn
}

func (c *peerCloseConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		fmt.Println("bridge-peer-closed")
	}
	return n, err
}

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

type scriptedConn struct {
	net.Conn
	readFunc  func([]byte) (int, error)
	closed    chan struct{}
	closeErr  error
	closeOnce sync.Once
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	return c.readFunc(p)
}

func (c *scriptedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	_ = c.Conn.Close()
	return c.closeErr
}

type halfCloseRecordingConn struct {
	net.Conn
	closeWriteErr  error
	closeWriteDone chan struct{}
	closeWriteOnce sync.Once
}

func (c *halfCloseRecordingConn) CloseWrite() error {
	c.closeWriteOnce.Do(func() { close(c.closeWriteDone) })
	return c.closeWriteErr
}

func newScriptedConn(t *testing.T, readFunc func([]byte) (int, error)) *scriptedConn {
	t.Helper()
	conn, peer := net.Pipe()
	result := &scriptedConn{
		Conn:     conn,
		readFunc: readFunc,
		closed:   make(chan struct{}),
	}
	t.Cleanup(func() { _ = result.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	return result
}

func TestPumpRawDirectionPanicReturnsFromOwnerAfterPeerCleanup(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestPumpRawDirectionPanicHelper$")
	cmd.Env = append(os.Environ(), "APISIX_GO_BRIDGE_PANIC_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper exited before owner recovery: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("bridge-owner-recovered")) ||
		!bytes.Contains(out, []byte("bridge-peer-closed")) {
		t.Fatalf("missing ownership markers: %s", out)
	}
}

func TestPumpRawDirectionPanicHelper(t *testing.T) {
	if os.Getenv("APISIX_GO_BRIDGE_PANIC_HELPER") != "1" {
		return
	}

	leftBase, leftPeer := net.Pipe()
	rightBase, rightPeer := net.Pipe()
	left := &panicReadConn{Conn: leftBase}
	right := &peerCloseConn{Conn: rightBase}
	defer func() { _ = leftPeer.Close() }()
	defer func() { _ = rightPeer.Close() }()
	defer func() {
		recovered := recover()
		if recovered != bridgeRawPanic {
			fmt.Fprintf(os.Stderr, "recovered panic = %#v, want %#v\n", recovered, bridgeRawPanic)
			os.Exit(1)
		}
		fmt.Println("bridge-owner-recovered")
	}()

	if err := Pump(context.Background(), left, right, nil, time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "Pump() error = %v\n", err)
		os.Exit(1)
	}
}

func TestPumpEOFWaitsForReverseDirection(t *testing.T) {
	reverseReadStarted := make(chan struct{})
	releaseReverseRead := make(chan struct{})
	left := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	right := newScriptedConn(t, func([]byte) (int, error) {
		close(reverseReadStarted)
		<-releaseReverseRead
		return 0, io.EOF
	})
	leftHalfClose := &halfCloseRecordingConn{
		Conn:           left,
		closeWriteDone: make(chan struct{}),
	}
	rightHalfClose := &halfCloseRecordingConn{
		Conn:           right,
		closeWriteDone: make(chan struct{}),
	}

	pumpDone := make(chan error, 1)
	go func() {
		pumpDone <- Pump(context.Background(), leftHalfClose, rightHalfClose, bytes.NewReader(nil), time.Second)
	}()

	select {
	case <-rightHalfClose.closeWriteDone:
	case <-time.After(time.Second):
		t.Fatal("Pump() did not half-close the first destination")
	}
	select {
	case <-reverseReadStarted:
	case <-time.After(time.Second):
		t.Fatal("reverse direction did not start")
	}
	select {
	case <-leftHalfClose.closeWriteDone:
		t.Fatal("Pump() half-closed the reverse destination before reverse completion")
	default:
	}
	select {
	case err := <-pumpDone:
		t.Fatalf("Pump() returned before reverse completion: %v", err)
	default:
	}

	close(releaseReverseRead)
	select {
	case err := <-pumpDone:
		if err != nil {
			t.Fatalf("Pump() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pump() did not return after both directions completed")
	}
	select {
	case <-leftHalfClose.closeWriteDone:
	default:
		t.Fatal("Pump() did not half-close the reverse destination")
	}
}

func TestPumpHardErrorWaitsForReverseDirection(t *testing.T) {
	firstErr := errors.New("first copy error")
	secondErr := errors.New("second copy error")
	reverseReadStarted := make(chan struct{})
	reverseCloseObserved := make(chan struct{})
	releaseReverseRead := make(chan struct{})
	left := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	var right *scriptedConn
	right = newScriptedConn(t, func([]byte) (int, error) {
		close(reverseReadStarted)
		<-right.closed
		close(reverseCloseObserved)
		<-releaseReverseRead
		return 0, secondErr
	})

	pumpDone := make(chan error, 1)
	go func() {
		pumpDone <- Pump(context.Background(), left, right, errorReader{err: firstErr}, time.Second)
	}()
	select {
	case <-reverseReadStarted:
	case <-time.After(time.Second):
		t.Fatal("reverse direction did not start")
	}
	select {
	case <-reverseCloseObserved:
	case <-time.After(time.Second):
		t.Fatal("hard copy error did not close the reverse endpoint")
	}
	select {
	case err := <-pumpDone:
		t.Fatalf("Pump() returned before reverse completion: %v", err)
	default:
	}

	close(releaseReverseRead)
	select {
	case err := <-pumpDone:
		if !errors.Is(err, firstErr) {
			t.Fatalf("Pump() error = %v, want first copy error %v", err, firstErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Pump() did not wait for reverse completion")
	}
}

func TestPumpCancellationWaitsForBothDirections(t *testing.T) {
	leftReadStarted := make(chan struct{})
	rightReadStarted := make(chan struct{})
	leftReleaseRead := make(chan struct{})
	rightReleaseRead := make(chan struct{})
	leftCloseObserved := make(chan struct{})
	rightCloseObserved := make(chan struct{})
	var left *scriptedConn
	left = newScriptedConn(t, func([]byte) (int, error) {
		close(leftReadStarted)
		<-left.closed
		close(leftCloseObserved)
		<-leftReleaseRead
		return 0, net.ErrClosed
	})
	var right *scriptedConn
	right = newScriptedConn(t, func([]byte) (int, error) {
		close(rightReadStarted)
		<-right.closed
		close(rightCloseObserved)
		<-rightReleaseRead
		return 0, net.ErrClosed
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pumpDone := make(chan error, 1)
	go func() { pumpDone <- Pump(ctx, left, right, nil, time.Second) }()
	select {
	case <-leftReadStarted:
	case <-time.After(time.Second):
		t.Fatal("left direction did not start")
	}
	select {
	case <-rightReadStarted:
	case <-time.After(time.Second):
		t.Fatal("right direction did not start")
	}

	cancel()
	select {
	case <-leftCloseObserved:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the left endpoint")
	}
	select {
	case <-rightCloseObserved:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the right endpoint")
	}
	select {
	case err := <-pumpDone:
		t.Fatalf("Pump() returned before both directions completed: %v", err)
	default:
	}

	close(leftReleaseRead)
	select {
	case err := <-pumpDone:
		t.Fatalf("Pump() returned before right direction completed: %v", err)
	default:
	}
	close(rightReleaseRead)
	select {
	case err := <-pumpDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Pump() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pump() did not return after cancellation cleanup")
	}
}

func TestPumpFirstCopyErrorPrecedence(t *testing.T) {
	firstErr := errors.New("first copy error")
	secondErr := errors.New("second copy error")
	reverseReadStarted := make(chan struct{})
	left := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	var right *scriptedConn
	right = newScriptedConn(t, func([]byte) (int, error) {
		close(reverseReadStarted)
		<-right.closed
		return 0, secondErr
	})
	leftErrorReady := make(chan struct{})
	releaseLeftError := make(chan struct{})
	firstReader := &gatedErrorReader{
		ready:   leftErrorReady,
		release: releaseLeftError,
		err:     firstErr,
	}

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- Pump(context.Background(), left, right, firstReader, time.Second) }()
	select {
	case <-reverseReadStarted:
	case <-time.After(time.Second):
		t.Fatal("reverse direction did not start")
	}
	select {
	case <-leftErrorReady:
	case <-time.After(time.Second):
		t.Fatal("first direction did not start")
	}
	close(releaseLeftError)

	select {
	case err := <-pumpDone:
		if !errors.Is(err, firstErr) || errors.Is(err, secondErr) {
			t.Fatalf("Pump() error = %v, want first error %v over second %v", err, firstErr, secondErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Pump() did not return after both errors")
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

type gatedErrorReader struct {
	ready   chan struct{}
	release chan struct{}
	err     error
}

func (r *gatedErrorReader) Read([]byte) (int, error) {
	close(r.ready)
	<-r.release
	return 0, r.err
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
