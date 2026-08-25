package store

import (
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const storeOpenTimeout = time.Second

// Store owns the durable generation journal.
type Store struct {
	db *bolt.DB

	closeOnce sync.Once
	closeErr  error
}

// Close releases the journal database exactly once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}
