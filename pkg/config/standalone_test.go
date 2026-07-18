package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/store"
)

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
			watcher := NewStandaloneFileWatcher(path, tt.provider, events)
			if err := watcher.Reload(); err != nil {
				t.Fatalf("Reload() error = %v", err)
			}

			got := collectStandaloneEvents(events)
			if len(got) != 2 {
				t.Fatalf("loaded event count = %d, want 2", len(got))
			}
			for _, key := range []string{"/apisix/routes/1", "/apisix/upstreams/2"} {
				if _, ok := got[key]; !ok {
					t.Fatalf("loaded events do not contain %q: %#v", key, got)
				}
			}

			var route map[string]any
			if err := json.Unmarshal(got["/apisix/routes/1"].Value, &route); err != nil {
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
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if err := watcher.Reload(); err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	collectStandaloneEvents(events)

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

	got := collectStandaloneEvents(events)
	if got["/apisix/routes/route-1"].Type != store.EventTypeDelete {
		t.Fatalf("removed route event = %#v, want delete", got["/apisix/routes/route-1"])
	}
	if got["/apisix/upstreams/upstream-1"].Type != store.EventTypeDelete {
		t.Fatalf("removed upstream event = %#v, want delete", got["/apisix/upstreams/upstream-1"])
	}
	if got["/apisix/routes/route-2"].Type != store.EventTypePut {
		t.Fatalf("new route event = %#v, want put", got["/apisix/routes/route-2"])
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
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	result, err := watcher.ReloadSnapshot()
	if err != nil {
		t.Fatalf("ReloadSnapshot() error = %v", err)
	}
	if got, want := result.ChangedHTTPRouteBuckets, []string{"routes", "upstreams"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed HTTP route buckets = %v, want %v", got, want)
	}
	if got, want := result.ChangedStreamBuckets, []string{"upstreams"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed stream buckets = %v, want %v", got, want)
	}
	if got := len(events); got != 3 {
		t.Fatalf("queued event count at snapshot acknowledgement = %d, want 3", got)
	}

	collectStandaloneEvents(events)
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
	if got, want := result.ChangedHTTPRouteBuckets, []string{"routes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updated changed HTTP route buckets = %v, want %v", got, want)
	}
	if len(result.ChangedStreamBuckets) != 0 {
		t.Fatalf("updated changed stream buckets = %v, want none", result.ChangedStreamBuckets)
	}
	if got := len(events); got != 2 {
		t.Fatalf("updated queued event count at snapshot acknowledgement = %d, want 2", got)
	}
}

func TestStandaloneReloadSnapshotDoesNotReportMetadataOnlyChangeAsRouteChange(t *testing.T) {
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
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if _, err := watcher.ReloadSnapshot(); err != nil {
		t.Fatalf("initial ReloadSnapshot() error = %v", err)
	}
	collectStandaloneEvents(events)

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
	if len(result.ChangedHTTPRouteBuckets) != 0 || len(result.ChangedStreamBuckets) != 0 {
		t.Fatalf(
			"metadata-only changed buckets = HTTP %v stream %v, want none",
			result.ChangedHTTPRouteBuckets,
			result.ChangedStreamBuckets,
		)
	}
	if got := len(events); got != 1 {
		t.Fatalf("metadata event count = %d, want 1", got)
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

func TestStandaloneWatchReconcilesUpdateBeforeRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	initial := "routes:\n  - id: route-1\n    uri: /one\n#END\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial standalone config: %v", err)
	}

	events := make(chan *store.Event, 4)
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
	if err := watcher.Reload(); err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	collectStandaloneEvents(events)

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

	select {
	case event := <-events:
		var route map[string]any
		if err := json.Unmarshal(event.Value, &route); err != nil {
			t.Fatalf("decode reconciled route: %v", err)
		}
		if got, want := route["uri"], "/two"; got != want {
			t.Fatalf("reconciled route URI = %#v, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch reconciliation did not emit the pre-registration update")
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
	watcher := NewStandaloneFileWatcher(path, "yaml", events)
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
	collectStandaloneEvents(events)
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
	select {
	case event := <-events:
		t.Fatalf("incomplete snapshot emitted event %#v", event)
	default:
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
	select {
	case event := <-events:
		if got, want := string(event.Key), "/apisix/routes/route-1"; got != want {
			t.Fatalf("updated event key = %q, want %q", got, want)
		}
		var route map[string]any
		if err := json.Unmarshal(event.Value, &route); err != nil {
			t.Fatalf("decode updated route: %v", err)
		}
		if got, want := route["uri"], "/two"; got != want {
			t.Fatalf("updated route URI = %#v, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher did not recover after an atomic invalid replacement")
	}
}

func TestStandaloneConfigFile(t *testing.T) {
	if got, want := StandaloneConfigFile("yaml"), "conf/apisix.yaml"; got != want {
		t.Fatalf("StandaloneConfigFile(yaml) = %q, want %q", got, want)
	}
	if got, want := StandaloneConfigFile("json"), "conf/apisix.json"; got != want {
		t.Fatalf("StandaloneConfigFile(json) = %q, want %q", got, want)
	}
}

type standaloneEvent struct {
	Type  store.EventType
	Value []byte
}

func collectStandaloneEvents(events chan *store.Event) map[string]standaloneEvent {
	collected := make(map[string]standaloneEvent)
	for {
		select {
		case event := <-events:
			collected[string(event.Key)] = standaloneEvent{
				Type:  event.Type,
				Value: append([]byte(nil), event.Value...),
			}
		default:
			return collected
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
