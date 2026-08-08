package base

import "github.com/wklken/apisix-go/pkg/store"

// LoadPluginMetadata returns the stored plugin metadata or a zero value. The
// store getters guard against a missing process-wide store, so no panic can
// be masked here; errors are returned as zero metadata.
func LoadPluginMetadata[T any](name string) (metadata T) {
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		var zero T
		return zero
	}
	return metadata
}
