package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
)

func TestAcknowledgedConsumerValidationErrorIsNotRepeatedBySync(t *testing.T) {
	storage := newConsumerSnapshotStore(t)
	invalid := NewAcknowledgedEvent()
	invalid.Type = EventTypePut
	invalid.Key = []byte("/apisix/consumers/foo")
	invalid.Value = []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo"}}}`)
	storage.events <- invalid
	err := invalid.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("acknowledged invalid consumer error = %v, want password validation error", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() repeated acknowledged error = %v, want nil", err)
	}
}

func TestUnacknowledgedConsumerValidationErrorIsReturnedBySyncOnce(t *testing.T) {
	storage := newConsumerSnapshotStore(t)
	storage.events <- &Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/consumers/foo"),
		Value: []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo"}}}`),
	}
	if err := storage.Sync(); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("Sync() error = %v, want password validation error", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("second Sync() error = %v, want nil after clearing pending errors", err)
	}
}

func TestAcknowledgedWaitContextCancellationStillCompletesStoreOwnership(t *testing.T) {
	storage := newConsumerSnapshotStore(t)
	event := NewAcknowledgedEvent()
	event.Type = EventTypePut
	event.Key = []byte("/apisix/routes/route-1")
	event.Value = []byte(`{"id":"route-1"}`)
	storage.events <- event
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := event.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() after canceled acknowledged event = %v", err)
	}
}

func TestReadOnlyStoreMutationPreservesLastGoodState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read-only.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("open writable database: %v", err)
	}
	seedConsumer := []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"old"}}}`)
	seedRoute := []byte(`{"id":"route-1","uri":"/old"}`)
	if err := (&Store{db: db}).InitBuckets(); err != nil {
		t.Fatalf("InitBuckets() error = %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte("consumers")).Put([]byte("foo"), seedConsumer); err != nil {
			return err
		}
		return tx.Bucket([]byte("routes")).Put([]byte("route-1"), seedRoute)
	}); err != nil {
		t.Fatalf("seed database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writable database: %v", err)
	}

	readOnlyDB, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only database: %v", err)
	}
	storage := &Store{
		events:                  make(chan *Event, 2),
		db:                      readOnlyDB,
		consumerKV:              make(map[string][]byte),
		consumerToKeys:          make(map[string][]string),
		consumerValues:          make(map[string]resource.Consumer),
		consumerReferenceKV:     make(map[string]map[string][]byte),
		consumerToReferences:    make(map[string][]string),
		validatedPluginMetadata: newValidatedPluginMetadataCache(),
	}
	if snapshot, err := storage.prepareConsumerSnapshot([]byte("foo"), seedConsumer); err != nil {
		t.Fatalf("prepare seeded consumer: %v", err)
	} else {
		storage.applyConsumerSnapshot(snapshot)
	}
	storage.rebuildSSLCertificateIndex()
	var hookCalls int
	storage.AddEventUpdateHook(func(*Event) { hookCalls++ })
	storage.configGeneration.Store(7)
	storage.protosGeneration.Store(11)
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	updatedConsumer := NewAcknowledgedEvent()
	updatedConsumer.Type = EventTypePut
	updatedConsumer.Key = []byte("/apisix/consumers/foo")
	updatedConsumer.Value = []byte(`{"username":"foo","plugins":{"basic-auth":{"username":"foo","password":"new"}}}`)
	storage.events <- updatedConsumer
	if err := updatedConsumer.Wait(context.Background()); err == nil {
		t.Fatal("read-only consumer mutation returned nil error")
	}
	gotConsumer, err := storage.GetFromBucket("consumers", []byte("foo"))
	if err != nil || string(gotConsumer) != string(seedConsumer) {
		t.Fatalf("consumer durable state = %q, %v; want seed value", gotConsumer, err)
	}
	consumerConfig := storage.consumerValues["foo"].Plugins["basic-auth"].(map[string]any)
	if got := string(consumerConfig["password"].(string)); got != "old" {
		t.Fatalf("consumer in-memory last-good password = %q, want old", got)
	}

	updatedRoute := NewAcknowledgedEvent()
	updatedRoute.Type = EventTypePut
	updatedRoute.Key = []byte("/apisix/routes/route-1")
	updatedRoute.Value = []byte(`{"id":"route-1","uri":"/new"}`)
	storage.events <- updatedRoute
	if err := updatedRoute.Wait(context.Background()); err == nil {
		t.Fatal("read-only route mutation returned nil error")
	}
	gotRoute, err := storage.GetFromBucket("routes", []byte("route-1"))
	if err != nil || string(gotRoute) != string(seedRoute) {
		t.Fatalf("route durable state = %q, %v; want seed value", gotRoute, err)
	}
	if got := storage.configGeneration.Load(); got != 7 {
		t.Fatalf("config generation after failed route = %d, want 7", got)
	}
	if hookCalls != 0 {
		t.Fatalf("reload hooks after failed route = %d, want 0", hookCalls)
	}

	proto := NewAcknowledgedEvent()
	proto.Type = EventTypePut
	proto.Key = []byte("/apisix/protos/proto-1")
	proto.Value = []byte(`{"id":"proto-1"}`)
	storage.events <- proto
	if err := proto.Wait(context.Background()); err == nil {
		t.Fatal("read-only proto mutation returned nil error")
	}
	if got := storage.protosGeneration.Load(); got != 11 {
		t.Fatalf("proto generation after failed mutation = %d, want 11", got)
	}
}
