package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type EventType int

const (
	EventTypePut    EventType = 0
	EventTypeDelete EventType = 1
)

func (et EventType) String() string {
	switch et {
	case EventTypePut:
		return "PUT"
	case EventTypeDelete:
		return "DELETE"
	default:
		return "UNKNOWN"
	}
}

// Mutation is a detached Store change. NewAcknowledgedBatch clones its byte
// slices so callers may reuse their source buffers after enqueueing the batch.
type Mutation struct {
	Type  EventType
	Key   []byte
	Value []byte
}

// ResourceKey identifies a persisted resource that an authoritative
// replacement must retain while applying the rest of its candidate rows.
type ResourceKey struct {
	Bucket string
	ID     string
}

// BatchOptions controls how an acknowledged batch is applied.
type BatchOptions struct {
	ReplaceManaged bool
	Preserve       []ResourceKey
}

// RejectedMutation records one deterministic validation failure by its stable
// input index.
type RejectedMutation struct {
	Index int
	Err   *ResourceValidationError
}

// BatchValidationError reports all deterministic validation failures found
// before a batch transaction begins.
type BatchValidationError struct {
	Rejected []RejectedMutation
}

func (e *BatchValidationError) Error() string {
	if e == nil || len(e.Rejected) == 0 {
		return "batch validation failed"
	}
	return fmt.Sprintf("batch validation failed: %d rejected mutation(s)", len(e.Rejected))
}

type Event struct {
	// Type is the type of event, create, update, delete
	Type EventType
	// Key is the key of the event
	Key []byte
	// Value is the value of the event
	Value []byte
	// done marks an in-order processing barrier. Barrier events are internal to Store.
	done chan struct{}
	// result carries the apply result for an acknowledged event. It is buffered
	// so the Store processor can publish the result before waiting for its waiter.
	result chan error
	// waitDone tells the processor that the waiter has consumed the result or
	// returned because its context was canceled.
	waitDone chan struct{}
	// barrier marks an acknowledged event as a Store Sync barrier rather than a
	// normal mutation.
	barrier bool
	// batch marks an acknowledged event carrying one atomic mutation set. The
	// child mutations are values rather than pooled Events.
	batch     bool
	mutations []Mutation
	options   BatchOptions
}

func (e *Event) String() string {
	return e.Type.String() + " " + string(e.Key) + " : " + string(e.Value)
}

// add a event pool here, for new, save
var eventPool = sync.Pool{
	New: func() any {
		return &Event{}
	},
}

func NewEvent() *Event {
	return eventPool.Get().(*Event)
}

// NewAcknowledgedEvent allocates an Event whose apply result can be awaited by
// its producer. The buffered result channel lets the Store processor hand off
// the result before it waits for the producer to finish with the Event.
func NewAcknowledgedEvent() *Event {
	event := NewEvent()
	event.result = make(chan error, 1)
	event.waitDone = make(chan struct{})
	return event
}

// NewAcknowledgedBatch allocates an acknowledged event containing one cloned
// mutation set. The Store applies the set in one validation/write/publication
// unit.
func NewAcknowledgedBatch(mutations []Mutation, options BatchOptions) *Event {
	event := NewAcknowledgedEvent()
	event.batch = true
	event.mutations = make([]Mutation, len(mutations))
	for index, mutation := range mutations {
		event.mutations[index] = Mutation{
			Type:  mutation.Type,
			Key:   append([]byte(nil), mutation.Key...),
			Value: append([]byte(nil), mutation.Value...),
		}
	}
	event.options = BatchOptions{
		ReplaceManaged: options.ReplaceManaged,
		Preserve:       append([]ResourceKey(nil), options.Preserve...),
	}
	if len(event.mutations) > 0 {
		event.Type = event.mutations[0].Type
		event.Key = append([]byte(nil), event.mutations[0].Key...)
		event.Value = append([]byte(nil), event.mutations[0].Value...)
	}
	return event
}

// Wait waits for one acknowledged apply result. The channel references are
// copied before selecting because the Store may pool the Event immediately
// after the waiter signals completion.
func (e *Event) Wait(ctx context.Context) error {
	result := e.result
	waitDone := e.waitDone
	if result == nil || waitDone == nil {
		return errors.New("event is not acknowledged")
	}
	defer close(waitDone)
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func PutBack(event *Event) {
	if event == nil {
		return
	}
	// Reset event fields
	event.Type = 0
	event.Key = []byte{}
	event.Value = []byte{}
	event.done = nil
	event.result = nil
	event.waitDone = nil
	event.barrier = false
	event.batch = false
	event.mutations = nil
	event.options = BatchOptions{}

	// Save event to storage or perform other operations
	// ...

	// Put event back to the pool
	eventPool.Put(event)
}
