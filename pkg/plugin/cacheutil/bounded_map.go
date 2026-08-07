package cacheutil

import (
	"container/heap"
	"sync"
	"time"
)

// ttlEntry is a value stored for a bounded TTL.
type ttlEntry[V any] struct {
	key       string
	value     V
	expiresAt time.Time
	seq       uint64
}

// ttlHeap orders entries by expiration, breaking ties by insertion order.
type ttlHeap[V any] []ttlEntry[V]

func (h ttlHeap[V]) Len() int { return len(h) }

func (h ttlHeap[V]) Less(i, j int) bool {
	if !h[i].expiresAt.Equal(h[j].expiresAt) {
		return h[i].expiresAt.Before(h[j].expiresAt)
	}
	return h[i].seq < h[j].seq
}

func (h ttlHeap[V]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *ttlHeap[V]) Push(value any) {
	*h = append(*h, value.(ttlEntry[V]))
}

func (h *ttlHeap[V]) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}

// BoundedTTLMap is a goroutine-safe key/value map that evicts entries once
// their TTL expires and never holds more than capacity live entries. At
// capacity, the entry with the earliest expiration is evicted first.
type BoundedTTLMap[V any] struct {
	mu      sync.Mutex
	now     func() time.Time
	cap     int
	seq     uint64
	entries map[string]ttlEntry[V]
	order   ttlHeap[V]
}

// NewBoundedTTLMap returns an empty map holding at most capacity entries.
// now supplies the clock used for expiry and capacity eviction.
func NewBoundedTTLMap[V any](capacity int, now func() time.Time) *BoundedTTLMap[V] {
	return &BoundedTTLMap[V]{
		now:     now,
		cap:     capacity,
		entries: make(map[string]ttlEntry[V]),
	}
}

// Get returns the live entry for key. Expired entries are evicted.
func (m *BoundedTTLMap[V]) Get(key string) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getLocked(key)
}

func (m *BoundedTTLMap[V]) getLocked(key string) (V, bool) {
	entry, ok := m.entries[key]
	if !ok || !m.now().Before(entry.expiresAt) {
		delete(m.entries, key)
		var zero V
		return zero, false
	}
	return entry.value, true
}

// Set stores value for key with the given TTL, replacing any previous value,
// evicting expired entries first and then the earliest expiring entry while
// the map is at capacity.
func (m *BoundedTTLMap[V]) Set(key string, value V, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	m.seq++
	entry := ttlEntry[V]{key: key, value: value, expiresAt: now.Add(ttl), seq: m.seq}
	m.entries[key] = entry
	heap.Push(&m.order, entry)

	m.evictExpiredLocked(now)
	for len(m.entries) > m.cap {
		top := heap.Pop(&m.order).(ttlEntry[V])
		live, ok := m.entries[top.key]
		if !ok || !live.expiresAt.Equal(top.expiresAt) {
			continue
		}
		delete(m.entries, top.key)
	}
}

// Mutate runs fn under the map lock with the live value for key (or the zero
// value when missing or expired). The entry is replaced by the value and TTL
// fn returns when store is true; otherwise the entry is left untouched.
func (m *BoundedTTLMap[V]) Mutate(key string, fn func(value V, now time.Time) (V, time.Duration, bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	value, ok := m.getLocked(key)
	if !ok {
		var zero V
		value = zero
	}
	next, ttl, store := fn(value, now)
	if !store {
		return
	}
	m.seq++
	entry := ttlEntry[V]{key: key, value: next, expiresAt: now.Add(ttl), seq: m.seq}
	m.entries[key] = entry
	heap.Push(&m.order, entry)

	m.evictExpiredLocked(now)
	for len(m.entries) > m.cap {
		top := heap.Pop(&m.order).(ttlEntry[V])
		live, ok := m.entries[top.key]
		if !ok || !live.expiresAt.Equal(top.expiresAt) {
			continue
		}
		delete(m.entries, top.key)
	}
}

// Delete removes key.
func (m *BoundedTTLMap[V]) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}

// Len returns the number of entries currently held.
func (m *BoundedTTLMap[V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// evictExpiredLocked removes heap roots that are stale or expired. When it
// returns, the heap root is a live, unexpired entry.
func (m *BoundedTTLMap[V]) evictExpiredLocked(now time.Time) {
	for m.order.Len() > 0 {
		top := m.order[0]
		live, ok := m.entries[top.key]
		if !ok || !live.expiresAt.Equal(top.expiresAt) {
			heap.Pop(&m.order)
			continue
		}
		if now.Before(top.expiresAt) {
			return
		}
		heap.Pop(&m.order)
		delete(m.entries, top.key)
	}
}
