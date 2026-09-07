package log_rotate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogRotateRunsWithoutRequests(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"access.log", "error.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("idle entry"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	newTestPlugin(t, Config{LogDir: dir, Interval: 1, MaxSize: -1, EnableAccessLog: new(true)})
	deadline := time.Now().Add(3500 * time.Millisecond)
	for time.Now().Before(deadline) {
		files, err := filepath.Glob(filepath.Join(dir, "*__access.log"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no interval rotation occurred without an HTTP request")
}
