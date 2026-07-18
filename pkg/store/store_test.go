package store

import (
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
		flush:          make(chan chan struct{}),
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
