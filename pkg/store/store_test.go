package store

import (
	"fmt"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestSyncWaitsForQueuedEvents(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/store.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	storage := &Store{
		events:         make(chan *Event),
		db:             db,
		consumerKV:     map[string][]byte{},
		consumerToKeys: map[string][]string{},
	}
	storage.InitBuckets()
	storage.Start()
	t.Cleanup(storage.Stop)

	storage.events <- &Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/route-1"),
		Value: []byte(`{"id":"route-1"}`),
	}
	storage.Sync()

	if got := storage.GetFromBucket("routes", []byte("route-1")); got == nil {
		t.Fatal("Sync() returned before the route event was stored")
	}
}

func TestSyncWaitsForAllPrequeuedBufferedEvents(t *testing.T) {
	const eventCount = 64
	db, err := bolt.Open(t.TempDir()+"/buffered-store.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	events := make(chan *Event, eventCount)
	storage := &Store{
		events:         events,
		db:             db,
		consumerKV:     map[string][]byte{},
		consumerToKeys: map[string][]string{},
	}
	storage.InitBuckets()
	for index := range eventCount {
		id := fmt.Sprintf("route-%d", index)
		events <- &Event{
			Type:  EventTypePut,
			Key:   []byte("/apisix/routes/" + id),
			Value: []byte(`{"id":"` + id + `"}`),
		}
	}
	storage.Start()
	t.Cleanup(storage.Stop)

	storage.Sync()
	for index := range eventCount {
		id := fmt.Sprintf("route-%d", index)
		if got := storage.GetFromBucket("routes", []byte(id)); got == nil {
			t.Fatalf("Sync() returned before buffered event %q was stored", id)
		}
	}
}

func TestGetTypeAndIDFromKeyPreservesSecretManagerID(t *testing.T) {
	bucket, id := getTypeAndIDFromKey([]byte("/apisix/secrets/vault/test1"))
	if got, want := string(bucket), "secrets"; got != want {
		t.Fatalf("bucket = %q, want %q", got, want)
	}
	if got, want := string(id), "vault/test1"; got != want {
		t.Fatalf("id = %q, want %q", got, want)
	}
}

func TestRouteReloadBucketSemantics(t *testing.T) {
	tests := []struct {
		bucket string
		http   bool
		stream bool
	}{
		{bucket: "routes", http: true},
		{bucket: "services", http: true},
		{bucket: "upstreams", http: true, stream: true},
		{bucket: "stream_routes", stream: true},
		{bucket: "global_rules", http: true},
		{bucket: "plugin_configs", http: true},
		{bucket: "plugin_metadata"},
		{bucket: "consumers"},
	}

	for _, test := range tests {
		if got := IsHTTPRouteReloadBucket(test.bucket); got != test.http {
			t.Errorf("IsHTTPRouteReloadBucket(%q) = %v, want %v", test.bucket, got, test.http)
		}
		if got := IsStreamReloadBucket(test.bucket); got != test.stream {
			t.Errorf("IsStreamReloadBucket(%q) = %v, want %v", test.bucket, got, test.stream)
		}
	}
}
