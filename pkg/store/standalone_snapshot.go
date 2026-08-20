package store

import (
	"bytes"
	"fmt"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
)

// SnapshotBuckets returns a cloned view of every requested bucket. All
// buckets are read in one bbolt transaction so the returned baseline is
// internally consistent. Missing buckets are reported as errors.
func (s *Store) SnapshotBuckets(bucketNames []string) (map[string]map[string][]byte, error) {
	snapshot := make(map[string]map[string][]byte, len(bucketNames))
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, bucketName := range bucketNames {
			bucket := tx.Bucket([]byte(bucketName))
			if bucket == nil {
				return fmt.Errorf("snapshot bucket %q: %w", bucketName, errBucketNotFound)
			}

			entries := make(map[string][]byte)
			if err := bucket.ForEach(func(key, value []byte) error {
				entries[string(key)] = bytes.Clone(value)
				return nil
			}); err != nil {
				return fmt.Errorf("snapshot bucket %q: %w", bucketName, err)
			}
			snapshot[bucketName] = entries
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *Store) rebuildPersistedConsumerIndexes() error {
	snapshots := make([]consumerSnapshot, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("consumers"))
		if bucket == nil {
			return errBucketNotFound
		}
		return bucket.ForEach(func(id, value []byte) error {
			snapshot, err := s.prepareConsumerSnapshot(bytes.Clone(id), bytes.Clone(value))
			if err != nil {
				logger.Warnf("skip invalid persisted consumer %q: %s", id, err)
				return nil
			}
			snapshots = append(snapshots, snapshot)
			return nil
		})
	})
	if err != nil {
		return err
	}

	consumerKV := make(map[string][]byte)
	consumerToKeys := make(map[string][]string)
	consumerValues := make(map[string]resource.Consumer)
	consumerReferenceKV := make(map[string]map[string][]byte)
	consumerToReferences := make(map[string][]string)
	for _, snapshot := range snapshots {
		key := string(snapshot.id)
		consumerID := append([]byte(nil), snapshot.id...)
		consumerKV[key] = consumerID
		consumerToKeys[key] = append([]string(nil), snapshot.pluginKeys...)
		for _, pluginKey := range snapshot.pluginKeys {
			if owner := string(consumerKV[pluginKey]); owner != "" && owner != key {
				return duplicateConsumerLookupKeyError(pluginKey, owner)
			}
			consumerKV[pluginKey] = consumerID
		}
		if len(snapshot.referencePlugins) > 0 {
			consumerToReferences[key] = append([]string(nil), snapshot.referencePlugins...)
		}
		for _, pluginName := range snapshot.referencePlugins {
			if consumerReferenceKV[pluginName] == nil {
				consumerReferenceKV[pluginName] = make(map[string][]byte)
			}
			consumerReferenceKV[pluginName][key] = consumerID
		}
		consumerValues[key] = snapshot.consumer
	}

	s.consumerMu.Lock()
	s.consumerKV = consumerKV
	s.consumerToKeys = consumerToKeys
	s.consumerValues = consumerValues
	s.consumerReferenceKV = consumerReferenceKV
	s.consumerToReferences = consumerToReferences
	s.consumerMu.Unlock()
	return nil
}
