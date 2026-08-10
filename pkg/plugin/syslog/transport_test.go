package syslog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestTransportBuffersUntilExplicitFlush(t *testing.T) {
	transport, accepted, received := newTestTCPTransport(t, 100, 1000, 6)

	if _, err := transport.Log([]byte("abc")); err != nil {
		t.Fatalf("Log(abc) error = %v", err)
	}
	if _, err := transport.Log([]byte("efg")); err != nil {
		t.Fatalf("Log(efg) error = %v", err)
	}
	select {
	case <-accepted:
		t.Fatal("transport connected before explicit flush")
	case <-time.After(50 * time.Millisecond):
	}

	if err := transport.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	select {
	case payload := <-received:
		if payload != "abcefg" {
			t.Fatalf("payload = %q, want buffered messages concatenated", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for explicitly flushed payload")
	}
}

func TestTransportFlushLimitTriggersImmediateWrite(t *testing.T) {
	transport, _, received := newTestTCPTransport(t, 1, 1000, 5)

	if _, err := transport.Log([]byte("frame")); err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	select {
	case payload := <-received:
		if payload != "frame" {
			t.Fatalf("payload = %q, want frame", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("flush_limit=1 did not trigger immediate write")
	}
}

func TestTransportDropLimitFlushesBufferAndDropsCurrentMessage(t *testing.T) {
	transport, _, received := newTestTCPTransport(t, 5, 10, 4)

	if _, err := transport.Log([]byte("abcd")); err != nil {
		t.Fatalf("Log(buffered) error = %v", err)
	}
	written, err := transport.Log([]byte("1234567"))
	if err != nil {
		t.Fatalf("Log(dropped) error = %v", err)
	}
	if written != 0 {
		t.Fatalf("Log(dropped) bytes = %d, want 0", written)
	}
	select {
	case payload := <-received:
		if payload != "abcd" {
			t.Fatalf("payload = %q, want prior buffer only", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered payload")
	}
	if stats := transport.Stats(); stats.Dropped != 1 {
		t.Fatalf("transport dropped = %d, want 1", stats.Dropped)
	}
}

func TestTransportRetainsBufferedFramesAfterDialFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve tcp address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	transport, err := newSyslogTransport(Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		FlushLimit: 100,
		DropLimit:  1000,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err != nil {
		t.Fatalf("newSyslogTransport() error = %v", err)
	}
	t.Cleanup(transport.Close)

	if _, err := transport.Log([]byte("first")); err != nil {
		t.Fatalf("Log(first) error = %v", err)
	}
	if err := transport.Flush(); err == nil {
		t.Fatal("Flush() error = nil, want dial failure")
	}
	if stats := transport.Stats(); stats.Buffered != len("first") {
		t.Fatalf("buffered bytes after dial failure = %d, want %d", stats.Buffered, len("first"))
	}

	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen for recovery: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		payload := make([]byte, len("firstsecond"))
		if _, readErr := io.ReadFull(connection, payload); readErr == nil {
			received <- string(payload)
		}
	}()

	if _, err := transport.Log([]byte("second")); err != nil {
		t.Fatalf("Log(second) error = %v", err)
	}
	if err := transport.Flush(); err != nil {
		t.Fatalf("recovery Flush() error = %v", err)
	}
	select {
	case payload := <-received:
		if payload != "firstsecond" {
			t.Fatalf("recovery payload = %q, want firstsecond", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for preserved dial-failure payload")
	}
}

func TestTransportDiscardsAmbiguouslySplitFramesAfterPartialWriteFailure(t *testing.T) {
	transport, err := newSyslogTransport(Config{
		Host:       "127.0.0.1",
		Port:       1,
		FlushLimit: 9,
		DropLimit:  1000,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err != nil {
		t.Fatalf("newSyslogTransport() error = %v", err)
	}
	t.Cleanup(transport.Close)

	if _, err := transport.Log([]byte("abcdef")); err != nil {
		t.Fatalf("Log(abcdef) error = %v", err)
	}
	writeFailure := errors.New("injected write failure")
	partial := &scriptedConn{results: []writeResult{{n: 3, err: writeFailure}}}
	transport.idle <- partial
	if err := transport.Flush(); !errors.Is(err, writeFailure) {
		t.Fatalf("Flush() error = %v, want injected write failure", err)
	}
	if got := partial.Written(); got != "abc" {
		t.Fatalf("partially written payload = %q, want abc", got)
	}
	if stats := transport.Stats(); stats.Buffered != 0 {
		t.Fatalf("buffered bytes after partial write = %d, want 0 to discard the orphan suffix", stats.Buffered)
	}

	recovery := &scriptedConn{}
	transport.idle <- recovery
	if _, err := transport.Log([]byte("ghijkl")); err != nil {
		t.Fatalf("recovery Log() error = %v", err)
	}
	if err := transport.Flush(); err != nil {
		t.Fatalf("recovery Flush() error = %v", err)
	}
	if got := recovery.Written(); got != "ghijkl" {
		t.Fatalf("recovery payload = %q, want exactly ghijkl and never defghijkl", got)
	}
}

func TestSendBodyReturnsErrorAfterAmbiguousPartialWrite(t *testing.T) {
	transport, err := newSyslogTransport(Config{
		Host:       "127.0.0.1",
		Port:       1,
		FlushLimit: 5,
		DropLimit:  1000,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err != nil {
		t.Fatalf("newSyslogTransport() error = %v", err)
	}
	t.Cleanup(transport.Close)

	if _, err := transport.Log([]byte("old")); err != nil {
		t.Fatalf("Log(old) error = %v", err)
	}
	writeFailure := errors.New("injected write failure")
	partial := &scriptedConn{results: []writeResult{{n: 4, err: writeFailure}}}
	transport.idle <- partial

	plugin := &Plugin{transport: transport}
	if err := plugin.sendBody(context.Background(), []byte("new")); !errors.Is(err, writeFailure) {
		t.Fatalf("sendBody(new) error = %v, want caller-owned retry for an ambiguous partial write", err)
	}
	if got := partial.Written(); got != "oldn" {
		t.Fatalf("partially written payload = %q, want oldn", got)
	}
	if stats := transport.Stats(); stats.Buffered != 0 {
		t.Fatalf("buffered bytes after ambiguous partial write = %d, want 0", stats.Buffered)
	}

	recovery := &scriptedConn{}
	transport.idle <- recovery
	if err := plugin.sendBody(context.Background(), []byte("new")); err != nil {
		t.Fatalf("retry sendBody(new) error = %v", err)
	}
	if err := transport.Flush(); err != nil {
		t.Fatalf("recovery Flush() error = %v", err)
	}
	if got := recovery.Written(); got != "new" {
		t.Fatalf("recovery payload = %q, want the full message without an orphan suffix", got)
	}
}

func TestSendBodyRetriesAfterPartialWriteDiscardsPriorBuffer(t *testing.T) {
	transport, err := newSyslogTransport(Config{
		Host:       "127.0.0.1",
		Port:       1,
		FlushLimit: 8,
		DropLimit:  1000,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err != nil {
		t.Fatalf("newSyslogTransport() error = %v", err)
	}
	t.Cleanup(transport.Close)

	if _, err := transport.Log([]byte("older")); err != nil {
		t.Fatalf("Log(older) error = %v", err)
	}
	writeFailure := errors.New("injected write failure")
	partial := &scriptedConn{results: []writeResult{{n: 2, err: writeFailure}}}
	transport.idle <- partial

	plugin := &Plugin{transport: transport}
	if err := plugin.sendBody(context.Background(), []byte("new")); !errors.Is(err, writeFailure) {
		t.Fatalf("sendBody(new) error = %v, want caller-owned retry", err)
	}
	if got := partial.Written(); got != "ol" {
		t.Fatalf("partially written payload = %q, want ol", got)
	}
	if stats := transport.Stats(); stats.Buffered != 0 {
		t.Fatalf("buffered bytes after partial write = %d, want 0 (prior batch discarded)", stats.Buffered)
	}

	recovery := &scriptedConn{}
	transport.idle <- recovery
	if err := plugin.sendBody(context.Background(), []byte("new")); err != nil {
		t.Fatalf("retry sendBody(new) error = %v", err)
	}
	if err := transport.Flush(); err != nil {
		t.Fatalf("recovery Flush() error = %v", err)
	}
	if got := recovery.Written(); got != "new" {
		t.Fatalf("recovery payload = %q, want only the retried message without a prior suffix", got)
	}
}

func TestTransportTreatsZeroByteWriteAsNoProgress(t *testing.T) {
	transport, err := newSyslogTransport(Config{
		Host:       "127.0.0.1",
		Port:       1,
		FlushLimit: 100,
		DropLimit:  1000,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err != nil {
		t.Fatalf("newSyslogTransport() error = %v", err)
	}

	if _, err := transport.Log([]byte("frame")); err != nil {
		t.Fatalf("Log(frame) error = %v", err)
	}
	connection := &scriptedConn{
		results: []writeResult{
			{},
			{err: errors.New("write called after zero progress")},
		},
	}
	transport.idle <- connection
	if err := transport.Flush(); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("Flush() error = %v, want io.ErrNoProgress", err)
	}
	if connection.WriteCalls() != 1 {
		t.Fatalf("Write() calls = %d, want one bounded attempt", connection.WriteCalls())
	}
	if stats := transport.Stats(); stats.Buffered != len("frame") {
		t.Fatalf("buffered bytes after zero progress = %d, want %d", stats.Buffered, len("frame"))
	}
}

func TestTransportDropLimitStaysBoundedWhenBufferedFlushFails(t *testing.T) {
	transport, err := newSyslogTransport(Config{
		Host:       "127.0.0.1",
		Port:       1,
		FlushLimit: 5,
		DropLimit:  10,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err != nil {
		t.Fatalf("newSyslogTransport() error = %v", err)
	}

	if _, err := transport.Log([]byte("abcd")); err != nil {
		t.Fatalf("Log(abcd) error = %v", err)
	}
	writeFailure := errors.New("injected write failure")
	transport.idle <- &scriptedConn{results: []writeResult{{err: writeFailure}}}
	written, err := transport.Log([]byte("1234567"))
	if written != 0 || !errors.Is(err, writeFailure) {
		t.Fatalf("Log(dropped) = %d, %v; want 0 and injected write failure", written, err)
	}
	stats := transport.Stats()
	if stats.Buffered != len("abcd") || stats.Dropped != 1 {
		t.Fatalf("transport stats = %+v, want four buffered bytes and one dropped message", stats)
	}

	recovery := &scriptedConn{}
	transport.idle <- recovery
	transport.Close()
	if got := recovery.Written(); got != "abcd" {
		t.Fatalf("shutdown recovery payload = %q, want bounded retained buffer", got)
	}
}

func TestTransportReusesTCPConnectionFromConfiguredPool(t *testing.T) {
	transport, _, received := newTestTCPTransport(t, 1, 1000, 3, 3)
	if capacity := cap(transport.idle); capacity != 5 {
		t.Fatalf("connection pool capacity = %d, want configured 5", capacity)
	}

	if _, err := transport.Log([]byte("one")); err != nil {
		t.Fatalf("Log(one) error = %v", err)
	}
	if payload := <-received; payload != "one" {
		t.Fatalf("first payload = %q, want one", payload)
	}
	if _, err := transport.Log([]byte("two")); err != nil {
		t.Fatalf("Log(two) error = %v", err)
	}
	select {
	case payload := <-received:
		if payload != "two" {
			t.Fatalf("second payload = %q, want two", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("second payload did not reuse accepted TCP connection")
	}
}

func TestTransportRejectsFlushLimitAtOrAboveDropLimit(t *testing.T) {
	_, err := newSyslogTransport(Config{
		Host:       "127.0.0.1",
		Port:       514,
		FlushLimit: 10,
		DropLimit:  10,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err == nil {
		t.Fatal("newSyslogTransport() error = nil, want invalid limit rejection")
	}
}

func newTestTCPTransport(
	t *testing.T,
	flushLimit int,
	dropLimit int,
	expectedLengths ...int,
) (*syslogTransport, <-chan struct{}, <-chan string) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan struct{}, 1)
	received := make(chan string, 2)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		accepted <- struct{}{}
		for _, expectedLength := range expectedLengths {
			buffer := make([]byte, expectedLength)
			count, readErr := io.ReadFull(connection, buffer)
			if count > 0 {
				received <- string(buffer[:count])
			}
			if readErr != nil {
				return
			}
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	transport, err := newSyslogTransport(Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		FlushLimit: flushLimit,
		DropLimit:  dropLimit,
		Timeout:    100,
		PoolSize:   5,
		SockType:   "tcp",
	})
	if err != nil {
		t.Fatalf("newSyslogTransport() error = %v", err)
	}
	t.Cleanup(transport.Close)
	return transport, accepted, received
}

type writeResult struct {
	n   int
	err error
}

type scriptedConn struct {
	mu      sync.Mutex
	results []writeResult
	written bytes.Buffer
	calls   int
	closed  bool
}

func (c *scriptedConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *scriptedConn) Write(payload []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls++
	result := writeResult{n: len(payload)}
	if len(c.results) > 0 {
		result = c.results[0]
		c.results = c.results[1:]
	}
	if result.n > len(payload) {
		result.n = len(payload)
	}
	if result.n > 0 {
		_, _ = c.written.Write(payload[:result.n])
	}
	return result.n, result.err
}

func (c *scriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *scriptedConn) LocalAddr() net.Addr {
	return testAddr("local")
}

func (c *scriptedConn) RemoteAddr() net.Addr {
	return testAddr("remote")
}

func (c *scriptedConn) SetDeadline(time.Time) error {
	return nil
}

func (c *scriptedConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *scriptedConn) SetWriteDeadline(time.Time) error {
	return nil
}

func (c *scriptedConn) Written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.String()
}

func (c *scriptedConn) WriteCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type testAddr string

func (a testAddr) Network() string {
	return "test"
}

func (a testAddr) String() string {
	return string(a)
}
