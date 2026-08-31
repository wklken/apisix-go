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
	"github.com/wklken/apisix-go/pkg/observability/metrics"
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

// StandaloneFileWatcher translates each complete standalone file and submits
// the resulting desired state to the generation coordinator. It retains only
// acknowledgement metadata; canonical resource bytes remain coordinator-owned.
type StandaloneFileWatcher struct {
	path           string
	provider       string
	applier        generation.DesiredApplier
	dataEncryption data_encryption.Service

	reloadMu sync.Mutex
	mu       sync.Mutex

	acknowledgedCursor    generation.ProviderCursor
	acknowledgedRevisions generation.RevisionSet
	acknowledgedDecisions map[generation.Domain][]generation.ResourceDecision
	knownKeys             map[generation.ResourceKey]struct{}
	quarantine            map[generation.ResourceKey]struct{}

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	lifecycleMu sync.Mutex
	applyWG     sync.WaitGroup
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

// desiredBatchFromStandalone translates an already-normalized standalone file
// snapshot. The cursor binds the translation contract to the exact canonical
// mutation bytes.
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
	// from every managed standalone kind rather than only resources present.
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
	applier generation.DesiredApplier,
	encryption data_encryption.Service,
) *StandaloneFileWatcher {
	if !encryption.Configured() {
		panic(data_encryption.ErrDeclarationCatalogUnavailable)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &StandaloneFileWatcher{
		path:                  path,
		provider:              strings.ToLower(strings.TrimSpace(provider)),
		applier:               applier,
		dataEncryption:        encryption,
		acknowledgedDecisions: make(map[generation.Domain][]generation.ResourceDecision),
		knownKeys:             make(map[generation.ResourceKey]struct{}),
		quarantine:            make(map[generation.ResourceKey]struct{}),
		ctx:                   ctx,
		cancel:                cancel,
		done:                  make(chan struct{}),
	}
}

func (w *StandaloneFileWatcher) Reload() error {
	return w.reconcile()
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

// StartAndReconcile registers the watcher before reading the file so changes
// cannot land in a registration gap. The initial reconciliation must succeed
// before the data plane starts serving traffic; later watcher failures are
// logged and retried by the next filesystem event.
func (w *StandaloneFileWatcher) StartAndReconcile() error {
	if err := w.Start(); err != nil {
		return err
	}
	return w.reconcile()
}

// Stop cancels and joins the watcher and every in-flight Apply. It does not
// close the coordinator and replays the first stop result.
func (w *StandaloneFileWatcher) Stop() error {
	w.stopOnce.Do(func() {
		w.lifecycleMu.Lock()
		w.stopped = true
		w.cancel()
		started := w.started
		watcher := w.watcher
		w.lifecycleMu.Unlock()

		if watcher != nil {
			if err := watcher.Close(); err != nil && !errors.Is(err, fsnotify.ErrClosed) {
				w.stopErr = err
			}
		}
		if !started {
			w.closeDone()
		} else {
			<-w.done
		}
		w.applyWG.Wait()
	})
	return w.stopErr
}

// Watch preserves the historical logging-only lifecycle entry point.
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
			if err := w.reconcile(); err != nil {
				logger.Errorf("reload standalone config %q failed: %s", w.path, err)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Errorf("watch standalone config %q failed: %s", w.path, err)
		}
	}
}

func (w *StandaloneFileWatcher) reconcile() error {
	w.reloadMu.Lock()
	defer w.reloadMu.Unlock()
	if err := w.ctx.Err(); err != nil {
		return err
	}

	snapshot, err := readStandaloneSnapshot(w.path, w.provider, w.dataEncryption)
	if err != nil {
		metrics.RecordConfigApplyAttemptFailure("standalone", "translate")
		return err
	}
	batch := desiredBatchFromStandalone(snapshot)
	if err := w.beginApply(); err != nil {
		return err
	}
	ack, applyErr := w.applier.Apply(w.ctx, batch)
	w.applyWG.Done()
	if applyErr != nil {
		if w.ctx.Err() == nil {
			metrics.RecordConfigApplyAttemptFailure("standalone", "apply")
		}
		return applyErr
	}
	if err := w.commitAcknowledgement(batch, ack); err != nil {
		if !errors.Is(err, context.Canceled) {
			metrics.RecordConfigApplyAttemptFailure("standalone", "acknowledgement")
		}
		return err
	}
	return nil
}

func (w *StandaloneFileWatcher) beginApply() error {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.stopped {
		return w.ctx.Err()
	}
	if w.applier == nil {
		return generation.ErrIntegrity
	}
	w.applyWG.Add(1)
	return nil
}

func (w *StandaloneFileWatcher) commitAcknowledgement(
	batch generation.DesiredBatch,
	ack generation.Acknowledgement,
) error {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.stopped || w.ctx.Err() != nil {
		return w.ctx.Err()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	nextKeys, nextDecisions, nextQuarantine, err := validateStandaloneAcknowledgement(
		batch,
		ack,
		w.acknowledgedCursor,
		w.acknowledgedRevisions,
		w.acknowledgedDecisions,
		w.knownKeys,
	)
	if err != nil {
		return err
	}
	w.acknowledgedCursor = ack.Cursor
	w.acknowledgedRevisions = ack.Revisions
	w.acknowledgedDecisions = nextDecisions
	w.knownKeys = nextKeys
	w.quarantine = nextQuarantine
	metrics.RecordConfigApplyAcknowledgement(
		ack.Decisions,
		len(nextQuarantine),
	)
	return nil
}

func validateStandaloneAcknowledgement(
	batch generation.DesiredBatch,
	ack generation.Acknowledgement,
	previousCursor generation.ProviderCursor,
	previousRevisions generation.RevisionSet,
	previousDecisions map[generation.Domain][]generation.ResourceDecision,
	previousKeys map[generation.ResourceKey]struct{},
) (
	map[generation.ResourceKey]struct{},
	map[generation.Domain][]generation.ResourceDecision,
	map[generation.ResourceKey]struct{},
	error,
) {
	invalid := func() (
		map[generation.ResourceKey]struct{},
		map[generation.Domain][]generation.ResourceDecision,
		map[generation.ResourceKey]struct{},
		error,
	) {
		return nil, nil, nil, generation.ErrIntegrity
	}
	if ack.Cursor != batch.Cursor || ack.Revisions.Desired == 0 {
		return invalid()
	}
	if previousRevisions.Desired != 0 {
		switch {
		case ack.Revisions.Desired < previousRevisions.Desired:
			return invalid()
		case ack.Cursor == previousCursor && ack.Revisions != previousRevisions:
			return invalid()
		case ack.Cursor == previousCursor && !equalStandaloneDecisions(ack.Decisions, previousDecisions):
			return invalid()
		case ack.Cursor != previousCursor && ack.Revisions.Desired <= previousRevisions.Desired:
			return invalid()
		}
	}
	required := make(map[generation.Domain]struct{}, len(batch.RequiredDomains))
	for _, domain := range batch.RequiredDomains {
		if domain != generation.DomainHTTP && domain != generation.DomainStream {
			return invalid()
		}
		if _, duplicate := required[domain]; duplicate {
			return invalid()
		}
		required[domain] = struct{}{}
		if standaloneRevisionForDomain(ack.Revisions, domain) != ack.Revisions.Desired {
			return invalid()
		}
		if _, exists := ack.Decisions[domain]; !exists {
			return invalid()
		}
	}
	if len(ack.Decisions) != len(required) {
		return invalid()
	}

	nextKeys := make(map[generation.ResourceKey]struct{}, len(batch.Mutations))
	requiredKeys := make(map[generation.ResourceKey]struct{}, len(batch.Mutations)+len(previousKeys))
	allowedKeys := make(map[generation.ResourceKey]struct{}, len(batch.Mutations)+len(previousKeys))
	for key := range previousKeys {
		requiredKeys[key] = struct{}{}
		allowedKeys[key] = struct{}{}
	}
	for _, decisions := range previousDecisions {
		for _, decision := range decisions {
			allowedKeys[decision.Key] = struct{}{}
		}
	}
	for _, mutation := range batch.Mutations {
		if mutation.Type != generation.MutationPut || mutation.Key.Kind == "" || mutation.Key.ID == "" {
			return invalid()
		}
		if _, duplicate := nextKeys[mutation.Key]; duplicate {
			return invalid()
		}
		nextKeys[mutation.Key] = struct{}{}
		requiredKeys[mutation.Key] = struct{}{}
		allowedKeys[mutation.Key] = struct{}{}
	}

	nextDecisions := make(map[generation.Domain][]generation.ResourceDecision, len(ack.Decisions))
	decisionIndex := make(map[generation.Domain]map[generation.ResourceKey]generation.ResourceDisposition)
	for domain, decisions := range ack.Decisions {
		if _, expectedDomain := required[domain]; !expectedDomain {
			return invalid()
		}
		seen := make(map[generation.ResourceKey]generation.ResourceDisposition, len(decisions))
		for _, decision := range decisions {
			if decision.Code == "" || !validStandaloneDisposition(decision.Disposition) ||
				!slices.Contains(generation.DomainsForResourceKind(decision.Key.Kind), domain) {
				return invalid()
			}
			if _, duplicate := seen[decision.Key]; duplicate {
				return invalid()
			}
			_, current := nextKeys[decision.Key]
			_, locallyAuthorized := allowedKeys[decision.Key]
			if !locallyAuthorized && (!batch.ReplaceManaged ||
				!generation.IsManagedResourceKind(decision.Key.Kind) ||
				decision.Disposition != generation.DispositionDeleted) {
				return invalid()
			}
			if current && decision.Disposition == generation.DispositionDeleted ||
				!current && decision.Disposition == generation.DispositionPublished {
				return invalid()
			}
			seen[decision.Key] = decision.Disposition
		}
		decisionIndex[domain] = seen
		nextDecisions[domain] = slices.Clone(decisions)
	}

	decisionKeys := make(map[generation.ResourceKey]struct{}, len(requiredKeys))
	for key := range requiredKeys {
		decisionKeys[key] = struct{}{}
	}
	for _, decisions := range ack.Decisions {
		for _, decision := range decisions {
			decisionKeys[decision.Key] = struct{}{}
		}
	}
	nextQuarantine := make(map[generation.ResourceKey]struct{})
	for key := range decisionKeys {
		affected := false
		rejected := false
		_, locallyAuthorized := allowedKeys[key]
		for _, domain := range generation.DomainsForResourceKind(key.Kind) {
			if _, isRequired := required[domain]; !isRequired {
				continue
			}
			affected = true
			disposition, exists := decisionIndex[domain][key]
			if !exists {
				return invalid()
			}
			if !locallyAuthorized && (!batch.ReplaceManaged ||
				!generation.IsManagedResourceKind(key.Kind) ||
				disposition != generation.DispositionDeleted) {
				return invalid()
			}
			if disposition == generation.DispositionLastGood ||
				disposition == generation.DispositionQuarantined ||
				disposition == generation.DispositionFailClosed {
				rejected = true
			}
		}
		if !affected {
			return invalid()
		}
		if rejected {
			nextQuarantine[key] = struct{}{}
		}
	}
	return nextKeys, nextDecisions, nextQuarantine, nil
}

func equalStandaloneDecisions(
	left, right map[generation.Domain][]generation.ResourceDecision,
) bool {
	if len(left) != len(right) {
		return false
	}
	for domain, decisions := range left {
		if !slices.Equal(decisions, right[domain]) {
			return false
		}
	}
	return true
}

func standaloneRevisionForDomain(revisions generation.RevisionSet, domain generation.Domain) uint64 {
	switch domain {
	case generation.DomainHTTP:
		return revisions.HTTP
	case generation.DomainStream:
		return revisions.Stream
	default:
		return 0
	}
}

func validStandaloneDisposition(disposition generation.ResourceDisposition) bool {
	switch disposition {
	case generation.DispositionPublished,
		generation.DispositionLastGood,
		generation.DispositionQuarantined,
		generation.DispositionFailClosed,
		generation.DispositionDeleted:
		return true
	default:
		return false
	}
}

func (w *StandaloneFileWatcher) closeDone() {
	w.doneOnce.Do(func() { close(w.done) })
}

func readStandaloneSnapshot(
	path, provider string,
	encryption data_encryption.Service,
) (standaloneSnapshot, error) {
	if !encryption.Configured() {
		return nil, data_encryption.ErrDeclarationCatalogUnavailable
	}
	if provider != standaloneProviderYAML && provider != standaloneProviderJSON {
		return nil, fmt.Errorf("unsupported standalone config provider %q", provider)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read standalone config %q: %w", path, err)
	}
	if provider == standaloneProviderYAML && !strings.HasSuffix(strings.TrimSpace(string(data)), "#END") {
		return nil, fmt.Errorf("standalone YAML config %q must end with #END", path)
	}

	var encoded []byte
	if provider == standaloneProviderYAML {
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("parse standalone YAML config %q: %w", path, err)
		}
		encoded, err = json.Marshal(document)
		if err != nil {
			return nil, fmt.Errorf("normalize standalone config %q: %w", path, err)
		}
	} else {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("parse standalone JSON config %q: %w", path, err)
		}
		encoded = data
	}

	var sections map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &sections); err != nil {
		return nil, fmt.Errorf("decode standalone resources %q: %w", path, err)
	}
	if sections == nil {
		return nil, fmt.Errorf("decode standalone resources %q: expected object", path)
	}
	if err := validateStandaloneSections(sections); err != nil {
		return nil, err
	}

	snapshot := make(standaloneSnapshot)
	for _, bucket := range standaloneBuckets {
		raw, ok := sections[bucket]
		if !ok {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("decode standalone %s: expected array", bucket)
		}
		var resources []json.RawMessage
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("decode standalone %s: %w", bucket, err)
		}
		for _, resource := range resources {
			id, value, normalizeErr := normalizeStandaloneResource(bucket, resource, encryption)
			if normalizeErr != nil {
				return nil, fmt.Errorf("decode standalone %s resource: %w", bucket, normalizeErr)
			}
			if snapshot[bucket] == nil {
				snapshot[bucket] = make(map[string][]byte)
			}
			if _, duplicate := snapshot[bucket][id]; duplicate {
				return nil, fmt.Errorf("decode standalone %s resource: duplicate id %q", bucket, id)
			}
			snapshot[bucket][id] = value
		}
	}
	return snapshot, nil
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
	if fields == nil {
		return "", nil, fmt.Errorf("expected object")
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
	if bucket == "plugin_metadata" && encryption.Enabled() && encryption.HasEncryptedPluginMetadata(id) {
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
	value, err := json.Marshal(fields)
	if err != nil {
		return id, nil, err
	}
	return id, value, nil
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
