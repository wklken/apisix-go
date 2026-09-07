package kafka_logger

import (
	"context"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

func TestStaleFullBatchTriggerDoesNotFlushLaterPartialBatch(t *testing.T) {
	writer := &blockingKafkaBatchWriter{batches: make(chan []kafka.Message, 4), release: make(chan struct{}, 4)}
	sender, err := newAsyncKafkaSender(newLoggerTestTaskOwner(t), writer, 4, 2, time.Hour, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 4 {
			writer.release <- struct{}{}
		}
		_ = sender.Close()
	})
	send := func(v string) {
		if err := sender.Send(context.Background(), kafkaMessage{Topic: "logs", Value: []byte(v)}); err != nil {
			t.Fatal(err)
		}
	}
	send("one")
	send("two")
	select {
	case <-writer.batches:
	case <-time.After(time.Second):
		t.Fatal("first batch not delivered")
	}
	// Queue a second full batch while the first is in flight. Its trigger remains
	// buffered because flush() drains this batch in its own loop.
	send("three")
	send("four")
	writer.release <- struct{}{}
	select {
	case <-writer.batches:
	case <-time.After(time.Second):
		t.Fatal("second batch not delivered")
	}
	// This partial batch arrived after the second full batch was drained.
	send("five")
	writer.release <- struct{}{}
	select {
	case batch := <-writer.batches:
		t.Fatalf("partial batch flushed before linger due to stale trigger: %q", batch[0].Value)
	case <-time.After(100 * time.Millisecond):
	}
}
