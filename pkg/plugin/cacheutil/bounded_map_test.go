package cacheutil

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestBoundedTTLMapEvictsExpiredEntries(t *testing.T) {
	now := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	clock := func() time.Time { return now }

	m := NewBoundedTTLMap[int](100, clock)
	m.Set("a", 1, time.Minute)
	m.Set("b", 2, time.Minute)

	now = now.Add(2 * time.Minute)
	if _, ok := m.Get("a"); ok {
		t.Fatal("Get returned an expired entry")
	}
	if _, ok := m.Get("b"); ok {
		t.Fatal("Get returned an expired entry")
	}
	if got := m.Len(); got != 0 {
		t.Fatalf("Len() after expiry = %d, want 0", got)
	}
}

func TestBoundedTTLMapEvictsOldestAtCapacity(t *testing.T) {
	now := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	m := NewBoundedTTLMap[int](3, func() time.Time { return now })

	for i := range 5 {
		m.Set("k-"+strconv.Itoa(i), i, time.Minute)
	}
	if got := m.Len(); got != 3 {
		t.Fatalf("Len() after exceeding capacity = %d, want 3", got)
	}
	if _, ok := m.Get("k-0"); ok {
		t.Fatal("oldest inserted entry was not evicted")
	}
	if _, ok := m.Get("k-1"); ok {
		t.Fatal("second oldest inserted entry was not evicted")
	}
	for _, key := range []string{"k-2", "k-3", "k-4"} {
		got, ok := m.Get(key)
		if !ok {
			t.Fatalf("live entry %s was evicted", key)
		}
		if got != 2 && got != 3 && got != 4 {
			t.Fatalf("live entry %s value = %d, want its stored value", key, got)
		}
	}
	if got := m.Len(); got != 3 {
		t.Fatalf("Len() after Gets = %d, want 3", got)
	}
}

func TestBoundedTTLMapMutatePreservesConcurrentIncrements(t *testing.T) {
	m := NewBoundedTTLMap[int](100, time.Now)

	const workers = 32
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range 100 {
				m.Mutate("counter", func(value int, _ time.Time) (int, time.Duration, bool) {
					return value + 1, time.Minute, true
				})
			}
		})
	}
	wg.Wait()

	value, ok := m.Get("counter")
	if !ok {
		t.Fatal("counter entry missing")
	}
	if value != workers*100 {
		t.Fatalf("counter = %d, want %d", value, workers*100)
	}
}

func TestBoundedTTLMapDiscardsReplacedEntriesWithSameExpiry(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	m := NewBoundedTTLMap[int](2, func() time.Time { return now })
	m.Set("a", 1, time.Minute)
	m.Set("b", 1, time.Minute)

	for value := 2; value <= 100; value++ {
		m.Mutate("a", func(_ int, _ time.Time) (int, time.Duration, bool) {
			return value, time.Minute, true
		})
	}
	if got := m.order.Len(); got > 66 {
		t.Fatalf("expiry heap entries = %d, want a bounded multiple of 2 live entries", got)
	}

	m.Set("c", 1, time.Minute)
	if value, ok := m.Get("a"); !ok || value != 100 {
		t.Fatalf("refreshed entry a = %d, %t; want 100, true", value, ok)
	}
	if _, ok := m.Get("b"); ok {
		t.Fatal("oldest live entry b was not evicted")
	}
}
