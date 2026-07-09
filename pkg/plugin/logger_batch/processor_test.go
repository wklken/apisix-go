package logger_batch

import (
	"fmt"
	"testing"
	"time"
)

func TestProcessorFlushesWhenBatchMaxSizeIsReached(t *testing.T) {
	delivered := make(chan []map[string]any, 1)
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    2,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		delivered <- entries
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	if !p.Push(map[string]any{"id": 2}) {
		t.Fatal("second push was rejected")
	}

	batch := waitBatch(t, delivered)
	if len(batch) != 2 {
		t.Fatalf("batch length = %d, want 2", len(batch))
	}
	if batch[0]["id"] != 1 || batch[1]["id"] != 2 {
		t.Fatalf("batch = %#v, want ids 1 and 2", batch)
	}
}

func TestProcessorFlushesAfterInactiveTimeout(t *testing.T) {
	delivered := make(chan []map[string]any, 1)
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    10,
		InactiveTimeout: 20 * time.Millisecond,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		delivered <- entries
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": "timeout"}) {
		t.Fatal("push was rejected")
	}

	batch := waitBatch(t, delivered)
	if len(batch) != 1 || batch[0]["id"] != "timeout" {
		t.Fatalf("batch = %#v, want timeout entry", batch)
	}
}

func TestProcessorDropsEntriesPastMaxPendingEntries(t *testing.T) {
	block := make(chan struct{})
	p := New(Config{
		Name:              "test logger",
		BatchMaxSize:      1,
		MaxPendingEntries: 1,
		InactiveTimeout:   time.Hour,
		BufferDuration:    time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		<-block
		return 0, nil
	})
	t.Cleanup(func() {
		close(block)
		p.Stop()
	})

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	if !p.Push(map[string]any{"id": 2}) {
		t.Fatal("second push should match APISIX pending-limit behavior")
	}
	if p.Push(map[string]any{"id": 3}) {
		t.Fatal("third push was accepted after max_pending_entries was exceeded")
	}
}

func TestProcessorRetriesFailedBatches(t *testing.T) {
	delivered := make(chan []map[string]any, 1)
	attempts := 0
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    1,
		MaxRetryCount:   1,
		RetryDelay:      10 * time.Millisecond,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		attempts++
		if attempts == 1 {
			return 0, fmt.Errorf("temporary failure")
		}
		delivered <- entries
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": "retry"}) {
		t.Fatal("push was rejected")
	}

	batch := waitBatch(t, delivered)
	if len(batch) != 1 || batch[0]["id"] != "retry" {
		t.Fatalf("batch = %#v, want retry entry", batch)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestProcessorPushDoesNotWaitForDelivery(t *testing.T) {
	block := make(chan struct{})
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    1,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		<-block
		return 0, nil
	})
	t.Cleanup(func() {
		close(block)
		p.Stop()
	})

	done := make(chan bool, 1)
	go func() {
		done <- p.Push(map[string]any{"id": "non-blocking"})
	}()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("push was rejected")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("push blocked on delivery")
	}
}

func waitBatch(t *testing.T, delivered <-chan []map[string]any) []map[string]any {
	t.Helper()

	select {
	case batch := <-delivered:
		return batch
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivered batch")
	}
	return nil
}
