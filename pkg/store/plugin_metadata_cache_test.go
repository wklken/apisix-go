package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestValidatedPluginMetadataCacheIsOwnedByStore(t *testing.T) {
	first := &Store{validatedPluginMetadata: newValidatedPluginMetadataCache()}
	second := &Store{validatedPluginMetadata: newValidatedPluginMetadataCache()}

	first.validatedPluginMetadata.put("batch-requests", []byte(`{"max_body_size":128}`))

	if _, ok := second.validatedPluginMetadata.get("batch-requests"); ok {
		t.Fatal("second Store observed first Store's validated metadata")
	}
	if value, ok := first.validatedPluginMetadata.get("batch-requests"); !ok ||
		string(value) != `{"max_body_size":128}` {
		t.Fatalf("first Store value = %q, %v", value, ok)
	}
}

func TestValidatedPluginMetadataCacheDeletionRestoresEmptyState(t *testing.T) {
	cache := newValidatedPluginMetadataCache()
	cache.put("batch-requests", []byte(`{"max_body_size":128}`))
	cache.delete("batch-requests")

	if _, ok := cache.get("batch-requests"); ok {
		t.Fatal("deleted metadata remains in last-good cache")
	}
}

func TestGetValidatedPluginMetadataPreservesLastGoodAndClearsOnDeletion(t *testing.T) {
	db, err := bolt.Open(filepath.Join(t.TempDir(), "metadata.db"), 0o600, nil)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	storage := &Store{
		db:                      db,
		validatedPluginMetadata: newValidatedPluginMetadataCache(),
	}
	storage.InitBuckets()

	writePluginMetadataForTest(t, storage, []byte(`{"max_body_size":128}`))
	var target struct {
		MaxBodySize int `json:"max_body_size"`
	}
	validate := func(metadata map[string]any) error {
		value, _ := metadata["max_body_size"].(float64)
		if value <= 0 {
			return fmt.Errorf("max_body_size must be positive")
		}
		return nil
	}
	usedLastGood, err := storage.getValidatedPluginMetadata("batch-requests", validate, &target)
	if err != nil || usedLastGood || target.MaxBodySize != 128 {
		t.Fatalf("valid metadata = (%v, %v, %d), want (false, nil, 128)",
			usedLastGood, err, target.MaxBodySize)
	}

	writePluginMetadataForTest(t, storage, []byte(`{"max_body_size":0}`))
	target.MaxBodySize = 0
	usedLastGood, err = storage.getValidatedPluginMetadata("batch-requests", validate, &target)
	if err == nil || !usedLastGood || target.MaxBodySize != 128 {
		t.Fatalf("invalid metadata fallback = (%v, %v, %d), want (true, error, 128)",
			usedLastGood, err, target.MaxBodySize)
	}

	writePluginMetadataForTest(t, storage, nil)
	target.MaxBodySize = 0
	usedLastGood, err = storage.getValidatedPluginMetadata("batch-requests", validate, &target)
	if !errors.Is(err, ErrNotFound) || usedLastGood {
		t.Fatalf("deleted metadata = (%v, %v), want (false, ErrNotFound)", usedLastGood, err)
	}
	if _, ok := storage.validatedPluginMetadata.get("batch-requests"); ok {
		t.Fatal("deleted desired metadata did not clear last-good snapshot")
	}
}

func TestValidatedPluginMetadataCacheConcurrentAccess(t *testing.T) {
	cache := newValidatedPluginMetadataCache()
	var group sync.WaitGroup
	for i := range 100 {
		group.Go(func() {
			id := strconv.Itoa(i % 4)
			cache.put(id, []byte(strconv.Itoa(i)))
			_, _ = cache.get(id)
			if i%3 == 0 {
				cache.delete(id)
			}
		})
	}
	group.Wait()
}

func writePluginMetadataForTest(t *testing.T, storage *Store, value []byte) {
	t.Helper()
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("plugin_metadata"))
		if value == nil {
			return bucket.Delete([]byte("batch-requests"))
		}
		return bucket.Put([]byte("batch-requests"), value)
	}); err != nil {
		t.Fatalf("write plugin metadata: %v", err)
	}
}
