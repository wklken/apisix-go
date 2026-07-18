package config

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if err := watcher.Reload(); err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	collectStandaloneEvents(events)
	watcher.Watch()

	invalid := []byte("routes:\n  - id: route-1\n    uri: /partial\n")
	if err := atomicReplaceStandaloneTestFile(path, invalid); err != nil {
		t.Fatalf("replace with incomplete standalone config: %v", err)
	}
	select {
	case event := <-events:
		t.Fatalf("incomplete snapshot emitted event %#v", event)
	case <-time.After(50 * time.Millisecond):
	}

	updated := []byte(`routes:
  - id: route-1
    uri: /two
#END
`)
	if err := atomicReplaceStandaloneTestFile(path, updated); err != nil {
		t.Fatalf("replace with complete standalone config: %v", err)
	}
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
