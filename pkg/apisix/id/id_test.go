package id

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/wklken/apisix-go/pkg/config"
)

func TestGetUsesConfiguredApisixID(t *testing.T) {
	oldConfig := config.GlobalConfig
	oldPath := uidFilePath
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
		uidFilePath = oldPath
		generatedOnce = sync.Once{}
		generatedID = ""
	})
	uidFilePath = filepath.Join(t.TempDir(), "apisix.uid")
	generatedOnce = sync.Once{}
	generatedID = ""
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{ID: "node-a"}}

	if got := Get(); got != "node-a" {
		t.Fatalf("Get() = %q, want configured APISIX id", got)
	}

	config.GlobalConfig.Apisix.ID = "node-b"
	if got := Get(); got != "node-b" {
		t.Fatalf("Get() after config update = %q, want node-b", got)
	}
}

func TestGetGeneratesStableUUID(t *testing.T) {
	oldConfig := config.GlobalConfig
	oldPath := uidFilePath
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
		uidFilePath = oldPath
		generatedOnce = sync.Once{}
		generatedID = ""
	})
	config.GlobalConfig = nil
	uidFilePath = filepath.Join(t.TempDir(), "apisix.uid")
	generatedOnce = sync.Once{}
	generatedID = ""

	first := Get()
	second := Get()
	if first == "" || first != second {
		t.Fatalf("Get() generated unstable IDs: first=%q second=%q", first, second)
	}
	if _, err := uuid.FromString(first); err != nil {
		t.Fatalf("Get() generated ID %q, want UUID: %v", first, err)
	}
}

func TestGetPersistsGeneratedID(t *testing.T) {
	oldConfig := config.GlobalConfig
	oldPath := uidFilePath
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
		uidFilePath = oldPath
		generatedOnce = sync.Once{}
		generatedID = ""
	})

	config.GlobalConfig = nil
	uidFilePath = filepath.Join(t.TempDir(), "apisix.uid")
	generatedOnce = sync.Once{}
	generatedID = ""

	first := Get()
	content, err := os.ReadFile(uidFilePath)
	if err != nil {
		t.Fatalf("read persisted uid: %v", err)
	}
	if string(content) != first {
		t.Fatalf("persisted uid = %q, want %q", content, first)
	}

	generatedOnce = sync.Once{}
	generatedID = ""
	if second := Get(); second != first {
		t.Fatalf("reloaded uid = %q, want %q", second, first)
	}
}
