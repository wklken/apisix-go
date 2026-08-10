package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestOpenErrorForMissingDatabaseDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "store.db")
	storage, err := Open(path, make(chan *Event))
	if err == nil {
		t.Fatal("Open() error = nil for an unopenable database path")
	}
	if storage != nil {
		t.Fatal("Open() returned a store together with an error")
	}
}

func TestTransactionErrorAfterDatabaseClose(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/closed.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	storage := &Store{db: db}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	if value, err := storage.GetFromBucket("routes", []byte("route-1")); err == nil {
		t.Fatalf("GetFromBucket() = %q, nil; want the closed-database error", value)
	}
	if data, err := storage.GetBucketData("routes"); err == nil {
		t.Fatalf("GetBucketData() = %v, nil; want the closed-database error", data)
	}
}

func TestConcurrentStopIsIdempotent(t *testing.T) {
	storage, err := Open(t.TempDir()+"/concurrent-stop.db", make(chan *Event))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()

	var group sync.WaitGroup
	group.Add(2)
	stopErrors := make([]error, 2)
	for index := range 2 {
		go func() {
			defer group.Done()
			stopErrors[index] = storage.Stop()
		}()
	}
	group.Wait()
	for index, stopErr := range stopErrors {
		if stopErr != nil {
			t.Fatalf("Stop() #%d error = %v, want nil", index, stopErr)
		}
	}
}

func TestEventDuringStopDoesNotPanic(t *testing.T) {
	events := make(chan *Event)
	storage, err := Open(t.TempDir()+"/event-during-stop.db", events)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()

	stopProducers := storage.stopProducers
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for {
			select {
			case events <- &Event{Type: EventTypePut, Key: []byte("/apisix/routes/route-1"), Value: []byte(`{"id":"route-1"}`)}:
			case <-stopProducers:
				return
			}
		}
	}()

	if err := storage.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-producerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("event producer did not stop after Store.Stop")
	}
}

func TestGetFromBucketReturnsCopyStableAcrossDatabaseGrowth(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/copy.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	storage := &Store{db: db}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}

	key := []byte("route-1")
	want := bytes.Repeat([]byte("r"), 64<<10)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put(key, want)
	}); err != nil {
		t.Fatalf("store route: %v", err)
	}

	got, err := storage.GetFromBucket("routes", key)
	if err != nil {
		t.Fatalf("GetFromBucket() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("GetFromBucket() returned unexpected %d-byte value", len(got))
	}
	if err := db.View(func(tx *bolt.Tx) error {
		stored := tx.Bucket([]byte("routes")).Get(key)
		if &got[0] == &stored[0] {
			t.Fatal("GetFromBucket() returned bbolt transaction-owned storage")
		}
		return nil
	}); err != nil {
		t.Fatalf("compare stored route: %v", err)
	}

	before, err := os.Stat(db.Path())
	if err != nil {
		t.Fatalf("stat database before growth: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put([]byte("growth"), make([]byte, 1<<20))
	}); err != nil {
		t.Fatalf("grow database: %v", err)
	}
	after, err := os.Stat(db.Path())
	if err != nil {
		t.Fatalf("stat database after growth: %v", err)
	}
	if after.Size() <= before.Size() {
		t.Fatalf("database size after growth = %d, want greater than %d", after.Size(), before.Size())
	}
	if !bytes.Equal(got, want) {
		t.Fatal("GetFromBucket() changed after database growth")
	}
}

func TestGetBucketDataReturnsCopies(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/bucket-copy.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	storage := &Store{db: db}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}

	key := []byte("route-1")
	want := bytes.Repeat([]byte("r"), 64<<10)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put(key, want)
	}); err != nil {
		t.Fatalf("store route: %v", err)
	}

	got, err := storage.GetBucketData("routes")
	if err != nil {
		t.Fatalf("GetBucketData() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("GetBucketData() returned %d values, want 1", len(got))
	}
	if !bytes.Equal(got[0], want) {
		t.Fatalf("GetBucketData() returned unexpected %d-byte value", len(got[0]))
	}
	if err := db.View(func(tx *bolt.Tx) error {
		stored := tx.Bucket([]byte("routes")).Get(key)
		if &got[0][0] == &stored[0] {
			t.Fatal("GetBucketData() returned bbolt transaction-owned storage")
		}
		return nil
	}); err != nil {
		t.Fatalf("compare stored route: %v", err)
	}
}

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
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	storage.events <- &Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/route-1"),
		Value: []byte(`{"id":"route-1"}`),
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	got, err := storage.GetFromBucket("routes", []byte("route-1"))
	if err != nil {
		t.Fatalf("GetFromBucket() error = %v", err)
	}
	if got == nil {
		t.Fatal("Sync() returned before the route event was stored")
	}
}

func TestEventHooksObserveCommittedRouteMutationsBeforeSyncReturns(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/hook-order.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	storage := &Store{
		events:         make(chan *Event),
		db:             db,
		consumerKV:     map[string][]byte{},
		consumerToKeys: map[string][]string{},
	}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}
	observations := make(chan string, 2)
	storage.AddEventUpdateHook(func(event *Event) {
		stored, _ := storage.GetFromBucket("routes", []byte("route-1"))
		observations <- fmt.Sprintf("%s:%s", event.Type, stored)
	})
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	storage.events <- &Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/route-1"),
		Value: []byte(`{"id":"route-1"}`),
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after PUT error = %v", err)
	}
	storage.events <- &Event{Type: EventTypeDelete, Key: []byte("/apisix/routes/route-1")}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after DELETE error = %v", err)
	}

	if got := <-observations; got != `PUT:{"id":"route-1"}` {
		t.Fatalf("PUT hook observation = %q, want committed route", got)
	}
	if got := <-observations; got != "DELETE:" {
		t.Fatalf("DELETE hook observation = %q, want removed route", got)
	}
}

func TestFailedRouteMutationDoesNotTriggerReloadHook(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/hook-failure.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	storage := &Store{
		events:         make(chan *Event),
		db:             db,
		consumerKV:     map[string][]byte{},
		consumerToKeys: map[string][]string{},
	}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("routes")) }); err != nil {
		t.Fatalf("delete routes bucket: %v", err)
	}
	hooks := make(chan struct{}, 1)
	storage.AddEventUpdateHook(func(*Event) { hooks <- struct{}{} })
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	storage.events <- &Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/route-1"),
		Value: []byte(`{"id":"route-1"}`),
	}
	if err := storage.Sync(); err == nil {
		t.Fatal("Sync() after failed route mutation returned nil")
	}

	select {
	case <-hooks:
		t.Fatal("failed route mutation triggered a reload hook")
	default:
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
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}
	for index := range eventCount {
		id := fmt.Sprintf("route-%d", index)
		events <- &Event{
			Type:  EventTypePut,
			Key:   []byte("/apisix/routes/" + id),
			Value: []byte(`{"id":"` + id + `"}`),
		}
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	for index := range eventCount {
		id := fmt.Sprintf("route-%d", index)
		got, err := storage.GetFromBucket("routes", []byte(id))
		if err != nil {
			t.Fatalf("GetFromBucket() error = %v", err)
		}
		if got == nil {
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
		{bucket: "plugin_metadata", http: true},
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

func TestGetBucketDataReturnsCopiesOutsideReadTransaction(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/list-copy.db", 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	storage := &Store{db: db}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}
	want := bytes.Repeat([]byte("r"), 64<<10)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put([]byte("route-1"), want)
	}); err != nil {
		t.Fatalf("store route: %v", err)
	}

	got, err := storage.GetBucketData("routes")
	if err != nil {
		t.Fatalf("GetBucketData() error = %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0], want) {
		t.Fatalf("GetBucketData() = %d values, want one copied route", len(got))
	}
	if err := db.View(func(tx *bolt.Tx) error {
		stored := tx.Bucket([]byte("routes")).Get([]byte("route-1"))
		if &got[0][0] == &stored[0] {
			t.Fatal("GetBucketData() returned bbolt transaction-owned storage")
		}
		return nil
	}); err != nil {
		t.Fatalf("compare stored route: %v", err)
	}
}
