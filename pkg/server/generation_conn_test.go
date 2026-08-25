package server

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

type generationCloseFuncConn struct {
	net.Conn
	calls atomic.Int32
	close func() error
}

func (connection *generationCloseFuncConn) Close() error {
	connection.calls.Add(1)
	return connection.close()
}

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

func TestGenerationConnCloseContinuesCleanupAfterStagePanic(t *testing.T) {
	tests := []struct {
		name              string
		panicRawClose     bool
		panicUnregister   bool
		panicRelease      bool
		wantPanicPosition int
	}{
		{name: "raw close", panicRawClose: true, wantPanicPosition: 0},
		{name: "unregister", panicUnregister: true, wantPanicPosition: 1},
		{name: "release", panicRelease: true, wantPanicPosition: 2},
		{
			name:              "first panic wins",
			panicRawClose:     true,
			panicUnregister:   true,
			panicRelease:      true,
			wantPanicPosition: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left, right := net.Pipe()
			t.Cleanup(func() {
				_ = left.Close()
				_ = right.Close()
			})
			panics := [...]any{
				&struct{ stage string }{stage: "raw close"},
				&struct{ stage string }{stage: "unregister"},
				&struct{ stage string }{stage: "release"},
			}
			raw := &generationCloseFuncConn{Conn: left}
			raw.close = func() error {
				if test.panicRawClose {
					panic(panics[0])
				}
				return left.Close()
			}
			var unregisters atomic.Int32
			var releases atomic.Int32
			connection := newGenerationConn(raw, func() {
				releases.Add(1)
				if test.panicRelease {
					panic(panics[2])
				}
			}, func() {
				unregisters.Add(1)
				if test.panicUnregister {
					panic(panics[1])
				}
			})

			for attempt := 1; attempt <= 2; attempt++ {
				if got := recoverPanic(func() { _ = connection.Close() }); got != panics[test.wantPanicPosition] {
					t.Fatalf("Close() attempt %d panic = %#v, want %#v", attempt, got, panics[test.wantPanicPosition])
				}
			}
			if got := raw.calls.Load(); got != 1 {
				t.Fatalf("raw close count = %d, want 1", got)
			}
			if got := unregisters.Load(); got != 1 {
				t.Fatalf("unregister count = %d, want 1", got)
			}
			if got := releases.Load(); got != 1 {
				t.Fatalf("release count = %d, want 1", got)
			}
		})
	}
}

func TestGenerationConnConcurrentCloseRepanicsCachedFirstPanic(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	want := &struct{ stage string }{stage: "raw close"}
	raw := &generationCloseFuncConn{Conn: left, close: func() error { panic(want) }}
	var unregisters atomic.Int32
	var releases atomic.Int32
	connection := newGenerationConn(
		raw,
		func() { releases.Add(1) },
		func() { unregisters.Add(1) },
	)

	const callers = 8
	start := make(chan struct{})
	panics := make(chan any, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			<-start
			panics <- recoverPanic(func() { _ = connection.Close() })
		}()
	}
	close(start)
	wait.Wait()
	close(panics)
	for got := range panics {
		if got != want {
			t.Fatalf("Close() panic = %#v, want %#v", got, want)
		}
	}
	if got := raw.calls.Load(); got != 1 {
		t.Fatalf("raw close count = %d, want 1", got)
	}
	if got := unregisters.Load(); got != 1 {
		t.Fatalf("unregister count = %d, want 1", got)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release count = %d, want 1", got)
	}
}
