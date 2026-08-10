package store

import (
	"context"
	"errors"
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

	// Save event to storage or perform other operations
	// ...

	// Put event back to the pool
	eventPool.Put(event)
}
