package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/store"
)

func TestStandaloneReloadCancellationUnblocksBlockedSend(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: route-1\n    uri: /orders\n#END\n")
	events := make(chan *store.Event)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	reloadDone := make(chan error, 1)
	go func() { reloadDone <- watcher.Reload() }()
	waitForStandaloneReloadBlocked(t, watcher)
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case err := <-reloadDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reload() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reload() remained blocked after watcher cancellation")
	}
}

func TestStandaloneReloadCancellationUnblocksAcknowledgedWait(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: route-1\n    uri: /orders\n#END\n")
	events := make(chan *store.Event, 1)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	reloadDone := make(chan error, 1)
	go func() { reloadDone <- watcher.Reload() }()
	var queued *store.Event
	select {
	case queued = <-events:
	case <-time.After(time.Second):
		t.Fatal("Reload() did not enqueue an acknowledged event")
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case err := <-reloadDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reload() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reload() remained blocked in Event.Wait after cancellation")
	}
	store.PutBack(queued)
}

func TestStandaloneReloadDoesNotHoldStateMutexWhileSendBlocked(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: route-1\n    uri: /orders\n#END\n")
	events := make(chan *store.Event)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	reloadDone := make(chan error, 1)
	go func() { reloadDone <- watcher.Reload() }()
	mutexAvailable := make(chan struct{})
	go func() {
		watcher.mu.Lock()
		close(mutexAvailable)
		watcher.mu.Unlock()
	}()
	select {
	case <-mutexAvailable:
	case <-time.After(time.Second):
		t.Fatal("state mutex remained locked while event send was blocked")
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-reloadDone:
	case <-time.After(time.Second):
		t.Fatal("Reload() remained blocked after watcher cancellation")
	}
}

func TestStandaloneFailedSecondEventRetainsBaselineForReplay(t *testing.T) {
	path := writeStandaloneTestConfig(t, `routes:
  - id: route-1
    uri: /orders
ssls:
  - id: ssl-1
    status: 1
    cert: invalid
    key: invalid
#END
`)
	events := make(chan *store.Event, 4)
	storage, err := store.Open(filepath.Join(t.TempDir(), "store.db"), events)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })
	var routePuts atomic.Int32
	storage.AddEventUpdateHook(func(event *store.Event) {
		if event.Type == store.EventTypePut && string(event.Key) == "/apisix/routes/route-1" {
			routePuts.Add(1)
		}
	})
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if err := watcher.Reload(); err == nil {
		t.Fatal("Reload() error = nil, want second-event acknowledgement failure")
	}
	if route, err := storage.GetFromBucket("routes", []byte("route-1")); err != nil || len(route) == 0 {
		t.Fatalf("route after partial apply = %q, %v; want durable first event", route, err)
	}

	if err := os.WriteFile(path, []byte(`routes:
  - id: route-1
    uri: /orders
ssls:
  - id: ssl-1
    status: 1
    cert: invalid
    key: invalid
#END
`), 0o600); err != nil {
		t.Fatalf("rewrite standalone config: %v", err)
	}
	if err := watcher.Reload(); err == nil {
		t.Fatal("second Reload() error = nil, want SSL acknowledgement failure")
	}
	if got := routePuts.Load(); got != 2 {
		t.Fatalf("route PUT applications = %d, want 2 replayed applications", got)
	}
	// The route remains part of the replay because the failed batch never
	// committed its full snapshot baseline.
	if route, err := storage.GetFromBucket("routes", []byte("route-1")); err != nil || len(route) == 0 {
		t.Fatalf("route after replay attempt = %q, %v; want durable route", route, err)
	}
}

func TestStandaloneStopBeforeStartAndRepeatedStop(t *testing.T) {
	watcher := NewStandaloneFileWatcher(filepath.Join(t.TempDir(), "apisix.yaml"), "yaml", make(chan *store.Event))
	if err := watcher.Stop(); err != nil {
		t.Fatalf("Stop() before Start error = %v", err)
	}
	if err := watcher.Stop(); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() after Stop() error = %v", err)
	}
}

func TestStandaloneStopWaitsForWatchExit(t *testing.T) {
	path := writeStandaloneTestConfig(t, "routes:\n  - id: route-1\n    uri: /orders\n#END\n")
	snapshot, err := readStandaloneSnapshot(path, "yaml")
	if err != nil {
		t.Fatalf("readStandaloneSnapshot() error = %v", err)
	}
	watcher := NewStandaloneFileWatcher(path, "yaml", make(chan *store.Event))
	watcher.SeedCurrentSnapshot(snapshot)
	callbackEntered := make(chan struct{})
	releaseCallback := make(chan struct{})
	watcher.SetReloadCallback(func(StandaloneReloadResult, error) {
		close(callbackEntered)
		<-releaseCallback
	})
	if err := watcher.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("routes:\n  - id: route-1\n    uri: /orders\n#END\n"), 0o600); err != nil {
		t.Fatalf("rewrite standalone config: %v", err)
	}
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		t.Fatal("watch callback did not start")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- watcher.Stop() }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop() returned before watch callback exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCallback)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop() did not wait for watch goroutine exit")
	}
}

func writeStandaloneTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}
	return path
}

func waitForStandaloneReloadBlocked(t *testing.T, watcher *StandaloneFileWatcher) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !watcher.reloadMu.TryLock() {
			if watcher.mu.TryLock() {
				watcher.mu.Unlock()
				return
			}
		} else {
			watcher.reloadMu.Unlock()
		}
		runtime.Gosched()
	}
	t.Fatal("standalone reload did not reach its blocked send")
}

func newStandaloneTestStore(t *testing.T, events chan *store.Event) *store.Store {
	t.Helper()
	storage, err := store.Open(filepath.Join(t.TempDir(), "store.db"), events)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	storage.Start()
	t.Cleanup(func() {
		if err := storage.Stop(); err != nil {
			t.Errorf("Store.Stop() error = %v", err)
		}
	})
	return storage
}

func TestStandaloneReloadFailureRetainsPreviousSnapshotForReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := "routes:\n  - id: route-1\n    uri: /orders\n#END\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	events := make(chan *store.Event, 2)
	storage := newStandaloneTestStore(t, events)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	var attempts []StandaloneReloadResult
	watcher.SetAcknowledgedReloadCallback(func(result StandaloneReloadResult, err error) error {
		if err != nil {
			return err
		}
		attempts = append(attempts, result)
		if len(attempts) == 1 {
			return errors.New("store apply failed")
		}
		return nil
	})

	watcher.reloadAndNotify()
	watcher.reloadAndNotify()
	if err := storage.Sync(); err != nil {
		t.Fatalf("Store.Sync() error = %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("reload attempts = %d, want 2", len(attempts))
	}
	for index, attempt := range attempts {
		if !attempt.AffectsHTTPRoutes() {
			t.Fatalf("reload attempt %d did not replay the route change", index+1)
		}
	}
}

func TestStandaloneLegacyReloadCallbackCanReenterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := "routes:\n  - id: route-1\n    uri: /orders\n#END\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	events := make(chan *store.Event, 1)
	newStandaloneTestStore(t, events)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	done := make(chan error, 1)
	watcher.SetReloadCallback(func(StandaloneReloadResult, error) {
		_, err := watcher.ReloadSnapshot()
		done <- err
	})
	go watcher.reloadAndNotify()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reentrant ReloadSnapshot() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy reload callback deadlocked while reentering ReloadSnapshot")
	}
}

func TestStandaloneFileWatcherLoadsYAMLAndJSON(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		ext      string
		content  string
	}{
		{
			name:     "yaml",
			provider: "yaml",
			ext:      ".yaml",
			content: `routes:
  - id: 1
    uri: /hello
    upstream:
      nodes:
        "127.0.0.1:1980": 1
      type: roundrobin
upstreams:
  - id: 2
    nodes:
      "127.0.0.1:1981": 1
    type: roundrobin
#END
`,
		},
		{
			name:     "json",
			provider: "json",
			ext:      ".json",
			content: `{
  "routes": [{
    "id": 1,
    "uri": "/hello",
    "upstream": {
      "nodes": {"127.0.0.1:1980": 1},
      "type": "roundrobin"
    }
  }],
  "upstreams": [{
    "id": 2,
    "nodes": {"127.0.0.1:1981": 1},
    "type": "roundrobin"
  }]
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "apisix"+tt.ext)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write standalone config: %v", err)
			}

			events := make(chan *store.Event, 8)
			storage := newStandaloneTestStore(t, events)
			watcher := NewStandaloneFileWatcher(path, tt.provider, events)
			if err := watcher.Reload(); err != nil {
				t.Fatalf("Reload() error = %v", err)
			}

			if err := storage.Sync(); err != nil {
				t.Fatalf("Store.Sync() error = %v", err)
			}
			raw, err := storage.GetFromBucket("routes", []byte("1"))
			if err != nil {
				t.Fatalf("GetFromBucket(routes) error = %v", err)
			}
			var route map[string]any
			if err := json.Unmarshal(raw, &route); err != nil {
				t.Fatalf("decode loaded route: %v", err)
			}
			if got, want := route["id"], "1"; got != want {
				t.Fatalf("normalized route id = %#v, want %q", got, want)
			}
		})
	}
}

func TestStandaloneYAMLRequiresEndMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	if err := os.WriteFile(path, []byte("routes:\n  - id: route-1\n    uri: /hello\n"), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	watcher := NewStandaloneFileWatcher(path, "yaml", make(chan *store.Event, 1))
	if err := watcher.Reload(); err == nil {
		t.Fatal("Reload() error = nil, want missing #END error")
	}
}

func TestStandaloneFileWatcherLoadsSecretResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := `secrets:
  - id: vault/test1
    uri: http://127.0.0.1:8200
    prefix: kv/apisix
    token: root
#END
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	events := make(chan *store.Event, 2)
	storage := newStandaloneTestStore(t, events)
	if err := NewStandaloneFileWatcher(path, "yaml", events).Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Store.Sync() error = %v", err)
	}
	if got, err := storage.GetFromBucket("secrets", []byte("vault/test1")); err != nil || len(got) == 0 {
		t.Fatalf("stored secret = %q, %v; want Vault secret resource", got, err)
	}
}

func TestStandaloneFileWatcherEncryptsAIRateLimitingPasswordsBeforeStoreEvents(t *testing.T) {
	const key = "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{key})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := `routes:
  - id: route-1
    uri: /ai
    plugins:
      ai-rate-limiting:
        limit: 30
        time_window: 60
        redis_password: redis-plaintext
        sentinel_password: sentinel-plaintext
      loggly:
        customer_token: loggly-plaintext
#END
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	events := make(chan *store.Event, 2)
	storage := newStandaloneTestStore(t, events)
	if err := NewStandaloneFileWatcher(path, "yaml", events).Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Store.Sync() error = %v", err)
	}
	raw, err := storage.GetFromBucket("routes", []byte("route-1"))
	if err != nil {
		t.Fatalf("GetFromBucket(routes) error = %v", err)
	}
	if strings.Contains(string(raw), "redis-plaintext") ||
		strings.Contains(string(raw), "sentinel-plaintext") ||
		strings.Contains(string(raw), "loggly-plaintext") {
		t.Fatalf("stored route contains plaintext secret: %s", raw)
	}
	var route struct {
		Plugins map[string]map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &route); err != nil {
		t.Fatalf("decode stored route: %v", err)
	}
	for field, plaintext := range map[string]string{
		"redis_password":    "redis-plaintext",
		"sentinel_password": "sentinel-plaintext",
	} {
		ciphertext, ok := route.Plugins["ai-rate-limiting"][field].(string)
		if !ok {
			t.Fatalf("%s = %T, want ciphertext string", field, route.Plugins["ai-rate-limiting"][field])
		}
		decrypted, err := data_encryption.Decrypt(ciphertext, []string{key})
		if err != nil || decrypted != plaintext {
			t.Fatalf("Decrypt(%s) = (%q, %v), want %q", field, decrypted, err, plaintext)
		}
	}
	logglyCiphertext, ok := route.Plugins["loggly"]["customer_token"].(string)
	if !ok {
		t.Fatalf("loggly.customer_token = %T, want ciphertext string", route.Plugins["loggly"]["customer_token"])
	}
	if decrypted, err := data_encryption.Decrypt(logglyCiphertext, []string{key}); err != nil ||
		decrypted != "loggly-plaintext" {
		t.Fatalf("Decrypt(loggly.customer_token) = (%q, %v), want loggly-plaintext", decrypted, err)
	}
}

func TestStandaloneFileWatcherEncryptsPluginMetadataBeforeRuntimeDecryption(t *testing.T) {
	const (
		key                       = "edd1c9f0985e76a2"
		ciphertextShapedPlaintext = "OqkDYcQx4FvgBsxFCybRzg=="
	)
	data_encryption.Configure(true, []string{key})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := `plugin_metadata:
  - id: azure-functions
    master_apikey: ` + ciphertextShapedPlaintext + `
    master_clientid: master-client
#END
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	events := make(chan *store.Event, 4)
	storage, err := store.GetStore(filepath.Join(t.TempDir(), "store.db"), events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })
	if err := NewStandaloneFileWatcher(path, "yaml", events).Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	raw, err := storage.GetFromBucket("plugin_metadata", []byte("azure-functions"))
	if err != nil {
		t.Fatalf("GetFromBucket() error = %v", err)
	}
	if strings.Contains(string(raw), ciphertextShapedPlaintext) {
		t.Fatalf("stored plugin metadata contains plaintext secret: %s", raw)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode stored plugin metadata: %v", err)
	}
	ciphertext, ok := stored["master_apikey"].(string)
	if !ok {
		t.Fatalf("stored master_apikey = %T, want ciphertext string", stored["master_apikey"])
	}
	if decrypted, err := data_encryption.Decrypt(ciphertext, []string{key}); err != nil ||
		decrypted != ciphertextShapedPlaintext {
		t.Fatalf("Decrypt(master_apikey) = (%q, %v), want %q", decrypted, err, ciphertextShapedPlaintext)
	}

	var runtimeMetadata struct {
		MasterAPIKey   string `json:"master_apikey"`
		MasterClientID string `json:"master_clientid"`
	}
	if err := store.GetPluginMetadata("azure-functions", &runtimeMetadata); err != nil {
		t.Fatalf("GetPluginMetadata() error = %v", err)
	}
	if runtimeMetadata.MasterAPIKey != ciphertextShapedPlaintext ||
		runtimeMetadata.MasterClientID != "master-client" {
		t.Fatalf("runtime plugin metadata = %#v, want decrypted secret", runtimeMetadata)
	}
}

func TestStandaloneFileWatcherDeletesRemovedResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	initial := `routes:
  - id: route-1
    uri: /one
upstreams:
  - id: upstream-1
    nodes:
      "127.0.0.1:1980": 1
#END
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial standalone config: %v", err)
	}

	events := make(chan *store.Event, 8)
	storage := newStandaloneTestStore(t, events)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if err := watcher.Reload(); err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("initial Store.Sync() error = %v", err)
	}

	updated := `routes:
  - id: route-2
    uri: /two
#END
`
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("write updated standalone config: %v", err)
	}
	if err := watcher.Reload(); err != nil {
		t.Fatalf("updated Reload() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("updated Store.Sync() error = %v", err)
	}
	for bucket, id := range map[string]string{
		"routes":    "route-1",
		"upstreams": "upstream-1",
	} {
		value, err := storage.GetFromBucket(bucket, []byte(id))
		if err != nil {
			t.Fatalf("GetFromBucket(%s/%s) error = %v", bucket, id, err)
		}
		if value != nil {
			t.Fatalf("removed %s/%s = %q, want deleted", bucket, id, value)
		}
	}
	if value, err := storage.GetFromBucket("routes", []byte("route-2")); err != nil || len(value) == 0 {
		t.Fatalf("new route = %q, %v; want persisted route", value, err)
	}
}

func TestStandaloneReloadSnapshotReportsChangedRouteBucketsAfterFullDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	initial := `routes:
  - id: route-1
    uri: /one
upstreams:
  - id: upstream-1
    nodes:
      "127.0.0.1:1980": 1
plugin_metadata:
  - id: metadata-1
    value: one
#END
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial standalone config: %v", err)
	}

	events := make(chan *store.Event, 8)
	storage := newStandaloneTestStore(t, events)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	result, err := watcher.ReloadSnapshot()
	if err != nil {
		t.Fatalf("ReloadSnapshot() error = %v", err)
	}
	if got, want := result.ChangedHTTPRouteBuckets,
		[]string{"routes", "upstreams", "plugin_metadata"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed HTTP route buckets = %v, want %v", got, want)
	}
	if got, want := result.ChangedStreamBuckets, []string{"upstreams"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed stream buckets = %v, want %v", got, want)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("initial Store.Sync() error = %v", err)
	}
	updated := `routes:
  - id: route-1
    uri: /two
upstreams:
  - id: upstream-1
    nodes:
      "127.0.0.1:1980": 1
plugin_metadata:
  - id: metadata-1
    value: two
#END
`
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("write updated standalone config: %v", err)
	}
	result, err = watcher.ReloadSnapshot()
	if err != nil {
		t.Fatalf("updated ReloadSnapshot() error = %v", err)
	}
	if got, want := result.ChangedHTTPRouteBuckets,
		[]string{"routes", "plugin_metadata"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated changed HTTP route buckets = %v, want %v", got, want)
	}
	if len(result.ChangedStreamBuckets) != 0 {
		t.Fatalf("updated changed stream buckets = %v, want none", result.ChangedStreamBuckets)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("updated Store.Sync() error = %v", err)
	}
}

func TestStandaloneReloadSnapshotReportsMetadataOnlyChangeAsRouteChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	initial := `plugin_metadata:
  - id: metadata-1
    value: one
#END
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial standalone config: %v", err)
	}

	events := make(chan *store.Event, 4)
	storage := newStandaloneTestStore(t, events)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if _, err := watcher.ReloadSnapshot(); err != nil {
		t.Fatalf("initial ReloadSnapshot() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("initial Store.Sync() error = %v", err)
	}

	updated := `plugin_metadata:
  - id: metadata-1
    value: two
#END
`
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("write updated standalone config: %v", err)
	}
	result, err := watcher.ReloadSnapshot()
	if err != nil {
		t.Fatalf("updated ReloadSnapshot() error = %v", err)
	}
	if got, want := result.ChangedHTTPRouteBuckets, []string{"plugin_metadata"}; !reflect.DeepEqual(got, want) {
		t.Fatalf(
			"metadata-only changed HTTP buckets = %v, want %v",
			got,
			want,
		)
	}
	if len(result.ChangedStreamBuckets) != 0 {
		t.Fatalf("metadata-only changed stream buckets = %v, want none", result.ChangedStreamBuckets)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("updated Store.Sync() error = %v", err)
	}
}

func TestStandaloneReloadSnapshotReportsStreamRoutesWithoutHTTPRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := `stream_routes:
  - id: stream-1
    server_addr: 127.0.0.1
    server_port: 9100
    upstream:
      scheme: tcp
      nodes:
        "127.0.0.1:1883": 1
#END
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	events := make(chan *store.Event, 2)
	newStandaloneTestStore(t, events)
	result, err := NewStandaloneFileWatcher(path, "yaml", events).ReloadSnapshot()
	if err != nil {
		t.Fatalf("ReloadSnapshot() error = %v", err)
	}
	if len(result.ChangedHTTPRouteBuckets) != 0 {
		t.Fatalf("stream-only changed HTTP route buckets = %v, want none", result.ChangedHTTPRouteBuckets)
	}
	if got, want := result.ChangedStreamBuckets, []string{"stream_routes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed stream buckets = %v, want %v", got, want)
	}
}

func TestStandaloneReloadSnapshotReportsBuilderResourceBucketsAsHTTPChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := `global_rules:
  - id: global-1
    plugins: {}
plugin_configs:
  - id: config-1
    plugins: {}
#END
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	events := make(chan *store.Event, 4)
	newStandaloneTestStore(t, events)
	result, err := NewStandaloneFileWatcher(path, "yaml", events).ReloadSnapshot()
	if err != nil {
		t.Fatalf("ReloadSnapshot() error = %v", err)
	}
	want := []string{"global_rules", "plugin_configs"}
	if !reflect.DeepEqual(result.ChangedHTTPRouteBuckets, want) {
		t.Fatalf("changed HTTP route buckets = %v, want %v", result.ChangedHTTPRouteBuckets, want)
	}
	if len(result.ChangedStreamBuckets) != 0 {
		t.Fatalf("changed stream buckets = %v, want none", result.ChangedStreamBuckets)
	}
}

func TestStandaloneWatchReconcilesUpdateBeforeRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	initial := "routes:\n  - id: route-1\n    uri: /one\n#END\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial standalone config: %v", err)
	}

	events := make(chan *store.Event, 4)
	storage := newStandaloneTestStore(t, events)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	t.Cleanup(func() { _ = watcher.Stop() })
	if err := watcher.Reload(); err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("initial Store.Sync() error = %v", err)
	}

	updated := "routes:\n  - id: route-1\n    uri: /two\n#END\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatalf("write update before Watch: %v", err)
	}
	type reloadAttempt struct {
		result StandaloneReloadResult
		err    error
	}
	attempts := make(chan reloadAttempt, 2)
	watcher.SetReloadCallback(func(result StandaloneReloadResult, err error) {
		attempts <- reloadAttempt{result: result, err: err}
	})
	watcher.Watch()

	select {
	case attempt := <-attempts:
		if attempt.err != nil {
			t.Fatalf("Watch reconciliation error = %v", attempt.err)
		}
		if got, want := attempt.result.ChangedHTTPRouteBuckets, []string{"routes"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("reconciled HTTP route buckets = %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not reconcile the update written before registration")
	}

	if err := storage.Sync(); err != nil {
		t.Fatalf("reconciled Store.Sync() error = %v", err)
	}
	raw, err := storage.GetFromBucket("routes", []byte("route-1"))
	if err != nil {
		t.Fatalf("GetFromBucket(routes) error = %v", err)
	}
	var route map[string]any
	if err := json.Unmarshal(raw, &route); err != nil {
		t.Fatalf("decode reconciled route: %v", err)
	}
	if got, want := route["uri"], "/two"; got != want {
		t.Fatalf("reconciled route URI = %#v, want %q", got, want)
	}
}

func TestStandaloneFileWatcherRecoversAfterAtomicInvalidReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	initial := `routes:
  - id: route-1
    uri: /one
#END
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial standalone config: %v", err)
	}

	events := make(chan *store.Event, 8)
	storage := newStandaloneTestStore(t, events)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	t.Cleanup(func() { _ = watcher.Stop() })
	type reloadAttempt struct {
		result StandaloneReloadResult
		err    error
	}
	reloadAttempts := make(chan reloadAttempt, 8)
	watcher.SetReloadCallback(func(result StandaloneReloadResult, err error) {
		reloadAttempts <- reloadAttempt{result: result, err: err}
	})
	if err := watcher.Reload(); err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("initial Store.Sync() error = %v", err)
	}
	watcher.Watch()

	invalid := []byte("routes:\n  - id: route-1\n    uri: /partial\n")
	if err := atomicReplaceStandaloneTestFile(path, invalid); err != nil {
		t.Fatalf("replace with incomplete standalone config: %v", err)
	}
	for {
		select {
		case attempt := <-reloadAttempts:
			if attempt.err != nil && strings.Contains(attempt.err.Error(), "must end with #END") {
				if len(attempt.result.ChangedHTTPRouteBuckets) != 0 || len(attempt.result.ChangedStreamBuckets) != 0 {
					t.Fatalf(
						"failed snapshot changed buckets = HTTP %v stream %v, want none",
						attempt.result.ChangedHTTPRouteBuckets,
						attempt.result.ChangedStreamBuckets,
					)
				}
				goto invalidObserved
			}
		case <-time.After(time.Second):
			t.Fatal("watcher did not report the invalid standalone snapshot")
		}
	}

invalidObserved:
	if value, err := storage.GetFromBucket("routes", []byte("route-1")); err != nil || string(value) == "" {
		t.Fatalf("route after invalid replacement = %q, %v; want previous durable value", value, err)
	}

	updated := []byte(`routes:
  - id: route-1
    uri: /two
#END
`)
	if err := atomicReplaceStandaloneTestFile(path, updated); err != nil {
		t.Fatalf("replace with complete standalone config: %v", err)
	}
	for {
		select {
		case attempt := <-reloadAttempts:
			if attempt.err == nil {
				got := attempt.result.ChangedHTTPRouteBuckets
				want := []string{"routes"}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("valid snapshot changed HTTP route buckets = %v, want %v", got, want)
				}
				goto validObserved
			}
		case <-time.After(time.Second):
			t.Fatal("watcher did not acknowledge the complete standalone snapshot")
		}
	}

validObserved:
	if err := storage.Sync(); err != nil {
		t.Fatalf("updated Store.Sync() error = %v", err)
	}
	value, err := storage.GetFromBucket("routes", []byte("route-1"))
	if err != nil {
		t.Fatalf("GetFromBucket(routes) error = %v", err)
	}
	var route map[string]any
	if err := json.Unmarshal(value, &route); err != nil {
		t.Fatalf("decode updated route: %v", err)
	}
	if got, want := route["uri"], "/two"; got != want {
		t.Fatalf("updated route URI = %#v, want %q", got, want)
	}
}

func TestStandaloneConfigFile(t *testing.T) {
	if got, want := StandaloneConfigFile("yaml"), "conf/apisix.yaml"; got != want {
		t.Fatalf("StandaloneConfigFile(yaml) = %q, want %q", got, want)
	}
	if got, want := StandaloneConfigFile("json"), "conf/apisix.json"; got != want {
		t.Fatalf("StandaloneConfigFile(json) = %q, want %q", got, want)
	}
	if got := StandaloneConfigFile("unsupported"); got != "" {
		t.Fatalf("StandaloneConfigFile(unsupported) = %q, want empty", got)
	}
}

func TestStandaloneReloadResultReportsAffectedSubsystems(t *testing.T) {
	if (StandaloneReloadResult{}).AffectsHTTPRoutes() || (StandaloneReloadResult{}).AffectsStreams() {
		t.Fatal("empty reload result reported affected resources")
	}
	if !(StandaloneReloadResult{ChangedHTTPRouteBuckets: []string{"routes"}}).AffectsHTTPRoutes() {
		t.Fatal("HTTP bucket change was not reported")
	}
	if !(StandaloneReloadResult{ChangedStreamBuckets: []string{"stream_routes"}}).AffectsStreams() {
		t.Fatal("stream bucket change was not reported")
	}
}

func TestReadStandaloneSnapshotRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		content   string
		wantError string
	}{
		{
			name:      "unsupported provider",
			provider:  "toml",
			content:   "",
			wantError: "unsupported standalone config provider",
		},
		{name: "invalid JSON", provider: "json", content: `{`, wantError: "parse standalone JSON config"},
		{
			name:      "section is not array",
			provider:  "json",
			content:   `{"routes":{}}`,
			wantError: "decode standalone routes",
		},
		{name: "resource missing ID", provider: "json", content: `{"routes":[{"uri":"/"}]}`, wantError: "missing id"},
		{name: "invalid YAML", provider: "yaml", content: "routes: [\n#END", wantError: "parse standalone YAML config"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "apisix."+test.provider)
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatalf("write standalone fixture: %v", err)
			}
			_, err := readStandaloneSnapshot(path, test.provider)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("readStandaloneSnapshot() error = %v, want containing %q", err, test.wantError)
			}
		})
	}

	_, err := readStandaloneSnapshot(filepath.Join(t.TempDir(), "missing.json"), standaloneProviderJSON)
	if err == nil || !strings.Contains(err.Error(), "read standalone config") {
		t.Fatalf("readStandaloneSnapshot(missing) error = %v", err)
	}
}

func TestNormalizeStandaloneResourceValidatesIDsAndPlugins(t *testing.T) {
	tests := []struct {
		name      string
		bucket    string
		raw       string
		wantError string
	}{
		{name: "malformed resource", bucket: "routes", raw: `{`, wantError: "invalid character"},
		{name: "empty ID", bucket: "routes", raw: `{"id":""}`, wantError: "id is empty"},
		{name: "object ID", bucket: "routes", raw: `{"id":{}}`, wantError: "id must be a string or number"},
		{
			name:      "invalid plugins",
			bucket:    "routes",
			raw:       `{"id":"route-a","plugins":"invalid"}`,
			wantError: "decode plugins",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := normalizeStandaloneResource(test.bucket, json.RawMessage(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("normalizeStandaloneResource() error = %v, want containing %q", err, test.wantError)
			}
		})
	}

	id, encoded, err := normalizeStandaloneResource("routes", json.RawMessage(`{"id":42,"uri":"/number"}`))
	if err != nil {
		t.Fatalf("normalizeStandaloneResource(number ID) error = %v", err)
	}
	if id != "42" || !strings.Contains(string(encoded), `"id":"42"`) {
		t.Fatalf("normalized number ID = %q, %s", id, encoded)
	}

	id, encoded, err = normalizeStandaloneResource("consumers", json.RawMessage(`{"username":"alice","id":"ignored"}`))
	if err != nil {
		t.Fatalf("normalizeStandaloneResource(consumer) error = %v", err)
	}
	if id != "alice" || !strings.Contains(string(encoded), `"username":"alice"`) {
		t.Fatalf("normalized consumer = %q, %s", id, encoded)
	}
}

func TestStandaloneProviderFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"conf/apisix.yaml": "yaml",
		"conf/apisix.JSON": "json",
		"conf/apisix":      "",
	} {
		if got := standaloneProviderFromPath(path); got != want {
			t.Fatalf("standaloneProviderFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func atomicReplaceStandaloneTestFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".standalone-test-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
