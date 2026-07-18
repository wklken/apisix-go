package store

import (
	"bytes"
	"fmt"
	"log"
	"sync"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
)

// eventUpdateHook is a function that is called when an event is updated.
type EventUpdateHook func(event *Event)

type Store struct {
	events  chan *Event
	runDone chan struct{}
	// Add other fields for kv storage in memory
	db *bolt.DB

	// eventUpdateHooks is a list of hooks that are called when an event is updated.
	eventUpdateHooks []EventUpdateHook

	// FIXME: not so sure about this
	// store uniq key->consumer_id, like key-auth:123456->foo
	consumerKV map[string][]byte
	// store consumer_id -> keys, like foo->[key-auth:123456], for update and delete
	consumerToKeys map[string][]string
	// store validated consumers with environment and managed secret references resolved
	consumerValues map[string]resource.Consumer
	consumerMu     sync.RWMutex
}

// should it be global store?
var (
	once              sync.Once
	s                 *Store
	errBucketNotFound = fmt.Errorf("bucket not found")
)

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
			consumerValues: map[string]resource.Consumer{},
		}

		s.InitBuckets()
	})
	return s
}

func (s *Store) AddEventUpdateHook(hook EventUpdateHook) {
	s.eventUpdateHooks = append(s.eventUpdateHooks, hook)
}

// IsHTTPRouteReloadBucket reports whether a resource change affects the built HTTP route handler.
func IsHTTPRouteReloadBucket(bucket string) bool {
	switch bucket {
	case "routes", "services", "upstreams", "global_rules", "plugin_configs":
		return true
	default:
		return false
	}
}

// IsStreamReloadBucket reports whether a resource change affects stream routing.
func IsStreamReloadBucket(bucket string) bool {
	return bucket == "upstreams" || bucket == "stream_routes"
}

var builtInBuckets = [][]byte{
	[]byte("routes"),
	[]byte("services"),
	[]byte("upstreams"),
	[]byte("global_rules"),
	[]byte("plugin_configs"),
	[]byte("plugin_metadata"),
	[]byte("consumers"),
	// []byte("secrets"),

	[]byte("consumer_groups"),
	[]byte("plugins"),
	[]byte("protos"),
	[]byte("ssls"),
	[]byte("stream_routes"),
	[]byte("secrets"),
}

func (s *Store) InitBuckets() {
	for _, bucket := range builtInBuckets {
		_ = s.db.Update(func(tx *bolt.Tx) error {
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
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return errBucketNotFound
		}
		_ = b.ForEach(func(_, v []byte) error {
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
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return errBucketNotFound
		}
		value = b.Get(id)
		return nil
	})
	return value
}

func (s *Store) Start() {
	// Start goroutine to receive and process events
	s.runDone = make(chan struct{})
	go func() {
		defer close(s.runDone)
		s.processEvents()
	}()
}

// Sync waits until all events sent before the call have been processed.
func (s *Store) Sync() {
	done := make(chan struct{})
	s.events <- &Event{done: done}
	<-done
}

func (s *Store) Stop() {
	// Close events channel
	close(s.events)
	if s.runDone != nil {
		<-s.runDone
	}
	_ = s.db.Close()
}

// []byte{}  get the last part split by / in the key

// /apisix/routes/505192286146003655
func getTypeAndIDFromKey(key []byte) ([]byte, []byte) {
	parts := bytes.Split(key, []byte("/"))
	if len(parts) >= 5 && bytes.Equal(parts[len(parts)-3], []byte("secrets")) {
		return parts[len(parts)-3], bytes.Join(parts[len(parts)-2:], []byte("/"))
	}

	return parts[len(parts)-2], parts[len(parts)-1]
}

func (s *Store) processEvents() {
	for event := range s.events {
		if event.done != nil {
			close(event.done)
			continue
		}

		bucketName, id := getTypeAndIDFromKey(event.Key)
		switch event.Type {
		case EventTypePut:
			_ = s.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucketName)
				if b == nil {
					return errBucketNotFound
				}

				if bytes.Equal(bucketName, []byte("consumers")) {
					if err := s.consumerKVAdd(id, event.Value); err != nil {
						logger.Errorf("store process the consumer fail, err=%s", err)
						return nil
					}
				}

				if err := b.Put(id, event.Value); err != nil {
					return fmt.Errorf("put key-value fail: %s", err)
				}
				return nil
			})
		case EventTypeDelete:
			_ = s.db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket(bucketName)
				if b == nil {
					return errBucketNotFound
				}

				err := b.Delete(id)
				if err != nil {
					return fmt.Errorf("delete key-value fail: %s", err)
				}

				if bytes.Equal(bucketName, []byte("consumers")) {
					if err := s.consumerKVDelete(id); err != nil {
						logger.Errorf("store process the consumer fail, err=%s", err)
					}
				}

				return nil
			})
		}

		// FIXME: what type of event should trigger the hooks?
		bucket := string(bucketName)
		if IsHTTPRouteReloadBucket(bucket) || IsStreamReloadBucket(bucket) {
			s.triggerEventUpdateHooks(event)
		}
		PutBack(event)
	}
}

// trigger the hooks
func (s *Store) triggerEventUpdateHooks(event *Event) {
	for _, hook := range s.eventUpdateHooks {
		hook(event)
	}
}
