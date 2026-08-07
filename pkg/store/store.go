package store

import (
	"bytes"
	"errors"
	"fmt"
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

	// stopProducers ends the event loop; it is closed once by Stop and is
	// never closed while external producers may still send.
	stopProducers chan struct{}
	stopOnce      sync.Once
	stopErr       error

	// eventUpdateHooks is a list of hooks that are called when an event is updated.
	eventUpdateHooks []EventUpdateHook

	// FIXME: not so sure about this
	// store uniq key->consumer_id, like key-auth:123456->foo
	consumerKV map[string][]byte
	// store consumer_id -> keys, like foo->[key-auth:123456], for update and delete
	consumerToKeys map[string][]string
	// store validated consumers with environment and managed secret references unresolved
	consumerValues map[string]resource.Consumer
	// store plugin name -> consumer IDs whose lookup key is a secret reference
	consumerReferenceKV map[string]map[string][]byte
	// store consumer ID -> plugin names registered in consumerReferenceKV
	consumerToReferences map[string][]string
	consumerMu           sync.RWMutex

	validatedPluginMetadata *validatedPluginMetadataCache
}

// should it be global store?
var (
	globalStoreMu sync.Mutex
	s             *Store
	errBucketNotFound = errors.New("bucket not found")
)

// Open constructs a store that owns its database file. It has no global side
// effects; callers that also need the package-level getters must register
// the store through GetStore.
func Open(dbPath string, events chan *Event) (*Store, error) {
	db, err := bolt.Open(dbPath, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("open store database %q: %w", dbPath, err)
	}
	storage := &Store{
		events: events,
		db:     db,

		consumerKV:              map[string][]byte{},
		consumerToKeys:          map[string][]string{},
		consumerValues:          map[string]resource.Consumer{},
		consumerReferenceKV:     map[string]map[string][]byte{},
		consumerToReferences:    map[string][]string{},
		validatedPluginMetadata: newValidatedPluginMetadataCache(),
	}
	if err := storage.InitBuckets(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return storage, nil
}

// GetStore returns the process-wide store backing the package-level getters,
// opening it on first use. It returns errors instead of the historical
// log.Fatal singleton construction.
func GetStore(dbPath string, events chan *Event) (*Store, error) {
	globalStoreMu.Lock()
	defer globalStoreMu.Unlock()
	if s == nil {
		storage, err := Open(dbPath, events)
		if err != nil {
			return nil, err
		}
		s = storage
	}
	return s, nil
}

func (s *Store) AddEventUpdateHook(hook EventUpdateHook) {
	s.eventUpdateHooks = append(s.eventUpdateHooks, hook)
}

// IsHTTPRouteReloadBucket reports whether a resource change affects the built HTTP route handler.
func IsHTTPRouteReloadBucket(bucket string) bool {
	switch bucket {
	case "routes", "services", "upstreams", "global_rules", "plugin_configs", "plugin_metadata":
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

func (s *Store) InitBuckets() error {
	for _, bucket := range builtInBuckets {
		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucket)
			if b != nil {
				return nil
			}
			_, err := tx.CreateBucket(bucket)
			if err != nil {
				return fmt.Errorf("create bucket %q: %w", bucket, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetBucketData(bucketName string) ([][]byte, error) {
	var data [][]byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return errBucketNotFound
		}
		return b.ForEach(func(_, value []byte) error {
			data = append(data, bytes.Clone(value))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// get specific key from bucket
func (s *Store) GetFromBucket(bucketName string, id []byte) ([]byte, error) {
	var value []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if b == nil {
			return errBucketNotFound
		}
		value = bytes.Clone(b.Get(id))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (s *Store) Start() {
	// Start goroutine to receive and process events
	s.runDone = make(chan struct{})
	if s.stopProducers == nil {
		s.stopProducers = make(chan struct{})
	}
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

// Stop is idempotent: it stops the event loop and closes the database. The
// events channel itself is never closed here — external producers (the etcd
// watcher) select on their own context, so closing the channel while a
// producer may still send would be a send-on-closed-channel panic. The stop
// signal ends the consumer first and waits for it before closing the db.
func (s *Store) Stop() error {
	s.stopOnce.Do(func() {
		if s.stopProducers != nil {
			close(s.stopProducers)
		}
		if s.runDone != nil {
			<-s.runDone
		}
		s.stopErr = s.db.Close()
	})
	return s.stopErr
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
	for {
		select {
		case event := <-s.events:
			if event == nil {
				return
			}
			s.processEvent(event)
		case <-s.stopProducers:
			return
		}
	}
}

func (s *Store) processEvent(event *Event) {
	if event.done != nil {
		close(event.done)
		return
	}

	bucketName, id := getTypeAndIDFromKey(event.Key)
	processed := false
	switch event.Type {
	case EventTypePut:
		var snapshot consumerSnapshot
		isConsumer := bytes.Equal(bucketName, []byte("consumers"))
		if isConsumer {
			var err error
			snapshot, err = s.prepareConsumerSnapshot(id, event.Value)
			if err != nil {
				logger.Errorf("store process the consumer fail, err=%s", err)
				PutBack(event)
				return
			}
		}

		// Index consumers before the bolt write becomes visible so
		// GetConsumer's bucket fallback cannot race ahead of plugin-key lookup.
		if isConsumer {
			s.applyConsumerSnapshot(snapshot)
		}
		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketName)
			if b == nil {
				return errBucketNotFound
			}
			if err := b.Put(id, event.Value); err != nil {
				return fmt.Errorf("put key-value fail: %s", err)
			}
			return nil
		})
		if err != nil && isConsumer {
			if delErr := s.consumerKVDelete(id); delErr != nil {
				logger.Errorf("rollback consumer index after put fail: %s", delErr)
			}
		}
		processed = err == nil
	case EventTypeDelete:
		isConsumer := bytes.Equal(bucketName, []byte("consumers"))
		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketName)
			if b == nil {
				return errBucketNotFound
			}

			err := b.Delete(id)
			if err != nil {
				return fmt.Errorf("delete key-value fail: %s", err)
			}

			return nil
		})
		if err == nil && isConsumer {
			if err := s.consumerKVDelete(id); err != nil {
				logger.Errorf("store process the consumer fail, err=%s", err)
			}
		}
		processed = err == nil
	}

	// FIXME: what type of event should trigger the hooks?
	bucket := string(bucketName)
	if processed && (IsHTTPRouteReloadBucket(bucket) || IsStreamReloadBucket(bucket)) {
		s.triggerEventUpdateHooks(event)
	}
	PutBack(event)
}

// trigger the hooks
func (s *Store) triggerEventUpdateHooks(event *Event) {
	for _, hook := range s.eventUpdateHooks {
		hook(event)
	}
}
