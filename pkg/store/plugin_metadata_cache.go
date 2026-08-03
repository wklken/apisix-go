package store

import (
	"errors"
	"fmt"
	"sync"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/util"
	bolt "go.etcd.io/bbolt"
)

type validatedPluginMetadataCache struct {
	states sync.Map
}

type validatedPluginMetadataState struct {
	mu    sync.RWMutex
	value []byte
	// version is the bbolt transaction ID of the newest desired metadata
	// observed for this plugin, including invalid metadata and deletion.
	version int
	valid   bool
}

func newValidatedPluginMetadataCache() *validatedPluginMetadataCache {
	return &validatedPluginMetadataCache{}
}

func (c *validatedPluginMetadataCache) state(id string) *validatedPluginMetadataState {
	state, _ := c.states.LoadOrStore(id, &validatedPluginMetadataState{})
	return state.(*validatedPluginMetadataState)
}

func (c *validatedPluginMetadataCache) get(id string) ([]byte, bool) {
	state := c.state(id)
	state.mu.RLock()
	value, valid := append([]byte(nil), state.value...), state.valid
	state.mu.RUnlock()
	return value, valid
}

func (c *validatedPluginMetadataCache) publish(id string, value []byte, version int) ([]byte, bool) {
	state := c.state(id)
	state.mu.Lock()
	if version >= state.version {
		state.value = append([]byte(nil), value...)
		state.version = version
		state.valid = true
	}
	current, valid := append([]byte(nil), state.value...), state.valid
	state.mu.Unlock()
	return current, valid
}

func (c *validatedPluginMetadataCache) fallback(id string, version int) ([]byte, bool) {
	state := c.state(id)
	state.mu.Lock()
	if version > state.version {
		state.version = version
	}
	value, valid := append([]byte(nil), state.value...), state.valid
	state.mu.Unlock()
	return value, valid
}

func (c *validatedPluginMetadataCache) delete(id string, version int) {
	state := c.state(id)
	state.mu.Lock()
	if version >= state.version {
		state.value = nil
		state.version = version
		state.valid = false
	}
	state.mu.Unlock()
}

// GetValidatedPluginMetadata returns the current metadata when it validates.
// Invalid desired metadata leaves the Store-owned last-good snapshot unchanged
// and decodes that snapshot into target instead. Transaction-versioned
// publication prevents a slow older validation from replacing a newer result.
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
	raw, version := s.getPluginMetadataWithVersion(id)
	if raw == nil {
		s.validatedPluginMetadata.delete(id, version)
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
		current, valid := s.validatedPluginMetadata.publish(id, normalized, version)
		if !valid {
			return false, ErrNotFound
		}
		if decodeErr := json.Unmarshal(current, target); decodeErr != nil {
			return false, fmt.Errorf("decode current valid plugin metadata %q: %w", id, decodeErr)
		}
		return false, nil
	}

	lastGood, ok := s.validatedPluginMetadata.fallback(id, version)
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

func (s *Store) getPluginMetadataWithVersion(id string) ([]byte, int) {
	var raw []byte
	var version int
	_ = s.db.View(func(tx *bolt.Tx) error {
		version = tx.ID()
		bucket := tx.Bucket([]byte("plugin_metadata"))
		if bucket == nil {
			return errBucketNotFound
		}
		raw = append([]byte(nil), bucket.Get([]byte(id))...)
		return nil
	})
	return raw, version
}
