package syslog

import (
	"io"
	"net"
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
