package store

import (
	"testing"
)

func TestEventTypeString(t *testing.T) {
	if got := EventTypePut.String(); got != "PUT" {
		t.Fatalf("EventTypePut.String() = %q, want PUT", got)
	}
	if got := EventTypeDelete.String(); got != "DELETE" {
		t.Fatalf("EventTypeDelete.String() = %q, want DELETE", got)
	}
	if got := EventType(99).String(); got != "UNKNOWN" {
		t.Fatalf("EventType(99).String() = %q, want UNKNOWN", got)
	}
}

func TestEventStringAndPoolRoundTrip(t *testing.T) {
	event := NewEvent()
	event.Type = EventTypePut
	event.Key = []byte("/apisix/routes/1")
	event.Value = []byte(`{"id":"1"}`)

	if got := event.String(); got != "PUT /apisix/routes/1 : {\"id\":\"1\"}" {
		t.Fatalf("Event.String() = %q", got)
	}
	PutBack(event)
}
