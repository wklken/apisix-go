package shared

import (
	"fmt"
	"sync"

	"github.com/go-resty/resty/v2"
	"github.com/redis/go-redis/v9"
)

// clientEntry tracks the reference count and lifecycle of one shared client.
// The registry mutex is only used to look up and insert entries; creation and
// closing run under the entry mutex so a slow dial never blocks the registry
// and unrelated keys never wait on each other.
type clientEntry struct {
	mu     sync.Mutex
	value  any
	refs   int
	closed bool
}

var clientRegistry = struct {
	mu      sync.Mutex
	entries map[string]*clientEntry
}{entries: map[string]*clientEntry{}}

// ClientKey builds the plugin-qualified registry key for a configuration UID.
func ClientKey(pluginName string, uid *ConfigUID) string {
	return fmt.Sprintf("%s:%s", pluginName, uid.String())
}

// AcquireClient returns the shared client registered under key, creating it
// through create on first use. The returned release function drops one
// reference; the client is closed through closeFn and removed from the
// registry only after the final release. The registry mutex is never held
// while create or closeFn run.
func AcquireClient(key string, create func() (any, error), closeFn func(any)) (any, func(), error) {
	for {
		clientRegistry.mu.Lock()
		entry := clientRegistry.entries[key]
		if entry == nil {
			entry = &clientEntry{}
			clientRegistry.entries[key] = entry
		}
		clientRegistry.mu.Unlock()

		entry.mu.Lock()
		if entry.closed {
			// The previous owner is tearing this entry down; drop the stale
			// entry and re-acquire a fresh one.
			entry.mu.Unlock()
			clientRegistry.mu.Lock()
			if clientRegistry.entries[key] == entry {
				delete(clientRegistry.entries, key)
			}
			clientRegistry.mu.Unlock()
			continue
		}
		if entry.value == nil {
			value, createErr := create()
			if createErr != nil {
				clientRegistry.mu.Lock()
				if clientRegistry.entries[key] == entry {
					delete(clientRegistry.entries, key)
				}
				clientRegistry.mu.Unlock()
				entry.mu.Unlock()
				return nil, nil, createErr
			}
			entry.value = value
		}
		entry.refs++
		value := entry.value
		entry.mu.Unlock()

		var once sync.Once
		release := func() {
			once.Do(func() {
				entry.mu.Lock()
				entry.refs--
				final := entry.refs == 0
				if final {
					entry.closed = true
				}
				entry.mu.Unlock()
				if !final {
					return
				}
				if closeFn != nil {
					closeFn(value)
				}
				clientRegistry.mu.Lock()
				if clientRegistry.entries[key] == entry {
					delete(clientRegistry.entries, key)
				}
				clientRegistry.mu.Unlock()
			})
		}
		return value, release, nil
	}
}

// CloseRestyClient closes idle connections of a resty client.
func CloseRestyClient(value any) {
	value.(*resty.Client).GetClient().CloseIdleConnections()
}

// CloseRedisClient closes a redis universal client.
func CloseRedisClient(value any) {
	_ = value.(redis.UniversalClient).Close()
}
