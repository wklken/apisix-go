package store

import (
	"bytes"
	"fmt"
	"log"
	"sync"

	"github.com/wklken/apisix-go/pkg/logger"
	bolt "go.etcd.io/bbolt"
)

// eventUpdateHook is a function that is called when an event is updated.
type EventUpdateHook func(event *Event)

type Store struct {
	events chan *Event
	// Add other fields for kv storage in memory
	db *bolt.DB

	// eventUpdateHooks is a list of hooks that are called when an event is updated.
	eventUpdateHooks []EventUpdateHook

	// FIXME: not so sure about this
	// store uniq key->consumer_id, like key-auth:123456->foo
	consumerKV map[string][]byte
	// store consumer_id -> keys, like foo->[key-auth:123456], for update and delete
	consumerToKeys map[string][]string
}

// should it be global store?
var (
	once sync.Once
	s    *Store
)

// FIXME: use init()?
func NewStore(dbPath string, events chan *Event) *Store {
	once.Do(func() {
		db, err := bolt.Open(dbPath, 0o600, nil)
		if err != nil {
			log.Fatal(err)
		}

		s = &Store{
			events: events,
			// Initialize other fields for kv storage in memory
			db: db,

			consumerKV:     map[string][]byte{},
			consumerToKeys: map[string][]string{},
		}

		s.InitBuckets()
	})
	return s
}

func (s *Store) AddEventUpdateHook(hook EventUpdateHook) {
	s.eventUpdateHooks = append(s.eventUpdateHooks, hook)
}

var builtInBuckets = [][]byte{
	[]byte("routes"),
	[]byte("services"),
	[]byte("upstreams"),
	// []byte("secrets"),

	[]byte("consumers"),
	[]byte("consumer_groups"),
	[]byte("global_rules"),
	[]byte("plugin_configs"),
	[]byte("plugin_metadata"),
	[]byte("plugins"),
	[]byte("protos"),
	[]byte("ssls"),
	[]byte("stream_routes"),
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

// get specific key from bucket
func (s *Store) GetFromBucket(bucketName string, id []byte) []byte {
	var value []byte
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return fmt.Errorf("bucket not found")
		}
		value = b.Get(id)
		return nil
	})
	return value
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

// /apisix/routes/505192286146003655
func getTypeAndIDFromKey(key []byte) ([]byte, []byte) {
	parts := bytes.Split(key, []byte("/"))

	return parts[len(parts)-2], parts[len(parts)-1]
}

func (s *Store) processEvents() {
	for event := range s.events {
		bucketName, id := getTypeAndIDFromKey(event.Key)
		defer PutBack(event)

		if event.Type == EventTypePut {
			s.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucketName)
				if b == nil {
					return fmt.Errorf("bucket not found")
				}

				err := b.Put(id, event.Value)
				if err != nil {
					return fmt.Errorf("put key-value fail: %s", err)
				}

				err = s.consumerKVAdd(id, event.Value)
				if err != nil {
					logger.Errorf("store process the consumer fail, err=%w", err)
				}

				return nil
			})
		} else if event.Type == EventTypeDelete {
			s.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucketName)
				if b == nil {
					return fmt.Errorf("bucket not found")
				}

				err := b.Delete(id)
				if err != nil {
					return fmt.Errorf("delete key-value fail: %s", err)
				}

				err = s.consumerKVDelete(id)
				if err != nil {
					logger.Errorf("store process the consumer fail, err=%w", err)
				}

				return nil
			})
		}

		// FIXME: what type of event should trigger the hooks?
		if bytes.Equal(bucketName, []byte("routes")) || bytes.Equal(bucketName, []byte("services")) ||
			bytes.Equal(bucketName, []byte("upstreams")) {
			s.triggerEventUpdateHooks(event)
		}
	}
}

// trigger the hooks
func (s *Store) triggerEventUpdateHooks(event *Event) {
	for _, hook := range s.eventUpdateHooks {
		hook(event)
	}
}
