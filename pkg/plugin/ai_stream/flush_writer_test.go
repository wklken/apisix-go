package ai_stream

import (
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlushWriterSupportsSynchronousAndPeriodicFlush(t *testing.T) {
	for _, test := range []struct {
		name     string
		interval time.Duration
	}{
		{"synchronous", 0},
		{"periodic", time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			var firstWrites atomic.Int64
			writer := NewFlushWriter(recorder, test.interval, func() { firstWrites.Add(1) })
			_, _ = writer.Write([]byte("data"))
			writer.Flush()
			if test.interval > 0 {
				time.Sleep(5 * time.Millisecond)
			}
			writer.Close()
			if !recorder.Flushed || firstWrites.Load() != 1 {
				t.Fatalf("flushed = %v, first writes = %d", recorder.Flushed, firstWrites.Load())
			}
		})
	}
}
