package store

import (
	"bytes"
	"fmt"

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
	return s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("consumers"))
		if bucket == nil {
			return errBucketNotFound
		}
		return bucket.ForEach(func(id, value []byte) error {
			snapshot, err := s.prepareConsumerSnapshot(bytes.Clone(id), bytes.Clone(value))
			if err != nil {
				return fmt.Errorf("rebuild persisted consumer %q: %w", id, err)
			}
			s.applyConsumerSnapshot(snapshot)
			return nil
		})
	})
}
