package memory

import (
	"fmt"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// the lru cache should be with limited size, and the ttl can be set to 0 to turn off expiring
// var a = lru.New[int, any](128)

func NewLRU[K comparable, V any](size int, defaultTTL time.Duration) (*expirable.LRU[K, V], error) {
	if size < 0 {
		return nil, fmt.Errorf("size must be greater than 0")
	}

	return expirable.NewLRU[K, V](size, nil, defaultTTL), nil
}

func NewLRUWithEvict[K comparable, V any](size int, defaultTTL time.Duration, onEvict expirable.EvictCallback[K, V]) (*expirable.LRU[K, V], error) {
	if size < 0 {
		return nil, fmt.Errorf("size must be greater than 0")
	}
	return expirable.NewLRU[K, V](size, onEvict, defaultTTL), nil
}
