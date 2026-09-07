package kafka_logger

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

type blockingKafkaBatchWriter struct {
	batches chan []kafka.Message
	release chan struct{}
	closed  atomic.Bool
}

func (w *blockingKafkaBatchWriter) WriteMessages(ctx context.Context, messages ...kafka.Message) error {
	w.batches <- append([]kafka.Message(nil), messages...)
	select {
	case <-w.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *blockingKafkaBatchWriter) Close() error { w.closed.Store(true); return nil }

func TestAsyncProducerBoundsQueuedMessagesAndReleasesCapacityOnDrain(t *testing.T) {
	writer := &blockingKafkaBatchWriter{batches: make(chan []kafka.Message, 4), release: make(chan struct{}, 4)}
	sender, err := newAsyncKafkaSender(newLoggerTestTaskOwner(t), writer, 2, 2, time.Hour, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 4 {
			writer.release <- struct{}{}
		}
		_ = sender.Close()
	})
	send := func(value string) error {
		return sender.Send(context.Background(), kafkaMessage{Topic: "logs", Value: []byte(value)})
	}
	for _, value := range []string{"one", "two"} {
		if err := send(value); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case batch := <-writer.batches:
		if len(batch) != 2 || string(batch[0].Value) != "one" || string(batch[1].Value) != "two" {
			t.Fatalf("first batch=%v", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("full producer batch did not flush")
	}
	for _, value := range []string{"three", "four"} {
		if err := send(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := send("overflow"); err == nil || !strings.Contains(err.Error(), "buffer overflow") {
		t.Fatalf("overflow=%v", err)
	}
	writer.release <- struct{}{}
	select {
	case batch := <-writer.batches:
		if len(batch) != 2 || string(batch[0].Value) != "three" {
			t.Fatalf("second batch=%v", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("queued full batch did not flush after delivery")
	}
}

func TestAsyncProducerFlushesOnLingerAndClosesAdmission(t *testing.T) {
	writer := &blockingKafkaBatchWriter{batches: make(chan []kafka.Message, 2), release: make(chan struct{}, 2)}
	writer.release <- struct{}{}
	sender, err := newAsyncKafkaSender(newLoggerTestTaskOwner(t), writer, 4, 2, 20*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), kafkaMessage{Topic: "logs", Value: []byte("idle")}); err != nil {
		t.Fatal(err)
	}
	select {
	case batch := <-writer.batches:
		if len(batch) != 1 {
			t.Fatal(batch)
		}
	case <-time.After(time.Second):
		t.Fatal("idle queue did not flush")
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	if !writer.closed.Load() {
		t.Fatal("sender close returned before writer close")
	}
	if err := sender.Send(context.Background(), kafkaMessage{}); !errors.Is(err, errKafkaSenderClosed) {
		t.Fatalf("send after close=%v", err)
	}
}
