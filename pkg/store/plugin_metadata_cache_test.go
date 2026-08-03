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

	first.validatedPluginMetadata.publish("batch-requests", []byte(`{"max_body_size":128}`), 1)

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
	cache.publish("batch-requests", []byte(`{"max_body_size":128}`), 1)
	cache.delete("batch-requests", 2)

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
			cache.publish(id, []byte(strconv.Itoa(i)), i)
			_, _ = cache.get(id)
			if i%3 == 0 {
				cache.delete(id, i)
			}
		})
	}
	group.Wait()
}

func TestGetValidatedPluginMetadataRejectsOutOfOrderPublication(t *testing.T) {
	db, err := bolt.Open(filepath.Join(t.TempDir(), "metadata-order.db"), 0o600, nil)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	storage := &Store{
		db:                      db,
		validatedPluginMetadata: newValidatedPluginMetadataCache(),
	}
	storage.InitBuckets()

	validate := func(metadata map[string]any) error {
		value, _ := metadata["max_body_size"].(float64)
		if value <= 0 {
			return fmt.Errorf("max_body_size must be positive")
		}
		return nil
	}

	writePluginMetadataForTest(t, storage, []byte(`{"max_body_size":256}`))
	oldValidationStarted := make(chan struct{})
	resumeOldValidation := make(chan struct{})
	oldDone := make(chan struct{})
	var oldTarget struct {
		MaxBodySize int `json:"max_body_size"`
	}
	var oldUsedLastGood bool
	var oldErr error
	go func() {
		defer close(oldDone)
		oldUsedLastGood, oldErr = storage.getValidatedPluginMetadata(
			"batch-requests",
			func(metadata map[string]any) error {
				close(oldValidationStarted)
				<-resumeOldValidation
				return validate(metadata)
			},
			&oldTarget,
		)
	}()
	<-oldValidationStarted

	writePluginMetadataForTest(t, storage, []byte(`{"max_body_size":64}`))
	var restrictiveTarget struct {
		MaxBodySize int `json:"max_body_size"`
	}
	usedLastGood, err := storage.getValidatedPluginMetadata(
		"batch-requests",
		validate,
		&restrictiveTarget,
	)
	if err != nil || usedLastGood || restrictiveTarget.MaxBodySize != 64 {
		t.Fatalf("newer restrictive metadata = (%v, %v, %d), want (false, nil, 64)",
			usedLastGood, err, restrictiveTarget.MaxBodySize)
	}

	close(resumeOldValidation)
	<-oldDone
	if oldErr != nil || oldUsedLastGood || oldTarget.MaxBodySize != 64 {
		t.Fatalf("resumed old metadata = (%v, %v, %d), want current (false, nil, 64)",
			oldUsedLastGood, oldErr, oldTarget.MaxBodySize)
	}

	writePluginMetadataForTest(t, storage, []byte(`{"max_body_size":0}`))
	var fallbackTarget struct {
		MaxBodySize int `json:"max_body_size"`
	}
	usedLastGood, err = storage.getValidatedPluginMetadata(
		"batch-requests",
		validate,
		&fallbackTarget,
	)
	if err == nil || !usedLastGood || fallbackTarget.MaxBodySize != 64 {
		t.Fatalf("invalid metadata fallback = (%v, %v, %d), want (true, error, 64)",
			usedLastGood, err, fallbackTarget.MaxBodySize)
	}
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
