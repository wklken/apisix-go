package base

import "github.com/wklken/apisix-go/pkg/store"

func LoadPluginMetadata[T any](name string) (metadata T) {
	defer func() {
		if recover() != nil {
			var zero T
			metadata = zero
		}
	}()

	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		var zero T
		return zero
	}
	return metadata
}
