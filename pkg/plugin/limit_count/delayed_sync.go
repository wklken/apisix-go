package limit_count

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/samber/lo"
	limiter "github.com/ulule/limiter/v3"
	"github.com/wklken/apisix-go/pkg/logger"
)

const maxDelayedSyncStates = 10_000

var errDelayedSyncRejected = errors.New("delayed-sync request rejected")

type delayedSyncBackend interface {
	sync(
		ctx context.Context,
		key string,
		delta int64,
		now time.Time,
	) (remaining int64, reset time.Duration, err error)
}

type delayedSyncState struct {
	remoteRemaining int64
	localDelta      int64
	reservationAt   time.Time
	resetAt         time.Time
	initialized     bool
	inFlight        bool
	inFlightDone    chan struct{}
}

type delayedSyncer struct {
	backend       delayedSyncBackend
	limit         int64
	window        time.Duration
	syncInterval  time.Duration
	queue         chan string
	warnQueueFull func()

	mu        sync.Mutex
	states    map[string]*delayedSyncState
	retry     map[string]struct{}
	retryNext map[string]struct{}
	maxStates int
	stop      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once

	warnMu            sync.Mutex
	lastQueueFullWarn time.Time
}

func newDelayedSyncer(
	backend delayedSyncBackend,
	limit int64,
	window time.Duration,
	syncInterval time.Duration,
	queueSize int,
) *delayedSyncer {
	s := &delayedSyncer{
		backend:      backend,
		limit:        limit,
		window:       window,
		syncInterval: syncInterval,
		queue:        make(chan string, queueSize),
		states:       make(map[string]*delayedSyncState),
		retry:        make(map[string]struct{}),
		retryNext:    make(map[string]struct{}),
		maxStates:    maxDelayedSyncStates,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	s.warnQueueFull = func() {
		logger.Warn("delayed-sync queue saturated, skipping enqueue")
	}
	go s.run()
	return s
}

func (s *delayedSyncer) incoming(
	ctx context.Context,
	key string,
	cost int64,
	now time.Time,
) (remaining int64, reset time.Duration, err error) {
	for {
		s.mu.Lock()
		state := s.states[key]
		if state == nil {
			s.cleanupExpiredLocked(now)
			if len(s.states) >= s.stateCapacity() {
				s.mu.Unlock()
				return 0, s.window, errDelayedSyncRejected
			}
			state = &delayedSyncState{}
			s.states[key] = state
		}
		if state.initialized && !now.Before(state.resetAt) {
			if state.inFlight {
				done := state.inFlightDone
				s.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return 0, 0, ctx.Err()
				}
			}
			if state.localDelta > 0 {
				_, _, syncErr := s.backend.sync(ctx, key, state.localDelta, state.reservationAt)
				if syncErr != nil {
					s.mu.Unlock()
					return 0, 0, syncErr
				}
				state.localDelta = 0
				state.initialized = false
			}
		}
		if !state.initialized || !now.Before(state.resetAt) {
			remoteRemaining, remoteReset, syncErr := s.backend.sync(ctx, key, 0, now)
			if syncErr != nil {
				s.mu.Unlock()
				return 0, 0, syncErr
			}
			state.remoteRemaining = remoteRemaining
			state.localDelta = 0
			state.reservationAt = now
			state.resetAt = now.Add(remoteReset)
			state.initialized = true
		}

		remaining = state.remoteRemaining - state.localDelta - cost
		reset = max(state.resetAt.Sub(now), 0)
		if remaining < 0 {
			s.mu.Unlock()
			return remaining, reset, errDelayedSyncRejected
		}
		state.localDelta += cost
		s.mu.Unlock()

		s.enqueue(key)
		return remaining, reset, nil
	}
}

func (s *delayedSyncer) enqueue(key string) bool {
	select {
	case s.queue <- key:
		return true
	default:
		s.warnMu.Lock()
		now := time.Now()
		shouldWarn := s.lastQueueFullWarn.IsZero() ||
			now.Sub(s.lastQueueFullWarn) >= 10*time.Second
		if shouldWarn {
			s.lastQueueFullWarn = now
		}
		s.warnMu.Unlock()
		if shouldWarn && s.warnQueueFull != nil {
			s.warnQueueFull()
		}
		s.scheduleRetry(key)
		return false
	}
}

func (s *delayedSyncer) run() {
	ticker := time.NewTicker(s.syncInterval)
	defer func() {
		ticker.Stop()
		close(s.done)
	}()
	for {
		select {
		case now := <-ticker.C:
			if err := s.flushNow(context.Background(), now); err != nil {
				logger.Errorf("delayed-sync flush failed: %v", err)
			}
		case <-s.stop:
			// Shutdown is the only path that deliberately bypasses the bounded
			// queue so accepted local deltas are not lost when the process exits.
			if err := s.flushAllDirty(context.Background()); err != nil {
				logger.Errorf("delayed-sync shutdown flush failed: %v", err)
			}
			return
		}
	}
}

func (s *delayedSyncer) flushNow(ctx context.Context, now time.Time) error {
	keys := s.drainQueue()

	s.mu.Lock()
	for key := range s.retry {
		keys = append(keys, key)
	}
	clear(s.retry)
	s.mu.Unlock()

	err := s.flushKeys(ctx, lo.Uniq(keys), true)

	s.mu.Lock()
	for key := range s.retryNext {
		s.retry[key] = struct{}{}
	}
	clear(s.retryNext)
	s.cleanupExpiredLocked(now)
	s.mu.Unlock()

	return err
}

func (s *delayedSyncer) stateCapacity() int {
	if s.maxStates > 0 {
		return s.maxStates
	}
	return maxDelayedSyncStates
}

func (s *delayedSyncer) cleanupExpiredLocked(now time.Time) {
	for key, state := range s.states {
		if state == nil || !state.initialized || now.Before(state.resetAt) || state.localDelta != 0 || state.inFlight {
			continue
		}
		if _, owned := s.retry[key]; owned {
			continue
		}
		if _, owned := s.retryNext[key]; owned {
			continue
		}
		delete(s.states, key)
	}
}

func (s *delayedSyncer) flushAllDirty(ctx context.Context) error {
	s.mu.Lock()
	keys := make([]string, 0, len(s.states))
	for key, state := range s.states {
		if state.localDelta > 0 {
			keys = append(keys, key)
		}
	}
	s.mu.Unlock()
	return s.flushKeys(ctx, keys, false)
}

func (s *delayedSyncer) flushKeys(ctx context.Context, keys []string, retryFailures bool) error {
	type pendingMutation struct {
		delta         int64
		reservationAt time.Time
		done          chan struct{}
	}
	// Reserve each pending delta under the lock so a concurrent expiry flush
	// in incoming can never commit the same delta twice, then release the
	// lock for the Redis I/O so a slow backend never blocks unrelated key
	// mutations.
	s.mu.Lock()
	pending := make(map[string]pendingMutation, len(keys))
	for _, key := range keys {
		state := s.states[key]
		if state == nil || state.localDelta == 0 || state.inFlight {
			continue
		}
		delta := state.localDelta
		state.localDelta -= delta
		state.inFlight = true
		state.inFlightDone = make(chan struct{})
		pending[key] = pendingMutation{delta: delta, reservationAt: state.reservationAt, done: state.inFlightDone}
	}
	s.mu.Unlock()

	var firstErr error
	for _, key := range keys {
		mutation, ok := pending[key]
		if !ok {
			continue
		}
		remaining, _, err := s.backend.sync(ctx, key, mutation.delta, mutation.reservationAt)
		s.mu.Lock()
		state := s.states[key]
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			state.localDelta += mutation.delta
			if retryFailures {
				s.retryNext[key] = struct{}{}
			}
		} else {
			state.remoteRemaining = remaining
		}
		state.inFlight = false
		state.inFlightDone = nil
		close(mutation.done)
		s.mu.Unlock()
	}
	return firstErr
}

func (s *delayedSyncer) scheduleRetry(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retryNext == nil {
		s.retryNext = make(map[string]struct{})
	}
	s.retryNext[key] = struct{}{}
}

func (s *delayedSyncer) drainQueue() []string {
	keys := make([]string, 0, len(s.queue))
	for {
		select {
		case key := <-s.queue:
			keys = append(keys, key)
		default:
			return lo.Uniq(keys)
		}
	}
}

func (s *delayedSyncer) Stop() {
	if s == nil || s.stop == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
}

type fixedWindowDelayedBackend struct {
	limiter *limiter.Limiter
}

func (b fixedWindowDelayedBackend) sync(
	ctx context.Context,
	key string,
	delta int64,
	now time.Time,
) (int64, time.Duration, error) {
	var quota limiter.Context
	var err error
	if delta == 0 {
		quota, err = b.limiter.Peek(ctx, key)
	} else {
		quota, err = b.limiter.Increment(ctx, key, delta)
	}
	if err != nil {
		return 0, 0, err
	}
	reset := max(time.Unix(quota.Reset, 0).Sub(now), 0)
	return max(quota.Remaining, 0), reset, nil
}

type slidingWindowDelayedBackend struct {
	limiter *slidingWindowLimiter
}

func (b slidingWindowDelayedBackend) sync(
	ctx context.Context,
	key string,
	delta int64,
	now time.Time,
) (int64, time.Duration, error) {
	if delta == 0 {
		remaining, reset, err := b.limiter.incoming(ctx, key, 0, now)
		return remaining, time.Duration(reset * float64(time.Second)), err
	}
	remaining, reset, err := b.limiter.commit(ctx, key, delta, now)
	return remaining, time.Duration(reset * float64(time.Second)), err
}
