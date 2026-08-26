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
	goruntime "runtime"
	"sync"
	"testing"
	"time"
)

var bridgeRawPanic = &struct{ marker string }{marker: "bridge-raw-panic"}

type panicReadConn struct {
	net.Conn
	readReady     chan struct{}
	readReadyOnce sync.Once
	release       <-chan struct{}
}

func (c *panicReadConn) Read([]byte) (int, error) {
	c.readReadyOnce.Do(func() {
		if c.readReady != nil {
			close(c.readReady)
		}
	})
	if c.release != nil {
		<-c.release
	}
	panic(bridgeRawPanic)
}

type peerCloseConn struct {
	net.Conn
	readReady     chan struct{}
	readReadyOnce sync.Once
	closed        chan struct{}
	closeOnce     sync.Once
	exited        chan struct{}
	exitOnce      sync.Once
}

func (c *peerCloseConn) Read(p []byte) (int, error) {
	c.readReadyOnce.Do(func() {
		if c.readReady != nil {
			close(c.readReady)
		}
	})
	n, err := c.Conn.Read(p)
	if err != nil {
		select {
		case <-c.closed:
			fmt.Println("bridge-peer-closed")
		default:
			fmt.Fprintln(os.Stderr, "bridge-peer-read-ended-before-close")
		}
		c.exitOnce.Do(func() {
			if c.exited != nil {
				close(c.exited)
			}
		})
	}
	return n, err
}

func (c *peerCloseConn) Close() error {
	c.closeOnce.Do(func() {
		if c.closed != nil {
			close(c.closed)
		}
	})
	return c.Conn.Close()
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

type panicCloseConn struct {
	net.Conn
	panicValue     any
	closeAttempted chan struct{}
	closeOnce      sync.Once
}

func (c *panicCloseConn) Close() error {
	c.closeOnce.Do(func() {
		if c.closeAttempted != nil {
			close(c.closeAttempted)
		}
	})
	panic(c.panicValue)
}

type panicHalfCloseConn struct {
	net.Conn
	panicValue     any
	closeWriteDone chan struct{}
	closeWriteOnce sync.Once
}

type panicIsError struct {
	panicValue any
}

func (err *panicIsError) Error() string { return "panic while matching copy error" }

func (err *panicIsError) Is(error) bool { panic(err.panicValue) }

type gatedReadDeadlineErrorConn struct {
	net.Conn
	ready     chan struct{}
	readyOnce sync.Once
	release   <-chan struct{}
	err       error
}

func (c *gatedReadDeadlineErrorConn) SetReadDeadline(time.Time) error {
	c.readyOnce.Do(func() { close(c.ready) })
	<-c.release
	return c.err
}

func (c *panicHalfCloseConn) CloseWrite() error {
	c.closeWriteOnce.Do(func() { close(c.closeWriteDone) })
	panic(c.panicValue)
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
	helperCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(helperCtx, os.Args[0], "-test.run=^TestPumpRawDirectionPanicHelper$")
	cmd.Env = append(os.Environ(), "APISIX_GO_BRIDGE_PANIC_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper exited before owner recovery (ctx err %v): %v\n%s", helperCtx.Err(), err, out)
	}
	peerIndex := bytes.Index(out, []byte("bridge-peer-closed"))
	ownerIndex := bytes.Index(out, []byte("bridge-owner-recovered"))
	if peerIndex < 0 || ownerIndex < 0 {
		t.Fatalf("missing ownership markers: %s", out)
	}
	if peerIndex > ownerIndex {
		t.Fatalf("owner recovered before peer cleanup: %s", out)
	}
}

func TestPumpRawDirectionPanicHelper(t *testing.T) {
	if os.Getenv("APISIX_GO_BRIDGE_PANIC_HELPER") != "1" {
		return
	}

	leftBase, leftPeer := net.Pipe()
	rightBase, rightPeer := net.Pipe()
	leftReadReady := make(chan struct{})
	rightReadReady := make(chan struct{})
	releaseLeftRead := make(chan struct{})
	left := &panicReadConn{Conn: leftBase, readReady: leftReadReady, release: releaseLeftRead}
	right := &peerCloseConn{
		Conn:      rightBase,
		readReady: rightReadReady,
		closed:    make(chan struct{}),
		exited:    make(chan struct{}),
	}
	defer func() { _ = leftPeer.Close() }()
	defer func() { _ = rightPeer.Close() }()
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		defer func() {
			recovered := recover()
			if recovered != bridgeRawPanic {
				fmt.Fprintf(os.Stderr, "recovered panic = %#v, want %#v\n", recovered, bridgeRawPanic)
				os.Exit(1)
			}
			select {
			case <-right.exited:
			default:
				fmt.Fprintln(os.Stderr, "bridge-owner-recovered-before-peer-exit")
				os.Exit(1)
			}
			fmt.Println("bridge-owner-recovered")
		}()
		if err := Pump(context.Background(), left, right, nil, time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "Pump() error = %v\n", err)
			os.Exit(1)
		}
	}()
	<-leftReadReady
	<-rightReadReady
	close(releaseLeftRead)
	<-pumpDone
}

func TestPumpHalfClosePanicReturnsAfterPeerCleanup(t *testing.T) {
	halfClosePanic := &struct{ marker string }{marker: "half-close-panic"}
	peerErr := errors.New("peer direction completed")
	leftReader := &gatedEOFReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	left := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	reverseReadStarted := make(chan struct{})
	peerExited := make(chan struct{})
	var rightBase *scriptedConn
	rightBase = newScriptedConn(t, func([]byte) (int, error) {
		close(reverseReadStarted)
		<-rightBase.closed
		close(peerExited)
		return 0, peerErr
	})
	right := &panicHalfCloseConn{
		Conn:           rightBase,
		panicValue:     halfClosePanic,
		closeWriteDone: make(chan struct{}),
	}
	done := make(chan pumpOutcome, 1)
	go func() {
		outcome := pumpOutcome{}
		defer func() {
			outcome.panicValue = recover()
			done <- outcome
		}()
		outcome.err = Pump(context.Background(), left, right, leftReader, time.Second)
	}()
	select {
	case <-leftReader.started:
	case <-time.After(time.Second):
		t.Fatal("left direction did not start")
	}
	select {
	case <-reverseReadStarted:
	case <-time.After(time.Second):
		t.Fatal("reverse direction did not reach Read")
	}
	close(leftReader.release)
	select {
	case <-right.closeWriteDone:
	case <-time.After(time.Second):
		t.Fatal("first half-close was not attempted")
	}
	outcome := <-done
	if outcome.err != nil || outcome.panicValue != halfClosePanic {
		t.Fatalf(
			"Pump() outcome = (%v, %#v), want exact half-close panic %#v",
			outcome.err,
			outcome.panicValue,
			halfClosePanic,
		)
	}
	select {
	case <-peerExited:
	default:
		t.Fatal("Pump() replayed half-close panic before peer direction exited")
	}
	select {
	case <-left.closed:
	default:
		t.Fatal("Pump() did not attempt left endpoint close")
	}
	select {
	case <-rightBase.closed:
	default:
		t.Fatal("Pump() did not attempt right endpoint close")
	}
}

func TestPumpIncompleteDirectionWaitsForPeerErrorAfterGoexit(t *testing.T) {
	peerErr := errors.New("peer ordinary error")
	goexitStarted := make(chan struct{})
	releaseGoexit := make(chan struct{})
	left := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	rightReadStarted := make(chan struct{})
	var right *scriptedConn
	right = newScriptedConn(t, func([]byte) (int, error) {
		close(rightReadStarted)
		<-right.closed
		return 0, peerErr
	})
	done := make(chan pumpOutcome, 1)
	go func() {
		outcome := pumpOutcome{}
		defer func() {
			outcome.panicValue = recover()
			done <- outcome
		}()
		outcome.err = Pump(context.Background(), left, right, &goexitReader{
			started: goexitStarted,
			release: releaseGoexit,
		}, time.Second)
	}()
	select {
	case <-goexitStarted:
	case <-time.After(time.Second):
		t.Fatal("goexit direction did not start")
	}
	select {
	case <-rightReadStarted:
	case <-time.After(time.Second):
		t.Fatal("peer direction did not reach Read")
	}
	close(releaseGoexit)
	outcome := <-done
	if outcome.panicValue != nil {
		t.Fatalf("Pump() unexpectedly panicked: %#v", outcome.panicValue)
	}
	if !errors.Is(outcome.err, peerErr) {
		t.Fatalf("Pump() error = %v, want peer ordinary error %v", outcome.err, peerErr)
	}
}

func TestCloseBothAttemptsSecondCloseAfterFirstPanic(t *testing.T) {
	cleanupPanic := &struct{ marker string }{marker: "first-close-panic"}
	firstBase, firstPeer := net.Pipe()
	t.Cleanup(func() { _ = firstBase.Close() })
	t.Cleanup(func() { _ = firstPeer.Close() })
	firstAttempted := make(chan struct{})
	first := &panicCloseConn{Conn: firstBase, panicValue: cleanupPanic, closeAttempted: firstAttempted}
	second := newScriptedConn(t, func([]byte) (int, error) { return 0, io.EOF })

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		closeBoth(first, second)
	}()
	if recovered != cleanupPanic {
		t.Fatalf("closeBoth() panic = %#v, want %#v", recovered, cleanupPanic)
	}
	select {
	case <-firstAttempted:
	default:
		t.Fatal("first endpoint Close was not attempted")
	}
	select {
	case <-second.closed:
	default:
		t.Fatal("second endpoint Close was not attempted after first panic")
	}
}

func TestPumpChildPanicPrecedesCleanupPanicAfterJoin(t *testing.T) {
	childPanic := &struct{ marker string }{marker: "child-panic"}
	cleanupPanic := &struct{ marker string }{marker: "cleanup-panic"}
	leftBase := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	leftAttempted := make(chan struct{})
	left := &panicCloseConn{Conn: leftBase, panicValue: cleanupPanic, closeAttempted: leftAttempted}
	childReady := make(chan struct{})
	releaseChild := make(chan struct{})
	peerReadStarted := make(chan struct{})
	peerExited := make(chan struct{})
	var right *scriptedConn
	right = newScriptedConn(t, func([]byte) (int, error) {
		close(peerReadStarted)
		<-right.closed
		close(peerExited)
		return 0, net.ErrClosed
	})
	done := make(chan pumpOutcome, 1)
	go func() {
		outcome := pumpOutcome{}
		defer func() {
			outcome.panicValue = recover()
			done <- outcome
		}()
		outcome.err = Pump(context.Background(), left, right, &gatedPanicReader{
			ready:   childReady,
			release: releaseChild,
			value:   childPanic,
		}, time.Second)
	}()
	<-childReady
	<-peerReadStarted
	close(releaseChild)
	outcome := <-done
	if outcome.err != nil || outcome.panicValue != childPanic {
		t.Fatalf("Pump() outcome = (%v, %#v), want child panic %#v", outcome.err, outcome.panicValue, childPanic)
	}
	select {
	case <-leftAttempted:
	default:
		t.Fatal("cleanup panic endpoint was not attempted")
	}
	select {
	case <-right.closed:
	default:
		t.Fatal("peer endpoint was not closed")
	}
	select {
	case <-peerExited:
	default:
		t.Fatal("child panic replayed before peer direction joined")
	}
}

func TestPumpCleanupPanicReplaysAfterAllCleanup(t *testing.T) {
	copyErr := errors.New("copy failed")
	cleanupPanic := &struct{ marker string }{marker: "cleanup-only-panic"}
	leftBase := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	leftAttempted := make(chan struct{})
	left := &panicCloseConn{Conn: leftBase, panicValue: cleanupPanic, closeAttempted: leftAttempted}
	copyErrorReady := make(chan struct{})
	releaseCopyError := make(chan struct{})
	peerReadStarted := make(chan struct{})
	peerExited := make(chan struct{})
	var right *scriptedConn
	right = newScriptedConn(t, func([]byte) (int, error) {
		close(peerReadStarted)
		<-right.closed
		close(peerExited)
		return 0, net.ErrClosed
	})
	done := make(chan pumpOutcome, 1)
	go func() {
		outcome := pumpOutcome{}
		defer func() {
			outcome.panicValue = recover()
			done <- outcome
		}()
		outcome.err = Pump(context.Background(), left, right, &gatedErrorReader{
			ready:   copyErrorReady,
			release: releaseCopyError,
			err:     copyErr,
		}, time.Second)
	}()
	<-copyErrorReady
	<-peerReadStarted
	close(releaseCopyError)
	outcome := <-done
	if outcome.err != nil || outcome.panicValue != cleanupPanic {
		t.Fatalf("Pump() outcome = (%v, %#v), want cleanup panic %#v", outcome.err, outcome.panicValue, cleanupPanic)
	}
	select {
	case <-leftAttempted:
	default:
		t.Fatal("cleanup panic endpoint was not attempted")
	}
	select {
	case <-right.closed:
	default:
		t.Fatal("second endpoint was not attempted after cleanup panic")
	}
	select {
	case <-peerExited:
	default:
		t.Fatal("cleanup panic replayed before peer direction joined")
	}
}

func TestPumpErrorNormalizationPanicReturnsAfterCleanupAndJoin(t *testing.T) {
	normalizePanic := &struct{ marker string }{marker: "normalize-panic"}
	leftReader := &gatedEOFReader{started: make(chan struct{}), release: make(chan struct{})}
	left := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	peerDeadlineStarted := make(chan struct{})
	releasePeerError := make(chan struct{})
	rightBase := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("right connection should not be read")
	})
	rightDeadline := &gatedReadDeadlineErrorConn{
		Conn:    rightBase,
		ready:   peerDeadlineStarted,
		release: releasePeerError,
		err:     &panicIsError{panicValue: normalizePanic},
	}
	right := &halfCloseRecordingConn{
		Conn:           rightDeadline,
		closeWriteDone: make(chan struct{}),
	}
	done := make(chan pumpOutcome, 1)
	go func() {
		outcome := pumpOutcome{}
		defer func() {
			outcome.panicValue = recover()
			done <- outcome
		}()
		outcome.err = Pump(context.Background(), left, right, leftReader, time.Second)
	}()
	<-leftReader.started
	<-peerDeadlineStarted
	close(leftReader.release)
	<-right.closeWriteDone
	close(releasePeerError)
	outcome := <-done
	if outcome.err != nil || outcome.panicValue != normalizePanic {
		t.Fatalf(
			"Pump() outcome = (%v, %#v), want normalization panic %#v",
			outcome.err,
			outcome.panicValue,
			normalizePanic,
		)
	}
	select {
	case <-left.closed:
	default:
		t.Fatal("normalization panic bypassed left endpoint cleanup")
	}
	select {
	case <-rightBase.closed:
	default:
		t.Fatal("normalization panic bypassed right endpoint cleanup")
	}
}

func TestPumpChildPanicPrecedesOwnerAndCleanupPanics(t *testing.T) {
	childPanic := &struct{ marker string }{marker: "child-panic"}
	ownerPanic := &struct{ marker string }{marker: "half-close-panic"}
	cleanupPanic := &struct{ marker string }{marker: "cleanup-panic"}
	leftReader := &gatedEOFReader{started: make(chan struct{}), release: make(chan struct{})}
	leftBase := newScriptedConn(t, func([]byte) (int, error) {
		return 0, errors.New("left connection should not be read")
	})
	leftCloseAttempted := make(chan struct{})
	left := &panicCloseConn{
		Conn:           leftBase,
		panicValue:     cleanupPanic,
		closeAttempted: leftCloseAttempted,
	}
	peerReadStarted := make(chan struct{})
	peerExited := make(chan struct{})
	var rightBase *scriptedConn
	rightBase = newScriptedConn(t, func([]byte) (int, error) {
		close(peerReadStarted)
		<-rightBase.closed
		close(peerExited)
		panic(childPanic)
	})
	right := &panicHalfCloseConn{
		Conn:           rightBase,
		panicValue:     ownerPanic,
		closeWriteDone: make(chan struct{}),
	}
	done := make(chan pumpOutcome, 1)
	go func() {
		outcome := pumpOutcome{}
		defer func() {
			outcome.panicValue = recover()
			done <- outcome
		}()
		outcome.err = Pump(context.Background(), left, right, leftReader, time.Second)
	}()
	<-leftReader.started
	<-peerReadStarted
	close(leftReader.release)
	outcome := <-done
	if outcome.err != nil || outcome.panicValue != childPanic {
		t.Fatalf("Pump() outcome = (%v, %#v), want child panic %#v", outcome.err, outcome.panicValue, childPanic)
	}
	select {
	case <-right.closeWriteDone:
	default:
		t.Fatal("half-close owner panic was not attempted")
	}
	select {
	case <-leftCloseAttempted:
	default:
		t.Fatal("cleanup panic endpoint was not attempted")
	}
	select {
	case <-rightBase.closed:
	default:
		t.Fatal("peer endpoint was not closed")
	}
	select {
	case <-peerExited:
	default:
		t.Fatal("child panic replayed before peer direction joined")
	}
}

type pumpOutcome struct {
	err        error
	panicValue any
}

type gatedEOFReader struct {
	started chan struct{}
	release chan struct{}
}

func (r *gatedEOFReader) Read([]byte) (int, error) {
	close(r.started)
	<-r.release
	return 0, io.EOF
}

type goexitReader struct {
	started chan struct{}
	release <-chan struct{}
}

func (r *goexitReader) Read([]byte) (int, error) {
	close(r.started)
	<-r.release
	goruntime.Goexit()
	return 0, nil
}

type gatedPanicReader struct {
	ready   chan struct{}
	release <-chan struct{}
	value   any
}

func (r *gatedPanicReader) Read([]byte) (int, error) {
	close(r.ready)
	<-r.release
	panic(r.value)
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
	firstErrorReady := make(chan struct{})
	releaseFirstError := make(chan struct{})
	firstReader := &gatedErrorReader{
		ready:   firstErrorReady,
		release: releaseFirstError,
		err:     firstErr,
	}
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
		pumpDone <- Pump(context.Background(), left, right, firstReader, time.Second)
	}()
	select {
	case <-firstErrorReady:
	case <-time.After(time.Second):
		t.Fatal("first direction did not start")
	}
	select {
	case <-reverseReadStarted:
	case <-time.After(time.Second):
		t.Fatal("reverse direction did not start")
	}
	close(releaseFirstError)
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
