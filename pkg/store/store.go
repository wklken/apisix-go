package store

import (
	"bytes"
	"fmt"
	"log"

	bolt "go.etcd.io/bbolt"
)

type Store struct {
	events chan *Event
	// Add other fields for kv storage in memory
	db *bolt.DB
}

func NewStore(dbPath string, events chan *Event) *Store {
	db, err := bolt.Open(dbPath, 0o600, nil)
	if err != nil {
		log.Fatal(err)
	}

	store := &Store{
		events: events,
		// Initialize other fields for kv storage in memory
		db: db,
	}

	store.InitBuckets()
	return store
}

var builtInBuckets = [][]byte{
	[]byte("plugin_metadata"),
	[]byte("routes"),
	[]byte("global_rules"),
	[]byte("services"),
	[]byte("plugin_configs"),
	[]byte("upstreams"),
	[]byte("secrets"),
	[]byte("consumers"),
	[]byte("consumer_groups"),
}

func (s *Store) InitBuckets() {
	for _, bucket := range builtInBuckets {
		s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucket)
			if b != nil {
				return nil
			}

			var err error
			_, err = tx.CreateBucket(bucket)
			if err != nil {
				return fmt.Errorf("create bucket fail: %s", err)
			}
			return nil
		})
	}
}

func (s *Store) GetBucketData(bucketName string) [][]byte {
	var data [][]byte
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}
		b.ForEach(func(_, v []byte) error {
			data = append(data, v)
			return nil
		})
		return nil
	})
	return data
}

func (s *Store) Start() {
	// Start goroutine to receive and process events
	go s.processEvents()
}

func (s *Store) Stop() {
	// Close events channel
	close(s.events)
	s.db.Close()
}

// []byte{}  get the last part split by / in the key

func getLastPart(key []byte) []byte {
	parts := bytes.Split(key, []byte("/"))
	return parts[len(parts)-2]
}

func (s *Store) processEvents() {
	for event := range s.events {
		bucketName := getLastPart(event.Key)
		defer PutBack(event)

		if event.Type == EventTypePut {
			s.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucketName)
				if b == nil {
					return fmt.Errorf("bucket not found")
				}

				err := b.Put(event.Key, event.Value)
				if err != nil {
					return fmt.Errorf("put key-value fail: %s", err)
				}
				return nil
			})
		} else if event.Type == EventTypeDelete {
			s.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucketName)
				if b == nil {
					return fmt.Errorf("bucket not found")
				}

				err := b.Delete(event.Key)
				if err != nil {
					return fmt.Errorf("delete key-value fail: %s", err)
				}
				return nil
			})
		}
	}
}
