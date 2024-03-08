package store

import "sync"

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
}

func (e *Event) String() string {
	return e.Type.String() + " " + string(e.Key) + " : " + string(e.Value)
}

// add a event pool here, for new, save
var eventPool = sync.Pool{
	New: func() interface{} {
		return &Event{}
	},
}

func NewEvent() *Event {
	return eventPool.Get().(*Event)
}

func PutBack(event *Event) {
	// Reset event fields
	event.Type = 0
	event.Key = []byte{}
	event.Value = []byte{}

	// Save event to storage or perform other operations
	// ...

	// Put event back to the pool
	eventPool.Put(event)
}
