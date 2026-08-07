package memory

import (
	"testing"
	"time"
)

func TestNewLRURejectsNegativeSize(t *testing.T) {
	if _, err := NewLRU[string, string](-1, 0); err == nil {
		t.Fatal("NewLRU() error = nil, want invalid size rejection")
	}
}

func TestNewLRUWithEvictRejectsNegativeSize(t *testing.T) {
	if _, err := NewLRUWithEvict[string, string](-1, 0, nil); err == nil {
		t.Fatal("NewLRUWithEvict() error = nil, want invalid size rejection")
	}
}

func TestNewLRUWithEvictInvokesCallback(t *testing.T) {
	evicted := make(chan string, 1)
	cache, err := NewLRUWithEvict[string, string](1, 0, func(key, value string) {
		evicted <- key + "=" + value
	})
	if err != nil {
		t.Fatal(err)
	}
	cache.Add("first", "one")
	cache.Add("second", "two")
	if got := <-evicted; got != "first=one" {
		t.Fatalf("evicted = %q, want first=one", got)
	}
}

func TestNewLRUExpiresEntriesAfterTTL(t *testing.T) {
	cache, err := NewLRU[string, string](16, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cache.Add("key", "value")
	if got, ok := cache.Get("key"); !ok || got != "value" {
		t.Fatalf("Get() before TTL = %q/%t, want value/true", got, ok)
	}

	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		if _, ok := cache.Get("key"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("entry still present after TTL window")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
