package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestStandaloneBaselineSnapshotPreservesConsumerAndNestedSecretIDs(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "standalone-baseline.db"), make(chan *Event))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Stop() })

	consumerValue := []byte(`{"username":"alice","plugins":{}}`)
	secretValue := bytes.Repeat([]byte("secret"), 1024)
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte("consumers")).Put([]byte("alice"), consumerValue); err != nil {
			return err
		}
		return tx.Bucket([]byte("secrets")).Put([]byte("vault/item"), secretValue)
	}); err != nil {
		t.Fatalf("seed standalone buckets: %v", err)
	}

	snapshot, err := storage.SnapshotBuckets([]string{"consumers", "secrets"})
	if err != nil {
		t.Fatalf("SnapshotBuckets() error = %v", err)
	}
	if got := string(snapshot["consumers"]["alice"]); got != string(consumerValue) {
		t.Fatalf("consumer snapshot = %q, want %q", got, consumerValue)
	}
	if got := string(snapshot["secrets"]["vault/item"]); got != string(secretValue) {
		t.Fatalf("nested secret snapshot length = %d, want %d", len(got), len(secretValue))
	}

	snapshot["consumers"]["alice"][0] = 'X'
	delete(snapshot["secrets"], "vault/item")
	second, err := storage.SnapshotBuckets([]string{"consumers", "secrets"})
	if err != nil {
		t.Fatalf("second SnapshotBuckets() error = %v", err)
	}
	if got := string(second["consumers"]["alice"]); got != string(consumerValue) {
		t.Fatalf("stored consumer changed through returned snapshot = %q", got)
	}
	if got := second["secrets"]["vault/item"]; !bytes.Equal(got, secretValue) {
		t.Fatalf("stored nested secret changed through returned snapshot")
	}
}

func TestSnapshotBucketsMissingBucketReturnsError(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "missing-bucket.db"), make(chan *Event))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Stop() })

	if snapshot, err := storage.SnapshotBuckets([]string{"routes", "does-not-exist"}); err == nil {
		t.Fatalf("SnapshotBuckets() = %#v, nil; want missing bucket error", snapshot)
	} else if !errors.Is(err, errBucketNotFound) {
		t.Fatalf("SnapshotBuckets() error = %v, want errBucketNotFound", err)
	}
}

func TestConsumerRestartRebuildsPersistedPluginLookup(t *testing.T) {
	previous := ReplaceGlobalStoreForTest(nil)
	t.Cleanup(func() { ReplaceGlobalStoreForTest(previous) })

	path := filepath.Join(t.TempDir(), "consumer-restart.db")
	first, err := GetStore(path, make(chan *Event))
	if err != nil {
		t.Fatalf("first GetStore() error = %v", err)
	}
	consumerValue := []byte(`{"username":"alice","plugins":{"key-auth":{"key":"api-key"}}}`)
	if err := first.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("consumers")).Put([]byte("alice"), consumerValue)
	}); err != nil {
		t.Fatalf("persist consumer: %v", err)
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("first Store.Stop() error = %v", err)
	}

	second, err := GetStore(path, make(chan *Event))
	if err != nil {
		t.Fatalf("second GetStore() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Stop() })
	consumer, err := GetConsumerByPluginKey("key-auth", "api-key")
	if err != nil {
		t.Fatalf("GetConsumerByPluginKey() after restart error = %v", err)
	}
	if consumer.Username != "alice" {
		t.Fatalf("restarted consumer username = %q, want alice", consumer.Username)
	}
}

func TestOpenSkipsInvalidPersistedConsumerWithContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid-consumer.db")
	first, err := Open(path, make(chan *Event))
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	invalid := []byte(`{"username":"alice","plugins":{"basic-auth":{"username":"alice"}}}`)
	if err := first.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("consumers")).Put([]byte("alice"), invalid)
	}); err != nil {
		t.Fatalf("persist invalid consumer: %v", err)
	}
	if err := first.Stop(); err != nil {
		t.Fatalf("first Store.Stop() error = %v", err)
	}

	storage, err := Open(path, make(chan *Event))
	if err != nil {
		t.Fatalf("Open() error = %v, want invalid persisted consumer to be skipped", err)
	}
	t.Cleanup(func() { _ = storage.Stop() })
	if _, err := storage.GetConsumerNameByPluginKey("basic-auth", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid persisted consumer lookup error = %v, want ErrNotFound", err)
	}
}

func TestOpenUsesFiniteDatabaseLockTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	started := time.Now()
	storage, err := Open(path, make(chan *Event))
	if storage != nil {
		_ = storage.Stop()
		t.Fatal("Open() returned a Store while database lock was held")
	}
	if err == nil {
		t.Fatal("Open() returned nil error while database lock was held")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Open() lock timeout took %s, want bounded timeout", elapsed)
	}
}
