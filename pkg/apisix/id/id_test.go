package id

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"github.com/wklken/apisix-go/pkg/logger"
)

func TestGetUsesConfiguredApisixID(t *testing.T) {
	oldPath := uidFilePath
	t.Cleanup(func() {
		uidFilePath = oldPath
		generatedOnce = sync.Once{}
		generatedID = ""
	})
	uidFilePath = filepath.Join(t.TempDir(), "apisix.uid")
	generatedOnce = sync.Once{}
	generatedID = ""
	if got := Get("node-a"); got != "node-a" {
		t.Fatalf("Get() = %q, want configured APISIX id", got)
	}

	if got := Get("node-b"); got != "node-b" {
		t.Fatalf("Get() after config update = %q, want node-b", got)
	}
}

func TestGetGeneratesStableUUID(t *testing.T) {
	oldPath := uidFilePath
	t.Cleanup(func() {
		uidFilePath = oldPath
		generatedOnce = sync.Once{}
		generatedID = ""
	})
	uidFilePath = filepath.Join(t.TempDir(), "apisix.uid")
	generatedOnce = sync.Once{}
	generatedID = ""

	first := Get("")
	second := Get("")
	if first == "" || first != second {
		t.Fatalf("Get() generated unstable IDs: first=%q second=%q", first, second)
	}
	if _, err := uuid.FromString(first); err != nil {
		t.Fatalf("Get() generated ID %q, want UUID: %v", first, err)
	}
}

func TestGetPersistsGeneratedID(t *testing.T) {
	oldPath := uidFilePath
	t.Cleanup(func() {
		uidFilePath = oldPath
		generatedOnce = sync.Once{}
		generatedID = ""
	})

	uidFilePath = filepath.Join(t.TempDir(), "apisix.uid")
	generatedOnce = sync.Once{}
	generatedID = ""

	first := Get("")
	content, err := os.ReadFile(uidFilePath)
	if err != nil {
		t.Fatalf("read persisted uid: %v", err)
	}
	if string(content) != first {
		t.Fatalf("persisted uid = %q, want %q", content, first)
	}

	generatedOnce = sync.Once{}
	generatedID = ""
	if second := Get(""); second != first {
		t.Fatalf("reloaded uid = %q, want %q", second, first)
	}
}

func TestGetLogsPersistFailure(t *testing.T) {
	oldPath := uidFilePath
	t.Cleanup(func() {
		uidFilePath = oldPath
		generatedOnce = sync.Once{}
		generatedID = ""
	})

	uidFilePath = filepath.Join(t.TempDir(), "missing", "apisix.uid")
	generatedOnce = sync.Once{}
	generatedID = ""

	observed := make(chan logger.Entry, 4)
	stop := logger.ReplaceObserver("id-persist", func(entry logger.Entry) { observed <- entry })
	t.Cleanup(stop)

	if got := Get(""); got == "" {
		t.Fatal("Get() generated an empty id")
	}
	select {
	case entry := <-observed:
		if !strings.Contains(entry.Message, "persist generated apisix id") {
			t.Fatalf("observed log = %q, want persist failure context", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("uid persist failure was not logged")
	}
}
