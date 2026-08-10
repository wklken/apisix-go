package store

import (
	"context"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestConfigSnapshotRetriesAfterConcurrentRouteApply(t *testing.T) {
	events := make(chan *Event, 1)
	storage, err := Open(t.TempDir()+"/snapshot.db", events)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Stop(); err != nil {
			t.Errorf("Store.Stop() error = %v", err)
		}
	})
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put(
			[]byte("route-1"),
			[]byte(`{"id":"route-1","uri":"/first"}`),
		)
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	storage.Start()

	var hookCalled bool
	storage.afterConfigSnapshotBucketRead = func(bucket string) {
		if bucket != "routes" || hookCalled {
			return
		}
		hookCalled = true

		event := NewAcknowledgedEvent()
		event.Type = EventTypePut
		event.Key = []byte("/apisix/routes/route-2")
		event.Value = []byte(`{"id":"route-2","uri":"/second"}`)
		storage.events <- event
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := event.Wait(ctx); err != nil {
			t.Errorf("concurrent route apply error = %v", err)
		}
	}

	snapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("getConfigSnapshot() error = %v", err)
	}
	if len(snapshot.Routes()) != 2 {
		t.Fatalf("snapshot routes = %d, want 2", len(snapshot.Routes()))
	}
	if got := storage.configGeneration.Load(); snapshot.generation != got {
		t.Fatalf("snapshot generation = %d, store generation = %d", snapshot.generation, got)
	}
	var foundSecond bool
	for _, route := range snapshot.Routes() {
		if route.ID == "route-2" {
			foundSecond = true
			break
		}
	}
	if !foundSecond {
		t.Fatalf("snapshot routes = %+v, want route-2", snapshot.Routes())
	}
}

func TestConfigSnapshotGenerationTracksGlobalRulesAndPluginMetadata(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)

	first, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("initial getConfigSnapshot() error = %v", err)
	}
	if first.generation != 0 {
		t.Fatalf("initial snapshot generation = %d, want 0", first.generation)
	}
	if cached, err := storage.getConfigSnapshot(); err != nil || cached != first {
		t.Fatalf("cached getConfigSnapshot() = %p/%v, want initial pointer %p", cached, err, first)
	}

	applyConfigSnapshotEvent(t, storage, EventTypePut, "/apisix/global_rules/rule-1", `{"id":"rule-1","plugins":{}}`)
	second, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("global rule getConfigSnapshot() error = %v", err)
	}
	if second == first || second.generation != 1 || len(second.GlobalRules()) != 1 {
		t.Fatalf(
			"global rule snapshot = %p generation %d rules %d, want new generation 1 with one rule",
			second,
			second.generation,
			len(second.GlobalRules()),
		)
	}

	applyConfigSnapshotEvent(
		t,
		storage,
		EventTypePut,
		"/apisix/plugin_metadata/metadata-1",
		`{"id":"metadata-1","mode":"new"}`,
	)
	third, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("plugin metadata getConfigSnapshot() error = %v", err)
	}
	metadata, ok := third.PluginMetadata("metadata-1")
	if third == second || third.generation != 2 || !ok || metadata["mode"] != "new" {
		t.Fatalf(
			"plugin metadata snapshot = %p generation %d metadata %#v/%v, want new generation 2",
			third,
			third.generation,
			metadata,
			ok,
		)
	}
}

func TestConfigSnapshotConcurrentCallersUsePublishedGeneration(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	applyConfigSnapshotEvent(t, storage, EventTypePut, "/apisix/routes/route-1", `{"id":"route-1","uri":"/orders"}`)

	const callers = 16
	results := make(chan *ConfigSnapshot, callers)
	errorsCh := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			snapshot, err := storage.getConfigSnapshot()
			if err != nil {
				errorsCh <- err
				return
			}
			results <- snapshot
		}()
	}
	group.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent getConfigSnapshot() error = %v", err)
	}
	var first *ConfigSnapshot
	for snapshot := range results {
		if first == nil {
			first = snapshot
			continue
		}
		if snapshot != first {
			t.Fatalf("concurrent snapshot pointer = %p, want %p", snapshot, first)
		}
	}
	if first == nil || first.generation != storage.configGeneration.Load() {
		t.Fatalf("concurrent snapshot generation = %v, store generation = %d", first, storage.configGeneration.Load())
	}
}

func newConfigSnapshotTestStore(t *testing.T) *Store {
	t.Helper()
	storage, err := Open(t.TempDir()+"/snapshot.db", make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() {
		if err := storage.Stop(); err != nil {
			t.Errorf("Store.Stop() error = %v", err)
		}
	})
	return storage
}

func applyConfigSnapshotEvent(t *testing.T, storage *Store, eventType EventType, key, value string) {
	t.Helper()
	event := NewAcknowledgedEvent()
	event.Type = eventType
	event.Key = []byte(key)
	event.Value = []byte(value)
	storage.events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("apply %s event: %v", key, err)
	}
}
