package limit_count

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// blockingRecordingDelayedSyncBackend records every sync delta and blocks a
// mutation once armed, signaling when the blocked call has been reached.
type blockingRecordingDelayedSyncBackend struct {
	mu      sync.Mutex
	limit   int64
	reset   time.Duration
	calls   []int64
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingRecordingDelayedSyncBackend) sync(
	_ context.Context,
	_ string,
	delta int64,
	_ time.Time,
) (int64, time.Duration, error) {
	b.mu.Lock()
	b.calls = append(b.calls, delta)
	block := b.block
	b.mu.Unlock()
	if block != nil {
		b.once.Do(func() { close(b.entered) })
		<-block
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limit -= delta
	return b.limit, b.reset, nil
}

func (b *blockingRecordingDelayedSyncBackend) arm() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.block = make(chan struct{})
	b.entered = make(chan struct{})
	b.once = sync.Once{}
	return b.block
}

func (b *blockingRecordingDelayedSyncBackend) deltas() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.calls...)
}

func TestDelayedSyncDoesNotDoubleCommitExpiredDelta(t *testing.T) {
	backend := &blockingRecordingDelayedSyncBackend{limit: 10, reset: time.Minute}
	syncer := newDelayedSyncer(backend, 10, time.Minute, time.Hour, 100)
	t.Cleanup(syncer.Stop)

	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	if _, _, err := syncer.incoming(context.Background(), "contended", 1, base); err != nil {
		t.Fatalf("seed incoming() error = %v", err)
	}

	release := backend.arm()
	flushDone := make(chan struct{})
	go func() {
		_ = syncer.flushNow(context.Background(), base.Add(time.Second))
		close(flushDone)
	}()

	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not reach the blocked backend mutation")
	}

	// The window expired while the flush is stuck; the pending delta must not
	// be committed again by an inline expiry flush.
	afterReset := base.Add(61 * time.Second)
	incomingDone := make(chan error, 1)
	go func() {
		_, _, err := syncer.incoming(context.Background(), "contended", 1, afterReset)
		incomingDone <- err
	}()

	close(release)
	select {
	case err := <-incomingDone:
		if err != nil {
			t.Fatalf("incoming() after expired window error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("incoming() after expired window did not complete")
	}
	select {
	case <-flushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not complete after the blocked backend call returned")
	}

	sum := int64(0)
	for _, delta := range backend.deltas() {
		sum += delta
	}
	if sum != 1 {
		t.Fatalf("sum of non-zero backend deltas = %d, want exactly the one accepted request synced once", sum)
	}
}

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

func TestDelayedSyncReclaimsExpiredIdleStateBeforeAllocatingNewKey(t *testing.T) {
	backend := &blockingRecordingDelayedSyncBackend{limit: 10, reset: time.Minute}
	syncer := newTestDelayedSyncer(backend, 2)
	base := time.Date(2026, 8, 18, 1, 2, 3, 0, time.UTC)
	if _, _, err := syncer.incoming(context.Background(), "expired", 1, base); err != nil {
		t.Fatalf("seed incoming() error = %v", err)
	}
	if err := syncer.flushNow(context.Background(), base.Add(time.Second)); err != nil {
		t.Fatalf("flushNow() error = %v", err)
	}
	if err := syncer.flushNow(context.Background(), base.Add(2*time.Minute)); err != nil {
		t.Fatalf("expiry cleanup flushNow() error = %v", err)
	}
	if _, ok := syncer.states["expired"]; ok {
		t.Fatal("expired idle state remained after periodic flush cleanup")
	}
	if _, _, err := syncer.incoming(context.Background(), "replacement", 1, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("replacement incoming() error = %v", err)
	}
}

func TestDelayedSyncCleanupPreservesOwnedOrLiveState(t *testing.T) {
	now := time.Date(2026, 8, 18, 1, 2, 3, 0, time.UTC)
	expired := now.Add(-time.Second)
	future := now.Add(time.Minute)
	syncer := newTestDelayedSyncer(&blockingRecordingDelayedSyncBackend{}, 10)
	syncer.states = map[string]*delayedSyncState{
		"idle":       {initialized: true, resetAt: expired},
		"dirty":      {initialized: true, resetAt: expired, localDelta: 1},
		"in-flight":  {initialized: true, resetAt: expired, inFlight: true},
		"retry":      {initialized: true, resetAt: expired},
		"retry-next": {initialized: true, resetAt: expired},
		"live":       {initialized: true, resetAt: future},
	}
	syncer.retry["retry"] = struct{}{}
	syncer.retryNext["retry-next"] = struct{}{}

	syncer.cleanupExpiredLocked(now)
	if _, ok := syncer.states["idle"]; ok {
		t.Fatal("expired idle state was not reclaimed")
	}
	for _, key := range []string{"dirty", "in-flight", "retry", "retry-next", "live"} {
		if _, ok := syncer.states[key]; !ok {
			t.Fatalf("state %q was reclaimed while still live or owned", key)
		}
	}
}

func TestDelayedSyncRejectsNewKeyAtStateCapacity(t *testing.T) {
	backend := &blockingRecordingDelayedSyncBackend{limit: 10, reset: time.Hour}
	syncer := newTestDelayedSyncer(backend, 2)
	now := time.Date(2026, 8, 18, 1, 2, 3, 0, time.UTC)
	for _, key := range []string{"first", "second"} {
		if _, _, err := syncer.incoming(context.Background(), key, 1, now); err != nil {
			t.Fatalf("incoming(%q) error = %v", key, err)
		}
	}
	if _, _, err := syncer.incoming(context.Background(), "overflow", 1, now); !errors.Is(err, errDelayedSyncRejected) {
		t.Fatalf("overflow incoming() error = %v, want errDelayedSyncRejected", err)
	}
	if len(syncer.states) != 2 {
		t.Fatalf("state count = %d, want capacity 2", len(syncer.states))
	}
}

func TestDelayedSyncRemovesNewStateAfterInitialBackendFailure(t *testing.T) {
	backend := &recordingDelayedSyncBackend{
		limit:    10,
		reset:    time.Minute,
		failures: 1,
	}
	syncer := newTestDelayedSyncer(backend, 1)
	now := time.Date(2026, 8, 18, 1, 2, 3, 0, time.UTC)

	if _, _, err := syncer.incoming(context.Background(), "failed", 1, now); err == nil {
		t.Fatal("failed initial incoming() error = nil, want backend failure")
	}
	syncer.mu.Lock()
	stateCount := len(syncer.states)
	_, failedStatePresent := syncer.states["failed"]
	syncer.mu.Unlock()
	if stateCount != 0 || failedStatePresent {
		t.Fatalf("states after failed initial sync = %d/%t, want no placeholder state", stateCount, failedStatePresent)
	}

	if _, _, err := syncer.incoming(context.Background(), "unrelated", 1, now); err != nil {
		t.Fatalf("unrelated incoming() after backend recovery error = %v", err)
	}
}

func newTestDelayedSyncer(backend delayedSyncBackend, maxStates int) *delayedSyncer {
	return &delayedSyncer{
		backend:   backend,
		limit:     10,
		window:    time.Minute,
		queue:     make(chan string, 10),
		states:    make(map[string]*delayedSyncState),
		retry:     make(map[string]struct{}),
		retryNext: make(map[string]struct{}),
		maxStates: maxStates,
	}
}
