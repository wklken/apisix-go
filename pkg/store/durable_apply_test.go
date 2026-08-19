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

func TestAcknowledgedBatchValidationIsAtomic(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "batch-validation.db"), make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	event := NewAcknowledgedBatch([]Mutation{
		{
			Type:  EventTypePut,
			Key:   []byte("/apisix/routes/route-good"),
			Value: []byte(`{"id":"route-good","uri":"/good"}`),
		},
		{
			Type:  EventTypePut,
			Key:   []byte("/apisix/routes/route-bad"),
			Value: []byte(`{"id":"route-bad","plugins":[]}`),
		},
	}, BatchOptions{})
	storage.events <- event
	err = event.Wait(context.Background())
	var batchErr *BatchValidationError
	if !errors.As(err, &batchErr) || len(batchErr.Rejected) != 1 {
		t.Fatalf("batch error = %v, want one validation rejection", err)
	}
	if batchErr.Rejected[0].Index != 1 {
		t.Fatalf("rejected index = %d, want 1", batchErr.Rejected[0].Index)
	}
	if value, readErr := storage.GetFromBucket("routes", []byte("route-good")); readErr != nil {
		t.Fatalf("read route-good: %v", readErr)
	} else if value != nil {
		t.Fatalf("route-good persisted after rejected batch: %q", value)
	}
}

func TestAcknowledgedBatchAcceptsCustomPrefixResourceKeys(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "batch-custom-prefix.db"), make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	event := NewAcknowledgedBatch([]Mutation{
		{
			Type:  EventTypePut,
			Key:   []byte("/custom/root/routes/route-1"),
			Value: []byte(`{"id":"route-1","uri":"/custom"}`),
		},
		{
			Type:  EventTypePut,
			Key:   []byte("/custom/root/secrets/vault/item"),
			Value: []byte(`{"id":"vault/item","value":"secret"}`),
		},
	}, BatchOptions{})
	storage.events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("custom-prefix batch apply: %v", err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("route-1")); err != nil {
		t.Fatalf("read custom-prefix route: %v", err)
	} else if value == nil {
		t.Fatal("custom-prefix route was not persisted")
	}
	if value, err := storage.GetFromBucket("secrets", []byte("vault/item")); err != nil {
		t.Fatalf("read custom-prefix secret: %v", err)
	} else if value == nil {
		t.Fatal("custom-prefix secret was not persisted")
	}
}

func TestAuthoritativeReplacementRemovesPersistedRowsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authoritative-restart.db")
	first, err := Open(path, make(chan *Event, 1))
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if err := first.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte("routes")).
			Put([]byte("stale-route"), []byte(`{"id":"stale-route","uri":"/stale"}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("services")).
			Put([]byte("stale-service"), []byte(`{"id":"stale-service"}`)); err != nil {
			return err
		}
		return tx.Bucket([]byte("consumers")).Put(
			[]byte("stale-consumer"),
			[]byte(`{"username":"stale-consumer","plugins":{"key-auth":{"key":"stale-key"}}}`),
		)
	}); err != nil {
		t.Fatalf("seed stale rows: %v", err)
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("first Stop() error = %v", err)
	}

	second, err := Open(path, make(chan *Event, 1))
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	second.Start()
	t.Cleanup(func() { _ = second.Stop() })
	if _, err := second.GetConsumerNameByPluginKey("key-auth", "stale-key"); err != nil {
		t.Fatalf("rebuilt stale consumer lookup = %v, want persisted index", err)
	}

	event := NewAcknowledgedBatch([]Mutation{
		{
			Type:  EventTypePut,
			Key:   []byte("/apisix/routes/fresh-route"),
			Value: []byte(`{"id":"fresh-route","uri":"/fresh"}`),
		},
		{Type: EventTypePut, Key: []byte("/apisix/services/fresh-service"), Value: []byte(`{"id":"fresh-service"}`)},
	}, BatchOptions{ReplaceManaged: true})
	second.events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("authoritative replacement: %v", err)
	}
	for bucket, id := range map[string]string{
		"routes":    "stale-route",
		"services":  "stale-service",
		"consumers": "stale-consumer",
	} {
		value, err := second.GetFromBucket(bucket, []byte(id))
		if err != nil {
			t.Fatalf("read removed %s/%s: %v", bucket, id, err)
		}
		if value != nil {
			t.Fatalf("stale %s/%s remained after replacement: %q", bucket, id, value)
		}
	}
	if _, err := second.GetConsumerNameByPluginKey("key-auth", "stale-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale consumer index error = %v, want ErrNotFound", err)
	}
}

func TestAcknowledgedBatchPluginMetadataRequiresObject(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "batch-metadata.db"), make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	event := NewAcknowledgedBatch([]Mutation{{
		Type:  EventTypePut,
		Key:   []byte("/apisix/plugin_metadata/example"),
		Value: []byte(`[]`),
	}}, BatchOptions{})
	storage.events <- event
	err = event.Wait(context.Background())
	var batchErr *BatchValidationError
	var validationErr *ResourceValidationError
	if !errors.As(err, &batchErr) || len(batchErr.Rejected) != 1 ||
		!errors.As(batchErr.Rejected[0].Err, &validationErr) {
		t.Fatalf("plugin metadata error = %v, want one ResourceValidationError in BatchValidationError", err)
	}
	if validationErr.Bucket != "plugin_metadata" || validationErr.ID != "example" {
		t.Fatalf("plugin metadata validation context = %q/%q", validationErr.Bucket, validationErr.ID)
	}
}

func TestAcknowledgedBatchQuarantinePreserveAllowsUnrelatedReplacement(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "batch-preserve.db"), make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	apply := func(event *Event) error {
		storage.events <- event
		return event.Wait(context.Background())
	}
	lastGood := NewAcknowledgedEvent()
	lastGood.Type = EventTypePut
	lastGood.Key = []byte("/apisix/routes/keep")
	lastGood.Value = []byte(`{"id":"keep","uri":"/last-good"}`)
	if err := apply(lastGood); err != nil {
		t.Fatalf("seed last-good route: %v", err)
	}

	first := NewAcknowledgedBatch([]Mutation{
		{Type: EventTypePut, Key: []byte("/apisix/routes/new"), Value: []byte(`{"id":"new","uri":"/new"}`)},
		{Type: EventTypePut, Key: []byte("/apisix/routes/keep"), Value: []byte(`{"id":"keep","plugins":[]}`)},
	}, BatchOptions{ReplaceManaged: true})
	if err := apply(first); err == nil {
		t.Fatal("invalid replacement batch error = nil")
	}

	retry := NewAcknowledgedBatch([]Mutation{{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/new"),
		Value: []byte(`{"id":"new","uri":"/new"}`),
	}}, BatchOptions{
		ReplaceManaged: true,
		Preserve:       []ResourceKey{{Bucket: "routes", ID: "keep"}},
	})
	if err := apply(retry); err != nil {
		t.Fatalf("pruned replacement batch: %v", err)
	}
	if value, err := storage.GetFromBucket("routes", []byte("keep")); err != nil {
		t.Fatalf("read preserved route: %v", err)
	} else if string(value) != `{"id":"keep","uri":"/last-good"}` {
		t.Fatalf("preserved route = %q", value)
	}
	if value, err := storage.GetFromBucket("routes", []byte("new")); err != nil {
		t.Fatalf("read replacement route: %v", err)
	} else if value == nil {
		t.Fatal("replacement route was not persisted")
	}
}

func TestAcknowledgedBatchPublishesEachAffectedBucketOnce(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "batch-hooks.db"), make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	legacy := make(chan string, 8)
	acknowledged := make(chan string, 8)
	storage.AddEventUpdateHook(func(event *Event) {
		bucket, ok := EventBucket(event)
		if ok {
			legacy <- bucket
		}
	})
	storage.AddAcknowledgedEventUpdateHook(func(event *Event) error {
		bucket, ok := EventBucket(event)
		if ok {
			acknowledged <- bucket
		}
		return nil
	})

	event := NewAcknowledgedBatch([]Mutation{
		{Type: EventTypePut, Key: []byte("/apisix/routes/route-1"), Value: []byte(`{"id":"route-1","uri":"/one"}`)},
		{Type: EventTypePut, Key: []byte("/apisix/routes/route-2"), Value: []byte(`{"id":"route-2","uri":"/two"}`)},
		{Type: EventTypePut, Key: []byte("/apisix/services/service-1"), Value: []byte(`{"id":"service-1"}`)},
		{Type: EventTypePut, Key: []byte("/apisix/consumer_groups/group-1"), Value: []byte(`{"id":"group-1"}`)},
	}, BatchOptions{})
	storage.events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("batch apply: %v", err)
	}
	if got := storage.configGeneration.Load(); got != 1 {
		t.Fatalf("config generation = %d, want one increment for batch", got)
	}
	assertBucketsOnce := func(name string, events <-chan string) {
		counts := map[string]int{}
		for range 2 {
			counts[<-events]++
		}
		select {
		case bucket := <-events:
			t.Fatalf("%s hook emitted extra bucket %q", name, bucket)
		default:
		}
		if len(counts) != 2 || counts["routes"] != 1 || counts["services"] != 1 {
			t.Fatalf("%s hook buckets = %#v, want one per affected bucket", name, counts)
		}
	}
	assertBucketsOnce("legacy", legacy)
	assertBucketsOnce("acknowledged", acknowledged)
}

func TestAcknowledgedBatchHookFailureDoesNotUndoDurableCommit(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "batch-hook-error.db"), make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	wantErr := errors.New("stream publication failed")
	storage.AddAcknowledgedEventUpdateHook(func(*Event) error { return wantErr })
	event := NewAcknowledgedBatch([]Mutation{{
		Type:  EventTypePut,
		Key:   []byte("/apisix/routes/route-1"),
		Value: []byte(`{"id":"route-1","uri":"/one"}`),
	}}, BatchOptions{})
	storage.events <- event
	if err := event.Wait(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("acknowledged hook error = %v, want %v", err, wantErr)
	}
	if value, err := storage.GetFromBucket("routes", []byte("route-1")); err != nil {
		t.Fatalf("read durable route: %v", err)
	} else if value == nil {
		t.Fatal("route disappeared after acknowledged hook failure")
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
