package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
)

func TestProcessEventWrapsOnlyResourceValidationFailures(t *testing.T) {
	db, err := bolt.Open(filepath.Join(t.TempDir(), "validation.db"), 0o600, nil)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	storage := &Store{
		db:                      db,
		consumerKV:              map[string][]byte{},
		consumerToKeys:          map[string][]string{},
		consumerValues:          map[string]resource.Consumer{},
		consumerReferenceKV:     map[string]map[string][]byte{},
		consumerToReferences:    map[string][]string{},
		validatedPluginMetadata: newValidatedPluginMetadataCache(),
	}
	if err := storage.InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}

	err = storage.processEvent(&Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/consumers/alice"),
		Value: []byte(`{"username":"alice","plugins":{"basic-auth":{"username":"alice"}}}`),
	})
	var validationErr *ResourceValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("consumer validation error = %v, want ResourceValidationError", err)
	}
	if validationErr.Bucket != "consumers" || validationErr.ID != "alice" {
		t.Fatalf("validation context = %q/%q, want consumers/alice", validationErr.Bucket, validationErr.ID)
	}
	err = storage.processEvent(&Event{
		Type:  EventType(99),
		Key:   []byte("/apisix/ssls/bad"),
		Value: []byte(`{"id":"bad"}`),
	})
	if errors.As(err, &validationErr) {
		t.Fatalf("unsupported SSL event was wrapped as ResourceValidationError: %v", err)
	}
	if err == nil {
		t.Fatal("unsupported SSL event error = nil")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	err = storage.processEvent(&Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/route-1"),
		Value: []byte(`{"id":"route-1"}`),
	})
	if errors.As(err, &validationErr) {
		t.Fatalf("persistence error was wrapped as ResourceValidationError: %v", err)
	}
	if err == nil {
		t.Fatal("closed database persistence error = nil")
	}
}

func TestRouteAndGlobalRulePutValidationRetainsLastGoodAndSkipsHooks(t *testing.T) {
	storage, err := Open(
		filepath.Join(t.TempDir(), "resource-validation.db"),
		make(chan *Event, 8),
		testDataEncryption(),
	)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	var hookCalls int
	storage.AddEventUpdateHook(func(*Event) { hookCalls++ })
	apply := func(key string, value []byte) error {
		event := NewAcknowledgedEvent()
		event.Type = EventTypePut
		event.Key = []byte(key)
		event.Value = value
		storage.events <- event
		return event.Wait(context.Background())
	}
	for _, test := range []struct {
		bucket string
		id     string
		good   []byte
		bad    []byte
	}{
		{
			bucket: "routes",
			id:     "route-1",
			good:   []byte(`{"id":"route-1","uri":"/last-good"}`),
			bad:    []byte(`{"id":"route-1","uri":"/bad","plugins":[]}`),
		},
		{
			bucket: "global_rules",
			id:     "rule-1",
			good:   []byte(`{"id":"rule-1","plugins":{}}`),
			bad:    []byte(`{"id":"rule-1","plugins":[]}`),
		},
	} {
		t.Run(test.bucket, func(t *testing.T) {
			key := "/apisix/" + test.bucket + "/" + test.id
			if err := apply(key, test.good); err != nil {
				t.Fatalf("apply last-good resource: %v", err)
			}
			before, err := storage.GetFromBucket(test.bucket, []byte(test.id))
			if err != nil {
				t.Fatalf("read last-good resource: %v", err)
			}
			if err := apply(key, test.bad); err == nil {
				t.Fatal("malformed resource PUT error = nil")
			} else {
				var validationErr *ResourceValidationError
				if !errors.As(err, &validationErr) {
					t.Fatalf("malformed resource PUT error = %v, want ResourceValidationError", err)
				}
				if validationErr.Bucket != test.bucket || validationErr.ID != test.id {
					t.Fatalf(
						"validation context = %q/%q, want %q/%q",
						validationErr.Bucket,
						validationErr.ID,
						test.bucket,
						test.id,
					)
				}
			}
			after, err := storage.GetFromBucket(test.bucket, []byte(test.id))
			if err != nil {
				t.Fatalf("read retained resource: %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("retained %s bytes = %q, want %q", test.bucket, after, before)
			}
		})
	}
	if hookCalls != 2 {
		t.Fatalf("reload hook calls = %d, want only successful PUTs", hookCalls)
	}
}

func TestOpenErrorForMissingDatabaseDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "store.db")
	storage, err := Open(path, make(chan *Event), testDataEncryption())
	if err == nil {
		t.Fatal("Open() error = nil for an unopenable database path")
	}
	if storage != nil {
		t.Fatal("Open() returned a store together with an error")
	}
}

func TestGetStoreReopensAfterStopWithDifferentEventChannel(t *testing.T) {
	previous := ReplaceGlobalStoreForTest(nil)
	t.Cleanup(func() { ReplaceGlobalStoreForTest(previous) })

	path := filepath.Join(t.TempDir(), "reopen.db")
	firstEvents := make(chan *Event)
	first, err := GetStore(path, firstEvents, testDataEncryption())
	if err != nil {
		t.Fatalf("first GetStore() error = %v", err)
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("first Store.Stop() error = %v", err)
	}

	secondEvents := make(chan *Event)
	second, err := GetStore(path, secondEvents, testDataEncryption())
	if err != nil {
		t.Fatalf("second GetStore() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Stop() })
	if second == first {
		t.Fatal("GetStore() returned the stopped Store instance")
	}
	if second.events != secondEvents {
		t.Fatal("reopened Store retained the old event channel")
	}
	if second.events == firstEvents {
		t.Fatal("reopened Store shares the old event channel")
	}
}

func TestStopClearsOnlyMatchingGlobalStore(t *testing.T) {
	previous := ReplaceGlobalStoreForTest(nil)
	t.Cleanup(func() { ReplaceGlobalStoreForTest(previous) })

	old, err := Open(filepath.Join(t.TempDir(), "old.db"), make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("Open(old) error = %v", err)
	}
	newer, err := GetStore(filepath.Join(t.TempDir(), "new.db"), make(chan *Event), testDataEncryption())
	if err != nil {
		_ = old.Stop()
		t.Fatalf("GetStore(new) error = %v", err)
	}
	t.Cleanup(func() { _ = newer.Stop() })

	if err := old.Stop(); err != nil {
		t.Fatalf("old Store.Stop() error = %v", err)
	}
	got, err := GetStore(filepath.Join(t.TempDir(), "unexpected.db"), make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("GetStore() after non-global Stop error = %v", err)
	}
	if got != newer {
		t.Fatal("stopping a non-global Store cleared the newer global singleton")
	}
}

func TestStopHookCanReenterGetStore(t *testing.T) {
	previous := ReplaceGlobalStoreForTest(nil)
	path := filepath.Join(t.TempDir(), "hook-reentrancy.db")
	storage, err := Open(path, make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ReplaceGlobalStoreForTest(storage)

	hookEntered := make(chan struct{})
	stopSignal := make(chan bool, 1)
	lookupResult := make(chan struct {
		storage *Store
		err     error
	}, 1)
	releaseHook := make(chan struct{})
	storage.AddEventUpdateHook(func(*Event) {
		close(hookEntered)
		observedStop := false
		select {
		case <-storage.stopProducers:
			observedStop = true
		case <-time.After(500 * time.Millisecond):
		}
		stopSignal <- observedStop
		go func() {
			got, lookupErr := GetStore(path, make(chan *Event), testDataEncryption())
			lookupResult <- struct {
				storage *Store
				err     error
			}{storage: got, err: lookupErr}
		}()
		if observedStop {
			<-releaseHook
		}
	})
	storage.Start()
	go func() {
		storage.events <- &Event{
			Type:  EventTypePut,
			Key:   []byte("/apisix/routes/route-1"),
			Value: []byte(`{"id":"route-1"}`),
		}
	}()
	select {
	case <-hookEntered:
	case <-time.After(time.Second):
		t.Fatal("store hook did not start")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- storage.Stop() }()
	observedStop := <-stopSignal
	lookup := <-lookupResult
	if observedStop {
		close(releaseHook)
	}
	stopErr := <-stopDone
	if lookup.storage != nil && lookup.storage != storage {
		_ = lookup.storage.Stop()
	}
	if stopErr != nil {
		t.Fatalf("Store.Stop() error = %v", stopErr)
	}
	if !observedStop {
		t.Fatal("Store.Stop() did not signal the hook before waiting for it")
	}
	if !errors.Is(lookup.err, errStoreStopped) {
		t.Fatalf("GetStore() from Store hook error = %v, want errStoreStopped", lookup.err)
	}
	if lookup.storage != nil {
		t.Fatal("GetStore() returned the stopping Store to the reentrant hook")
	}

	reopened, err := GetStore(path, make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("GetStore() after Stop error = %v", err)
	}
	if reopened == storage {
		t.Fatal("GetStore() after Stop returned the closed Store")
	}
	_ = reopened.Stop()
	ReplaceGlobalStoreForTest(previous)
}

func TestSyncReturnsStoreStoppedBeforeAndDuringStop(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "sync-stop.db"), make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := storage.Stop(); err != nil {
		t.Fatalf("Store.Stop() error = %v", err)
	}
	assertSyncStopped := func(label string) {
		done := make(chan error, 1)
		go func() { done <- storage.Sync() }()
		select {
		case syncErr := <-done:
			if !errors.Is(syncErr, errStoreStopped) {
				t.Fatalf("Sync() %s error = %v, want errStoreStopped", label, syncErr)
			}
		case <-time.After(time.Second):
			t.Fatalf("Sync() %s remained blocked after Store.Stop", label)
		}
	}
	assertSyncStopped("before")

	active, err := Open(filepath.Join(t.TempDir(), "sync-concurrent-stop.db"), make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("Open(active) error = %v", err)
	}
	active.Start()
	stopDone := make(chan error, 1)
	go func() { stopDone <- active.Stop() }()
	select {
	case <-active.stopProducers:
	case <-time.After(time.Second):
		t.Fatal("Store.Stop() did not enter stopping state")
	}
	done := make(chan error, 1)
	go func() { done <- active.Sync() }()
	select {
	case syncErr := <-done:
		if !errors.Is(syncErr, errStoreStopped) {
			t.Fatalf("concurrent Sync() error = %v, want errStoreStopped", syncErr)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent Sync() remained blocked while Store.Stop was active")
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("active Store.Stop() error = %v", err)
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
	storage, err := Open(t.TempDir()+"/concurrent-stop.db", make(chan *Event), testDataEncryption())
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
	storage, err := Open(t.TempDir()+"/event-during-stop.db", events, testDataEncryption())
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
	bucket, id, err := getTypeAndIDFromKey([]byte("/apisix/secrets/vault/test1"))
	if err != nil {
		t.Fatalf("getTypeAndIDFromKey() error = %v", err)
	}
	if got, want := string(bucket), "secrets"; got != want {
		t.Fatalf("bucket = %q, want %q", got, want)
	}
	if got, want := string(id), "vault/test1"; got != want {
		t.Fatalf("id = %q, want %q", got, want)
	}
}

func TestGetTypeAndIDFromKeyPreservesPrefixWithoutLeadingSlash(t *testing.T) {
	bucket, id, err := getTypeAndIDFromKey([]byte("apisix/routes/route-1"))
	if err != nil {
		t.Fatalf("getTypeAndIDFromKey() error = %v", err)
	}
	if got, want := string(bucket), "routes"; got != want {
		t.Fatalf("bucket = %q, want %q", got, want)
	}
	if got, want := string(id), "route-1"; got != want {
		t.Fatalf("id = %q, want %q", got, want)
	}
}

func TestParseMutationKeySuccessfulShapesUseCanonicalMembership(t *testing.T) {
	for _, key := range []string{
		"/apisix/routes/route-1",
		"/apisix/plugins",
		"/apisix/secrets/vault/item",
	} {
		bucket, _, err := parseMutationKey([]byte(key))
		if err != nil {
			t.Fatalf("parseMutationKey(%q) error = %v", key, err)
		}
		if !generation.IsManagedResourceKind(bucket) {
			t.Errorf("parseMutationKey(%q) returned noncanonical bucket %q", key, bucket)
		}
	}
}

func TestProcessEventRejectsMalformedKeysWithoutPanic(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "malformed-key.db"), make(chan *Event), testDataEncryption())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Stop() })

	tests := []struct {
		name string
		key  string
	}{
		{name: "single segment", key: "malformed"},
		{name: "missing bucket", key: "/apisix//route-1"},
		{name: "empty id", key: "/apisix/routes/"},
		{name: "missing secret manager", key: "/apisix/secrets//secret-1"},
		{name: "missing secret name", key: "/apisix/secrets/vault/"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := &Event{Type: EventTypePut, Key: []byte(test.key)}
			firstErr := storage.processEvent(event)
			var validationErr *ResourceValidationError
			if !errors.As(firstErr, &validationErr) {
				t.Fatalf("processEvent(%q) error = %v, want ResourceValidationError", test.key, firstErr)
			}
			if _, ok := EventBucket(event); ok {
				t.Fatalf("EventBucket(%q) = ok, want invalid", test.key)
			}

			secondErr := storage.processEvent(event)
			if secondErr == nil || secondErr.Error() != firstErr.Error() {
				t.Fatalf("processEvent(%q) error changed: first=%v second=%v", test.key, firstErr, secondErr)
			}
		})
	}

	storage.Start()
	acknowledged := NewAcknowledgedEvent()
	acknowledged.Type = EventTypePut
	acknowledged.Key = []byte("malformed")
	storage.events <- acknowledged
	acknowledgedErr := acknowledged.Wait(context.Background())
	var validationErr *ResourceValidationError
	if !errors.As(acknowledgedErr, &validationErr) {
		t.Fatalf("acknowledged malformed key error = %v, want ResourceValidationError", acknowledgedErr)
	}
}

func TestRouteReloadBucketSemantics(t *testing.T) {
	tests := []struct {
		bucket string
		http   bool
		stream bool
	}{
		{bucket: "routes", http: true},
		{bucket: "services", http: true, stream: true},
		{bucket: "upstreams", http: true, stream: true},
		{bucket: "stream_routes", stream: true},
		{bucket: "global_rules", http: true},
		{bucket: "plugin_configs", http: true},
		{bucket: "plugin_metadata", http: true},
		{bucket: "ssls", http: true},
		{bucket: "protos", http: true},
		{bucket: "consumer_groups"},
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
