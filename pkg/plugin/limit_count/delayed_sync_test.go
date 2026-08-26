package limit_count

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/runtime"
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

type cancellationIgnoringDelayedSyncBackend struct {
	mu          sync.Mutex
	limit       int64
	reset       time.Duration
	calls       []delayedSyncCall
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
}

func (b *cancellationIgnoringDelayedSyncBackend) sync(
	_ context.Context,
	key string,
	delta int64,
	_ time.Time,
) (int64, time.Duration, error) {
	b.mu.Lock()
	b.calls = append(b.calls, delayedSyncCall{key: key, delta: delta})
	release := b.release
	b.mu.Unlock()
	if delta != 0 && release != nil {
		b.enteredOnce.Do(func() { close(b.entered) })
		<-release
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limit -= delta
	return b.limit, b.reset, nil
}

func (b *cancellationIgnoringDelayedSyncBackend) keyDeltas() []delayedSyncCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]delayedSyncCall(nil), b.calls...)
}

func TestDelayedSyncerCancellationFlushesAllDirtyStatesUnderOwnedTask(t *testing.T) {
	registry, failures := testTaskRegistry(t)
	owner := newPluginTaskOwnerForTest(t, registry, "plugin/test/limit-count/attempt-1")
	backend := &recordingDelayedSyncBackend{limit: 30, reset: time.Minute}
	syncer, err := newDelayedSyncer(owner, backend, 10, time.Minute, time.Hour, 2)
	if err != nil {
		t.Fatalf("newDelayedSyncer() error = %v", err)
	}
	dirtyThreeDelayedSyncKeys(t, syncer)
	stopTaskRegistry(t, registry)
	assertAllDelayedSyncDeltasFlushedOnce(t, backend)
	assertNoTaskFailure(t, failures)
}

func TestDelayedSyncerBlockingFinalFlushIsVisibleResidual(t *testing.T) {
	registry, failures := testTaskRegistry(t)
	owner := newPluginTaskOwnerForTest(t, registry, "plugin/test/limit-count/attempt-1")
	backend := &cancellationIgnoringDelayedSyncBackend{
		limit:   30,
		reset:   time.Minute,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	syncer, err := newDelayedSyncer(owner, backend, 10, time.Minute, time.Hour, 3)
	if err != nil {
		t.Fatalf("newDelayedSyncer() error = %v", err)
	}
	secondSyncer, err := newDelayedSyncer(owner, backend, 10, time.Minute, time.Hour, 3)
	if err != nil {
		t.Fatalf("newDelayedSyncer(second) error = %v", err)
	}
	dirtyThreeDelayedSyncKeys(t, syncer)
	dirtyDelayedSyncKeys(t, secondSyncer, "other-a", "other-b", "other-c")

	stopContext, cancelStop := context.WithCancel(context.Background())
	defer cancelStop()
	stopResult := make(chan struct {
		residuals []runtime.TaskResidual
		err       error
	}, 1)
	go func() {
		residuals, stopErr := registry.Stop(stopContext)
		stopResult <- struct {
			residuals []runtime.TaskResidual
			err       error
		}{residuals: residuals, err: stopErr}
	}()

	select {
	case <-backend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation flush did not reach the blocking backend")
	}
	cancelStop()
	select {
	case result := <-stopResult:
		if len(result.residuals) != 1 || result.residuals[0].Owner != "plugin/test/limit-count/attempt-1/delayed-sync" {
			t.Fatalf("TaskRegistry.Stop() residuals = %v, want exact delayed-sync owner", result.residuals)
		}
		if result.err == nil {
			t.Fatal("TaskRegistry.Stop() error = nil, want residual error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TaskRegistry.Stop() did not return residual owner after cancellation")
	}

	close(backend.release)
	stopTaskRegistry(t, registry)
	assertAllDelayedSyncCallsFlushedOnce(
		t,
		backend.keyDeltas(),
		[]string{"dirty-a", "dirty-b", "dirty-c", "other-a", "other-b", "other-c"},
	)
	assertNoTaskFailure(t, failures)
}

func testTaskRegistry(t *testing.T) (*runtime.TaskRegistry, <-chan runtime.TaskFailure) {
	t.Helper()
	failures := make(chan runtime.TaskFailure, 8)
	return runtime.NewTaskRegistry(context.Background(), func(failure runtime.TaskFailure) {
		failures <- failure
	}), failures
}

func newOwnedDelayedSyncerForTest(
	t *testing.T,
	backend delayedSyncBackend,
	limit int64,
	window time.Duration,
	syncInterval time.Duration,
	queueSize int,
) *delayedSyncer {
	t.Helper()
	registry, _ := testTaskRegistry(t)
	owner := newPluginTaskOwnerForTest(t, registry, "plugin/test/limit-count/attempt-1")
	syncer, err := newDelayedSyncer(owner, backend, limit, window, syncInterval, queueSize)
	if err != nil {
		t.Fatalf("newDelayedSyncer() error = %v", err)
	}
	t.Cleanup(func() {
		residuals, stopErr := registry.Stop(context.Background())
		if stopErr != nil || len(residuals) != 0 {
			t.Errorf("TaskRegistry.Stop() = (%v, %v)", residuals, stopErr)
		}
	})
	return syncer
}

func newPluginTaskOwnerForTest(
	t *testing.T,
	registry *runtime.TaskRegistry,
	prefix string,
) *runtime.TaskOwner {
	t.Helper()
	owner, err := runtime.NewTaskOwner(registry, prefix, runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	return owner
}

func stopTaskRegistry(t *testing.T, registry *runtime.TaskRegistry) {
	t.Helper()
	residuals, err := registry.Stop(context.Background())
	if err != nil || len(residuals) != 0 {
		t.Fatalf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
	}
}

func assertNoTaskFailure(t *testing.T, failures <-chan runtime.TaskFailure) {
	t.Helper()
	select {
	case failure := <-failures:
		t.Fatalf("unexpected task failure: %#v", failure)
	default:
	}
}

func dirtyThreeDelayedSyncKeys(t *testing.T, syncer *delayedSyncer) {
	t.Helper()
	dirtyDelayedSyncKeys(t, syncer, "dirty-a", "dirty-b", "dirty-c")
}

func dirtyDelayedSyncKeys(t *testing.T, syncer *delayedSyncer, keys ...string) {
	t.Helper()
	now := time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC)
	for _, key := range keys {
		if _, _, err := syncer.incoming(context.Background(), key, 1, now); err != nil {
			t.Fatalf("incoming(%q) error = %v", key, err)
		}
	}
}

func assertAllDelayedSyncDeltasFlushedOnce(t *testing.T, backend *recordingDelayedSyncBackend) {
	t.Helper()
	assertAllDelayedSyncCallsFlushedOnce(t, backend.keyDeltas(), []string{"dirty-a", "dirty-b", "dirty-c"})
}

func assertAllDelayedSyncCallsFlushedOnce(t *testing.T, calls []delayedSyncCall, wantKeys []string) {
	t.Helper()
	counts := map[string]int{}
	deltas := map[string]int64{}
	for _, call := range calls {
		if call.delta == 0 {
			continue
		}
		counts[call.key]++
		deltas[call.key] += call.delta
	}
	wantCounts := make(map[string]int, len(wantKeys))
	for _, key := range wantKeys {
		wantCounts[key]++
	}
	if len(counts) != len(wantCounts) {
		t.Fatalf("non-zero backend keys = %v, want %v", counts, wantCounts)
	}
	for key, wantCount := range wantCounts {
		if counts[key] != wantCount || deltas[key] != int64(wantCount) {
			t.Fatalf(
				"backend calls for %q = count %d delta %d, want count %d delta %d",
				key, counts[key], deltas[key], wantCount, wantCount,
			)
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, strings.Compare)
	sortedWantKeys := make([]string, 0, len(wantCounts))
	for key := range wantCounts {
		sortedWantKeys = append(sortedWantKeys, key)
	}
	slices.SortFunc(sortedWantKeys, strings.Compare)
	if !slices.Equal(keys, sortedWantKeys) {
		t.Fatalf("non-zero backend keys = %v, want all dirty keys %v", keys, sortedWantKeys)
	}
}

func TestDelayedSyncDoesNotDoubleCommitExpiredDelta(t *testing.T) {
	backend := &blockingRecordingDelayedSyncBackend{limit: 10, reset: time.Minute}
	syncer := newOwnedDelayedSyncerForTest(t, backend, 10, time.Minute, time.Hour, 100)

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
