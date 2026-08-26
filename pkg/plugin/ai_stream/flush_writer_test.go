package ai_stream

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type atomicFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed atomic.Bool
	flushes atomic.Int64
}

func (r *atomicFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.flushed.Store(true)
	r.flushes.Add(1)
}

type blockingFlushRecorder struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingFlushRecorder) Flush() {
	r.once.Do(func() { close(r.started) })
	<-r.release
}

func TestFlushWriterCloseJoinsPeriodicFlush(t *testing.T) {
	recorder := &blockingFlushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	writer := NewFlushWriter(context.Background(), recorder, time.Millisecond, nil)
	_, _ = writer.Write([]byte("data"))

	select {
	case <-recorder.started:
	case <-time.After(time.Second):
		t.Fatal("periodic flush did not start")
	}
	closed := make(chan struct{})
	go func() {
		writer.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while periodic Flush was blocked")
	case <-time.After(25 * time.Millisecond):
	}
	close(recorder.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join periodic Flush after release")
	}
}

func TestFlushWriterContextCancelStopsAndFlushes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recorder := &atomicFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	writer := NewFlushWriter(ctx, recorder, time.Hour, nil)
	_, _ = writer.Write([]byte("data"))
	cancel()

	deadline := time.Now().Add(time.Second)
	for recorder.flushes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := recorder.flushes.Load(); got != 1 {
		t.Fatalf("flushes after cancellation = %d, want 1 pending final flush", got)
	}
	writer.Close()
	time.Sleep(10 * time.Millisecond)
	if got := recorder.flushes.Load(); got != 1 {
		t.Fatalf("flushes after joined Close = %d, want no detached tick", got)
	}
}

func TestFlushWriterCloseFlushesWriteAfterCanceledLoopExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recorder := &atomicFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
	writer := NewFlushWriter(ctx, recorder, time.Hour, nil)
	cancel()
	if err := writer.tasks.Wait(); err != nil {
		t.Fatalf("wait for canceled loop = %v", err)
	}

	_, _ = writer.Write([]byte("late buffered data"))
	writer.Close()
	if got := recorder.flushes.Load(); got != 1 {
		t.Fatalf("flushes after canceled-loop write and Close = %d, want 1", got)
	}
}

var periodicFlushPanic = &struct{ marker string }{marker: "periodic-flush"}

type panicFlushRecorder struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    sync.Once
	panic   any
}

func (r *panicFlushRecorder) Flush() {
	r.once.Do(func() {
		if r.started != nil {
			close(r.started)
		}
	})
	if r.release != nil {
		<-r.release
	}
	panic(r.panic)
}

func TestFlushWriterPeriodicPanicReturnsFromCloseOwner(t *testing.T) {
	if os.Getenv("APISIX_GO_AI_STREAM_PANIC_HELPER") == "1" {
		recorder := &panicFlushRecorder{
			ResponseRecorder: httptest.NewRecorder(),
			panic:            periodicFlushPanic,
		}
		writer := NewFlushWriter(context.Background(), recorder, time.Millisecond, nil)
		_, _ = writer.Write([]byte("data"))
		time.Sleep(25 * time.Millisecond)
		defer func() {
			if recovered := recover(); recovered != periodicFlushPanic {
				fmt.Fprintf(os.Stderr, "unexpected panic: %#v\n", recovered)
				os.Exit(2)
			}
			fmt.Println("ai-stream-loop-joined")
			fmt.Println("ai-stream-owner-recovered")
		}()
		writer.Close()
		os.Exit(3)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFlushWriterPeriodicPanicReturnsFromCloseOwner$")
	cmd.Env = append(os.Environ(), "APISIX_GO_AI_STREAM_PANIC_HELPER=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("timer panic escaped request owner: %v\n%s", err, output)
	}
	for _, marker := range [][]byte{[]byte("ai-stream-loop-joined"), []byte("ai-stream-owner-recovered")} {
		if !bytes.Contains(output, marker) {
			t.Fatalf("missing marker %q in %s", marker, output)
		}
	}
}

func TestFlushWriterConcurrentCloseReplaysPanic(t *testing.T) {
	recorder := &panicFlushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		started:          make(chan struct{}),
		release:          make(chan struct{}),
		panic:            periodicFlushPanic,
	}
	writer := NewFlushWriter(context.Background(), recorder, time.Millisecond, nil)
	_, _ = writer.Write([]byte("data"))
	select {
	case <-recorder.started:
	case <-time.After(time.Second):
		t.Fatal("periodic flush did not start")
	}

	const callers = 8
	results := make(chan any, callers)
	for range callers {
		go func() {
			defer func() { results <- recover() }()
			writer.Close()
		}()
	}
	close(recorder.release)
	for range callers {
		if recovered := <-results; recovered != periodicFlushPanic {
			t.Fatalf("concurrent Close panic = %#v, want exact %#v", recovered, periodicFlushPanic)
		}
	}
	for range 2 {
		func() {
			defer func() {
				if recovered := recover(); recovered != periodicFlushPanic {
					t.Fatalf("repeated Close panic = %#v, want exact %#v", recovered, periodicFlushPanic)
				}
			}()
			writer.Close()
		}()
	}
}

type writePanicRecorder struct {
	*httptest.ResponseRecorder
	flushes atomic.Int64
	panic   any
}

type writeFlushPanicRecorder struct {
	*httptest.ResponseRecorder
	writePanic any
	flushPanic any
}

func (r *writeFlushPanicRecorder) Write([]byte) (int, error) {
	panic(r.writePanic)
}

func (r *writeFlushPanicRecorder) Flush() {
	panic(r.flushPanic)
}

func (r *writePanicRecorder) Write([]byte) (int, error) {
	panic(r.panic)
}

func (r *writePanicRecorder) Flush() {
	r.flushes.Add(1)
}

func TestFlushWriterWritePanicDoesNotStrandClose(t *testing.T) {
	want := &struct{ marker string }{marker: "write"}
	recorder := &writePanicRecorder{ResponseRecorder: httptest.NewRecorder(), panic: want}
	writer := NewFlushWriter(context.Background(), recorder, time.Hour, nil)
	func() {
		defer func() {
			if recovered := recover(); recovered != want {
				t.Fatalf("Write panic = %#v, want exact %#v", recovered, want)
			}
		}()
		_, _ = writer.Write([]byte("data"))
	}()

	closed := make(chan struct{})
	go func() {
		writer.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("deferred Close stranded on mutex after Write panic")
	}
	if got := recorder.flushes.Load(); got != 1 {
		t.Fatalf("final flushes = %d, want 1", got)
	}
}

func TestFlushWriterDeferredClosePreservesRequestStackPanic(t *testing.T) {
	writePanic := &struct{ marker string }{marker: "write-primary"}
	flushPanic := &struct{ marker string }{marker: "flush-cleanup"}
	recorder := &writeFlushPanicRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		writePanic:       writePanic,
		flushPanic:       flushPanic,
	}
	writer := NewFlushWriter(context.Background(), recorder, 0, nil)

	recovered := func() (recovered any) {
		defer func() { recovered = recover() }()
		defer ClosePreservingPanic(writer)
		_, _ = writer.Write([]byte("data"))
		return nil
	}()
	if recovered != writePanic {
		t.Fatalf("owner panic = %#v, want request-stack panic %#v", recovered, writePanic)
	}
}

func TestFlushWriterSupportsSynchronousAndPeriodicFlush(t *testing.T) {
	for _, test := range []struct {
		name     string
		interval time.Duration
	}{
		{"synchronous", 0},
		{"periodic", time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &atomicFlushRecorder{ResponseRecorder: httptest.NewRecorder()}
			var firstWrites atomic.Int64
			writer := NewFlushWriter(
				context.Background(),
				recorder,
				test.interval,
				func() { firstWrites.Add(1) },
			)
			writer.WriteHeader(http.StatusCreated)
			if got := recorder.Code; got != http.StatusOK {
				t.Fatalf("status before first body write = %d, want deferred 200", got)
			}
			_, _ = writer.Write([]byte("data"))
			if got := recorder.Code; got != http.StatusCreated {
				t.Fatalf("status after first body write = %d, want 201", got)
			}
			_, _ = writer.Write([]byte("more"))
			writer.Flush()
			if test.interval > 0 {
				deadline := time.Now().Add(100 * time.Millisecond)
				for !recorder.flushed.Load() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			if !recorder.flushed.Load() {
				t.Fatalf("data was not flushed before Close for interval %s", test.interval)
			}
			writer.Close()
			writer.Close()
			if firstWrites.Load() != 1 {
				t.Fatalf("first writes = %d, want 1", firstWrites.Load())
			}
		})
	}
}
