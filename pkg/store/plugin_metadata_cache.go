package store

import (
	"errors"
	"fmt"
	"sync"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/util"
)

type validatedPluginMetadataCache struct {
	mu     sync.RWMutex
	values map[string][]byte
}

func newValidatedPluginMetadataCache() *validatedPluginMetadataCache {
	return &validatedPluginMetadataCache{values: make(map[string][]byte)}
}

func (c *validatedPluginMetadataCache) get(id string) ([]byte, bool) {
	c.mu.RLock()
	value, ok := c.values[id]
	c.mu.RUnlock()
	return append([]byte(nil), value...), ok
}

func (c *validatedPluginMetadataCache) put(id string, value []byte) {
	c.mu.Lock()
	c.values[id] = append([]byte(nil), value...)
	c.mu.Unlock()
}

func (c *validatedPluginMetadataCache) delete(id string) {
	c.mu.Lock()
	delete(c.values, id)
	c.mu.Unlock()
}

// GetValidatedPluginMetadata returns the current metadata when it validates.
// Invalid desired metadata leaves the Store-owned last-good snapshot unchanged
// and decodes that snapshot into target instead.
func GetValidatedPluginMetadata(
	id string,
	validate func(map[string]any) error,
	target any,
) (usedLastGood bool, err error) {
	if s == nil {
		return false, ErrNotFound
	}
	return s.getValidatedPluginMetadata(id, validate, target)
}

func (s *Store) getValidatedPluginMetadata(
	id string,
	validate func(map[string]any) error,
	target any,
) (bool, error) {
	raw := s.GetFromBucket("plugin_metadata", []byte(id))
	if raw == nil {
		s.validatedPluginMetadata.delete(id)
		return false, ErrNotFound
	}

	var metadata map[string]any
	desiredErr := decodePluginMetadata(raw, id, &metadata)
	if desiredErr == nil {
		desiredErr = validate(metadata)
	}
	if desiredErr == nil {
		desiredErr = util.Parse(metadata, target)
	}
	if desiredErr == nil {
		normalized, marshalErr := json.Marshal(metadata)
		if marshalErr != nil {
			return false, marshalErr
		}
		s.validatedPluginMetadata.put(id, normalized)
		return false, nil
	}

	lastGood, ok := s.validatedPluginMetadata.get(id)
	if !ok {
		return false, desiredErr
	}
	if decodeErr := json.Unmarshal(lastGood, target); decodeErr != nil {
		return false, errors.Join(
			desiredErr,
			fmt.Errorf("decode last valid plugin metadata %q: %w", id, decodeErr),
		)
	}
	return true, desiredErr
}
