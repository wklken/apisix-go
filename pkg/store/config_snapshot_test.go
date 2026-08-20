package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestConfigSnapshotRetriesAfterConcurrentRouteApply(t *testing.T) {
	events := make(chan *Event, 1)
	storage, err := Open(t.TempDir()+"/snapshot.db", events)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Stop(); err != nil {
			t.Errorf("Store.Stop() error = %v", err)
		}
	})
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("routes")).Put(
			[]byte("route-1"),
			[]byte(`{"id":"route-1","uri":"/first"}`),
		)
	}); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	storage.Start()

	var hookCalled bool
	storage.afterConfigSnapshotBucketRead = func(bucket string) {
		if bucket != "routes" || hookCalled {
			return
		}
		hookCalled = true

		event := NewAcknowledgedEvent()
		event.Type = EventTypePut
		event.Key = []byte("/apisix/routes/route-2")
		event.Value = []byte(`{"id":"route-2","uri":"/second"}`)
		storage.events <- event
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := event.Wait(ctx); err != nil {
			t.Errorf("concurrent route apply error = %v", err)
		}
	}

	snapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("getConfigSnapshot() error = %v", err)
	}
	if len(snapshot.Routes()) != 2 {
		t.Fatalf("snapshot routes = %d, want 2", len(snapshot.Routes()))
	}
	if got := storage.configGeneration.Load(); snapshot.generation != got {
		t.Fatalf("snapshot generation = %d, store generation = %d", snapshot.generation, got)
	}
	var foundSecond bool
	for _, route := range snapshot.Routes() {
		if route.ID == "route-2" {
			foundSecond = true
			break
		}
	}
	if !foundSecond {
		t.Fatalf("snapshot routes = %+v, want route-2", snapshot.Routes())
	}
}

func TestConfigSnapshotGenerationTracksGlobalRulesAndPluginMetadata(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)

	first, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("initial getConfigSnapshot() error = %v", err)
	}
	if first.generation != 0 {
		t.Fatalf("initial snapshot generation = %d, want 0", first.generation)
	}
	if cached, err := storage.getConfigSnapshot(); err != nil || cached != first {
		t.Fatalf("cached getConfigSnapshot() = %p/%v, want initial pointer %p", cached, err, first)
	}

	applyConfigSnapshotEvent(t, storage, EventTypePut, "/apisix/global_rules/rule-1", `{"id":"rule-1","plugins":{}}`)
	second, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("global rule getConfigSnapshot() error = %v", err)
	}
	if second == first || second.generation != 1 || len(second.GlobalRules()) != 1 {
		t.Fatalf(
			"global rule snapshot = %p generation %d rules %d, want new generation 1 with one rule",
			second,
			second.generation,
			len(second.GlobalRules()),
		)
	}

	applyConfigSnapshotEvent(
		t,
		storage,
		EventTypePut,
		"/apisix/plugin_metadata/metadata-1",
		`{"id":"metadata-1","mode":"new"}`,
	)
	third, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("plugin metadata getConfigSnapshot() error = %v", err)
	}
	metadata, ok := third.PluginMetadata("metadata-1")
	if third == second || third.generation != 2 || !ok || metadata["mode"] != "new" {
		t.Fatalf(
			"plugin metadata snapshot = %p generation %d metadata %#v/%v, want new generation 2",
			third,
			third.generation,
			metadata,
			ok,
		)
	}
}

func TestConfigSnapshotTracksDynamicHTTPPluginsAndDelete(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)

	initial, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("initial getConfigSnapshot() error = %v", err)
	}
	if plugins, present := initial.HTTPPlugins(); present || plugins != nil {
		t.Fatalf("initial HTTPPlugins() = %#v/%t, want nil/false", plugins, present)
	}

	applyConfigSnapshotEvent(
		t,
		storage,
		EventTypePut,
		"/apisix/plugins",
		`[{"name":"request-id"},{"name":"mqtt-proxy","stream":true},{"name":"gzip","stream":false}]`,
	)
	withPlugins, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("dynamic plugin getConfigSnapshot() error = %v", err)
	}
	plugins, present := withPlugins.HTTPPlugins()
	if !present || !slices.Equal(plugins, []string{"request-id", "gzip"}) {
		t.Fatalf("HTTPPlugins() = %#v/%t, want request-id,gzip/true", plugins, present)
	}
	plugins[0] = "mutated"
	if got, _ := withPlugins.HTTPPlugins(); !slices.Equal(got, []string{"request-id", "gzip"}) {
		t.Fatalf("HTTPPlugins() returned mutable snapshot data: %#v", got)
	}

	applyConfigSnapshotEvent(t, storage, EventTypeDelete, "/apisix/plugins", "")
	withoutPlugins, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("getConfigSnapshot() after plugin delete error = %v", err)
	}
	if plugins, present := withoutPlugins.HTTPPlugins(); present || plugins != nil {
		t.Fatalf("HTTPPlugins() after delete = %#v/%t, want nil/false", plugins, present)
	}
	if withoutPlugins.generation != withPlugins.generation+1 {
		t.Fatalf("plugin delete generation = %d, want %d", withoutPlugins.generation, withPlugins.generation+1)
	}
}

func TestDynamicPluginPutValidationRetainsLastGoodAndSkipsReload(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	var hooks int
	storage.AddEventUpdateHook(func(event *Event) {
		if bucket, ok := EventBucket(event); ok && bucket == "plugins" {
			hooks++
		}
	})

	applyConfigSnapshotEvent(t, storage, EventTypePut, "/apisix/plugins", `[{"name":"request-id"}]`)
	before, err := storage.GetFromBucket("plugins", []byte("plugins"))
	if err != nil {
		t.Fatalf("read last-good plugin list: %v", err)
	}
	beforeSnapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("get last-good plugin snapshot: %v", err)
	}

	bad := NewAcknowledgedEvent()
	bad.Type = EventTypePut
	bad.Key = []byte("/apisix/plugins")
	bad.Value = []byte(`[{"name":123}]`)
	storage.events <- bad
	err = bad.Wait(context.Background())
	var validationErr *ResourceValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("malformed plugin list error = %v, want ResourceValidationError", err)
	}
	if validationErr.Bucket != "plugins" || validationErr.ID != "plugins" {
		t.Fatalf("validation context = %q/%q, want plugins/plugins", validationErr.Bucket, validationErr.ID)
	}
	after, err := storage.GetFromBucket("plugins", []byte("plugins"))
	if err != nil {
		t.Fatalf("read retained plugin list: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("retained plugin list = %q, want %q", after, before)
	}
	afterSnapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("get retained plugin snapshot: %v", err)
	}
	if afterSnapshot != beforeSnapshot || afterSnapshot.generation != beforeSnapshot.generation {
		t.Fatalf(
			"malformed plugin list published snapshot %p/%d, want %p/%d",
			afterSnapshot,
			afterSnapshot.generation,
			beforeSnapshot,
			beforeSnapshot.generation,
		)
	}
	if hooks != 1 {
		t.Fatalf("plugin reload hooks = %d, want 1 successful PUT", hooks)
	}
}

func TestDynamicPluginNullRetainsLastGoodAndSkipsReload(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	var hooks int
	storage.AddEventUpdateHook(func(event *Event) {
		if bucket, ok := EventBucket(event); ok && bucket == "plugins" {
			hooks++
		}
	})

	applyConfigSnapshotEvent(t, storage, EventTypePut, "/apisix/plugins", `[{"name":"request-id"}]`)
	before, err := storage.GetFromBucket("plugins", []byte("plugins"))
	if err != nil {
		t.Fatalf("read last-good plugin list: %v", err)
	}
	beforeSnapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("get last-good plugin snapshot: %v", err)
	}

	bad := NewAcknowledgedEvent()
	bad.Type = EventTypePut
	bad.Key = []byte("/apisix/plugins")
	bad.Value = []byte(`null`)
	storage.events <- bad
	err = bad.Wait(context.Background())
	var validationErr *ResourceValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("null plugin list error = %v, want ResourceValidationError", err)
	}
	after, err := storage.GetFromBucket("plugins", []byte("plugins"))
	if err != nil {
		t.Fatalf("read retained plugin list: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("retained plugin list = %q, want %q", after, before)
	}
	afterSnapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("get retained plugin snapshot: %v", err)
	}
	if afterSnapshot != beforeSnapshot || afterSnapshot.generation != beforeSnapshot.generation {
		t.Fatalf(
			"null plugin list published snapshot %p/%d, want %p/%d",
			afterSnapshot,
			afterSnapshot.generation,
			beforeSnapshot,
			beforeSnapshot.generation,
		)
	}
	if hooks != 1 {
		t.Fatalf("plugin reload hooks = %d, want 1 successful PUT", hooks)
	}
}

func TestDynamicPluginAliasKeysAreRejectedForEventAndBatch(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			storage := newConfigSnapshotTestStore(t)
			mutation := Mutation{
				Type:  EventTypePut,
				Key:   []byte("/apisix/plugins/plugins"),
				Value: []byte(`[{"name":"request-id"}]`),
			}
			var event *Event
			if batch {
				event = NewAcknowledgedBatch([]Mutation{mutation}, BatchOptions{})
			} else {
				event = NewAcknowledgedEvent()
				event.Type = mutation.Type
				event.Key = mutation.Key
				event.Value = mutation.Value
			}
			storage.events <- event
			err := event.Wait(context.Background())
			if batch {
				var validationErr *BatchValidationError
				if !errors.As(err, &validationErr) {
					t.Fatalf("alias batch error = %v, want BatchValidationError", err)
				}
			} else {
				var validationErr *ResourceValidationError
				if !errors.As(err, &validationErr) {
					t.Fatalf("alias event error = %v, want ResourceValidationError", err)
				}
			}
			stored, readErr := storage.GetFromBucket("plugins", []byte("plugins"))
			if readErr != nil {
				t.Fatalf("read plugin singleton: %v", readErr)
			}
			if stored != nil {
				t.Fatalf("alias event persisted plugin singleton: %q", stored)
			}
		})
	}
}

func TestConfigSnapshotConcurrentCallersUsePublishedGeneration(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	applyConfigSnapshotEvent(t, storage, EventTypePut, "/apisix/routes/route-1", `{"id":"route-1","uri":"/orders"}`)

	const callers = 16
	results := make(chan *ConfigSnapshot, callers)
	errorsCh := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			snapshot, err := storage.getConfigSnapshot()
			if err != nil {
				errorsCh <- err
				return
			}
			results <- snapshot
		}()
	}
	group.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent getConfigSnapshot() error = %v", err)
	}
	var first *ConfigSnapshot
	for snapshot := range results {
		if first == nil {
			first = snapshot
			continue
		}
		if snapshot != first {
			t.Fatalf("concurrent snapshot pointer = %p, want %p", snapshot, first)
		}
	}
	if first == nil || first.generation != storage.configGeneration.Load() {
		t.Fatalf("concurrent snapshot generation = %v, store generation = %d", first, storage.configGeneration.Load())
	}
}

func TestConfigSnapshotPublishesValidRoutesAndQuarantinesLegacyMalformedRows(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("routes"))
		if err := bucket.Put([]byte("legacy-bad"), []byte(`{"id":"legacy-bad","plugins":[]}`)); err != nil {
			return err
		}
		return bucket.Put([]byte("route-good"), []byte(`{"id":"route-good","uri":"/good"}`))
	}); err != nil {
		t.Fatalf("seed legacy routes: %v", err)
	}

	snapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("getConfigSnapshot() error = %v", err)
	}
	if len(snapshot.Routes()) != 1 || snapshot.Routes()[0].ID != "route-good" {
		t.Fatalf("snapshot routes = %+v, want only route-good", snapshot.Routes())
	}
	quarantined := snapshot.QuarantinedResources()
	if len(quarantined) != 1 || quarantined[0].Bucket != "routes" ||
		quarantined[0].ID != "legacy-bad" {
		t.Fatalf("snapshot quarantine = %+v, want routes/legacy-bad", quarantined)
	}

	applyConfigSnapshotEvent(
		t,
		storage,
		EventTypePut,
		"/apisix/routes/legacy-bad",
		`{"id":"legacy-bad","uri":"/recovered"}`,
	)
	snapshot, err = storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("getConfigSnapshot() after replacement error = %v", err)
	}
	if len(snapshot.QuarantinedResources()) != 0 {
		t.Fatalf(
			"snapshot quarantine after replacement = %+v, want empty",
			snapshot.QuarantinedResources(),
		)
	}

	applyConfigSnapshotEvent(t, storage, EventTypeDelete, "/apisix/routes/legacy-bad", "")
	snapshot, err = storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("getConfigSnapshot() after delete error = %v", err)
	}
	if len(snapshot.QuarantinedResources()) != 0 {
		t.Fatalf(
			"snapshot quarantine after delete = %+v, want empty",
			snapshot.QuarantinedResources(),
		)
	}
}

func TestConfigSnapshotPublishesValidGlobalRulesAndQuarantinesLegacyMalformedRows(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("global_rules"))
		if err := bucket.Put([]byte("legacy-bad-rule"), []byte(`{"id":"legacy-bad-rule","plugins":[]}`)); err != nil {
			return err
		}
		return bucket.Put([]byte("rule-good"), []byte(`{"id":"rule-good","plugins":{}}`))
	}); err != nil {
		t.Fatalf("seed legacy global rules: %v", err)
	}

	snapshot, err := storage.getConfigSnapshot()
	if err != nil {
		t.Fatalf("getConfigSnapshot() error = %v", err)
	}
	if len(snapshot.GlobalRules()) != 1 || snapshot.GlobalRules()[0].ID != "rule-good" {
		t.Fatalf("snapshot global rules = %+v, want only rule-good", snapshot.GlobalRules())
	}
	quarantined := snapshot.QuarantinedResources()
	if len(quarantined) != 1 || quarantined[0].Bucket != "global_rules" ||
		quarantined[0].ID != "legacy-bad-rule" {
		t.Fatalf("snapshot quarantine = %+v, want global_rules/legacy-bad-rule", quarantined)
	}
}

func TestConfigSnapshotIncludesCompleteHTTPBuildGeneration(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		for bucket, entries := range map[string]map[string]string{
			"routes": {
				"route-1": `{"id":"route-1","uri":"/orders","service_id":"service-1","plugin_config_id":"config-1","plugins":{"request-id":{"include_in_response":true}}}`,
			},
			"services": {
				"service-1": `{"id":"service-1","upstream_id":"upstream-1","plugins":{"request-id":{"include_in_response":true}}}`,
			},
			"upstreams": {
				"upstream-1": `{"scheme":"http","nodes":{"backend.test:80":1}}`,
			},
			"plugin_configs": {
				"config-1": `{"plugins":{"limit-req":{"rate":10}}}`,
			},
			"plugin_metadata": {
				"request-id": `{"header_name":"X-Request-ID","nested":{"enabled":true}}`,
			},
			"ssls": {
				"ssl-1": `{"id":"ssl-1","snis":["api.test"],"status":1}`,
			},
		} {
			bucketRef := tx.Bucket([]byte(bucket))
			for id, value := range entries {
				if err := bucketRef.Put([]byte(id), []byte(value)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed complete config snapshot: %v", err)
	}

	snapshot, err := storage.GetConfigSnapshot()
	if err != nil {
		t.Fatalf("GetConfigSnapshot() error = %v", err)
	}
	if len(snapshot.Routes()) != 1 {
		t.Fatalf("snapshot routes = %d, want 1", len(snapshot.Routes()))
	}
	if service, err := snapshot.GetService("service-1"); err != nil || service.UpstreamID != "upstream-1" {
		t.Fatalf("snapshot service = %+v/%v, want service-1 -> upstream-1", service, err)
	}
	if upstream, err := snapshot.GetUpstream("upstream-1"); err != nil || len(upstream.Nodes) != 1 {
		t.Fatalf("snapshot upstream = %+v/%v, want one node", upstream, err)
	}
	if config, err := snapshot.GetPluginConfigRule("config-1"); err != nil || config.Plugins["limit-req"] == nil {
		t.Fatalf("snapshot plugin config = %+v/%v, want limit-req", config, err)
	}
	if ssl, err := snapshot.GetSSL("ssl-1"); err != nil || ssl.Snis[0] != "api.test" {
		t.Fatalf("snapshot SSL = %+v/%v, want api.test", ssl, err)
	}

	routes := snapshot.Routes()
	routes[0].Uris = []string{"/mutated"}
	routes[0].Plugins["request-id"] = map[string]any{"mutated": true}
	metadata, ok := snapshot.PluginMetadata("request-id")
	if !ok {
		t.Fatal("snapshot metadata request-id missing")
	}
	metadata["nested"].(map[string]any)["enabled"] = false
	metadataAgain, ok := snapshot.PluginMetadata("request-id")
	routesAgain := snapshot.Routes()
	if !ok || routesAgain[0].Uri != "/orders" ||
		metadataAgain["nested"].(map[string]any)["enabled"] != true {
		t.Fatalf("snapshot returned mutable state: routes=%+v metadata=%+v", routesAgain, metadataAgain)
	}
}

func TestConfigSnapshotDoesNotMixServiceAndUpstreamGenerations(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte("services")).Put(
			[]byte("service-1"),
			[]byte(`{"id":"service-1","upstream_id":"upstream-old"}`),
		); err != nil {
			return err
		}
		return tx.Bucket([]byte("upstreams")).Put(
			[]byte("upstream-old"),
			[]byte(`{"scheme":"http","nodes":{"old.test:80":1}}`),
		)
	}); err != nil {
		t.Fatalf("seed initial service/upstream generation: %v", err)
	}

	var mutated bool
	storage.afterConfigSnapshotBucketRead = func(bucket string) {
		if bucket != "routes" || mutated {
			return
		}
		mutated = true
		event := NewAcknowledgedBatch([]Mutation{
			{
				Type:  EventTypePut,
				Key:   []byte("/apisix/services/service-1"),
				Value: []byte(`{"id":"service-1","upstream_id":"upstream-new"}`),
			},
			{Type: EventTypeDelete, Key: []byte("/apisix/upstreams/upstream-old")},
			{
				Type:  EventTypePut,
				Key:   []byte("/apisix/upstreams/upstream-new"),
				Value: []byte(`{"scheme":"http","nodes":{"new.test:80":1}}`),
			},
		}, BatchOptions{})
		storage.events <- event
		if err := event.Wait(context.Background()); err != nil {
			t.Errorf("apply replacement service/upstream generation: %v", err)
		}
	}

	snapshot, err := storage.GetConfigSnapshot()
	if err != nil {
		t.Fatalf("GetConfigSnapshot() error = %v", err)
	}
	service, err := snapshot.GetService("service-1")
	if err != nil {
		t.Fatalf("snapshot service lookup: %v", err)
	}
	if service.UpstreamID != "upstream-new" {
		t.Fatalf("snapshot service upstream_id = %q, want upstream-new", service.UpstreamID)
	}
	if _, err := snapshot.GetUpstream("upstream-old"); err != ErrNotFound {
		t.Fatalf("snapshot old upstream lookup error = %v, want ErrNotFound", err)
	}
	upstream, err := snapshot.GetUpstream("upstream-new")
	if err != nil || len(upstream.Nodes) != 1 || upstream.Nodes[0].Host != "new.test" {
		t.Fatalf("snapshot new upstream = %+v/%v, want new.test", upstream, err)
	}
}

func TestConfigSnapshotFailsClosedForMalformedPluginMetadata(t *testing.T) {
	storage := newConfigSnapshotTestStore(t)
	if err := storage.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("plugin_metadata")).Put(
			[]byte("metadata-bad"),
			[]byte(`[]`),
		)
	}); err != nil {
		t.Fatalf("seed malformed plugin metadata: %v", err)
	}

	if _, err := storage.GetConfigSnapshot(); err == nil ||
		!strings.Contains(err.Error(), "plugin_metadata") ||
		!strings.Contains(err.Error(), "metadata-bad") {
		t.Fatalf("GetConfigSnapshot() error = %v, want plugin_metadata/metadata-bad context", err)
	}
}

func newConfigSnapshotTestStore(t *testing.T) *Store {
	t.Helper()
	storage, err := Open(t.TempDir()+"/snapshot.db", make(chan *Event, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() {
		if err := storage.Stop(); err != nil {
			t.Errorf("Store.Stop() error = %v", err)
		}
	})
	return storage
}

func applyConfigSnapshotEvent(t *testing.T, storage *Store, eventType EventType, key, value string) {
	t.Helper()
	event := NewAcknowledgedEvent()
	event.Type = eventType
	event.Key = []byte(key)
	event.Value = []byte(value)
	storage.events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("apply %s event: %v", key, err)
	}
}
