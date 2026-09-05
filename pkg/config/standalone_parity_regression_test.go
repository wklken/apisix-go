package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestStandaloneOmittedIDGetsArrayIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	content := `routes:
  - uri: /one
  - id: explicit
    uri: /two
  - uri: /three
services:
  - desc: unnamed
consumers:
  - username: alice
#END
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readStandaloneSnapshot(path, "yaml", testStandaloneDataEncryption(t, false, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"arr_1", "explicit", "arr_3"} {
		var fields map[string]any
		if err := json.Unmarshal(snapshot["routes"][id], &fields); err != nil {
			t.Fatal(err)
		}
		if fields["id"] != id {
			t.Fatalf("route %s normalized as %v", id, fields)
		}
	}
	if len(snapshot["routes"]) != 3 || snapshot["services"]["arr_1"] == nil || snapshot["consumers"]["alice"] == nil {
		t.Fatalf("unexpected identities: %v", snapshot)
	}
}

func TestStandaloneProjectedVolumeSwapReloads(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"v1", "v2"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		content := "routes:\n  - id: route\n    uri: /" + name + "\n#END\n"
		path := filepath.Join(dir, name, "apisix.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(100, 0)
		if name == "v2" {
			stamp = time.Unix(200, 0)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "apisix.yaml")
	if err := os.Symlink("..data/apisix.yaml", path); err != nil {
		t.Fatal(err)
	}
	applier := &recordingStandaloneApplier{}
	watcher := NewStandaloneFileWatcher(path, "yaml", applier, testStandaloneDataEncryption(t, false, nil))
	defer func() { _ = watcher.Stop() }()
	if err := watcher.StartAndReconcile(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v2", filepath.Join(dir, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		batches := applier.snapshot()
		if len(batches) > 1 {
			for _, mutation := range batches[len(batches)-1].Mutations {
				if strings.Contains(string(mutation.Value), "/v2") {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("replacement readable=%q but only %d Apply call(s), wanted reload", fileBytes, len(applier.snapshot()))
}
