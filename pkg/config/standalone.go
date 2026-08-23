package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/store"
	"go.yaml.in/yaml/v3"
)

// for standalone mode: https://apisix.apache.org/docs/apisix/deployment-modes/#standalone

const (
	standaloneProviderYAML = "yaml"
	standaloneProviderJSON = "json"
)

var standaloneBuckets = []string{
	"routes",
	"upstreams",
	"services",
	"plugin_metadata",
	"ssls",
	"stream_routes",
	"secrets",
	"consumers",
	"consumer_groups",
	"global_rules",
	"plugin_configs",
	"protos",
}

type standaloneSnapshot map[string]map[string][]byte

// StandaloneReloadResult describes the route-relevant resources changed by one
// successfully applied standalone snapshot.
type StandaloneReloadResult struct {
	ChangedHTTPRouteBuckets []string
	ChangedStreamBuckets    []string
	QuarantinedResources    []store.ResourceKey
}

func (r StandaloneReloadResult) AffectsHTTPRoutes() bool {
	return len(r.ChangedHTTPRouteBuckets) > 0
}

func (r StandaloneReloadResult) AffectsStreams() bool {
	return len(r.ChangedStreamBuckets) > 0
}

func (r StandaloneReloadResult) QuarantinedResourceCount() int {
	return len(r.QuarantinedResources)
}

type standaloneResourceQuarantine struct {
	key store.ResourceKey
	err error
}

// StandaloneFileWatcher loads the APISIX file-driven configuration and emits
// store events for added, updated, and removed resources.
type StandaloneFileWatcher struct {
	path           string
	provider       string
	events         chan *store.Event
	dataEncryption data_encryption.Service

	reloadMu             sync.Mutex
	mu                   sync.Mutex
	current              standaloneSnapshot
	onReload             func(StandaloneReloadResult, error)
	onAcknowledgedReload func(StandaloneReloadResult, error) error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	lifecycleMu sync.Mutex
	watcher     *fsnotify.Watcher
	started     bool
	stopped     bool
	startOnce   sync.Once
	stopOnce    sync.Once
	doneOnce    sync.Once
	startErr    error
	stopErr     error
}

func StandaloneConfigFile(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case standaloneProviderJSON:
		return "conf/apisix.json"
	case standaloneProviderYAML:
		return "conf/apisix.yaml"
	default:
		return ""
	}
}

// StandaloneBuckets returns the buckets owned by the standalone configuration
// file. The returned slice is independent from the package-level definition.
func StandaloneBuckets() []string {
	return append([]string(nil), standaloneBuckets...)
}

// desiredBatchFromStandalone translates an already-normalized standalone file
// snapshot without selecting it as a production apply path. The cursor binds
// the translation contract to the exact canonical mutation bytes.
func desiredBatchFromStandalone(snapshot standaloneSnapshot) generation.DesiredBatch {
	batch := generation.DesiredBatch{
		Cursor: generation.ProviderCursor{
			Provider: "standalone/v1",
		},
		ReplaceManaged: true,
	}
	for kind, resources := range snapshot {
		for id, value := range resources {
			batch.Mutations = append(batch.Mutations, generation.Mutation{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: kind, ID: id},
				Value: bytes.Clone(value),
			})
		}
	}
	sort.Slice(batch.Mutations, func(i, j int) bool {
		if batch.Mutations[i].Key.Kind != batch.Mutations[j].Key.Kind {
			return batch.Mutations[i].Key.Kind < batch.Mutations[j].Key.Kind
		}
		return batch.Mutations[i].Key.ID < batch.Mutations[j].Key.ID
	})
	digest := digestStandaloneMutations(batch.Mutations)
	batch.Cursor.Revision = fmt.Sprintf("sha256:%x", digest)

	// Replacement implicitly deletes omitted resources, so domain impact comes
	// from every managed standalone kind rather than only the resources present.
	domainSet := make(map[generation.Domain]struct{})
	for _, kind := range standaloneBuckets {
		for _, domain := range generation.DomainsForResourceKind(kind) {
			domainSet[domain] = struct{}{}
		}
	}
	for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
		if _, ok := domainSet[domain]; ok {
			batch.RequiredDomains = append(batch.RequiredDomains, domain)
		}
	}
	return batch
}

func digestStandaloneMutations(mutations []generation.Mutation) [sha256.Size]byte {
	canonical := binary.AppendUvarint(nil, uint64(len(mutations)))
	for _, mutation := range mutations {
		canonical = appendStandaloneDigestString(canonical, mutation.Key.Kind)
		canonical = appendStandaloneDigestString(canonical, mutation.Key.ID)
		if mutation.Value == nil {
			canonical = append(canonical, 0)
			continue
		}
		canonical = append(canonical, 1)
		canonical = binary.AppendUvarint(canonical, uint64(len(mutation.Value)))
		canonical = append(canonical, mutation.Value...)
	}
	return sha256.Sum256(canonical)
}

func appendStandaloneDigestString(canonical []byte, value string) []byte {
	canonical = binary.AppendUvarint(canonical, uint64(len(value)))
	return append(canonical, value...)
}

func NewStandaloneFileWatcher(
	path, provider string,
	events chan *store.Event,
	encryption data_encryption.Service,
) *StandaloneFileWatcher {
	if !encryption.Configured() {
		panic(data_encryption.ErrDeclarationCatalogUnavailable)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &StandaloneFileWatcher{
		path:           path,
		provider:       strings.ToLower(strings.TrimSpace(provider)),
		events:         events,
		dataEncryption: encryption,
		current:        make(standaloneSnapshot),
		ctx:            ctx,
		cancel:         cancel,
		done:           make(chan struct{}),
	}
}

// SeedCurrentSnapshot sets the last-good standalone baseline. All map and byte
// slices are cloned so callers can safely reuse or mutate their input.
func (w *StandaloneFileWatcher) SeedCurrentSnapshot(snapshot map[string]map[string][]byte) {
	cloned := cloneStandaloneSnapshot(snapshot)
	w.mu.Lock()
	w.current = cloned
	w.mu.Unlock()
}

func (w *StandaloneFileWatcher) Reload() error {
	_, err := w.ReloadSnapshot()
	return err
}

// ReloadSnapshot applies the complete authoritative resource snapshot before
// returning its result.
func (w *StandaloneFileWatcher) ReloadSnapshot() (StandaloneReloadResult, error) {
	w.reloadMu.Lock()
	defer w.reloadMu.Unlock()
	result, _, err := w.reloadSnapshot()
	return result, err
}

func (w *StandaloneFileWatcher) reloadSnapshot() (StandaloneReloadResult, standaloneSnapshot, error) {
	next, quarantined, err := readStandaloneSnapshot(w.path, w.provider, w.dataEncryption)
	if err != nil {
		return StandaloneReloadResult{}, nil, err
	}

	w.mu.Lock()
	previous := w.current
	w.mu.Unlock()

	preserve := make(map[store.ResourceKey]struct{}, len(quarantined))
	for _, quarantine := range quarantined {
		logger.Errorf(
			"quarantine standalone %s/%s: %s",
			quarantine.key.Bucket,
			quarantine.key.ID,
			quarantine.err,
		)
		preserve[quarantine.key] = struct{}{}
		retainStandaloneLastGood(next, previous, quarantine.key)
	}

	for {
		mutations, mutationKeys := standaloneMutations(next, preserve)
		batch := store.NewAcknowledgedBatch(mutations, store.BatchOptions{
			ReplaceManaged: true,
			Preserve:       sortedStandaloneResourceKeys(preserve),
		})
		enqueued, applyErr := w.enqueueAndWait(batch)
		if applyErr == nil {
			break
		}
		if !enqueued {
			store.PutBack(batch)
			return StandaloneReloadResult{}, previous, applyErr
		}
		var validationErr *store.BatchValidationError
		if !errors.As(applyErr, &validationErr) {
			return StandaloneReloadResult{}, previous, applyErr
		}
		added := false
		for _, rejected := range validationErr.Rejected {
			if rejected.Index < 0 || rejected.Index >= len(mutationKeys) {
				return StandaloneReloadResult{}, previous, applyErr
			}
			key := mutationKeys[rejected.Index]
			if _, exists := preserve[key]; exists {
				continue
			}
			preserve[key] = struct{}{}
			quarantined = append(quarantined, standaloneResourceQuarantine{key: key, err: rejected.Err})
			logger.Errorf("quarantine standalone %s/%s: %s", key.Bucket, key.ID, rejected.Err)
			retainStandaloneLastGood(next, previous, key)
			added = true
		}
		if !added {
			return StandaloneReloadResult{}, previous, applyErr
		}
	}
	if err := w.contextErr(); err != nil {
		return StandaloneReloadResult{}, previous, err
	}

	result := standaloneReloadResult(previous, next, quarantined)
	w.mu.Lock()
	w.current = next
	w.mu.Unlock()
	return result, previous, nil
}

func (w *StandaloneFileWatcher) SetReloadCallback(callback func(StandaloneReloadResult, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onReload = callback
	w.onAcknowledgedReload = nil
}

// SetAcknowledgedReloadCallback installs a serialized apply callback. A
// non-nil callback error keeps the previous snapshot so the next file event
// reapplies the complete authoritative snapshot.
func (w *StandaloneFileWatcher) SetAcknowledgedReloadCallback(
	callback func(StandaloneReloadResult, error) error,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onReload = nil
	w.onAcknowledgedReload = callback
}

// Start registers the standalone file watcher. It is safe to call more than
// once; only the first call acquires filesystem resources.
func (w *StandaloneFileWatcher) Start() error {
	w.startOnce.Do(func() {
		w.lifecycleMu.Lock()
		if w.stopped {
			w.lifecycleMu.Unlock()
			return
		}

		watcher, err := fsnotify.NewWatcher()
		if err == nil {
			err = watcher.Add(filepath.Dir(w.path))
		}
		if err != nil {
			if watcher != nil {
				_ = watcher.Close()
			}
			w.startErr = fmt.Errorf("watch standalone config %q failed: %w", w.path, err)
			w.started = true
			w.lifecycleMu.Unlock()
			w.closeDone()
			return
		}

		w.watcher = watcher
		w.started = true
		w.lifecycleMu.Unlock()
		go w.watchLoop(watcher)
	})
	return w.startErr
}

// StartAndReconcile registers the standalone file watcher and then performs a
// synchronous read. The read closes the gap between the initial snapshot and
// filesystem watcher registration.
func (w *StandaloneFileWatcher) StartAndReconcile() error {
	if err := w.Start(); err != nil {
		return err
	}
	if err := w.reconcile(); err != nil {
		logger.Errorf("reconcile standalone config %q after watcher registration failed: %s", w.path, err)
	}
	return nil
}

// Stop cancels and joins the file watcher. It never closes the shared Store
// event channel and is safe before Start or when called repeatedly.
func (w *StandaloneFileWatcher) Stop() error {
	w.stopOnce.Do(func() {
		w.lifecycleMu.Lock()
		w.stopped = true
		started := w.started
		watcher := w.watcher
		w.lifecycleMu.Unlock()

		w.cancel()
		if watcher != nil {
			if err := watcher.Close(); err != nil && !errors.Is(err, fsnotify.ErrClosed) {
				w.stopErr = err
			}
		}
		if !started {
			w.closeDone()
			return
		}
		<-w.done
	})
	return w.stopErr
}

// Watch preserves the historical logging-only API while Start exposes setup
// errors to lifecycle owners such as Server.
func (w *StandaloneFileWatcher) Watch() {
	if err := w.StartAndReconcile(); err != nil {
		logger.Errorf("watch standalone config %q failed: %s", w.path, err)
	}
}

func (w *StandaloneFileWatcher) watchLoop(watcher *fsnotify.Watcher) {
	defer w.closeDone()
	defer func() {
		_ = watcher.Close()
		w.lifecycleMu.Lock()
		if w.watcher == watcher {
			w.watcher = nil
		}
		w.lifecycleMu.Unlock()
	}()

	configuredBase := filepath.Base(w.path)
	for {
		select {
		case <-w.ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != configuredBase ||
				!event.Has(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) {
				continue
			}
			w.reloadAndNotify()
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Errorf("watch standalone config %q failed: %s", w.path, err)
		}
	}
}

func (w *StandaloneFileWatcher) reloadAndNotify() {
	if err := w.reconcile(); err != nil {
		logger.Errorf("reload standalone config %q failed: %s", w.path, err)
	}
}

func (w *StandaloneFileWatcher) reconcile() error {
	w.reloadMu.Lock()
	result, previous, err := w.reloadSnapshot()
	w.mu.Lock()
	callback := w.onReload
	acknowledgedCallback := w.onAcknowledgedReload
	w.mu.Unlock()
	if acknowledgedCallback == nil {
		w.reloadMu.Unlock()
		if callback != nil {
			callback(result, err)
		}
		return err
	}
	applyErr := acknowledgedCallback(result, err)
	if err == nil && applyErr != nil {
		w.mu.Lock()
		w.current = previous
		w.mu.Unlock()
	}
	w.reloadMu.Unlock()
	if err != nil {
		return err
	}
	return applyErr
}

func (w *StandaloneFileWatcher) enqueueAndWait(event *store.Event) (bool, error) {
	select {
	case w.events <- event:
		return true, event.Wait(w.ctx)
	case <-w.ctx.Done():
		return false, w.ctx.Err()
	}
}

func (w *StandaloneFileWatcher) contextErr() error {
	select {
	case <-w.ctx.Done():
		return w.ctx.Err()
	default:
		return nil
	}
}

func (w *StandaloneFileWatcher) closeDone() {
	w.doneOnce.Do(func() { close(w.done) })
}

func cloneStandaloneSnapshot(snapshot map[string]map[string][]byte) standaloneSnapshot {
	cloned := make(standaloneSnapshot, len(snapshot))
	for bucket, resources := range snapshot {
		if resources == nil {
			cloned[bucket] = nil
			continue
		}
		clonedResources := make(map[string][]byte, len(resources))
		for id, value := range resources {
			clonedResources[id] = append([]byte(nil), value...)
		}
		cloned[bucket] = clonedResources
	}
	return cloned
}

func standaloneProviderFromPath(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

func readStandaloneSnapshot(
	path, provider string,
	encryption data_encryption.Service,
) (standaloneSnapshot, []standaloneResourceQuarantine, error) {
	if !encryption.Configured() {
		return nil, nil, data_encryption.ErrDeclarationCatalogUnavailable
	}
	if provider != standaloneProviderYAML && provider != standaloneProviderJSON {
		return nil, nil, fmt.Errorf("unsupported standalone config provider %q", provider)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read standalone config %q: %w", path, err)
	}
	if provider == standaloneProviderYAML && !strings.HasSuffix(strings.TrimSpace(string(data)), "#END") {
		return nil, nil, fmt.Errorf("standalone YAML config %q must end with #END", path)
	}

	var encoded []byte
	if provider == standaloneProviderYAML {
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, nil, fmt.Errorf("parse standalone YAML config %q: %w", path, err)
		}
		encoded, err = json.Marshal(document)
		if err != nil {
			return nil, nil, fmt.Errorf("normalize standalone config %q: %w", path, err)
		}
	} else {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, nil, fmt.Errorf("parse standalone JSON config %q: %w", path, err)
		}
		encoded = data
	}

	var sections map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &sections); err != nil {
		return nil, nil, fmt.Errorf("decode standalone resources %q: %w", path, err)
	}
	if sections == nil {
		return nil, nil, fmt.Errorf("decode standalone resources %q: expected object", path)
	}
	if err := validateStandaloneSections(sections); err != nil {
		return nil, nil, err
	}

	snapshot := make(standaloneSnapshot)
	var quarantined []standaloneResourceQuarantine
	for _, bucket := range standaloneBuckets {
		raw, ok := sections[bucket]
		if !ok {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, nil, fmt.Errorf("decode standalone %s: expected array", bucket)
		}
		var resources []json.RawMessage
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, nil, fmt.Errorf("decode standalone %s: %w", bucket, err)
		}
		for _, resource := range resources {
			id, value, err := normalizeStandaloneResource(bucket, resource, encryption)
			if err != nil {
				quarantined = append(quarantined, standaloneResourceQuarantine{
					key: store.ResourceKey{Bucket: bucket, ID: id},
					err: fmt.Errorf("decode standalone %s resource: %w", bucket, err),
				})
				continue
			}
			if snapshot[bucket] == nil {
				snapshot[bucket] = make(map[string][]byte)
			}
			snapshot[bucket][id] = value
		}
	}
	return snapshot, quarantined, nil
}

func validateStandaloneSections(sections map[string]json.RawMessage) error {
	unknown := make([]string, 0)
	for section := range sections {
		if !slices.Contains(standaloneBuckets, section) {
			unknown = append(unknown, section)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("decode standalone resources: unknown root section %q", unknown[0])
}

func normalizeStandaloneResource(
	bucket string,
	raw json.RawMessage,
	encryption data_encryption.Service,
) (string, []byte, error) {
	if !encryption.Configured() {
		return "", nil, data_encryption.ErrDeclarationCatalogUnavailable
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil, err
	}

	keys := []string{"id"}
	if bucket == "consumers" {
		keys = []string{"username", "id"}
	}
	var idKey string
	var idRaw json.RawMessage
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			idKey = key
			idRaw = value
			break
		}
	}
	if idKey == "" {
		return "", nil, fmt.Errorf("missing id")
	}
	id, err := standaloneResourceID(idRaw)
	if err != nil {
		return "", nil, err
	}
	if idKey == "id" {
		fields[idKey], err = json.Marshal(id)
		if err != nil {
			return "", nil, err
		}
	}
	if rawPlugins, ok := fields["plugins"]; ok {
		var plugins map[string]any
		if err := json.Unmarshal(rawPlugins, &plugins); err != nil {
			return id, nil, fmt.Errorf("decode plugins: %w", err)
		}
		if encryption.Enabled() {
			if err := encryption.EncryptPluginConfigs(plugins); err != nil {
				return id, nil, fmt.Errorf("encrypt plugin fields: %w", err)
			}
			fields["plugins"], err = json.Marshal(plugins)
			if err != nil {
				return id, nil, fmt.Errorf("encode encrypted plugins: %w", err)
			}
		}
	}
	if bucket == "plugin_metadata" {
		if encryption.Enabled() && encryption.HasEncryptedPluginMetadata(id) {
			encoded, err := json.Marshal(fields)
			if err != nil {
				return id, nil, fmt.Errorf("encode plugin metadata: %w", err)
			}
			var metadata map[string]any
			if err := json.Unmarshal(encoded, &metadata); err != nil {
				return id, nil, fmt.Errorf("decode plugin metadata: %w", err)
			}
			if err := encryption.EncryptPluginMetadata(id, metadata); err != nil {
				return id, nil, fmt.Errorf("encrypt plugin metadata fields: %w", err)
			}
			encoded, err = json.Marshal(metadata)
			if err != nil {
				return id, nil, fmt.Errorf("encode encrypted plugin metadata: %w", err)
			}
			if err := json.Unmarshal(encoded, &fields); err != nil {
				return id, nil, fmt.Errorf("decode encrypted plugin metadata: %w", err)
			}
		}
	}
	value, err := json.Marshal(fields)
	if err != nil {
		return id, nil, err
	}
	return id, value, nil
}

func standaloneMutations(
	snapshot standaloneSnapshot,
	preserve map[store.ResourceKey]struct{},
) ([]store.Mutation, []store.ResourceKey) {
	mutations := make([]store.Mutation, 0)
	keys := make([]store.ResourceKey, 0)
	for _, bucket := range standaloneBuckets {
		resources := snapshot[bucket]
		for _, id := range sortedSnapshotIDs(resources) {
			key := store.ResourceKey{Bucket: bucket, ID: id}
			if _, preserved := preserve[key]; preserved {
				continue
			}
			mutations = append(mutations, store.Mutation{
				Type:  store.EventTypePut,
				Key:   []byte("/apisix/" + bucket + "/" + id),
				Value: resources[id],
			})
			keys = append(keys, key)
		}
	}
	return mutations, keys
}

func retainStandaloneLastGood(next, previous standaloneSnapshot, key store.ResourceKey) {
	if key.ID == "" {
		return
	}
	previousValue, exists := previous[key.Bucket][key.ID]
	if !exists {
		if next[key.Bucket] != nil {
			delete(next[key.Bucket], key.ID)
		}
		return
	}
	if next[key.Bucket] == nil {
		next[key.Bucket] = make(map[string][]byte)
	}
	next[key.Bucket][key.ID] = append([]byte(nil), previousValue...)
}

func sortedStandaloneResourceKeys(keys map[store.ResourceKey]struct{}) []store.ResourceKey {
	result := make([]store.ResourceKey, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Bucket != result[j].Bucket {
			return result[i].Bucket < result[j].Bucket
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func standaloneReloadResult(
	previous, next standaloneSnapshot,
	quarantined []standaloneResourceQuarantine,
) StandaloneReloadResult {
	quarantinedResources := make([]store.ResourceKey, 0, len(quarantined))
	for _, quarantine := range quarantined {
		quarantinedResources = append(quarantinedResources, quarantine.key)
	}
	sort.Slice(quarantinedResources, func(i, j int) bool {
		if quarantinedResources[i].Bucket != quarantinedResources[j].Bucket {
			return quarantinedResources[i].Bucket < quarantinedResources[j].Bucket
		}
		return quarantinedResources[i].ID < quarantinedResources[j].ID
	})
	result := StandaloneReloadResult{QuarantinedResources: quarantinedResources}
	for _, bucket := range standaloneBuckets {
		previousBucket := previous[bucket]
		updated := next[bucket]
		changed := false
		for _, id := range sortedSnapshotIDs(previousBucket) {
			if _, ok := updated[id]; !ok {
				changed = true
			}
		}
		for _, id := range sortedSnapshotIDs(updated) {
			if previousValue, ok := previousBucket[id]; ok && bytes.Equal(previousValue, updated[id]) {
				continue
			}
			changed = true
		}
		if changed && store.IsHTTPRouteReloadBucket(bucket) {
			result.ChangedHTTPRouteBuckets = append(result.ChangedHTTPRouteBuckets, bucket)
		}
		if changed && store.IsStreamReloadBucket(bucket) {
			result.ChangedStreamBuckets = append(result.ChangedStreamBuckets, bucket)
		}
	}
	return result
}

func standaloneResourceID(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch value := value.(type) {
	case string:
		if value == "" {
			return "", fmt.Errorf("id is empty")
		}
		return value, nil
	case json.Number:
		return value.String(), nil
	default:
		return "", fmt.Errorf("id must be a string or number")
	}
}

func sortedSnapshotIDs(snapshot map[string][]byte) []string {
	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
