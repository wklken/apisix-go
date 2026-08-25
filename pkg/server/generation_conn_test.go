package server

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGenerationConnCloseIsExactlyOnce(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	underlying := &countingCloseConn{Conn: left, closed: &atomic.Int32{}}
	var releases atomic.Int32
	var unregisters atomic.Int32
	connection := newGenerationConn(underlying, func() { releases.Add(1) }, func() { unregisters.Add(1) })

	const callers = 8
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			_ = connection.Close()
		}()
	}
	wait.Wait()
	if got := underlying.closed.Load(); got != 1 {
		t.Fatalf("underlying close count = %d, want 1", got)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release count = %d, want 1", got)
	}
	if got := unregisters.Load(); got != 1 {
		t.Fatalf("unregister count = %d, want 1", got)
	}
}
