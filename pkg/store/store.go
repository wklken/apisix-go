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
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/resource"
	bolt "go.etcd.io/bbolt"
)

// eventUpdateHook is a function that is called when an event is updated.
type EventUpdateHook func(event *Event)

// AcknowledgedEventUpdateHook is a required publication stage. Its error is
// returned to the producer after the durable Store transaction has committed.
type AcknowledgedEventUpdateHook func(event *Event) error

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
	// acknowledgedEventUpdateHooks are called after a durable mutation and may
	// reject provider acknowledgement when required publication fails.
	acknowledgedEventUpdateHooks []AcknowledgedEventUpdateHook

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

const storeOpenTimeout = time.Second

// Open constructs a store that owns its database file. It has no global side
// effects; callers that also need the package-level getters must register
// the store through GetStore.
func Open(dbPath string, events chan *Event) (*Store, error) {
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: storeOpenTimeout})
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

func (s *Store) AddAcknowledgedEventUpdateHook(hook AcknowledgedEventUpdateHook) {
	s.acknowledgedEventUpdateHooks = append(s.acknowledgedEventUpdateHooks, hook)
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

type bucketEntry struct {
	id    string
	value []byte
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
	if event.batch {
		return s.processMutations(event.mutations, event.options, true)
	}
	return s.processMutations([]Mutation{{
		Type:  event.Type,
		Key:   event.Key,
		Value: event.Value,
	}}, BatchOptions{}, false)
}

type parsedMutation struct {
	mutation Mutation
	bucket   string
	id       string
}

func (s *Store) processMutations(mutations []Mutation, options BatchOptions, batch bool) error {
	parsed := make([]parsedMutation, 0, len(mutations))
	validationErr := &BatchValidationError{}
	affectedBuckets := make([]string, 0, len(mutations))
	affected := make(map[string]struct{}, len(mutations))
	addAffected := func(bucket string) {
		if !IsHTTPRouteReloadBucket(bucket) && !IsStreamReloadBucket(bucket) {
			return
		}
		if _, ok := affected[bucket]; ok {
			return
		}
		affected[bucket] = struct{}{}
		affectedBuckets = append(affectedBuckets, bucket)
	}
	needsConsumerRebuild := options.ReplaceManaged
	needsSSLRebuild := options.ReplaceManaged
	needsSecretCacheReset := options.ReplaceManaged
	needsProtoGeneration := options.ReplaceManaged
	needsConfigGeneration := options.ReplaceManaged
	for index, mutation := range mutations {
		bucket, id, err := parseMutationKey(mutation.Key)
		if err != nil {
			validationErr.Rejected = append(validationErr.Rejected, RejectedMutation{
				Index: index,
				Err:   &ResourceValidationError{Err: err},
			})
			continue
		}
		if mutation.Type != EventTypePut && mutation.Type != EventTypeDelete {
			return fmt.Errorf("unsupported store event type %d", mutation.Type)
		}
		parsed = append(parsed, parsedMutation{mutation: mutation, bucket: bucket, id: id})
		addAffected(bucket)
		if IsHTTPRouteReloadBucket(bucket) {
			needsConfigGeneration = true
		}
		switch bucket {
		case "consumers":
			needsConsumerRebuild = true
		case "ssls":
			needsSSLRebuild = true
		case "secrets":
			needsSecretCacheReset = true
		case "protos":
			needsProtoGeneration = true
		}
		if mutation.Type != EventTypePut {
			continue
		}
		var resourceErr error
		switch bucket {
		case "ssls":
			resourceErr = validateSSLCertificateEvent(mutation.Type, id, mutation.Value)
		case "routes", "global_rules":
			resourceErr = validateConfigResourcePut(bucket, id, mutation.Value)
		case "consumers":
			_, resourceErr = s.prepareConsumerSnapshot([]byte(id), mutation.Value)
			if resourceErr != nil {
				resourceErr = fmt.Errorf("store process the consumer fail: %w", resourceErr)
			}
		case "plugin_metadata":
			resourceErr = validatePluginMetadataPut(id, mutation.Value)
		}
		if resourceErr != nil {
			validationErr.Rejected = append(validationErr.Rejected, RejectedMutation{
				Index: index,
				Err:   &ResourceValidationError{Bucket: bucket, ID: id, Err: resourceErr},
			})
		}
	}
	if len(validationErr.Rejected) > 0 {
		if !batch && len(validationErr.Rejected) == 1 {
			return validationErr.Rejected[0].Err
		}
		return validationErr
	}
	if options.ReplaceManaged {
		for _, bucket := range builtInBuckets {
			addAffected(string(bucket))
		}
	}

	preserve := make(map[ResourceKey]struct{}, len(options.Preserve))
	for _, key := range options.Preserve {
		preserve[key] = struct{}{}
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if options.ReplaceManaged {
			if err := clearManagedBuckets(tx, preserve); err != nil {
				return err
			}
		}
		for _, entry := range parsed {
			bucket := tx.Bucket([]byte(entry.bucket))
			if bucket == nil {
				return errBucketNotFound
			}
			id := []byte(entry.id)
			switch entry.mutation.Type {
			case EventTypePut:
				if err := bucket.Put(id, entry.mutation.Value); err != nil {
					return fmt.Errorf("put key-value fail: %w", err)
				}
			case EventTypeDelete:
				if err := bucket.Delete(id); err != nil {
					return fmt.Errorf("delete key-value fail: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Derived indexes and caches are published only after the durable write.
	if needsConsumerRebuild {
		if err := s.rebuildPersistedConsumerIndexes(); err != nil {
			return err
		}
	}
	if needsSSLRebuild {
		s.rebuildSSLCertificateIndex()
	}
	if needsSecretCacheReset {
		s.vaultMu.Lock()
		s.vaultSecrets = nil
		s.vaultMu.Unlock()
	}
	if needsConfigGeneration {
		s.configGeneration.Add(1)
	}
	if needsProtoGeneration {
		s.protosGeneration.Add(1)
	}
	return s.triggerBatchUpdateHooks(parsed, affectedBuckets)
}

func parseMutationKey(key []byte) (string, string, error) {
	parts := bytes.Split(key, []byte("/"))
	if len(parts) > 0 && len(parts[0]) == 0 {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid resource key %q", key)
	}
	for _, part := range parts {
		if len(part) == 0 {
			return "", "", fmt.Errorf("invalid resource key %q", key)
		}
	}
	bucket := string(parts[len(parts)-2])
	id := string(parts[len(parts)-1])
	if len(parts) >= 3 && string(parts[len(parts)-3]) == "secrets" {
		bucket = "secrets"
		id = string(parts[len(parts)-2]) + "/" + id
	} else if bucket == "secrets" {
		return "", "", fmt.Errorf("invalid secret resource key %q", key)
	}
	if !isManagedBucket(bucket) {
		return "", "", fmt.Errorf("unsupported resource bucket %q", bucket)
	}
	return bucket, id, nil
}

func isManagedBucket(bucket string) bool {
	for _, managed := range builtInBuckets {
		if string(managed) == bucket {
			return true
		}
	}
	return false
}

func clearManagedBuckets(tx *bolt.Tx, preserve map[ResourceKey]struct{}) error {
	for _, bucketName := range builtInBuckets {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return errBucketNotFound
		}
		keys := make([][]byte, 0)
		if err := bucket.ForEach(func(id, _ []byte) error {
			key := ResourceKey{Bucket: string(bucketName), ID: string(id)}
			if _, ok := preserve[key]; !ok {
				keys = append(keys, bytes.Clone(id))
			}
			return nil
		}); err != nil {
			return fmt.Errorf("list managed bucket %q: %w", bucketName, err)
		}
		for _, id := range keys {
			if err := bucket.Delete(id); err != nil {
				return fmt.Errorf("clear managed bucket %q: %w", bucketName, err)
			}
		}
	}
	return nil
}

func validatePluginMetadataPut(id string, value []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return fmt.Errorf("decode plugin metadata %q: %w", id, err)
	}
	if object == nil {
		return fmt.Errorf("decode plugin metadata %q: expected JSON object", id)
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

func (s *Store) triggerBatchUpdateHooks(parsed []parsedMutation, buckets []string) error {
	byBucket := make(map[string]*Event, len(buckets))
	for _, entry := range parsed {
		if _, ok := byBucket[entry.bucket]; ok {
			continue
		}
		byBucket[entry.bucket] = &Event{
			Type:  entry.mutation.Type,
			Key:   append([]byte(nil), entry.mutation.Key...),
			Value: append([]byte(nil), entry.mutation.Value...),
		}
	}
	var acknowledgedErr error
	for _, bucket := range buckets {
		event := byBucket[bucket]
		if event == nil {
			event = &Event{
				Type:  EventTypePut,
				Key:   []byte("/apisix/" + bucket + "/__snapshot__"),
				Value: nil,
			}
		}
		if err := s.triggerAcknowledgedEventUpdateHooks(event); err != nil {
			acknowledgedErr = errors.Join(acknowledgedErr, err)
		}
	}
	return acknowledgedErr
}

func (s *Store) triggerAcknowledgedEventUpdateHooks(event *Event) error {
	for _, hook := range s.eventUpdateHooks {
		hook(event)
	}
	var acknowledgedErr error
	for _, hook := range s.acknowledgedEventUpdateHooks {
		if hook == nil {
			continue
		}
		if err := hook(event); err != nil {
			acknowledgedErr = errors.Join(acknowledgedErr, err)
		}
	}
	return acknowledgedErr
}
