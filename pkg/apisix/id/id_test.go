package id

import (
	"sync"
	"testing"

	"github.com/gofrs/uuid"
	"github.com/wklken/apisix-go/pkg/config"
)

func TestGetUsesConfiguredApisixID(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{ID: "node-a"}}

	if got := Get(); got != "node-a" {
		t.Fatalf("Get() = %q, want configured APISIX id", got)
	}
}

func TestGetGeneratesStableUUID(t *testing.T) {
	oldConfig := config.GlobalConfig
	oldOnce := generatedOnce
	oldID := generatedID
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
		generatedOnce = oldOnce
		generatedID = oldID
	})
	config.GlobalConfig = nil
	generatedOnce = sync.Once{}
	generatedID = ""

	first := Get()
	second := Get()

	if first == "" {
		t.Fatal("Get() returned empty generated id")
	}
	if first != second {
		t.Fatalf("Get() generated unstable ids: first=%q second=%q", first, second)
	}
	if _, err := uuid.FromString(first); err != nil {
		t.Fatalf("Get() generated id %q, want UUID: %v", first, err)
	}
}
