package store

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
	bolt "go.etcd.io/bbolt"
)

// eventUpdateHook is a function that is called when an event is updated.
type EventUpdateHook func(event *Event)

type Store struct {
	events  chan *Event
	runDone chan struct{}
	// Add other fields for kv storage in memory
	db *bolt.DB

	lifecycleMu sync.RWMutex
	started     bool
	stopped     bool

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

	vaultMu      sync.Mutex
	vaultClient  *http.Client
	vaultSecrets *cacheutil.BoundedTTLMap[string]

	validatedPluginMetadata *validatedPluginMetadataCache

	// sslCerts is the published immutable index of decoded frontend SSL
	// certificates, rebuilt on every ssls bucket change.
	sslCerts atomic.Pointer[sslCertificateIndex]

	// configSnapshot is the published immutable route-build generation
	// (routes, global rules, plugin metadata), rebuilt once per bucket change.
	configSnapshot                atomic.Pointer[ConfigSnapshot]
	configGeneration              atomic.Uint64
	afterConfigSnapshotBucketRead func(string)
	configSnapshotMu              sync.Mutex

	// protosGeneration increments on every protos bucket change so consumers
	// can detect proto resource updates without re-reading the bucket.
	protosGeneration atomic.Int64
}

// should it be global store?
var (
	globalStoreMu      sync.Mutex
	s                  *Store
	errBucketNotFound  = errors.New("bucket not found")
	errStoreNotStarted = errors.New("store is not started")
	errStoreStopped    = errors.New("store is stopped")
)

// ResourceValidationError identifies a deterministic resource validation
// failure. Configuration providers may quarantine this resource while
// continuing to apply unrelated resources; storage and I/O errors must not
// be wrapped with this type.
type ResourceValidationError struct {
	Bucket string
	ID     string
	Err    error
}

func (e *ResourceValidationError) Error() string {
	if e == nil {
		return "resource validation failed"
	}
	return fmt.Sprintf("validate %s/%s: %v", e.Bucket, e.ID, e.Err)
}

func (e *ResourceValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Open constructs a store that owns its database file. It has no global side
// effects; callers that also need the package-level getters must register
// the store through GetStore.
func Open(dbPath string, events chan *Event) (*Store, error) {
	db, err := bolt.Open(dbPath, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("open store database %q: %w", dbPath, err)
	}
	storage := &Store{
		events:        events,
		db:            db,
		stopProducers: make(chan struct{}),

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
	if err := storage.rebuildPersistedConsumerIndexes(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rebuild persisted consumer indexes: %w", err)
	}
	storage.rebuildSSLCertificateIndex()
	return storage, nil
}

// GetStore returns the process-wide store backing the package-level getters,
// opening it on first use. It returns errors instead of the historical
// log.Fatal singleton construction.
func GetStore(dbPath string, events chan *Event) (*Store, error) {
	globalStoreMu.Lock()
	defer globalStoreMu.Unlock()
	if s != nil {
		s.lifecycleMu.RLock()
		stopped := s.stopped
		s.lifecycleMu.RUnlock()
		if stopped {
			return nil, errStoreStopped
		}
	}
	if s == nil {
		storage, err := Open(dbPath, events)
		if err != nil {
			return nil, err
		}
		s = storage
	}
	return s, nil
}

// ReplaceGlobalStoreForTest swaps the process-wide store backing the
// package-level getters and returns the previous value so a test can restore
// it. It exists because the route builder reads the package-level getters and
// tests must isolate the store they publish without leaking it to later tests.
func ReplaceGlobalStoreForTest(storage *Store) *Store {
	globalStoreMu.Lock()
	defer globalStoreMu.Unlock()
	previous := s
	s = storage
	return previous
}

func (s *Store) AddEventUpdateHook(hook EventUpdateHook) {
	s.eventUpdateHooks = append(s.eventUpdateHooks, hook)
}

// IsHTTPRouteReloadBucket reports whether a resource change affects the built HTTP route handler.
func IsHTTPRouteReloadBucket(bucket string) bool {
	switch bucket {
	case "routes", "services", "upstreams", "global_rules", "plugin_configs", "plugin_metadata", "ssls", "secrets",
		"protos":
		return true
	default:
		return false
	}
}

// EventBucket returns the canonical resource bucket, including nested secret
// keys such as /apisix/secrets/vault/name.
func EventBucket(event *Event) (string, bool) {
	if event == nil {
		return "", false
	}
	parts := bytes.Split(event.Key, []byte("/"))
	if len(parts) < 4 {
		return "", false
	}
	bucket, _ := getTypeAndIDFromKey(event.Key)
	if len(bucket) == 0 {
		return "", false
	}
	return string(bucket), true
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
	s.lifecycleMu.Lock()
	if s.started || s.stopped {
		s.lifecycleMu.Unlock()
		return
	}
	s.started = true
	s.runDone = make(chan struct{})
	if s.stopProducers == nil {
		s.stopProducers = make(chan struct{})
	}
	runDone := s.runDone
	s.lifecycleMu.Unlock()
	go func() {
		defer close(runDone)
		s.processEvents()
	}()
}

// Sync waits until all events sent before the call have been processed and
// returns errors from those unacknowledged events. Errors delivered directly
// through acknowledged events are not included in this barrier.
func (s *Store) Sync() error {
	event := NewAcknowledgedEvent()
	event.barrier = true
	s.lifecycleMu.RLock()
	if s.stopped {
		s.lifecycleMu.RUnlock()
		PutBack(event)
		return errStoreStopped
	}
	if !s.started || s.events == nil {
		s.lifecycleMu.RUnlock()
		PutBack(event)
		return errStoreNotStarted
	}
	select {
	case <-s.stopProducers:
		s.lifecycleMu.RUnlock()
		PutBack(event)
		return errStoreStopped
	case s.events <- event:
		err := event.Wait(context.Background())
		s.lifecycleMu.RUnlock()
		return err
	}
}

// Stop is idempotent: it stops the event loop and closes the database. The
// events channel itself is never closed here — external producers (the etcd
// watcher) select on their own context, so closing the channel while a
// producer may still send would be a send-on-closed-channel panic. The stop
// signal ends the consumer first and waits for it before closing the db.
func (storage *Store) Stop() error {
	storage.stopOnce.Do(func() {
		storage.lifecycleMu.Lock()
		storage.stopped = true
		if storage.stopProducers == nil {
			storage.stopProducers = make(chan struct{})
		}
		close(storage.stopProducers)
		runDone := storage.runDone
		storage.lifecycleMu.Unlock()
		if runDone != nil {
			<-runDone
		}

		globalStoreMu.Lock()
		defer globalStoreMu.Unlock()
		storage.stopErr = storage.db.Close()
		if s == storage {
			s = nil
		}
	})
	return storage.stopErr
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
	var pendingUnacknowledged error
	for {
		select {
		case event := <-s.events:
			if event == nil {
				return
			}
			if event.barrier {
				err := pendingUnacknowledged
				pendingUnacknowledged = nil
				s.completeAcknowledgedEvent(event, err)
				continue
			}
			err := s.processEvent(event)
			if event.result != nil {
				s.completeAcknowledgedEvent(event, err)
				continue
			}
			if err != nil {
				pendingUnacknowledged = errors.Join(pendingUnacknowledged, err)
				logger.Errorf("store process event: %s", err)
			}
			PutBack(event)
		case <-s.stopProducers:
			return
		}
	}
}

func (s *Store) completeAcknowledgedEvent(event *Event, err error) {
	event.result <- err
	<-event.waitDone
	PutBack(event)
}

func (s *Store) processEvent(event *Event) error {
	if event.done != nil {
		close(event.done)
		return nil
	}

	bucketName, id := getTypeAndIDFromKey(event.Key)
	if event.Type == EventTypePut && bytes.Equal(bucketName, []byte("ssls")) {
		if err := validateSSLCertificateEvent(event.Type, util.BytesToString(id), event.Value); err != nil {
			return &ResourceValidationError{Bucket: string(bucketName), ID: string(id), Err: err}
		}
	}

	switch event.Type {
	case EventTypePut:
		var snapshot consumerSnapshot
		isConsumer := bytes.Equal(bucketName, []byte("consumers"))
		if isConsumer {
			var err error
			snapshot, err = s.prepareConsumerSnapshot(id, event.Value)
			if err != nil {
				return &ResourceValidationError{
					Bucket: string(bucketName),
					ID:     string(id),
					Err:    fmt.Errorf("store process the consumer fail: %w", err),
				}
			}
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
		if err != nil {
			return err
		}
		if isConsumer {
			s.applyConsumerSnapshot(snapshot)
		}
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
		if err != nil {
			return err
		}
		if isConsumer {
			if err := s.consumerKVDelete(id); err != nil {
				return fmt.Errorf("store process the consumer fail: %w", err)
			}
		}
	default:
		return fmt.Errorf("unsupported store event type %d", event.Type)
	}

	// FIXME: what type of event should trigger the hooks?
	bucket := string(bucketName)
	if bucket == "ssls" {
		s.applySSLCertificateEvent(event.Type, util.BytesToString(id), event.Value)
	}
	if bucket == "secrets" {
		s.vaultMu.Lock()
		s.vaultSecrets = nil
		s.vaultMu.Unlock()
	}
	if bucket == "routes" || bucket == "global_rules" || bucket == "plugin_metadata" {
		s.configGeneration.Add(1)
	}
	if bucket == "protos" {
		s.protosGeneration.Add(1)
	}
	if IsHTTPRouteReloadBucket(bucket) || IsStreamReloadBucket(bucket) {
		s.triggerEventUpdateHooks(event)
	}
	return nil
}

func validateSSLCertificateEvent(eventType EventType, id string, value []byte) error {
	if eventType == EventTypeDelete {
		return nil
	}
	if eventType != EventTypePut {
		return fmt.Errorf("unsupported SSL event type %d for %q", eventType, id)
	}
	ssl, err := ParseSSL(value)
	if err != nil {
		return fmt.Errorf("reject SSL resource %q: parse: %w", id, err)
	}
	if ssl.Status == 0 {
		return nil
	}
	if _, err := tls.X509KeyPair([]byte(ssl.Cert), []byte(ssl.Key)); err != nil {
		return fmt.Errorf("reject SSL resource %q: load: %w", id, err)
	}
	return nil
}

// trigger the hooks
func (s *Store) triggerEventUpdateHooks(event *Event) {
	for _, hook := range s.eventUpdateHooks {
		hook(event)
	}
}
