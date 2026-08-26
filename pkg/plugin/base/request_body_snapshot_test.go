package base

import (
	"crypto/sha256"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRequestBodySnapshotStreamsHashesSpillsReplaysAndRemoves(t *testing.T) {
	payload := strings.Repeat("snapshot-body-", 256)
	request := httptest.NewRequest("POST", "http://example.com", strings.NewReader(payload))
	snapshot, err := EnsureRequestBodySnapshot(request, int64(len(payload)+1), 128, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Size() != int64(len(payload)) || snapshot.SHA256() != sha256.Sum256([]byte(payload)) {
		t.Fatalf("snapshot size/digest = %d/%x", snapshot.Size(), snapshot.SHA256())
	}
	if snapshot.path == "" {
		t.Fatal("snapshot did not spill above memory threshold")
	}
	info, err := os.Stat(snapshot.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %o, want 600", info.Mode().Perm())
	}
	for attempt := range 2 {
		reader, err := snapshot.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil || string(got) != payload {
			t.Fatalf("replay %d = %q/%v", attempt, got, err)
		}
	}
	path := snapshot.path
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("snapshot file remains after Close: %v", err)
	}
}

func TestRequestBodySnapshotRejectsLimitAndReusesOneCapture(t *testing.T) {
	request := httptest.NewRequest("POST", "http://example.com", strings.NewReader("payload"))
	if _, err := EnsureRequestBodySnapshot(request, 3, 2, t.TempDir()); err != ErrRequestBodyTooLarge {
		t.Fatalf("EnsureRequestBodySnapshot() error = %v, want body too large", err)
	}

	request = httptest.NewRequest("POST", "http://example.com", strings.NewReader("payload"))
	first, err := EnsureRequestBodySnapshot(request, 16, 16, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureRequestBodySnapshot(request, 16, 16, t.TempDir())
	if err != nil || first != second {
		t.Fatalf("second snapshot = %p/%v, want reused %p", second, err, first)
	}
}
