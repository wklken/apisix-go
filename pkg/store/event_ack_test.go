package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcknowledgedEventWaitDeliversApplyResultOnce(t *testing.T) {
	event := NewAcknowledgedEvent()
	event.Type = EventTypeDelete
	wantErr := errors.New("apply failed")
	processed := make(chan struct{})
	processorDone := make(chan struct{})
	go func() {
		event.result <- wantErr
		close(processed)
		<-event.waitDone
		PutBack(event)
		close(processorDone)
	}()

	select {
	case <-processed:
	case <-time.After(time.Second):
		t.Fatal("processor did not deliver the acknowledged result")
	}
	if event.Type != EventTypeDelete {
		t.Fatalf("event was reset before waiter returned: type = %v", event.Type)
	}
	if err := event.Wait(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Wait() error = %v, want %v", err, wantErr)
	}
	select {
	case <-processorDone:
	case <-time.After(time.Second):
		t.Fatal("Wait() did not release the processor")
	}
}

func TestAcknowledgedEventWaitCancellationReleasesProcessor(t *testing.T) {
	event := NewAcknowledgedEvent()
	processorDone := make(chan struct{})
	go func() {
		<-event.waitDone
		event.result <- context.Canceled
		PutBack(event)
		close(processorDone)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := event.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	select {
	case <-processorDone:
	case <-time.After(time.Second):
		t.Fatal("processor did not observe cancellation handshake")
	}
}

func TestAcknowledgedEventPoolReuseWaitsForCompletion(t *testing.T) {
	for range 128 {
		event := NewAcknowledgedEvent()
		event.Type = EventTypePut
		event.Key = []byte("/apisix/routes/route-1")
		event.Value = []byte(`{"id":"route-1"}`)
		resultReady := make(chan struct{})
		processorDone := make(chan struct{})
		go func() {
			event.result <- nil
			close(resultReady)
			<-event.waitDone
			PutBack(event)
			close(processorDone)
		}()
		<-resultReady
		if err := event.Wait(context.Background()); err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
		select {
		case <-processorDone:
		case <-time.After(time.Second):
			t.Fatal("processor did not finish pooled event")
		}
	}
}

func TestAcknowledgedBatchClonesMutations(t *testing.T) {
	mutations := []Mutation{{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/route-1"),
		Value: []byte(`{"id":"route-1"}`),
	}}
	event := NewAcknowledgedBatch(mutations, BatchOptions{})
	defer PutBack(event)

	mutations[0].Key[0] = 'x'
	mutations[0].Value[0] = 'x'
	if got := string(event.mutations[0].Key); got != "/apisix/routes/route-1" {
		t.Fatalf("batch key = %q, want cloned original", got)
	}
	if got := string(event.mutations[0].Value); got != `{"id":"route-1"}` {
		t.Fatalf("batch value = %q, want cloned original", got)
	}
}
