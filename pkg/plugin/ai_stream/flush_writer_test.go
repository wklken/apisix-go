package ai_stream

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type atomicFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed atomic.Bool
}

func (r *atomicFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.flushed.Store(true)
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
			writer := NewFlushWriter(recorder, test.interval, func() { firstWrites.Add(1) })
			_, _ = writer.Write([]byte("data"))
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
			if firstWrites.Load() != 1 {
				t.Fatalf("first writes = %d, want 1", firstWrites.Load())
			}
		})
	}
}
