package limit_count

import (
	"slices"
	"testing"
)

func TestDrainQueueDeduplicatesStably(t *testing.T) {
	s := &delayedSyncer{queue: make(chan string, 8)}
	for _, key := range []string{"a", "b", "a", "c", "b", "a"} {
		s.queue <- key
	}
	got := s.drainQueue()
	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("drainQueue() = %v, want %v", got, want)
	}
}

func TestDrainQueueEmpty(t *testing.T) {
	s := &delayedSyncer{queue: make(chan string, 1)}
	if got := s.drainQueue(); len(got) != 0 {
		t.Fatalf("drainQueue() = %v, want an empty result", got)
	}
}
