package base

import (
	"testing"
)

func TestLoadPluginMetadataWithNilStoreReturnsZero(t *testing.T) {
	metadata := LoadPluginMetadata[map[string]any]("example-plugin")
	if metadata != nil {
		t.Fatalf("LoadPluginMetadata() = %v, want zero value without a store", metadata)
	}
}
