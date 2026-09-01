package limitbase

import (
	"math"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
)

const defaultFixedWindowCapacity = 100000

type fixedWindow struct {
	remaining int64
	expiresAt time.Time
}

type leakyBucket struct {
	excess float64
	last   time.Time
}

type LeakyBucketResult struct {
	Allowed bool
	Delay   time.Duration
}

// FixedWindowResult is the APISIX limit-count local-store result.
type FixedWindowResult struct {
	Allowed   bool
	Remaining int64
	Reset     time.Duration
}

type FixedWindowState struct {
	Exists    bool
	Remaining int64
	Reset     time.Duration
}

// ConnectionResult is the APISIX limit-conn active reservation result.
type ConnectionResult struct {
	Allowed bool
	Current int64
}

// State owns the process-scoped local state shared by APISIX rate-limit
// plugin instances across immutable generations.
type State struct {
	mu          sync.Mutex
	now         func() time.Time
	windows     *cacheutil.BoundedTTLMap[fixedWindow]
	requests    *cacheutil.BoundedTTLMap[leakyBucket]
	windowCap   int
	connections map[string]int64
	closed      bool
}

func NewState() *State {
	return newStateWithCapacity(time.Now, defaultFixedWindowCapacity)
}

func NewStateWithClock(now func() time.Time) *State {
	return newStateWithCapacity(now, defaultFixedWindowCapacity)
}

func newState(now func() time.Time) *State {
	return newStateWithCapacity(now, defaultFixedWindowCapacity)
}

func newStateWithCapacity(now func() time.Time, capacity int) *State {
	if now == nil {
		now = time.Now
	}
	if capacity <= 0 {
		capacity = defaultFixedWindowCapacity
	}
	return &State{
		now:         now,
		windows:     cacheutil.NewBoundedTTLMap[fixedWindow](capacity, now),
		requests:    cacheutil.NewBoundedTTLMap[leakyBucket](capacity, now),
		windowCap:   capacity,
		connections: make(map[string]int64),
	}
}

// LeakyBucket applies the APISIX limit-req local-policy algorithm. State is
// process-owned so an unchanged effective configuration survives immutable
// generation replacement, like APISIX's plugin-limit-req shared dictionary.
func (state *State) LeakyBucket(key string, rate float64, burst float64) LeakyBucketResult {
	if state == nil || key == "" || rate <= 0 || burst < 0 {
		return LeakyBucketResult{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return LeakyBucketResult{}
	}

	result := LeakyBucketResult{}
	ttl := time.Duration(math.Ceil(burst/rate)+1) * time.Second
	state.requests.Mutate(key, func(entry leakyBucket, now time.Time) (leakyBucket, time.Duration, bool) {
		if entry.last.IsZero() {
			result.Allowed = true
			return leakyBucket{last: now}, ttl, true
		}
		excess := math.Max(entry.excess-rate*math.Abs(now.Sub(entry.last).Seconds())+1, 0)
		if excess > burst {
			return entry, 0, false
		}
		result.Allowed = true
		result.Delay = time.Duration(excess / rate * float64(time.Second))
		return leakyBucket{excess: excess, last: now}, ttl, true
	})
	return result
}

// FixedWindow checks or commits one cost against a fixed window. A rejected
// committed cost remains visible until the window expires, matching APISIX's
// count store and Redis script behavior.
func (state *State) FixedWindow(
	key string,
	limit int64,
	cost int64,
	window time.Duration,
	commit bool,
) FixedWindowResult {
	if state == nil || key == "" || limit <= 0 || cost <= 0 || window <= 0 {
		return FixedWindowResult{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return FixedWindowResult{}
	}
	var remaining int64
	var reset time.Duration
	state.windows.Mutate(key, func(entry fixedWindow, now time.Time) (fixedWindow, time.Duration, bool) {
		if entry.expiresAt.IsZero() {
			entry = fixedWindow{remaining: limit, expiresAt: now.Add(window)}
		}
		remaining = entry.remaining - cost
		reset = max(entry.expiresAt.Sub(now), 0)
		if commit {
			entry.remaining = remaining
		}
		return entry, reset, commit
	})
	return FixedWindowResult{Allowed: remaining >= 0, Remaining: remaining, Reset: reset}
}

func (state *State) FixedWindowSnapshot(
	key string,
	limit int64,
	window time.Duration,
) FixedWindowState {
	if state == nil || key == "" || limit <= 0 || window <= 0 {
		return FixedWindowState{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return FixedWindowState{}
	}
	entry, exists := state.windows.Get(key)
	if !exists {
		return FixedWindowState{Remaining: limit, Reset: window}
	}
	return FixedWindowState{
		Exists: true, Remaining: entry.remaining, Reset: max(entry.expiresAt.Sub(state.now()), 0),
	}
}

func (state *State) AdjustFixedWindow(
	key string,
	limit int64,
	delta int64,
	window time.Duration,
	create bool,
) FixedWindowState {
	if state == nil || key == "" || limit <= 0 || window <= 0 {
		return FixedWindowState{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return FixedWindowState{}
	}
	result := FixedWindowState{Remaining: limit, Reset: window}
	state.windows.Mutate(key, func(entry fixedWindow, now time.Time) (fixedWindow, time.Duration, bool) {
		if entry.expiresAt.IsZero() {
			if !create {
				return entry, 0, false
			}
			entry = fixedWindow{remaining: limit, expiresAt: now.Add(window)}
		}
		entry.remaining = min(entry.remaining-delta, limit)
		reset := max(entry.expiresAt.Sub(now), 0)
		result = FixedWindowState{Exists: true, Remaining: entry.remaining, Reset: reset}
		return entry, reset, true
	})
	return result
}

func (state *State) FixedWindowCount() int {
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.windows.PurgeExpired()
	return state.windows.Len()
}

func (state *State) AcquireConnection(key string, limit int64) ConnectionResult {
	if state == nil || key == "" || limit <= 0 {
		return ConnectionResult{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return ConnectionResult{}
	}
	current := state.connections[key]
	if current >= limit {
		return ConnectionResult{Current: current}
	}
	current++
	state.connections[key] = current
	return ConnectionResult{Allowed: true, Current: current}
}

func (state *State) ReleaseConnection(key string) {
	if state == nil || key == "" {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	current := state.connections[key]
	if current <= 1 {
		delete(state.connections, key)
		return
	}
	state.connections[key] = current - 1
}

func (state *State) ConnectionCount(key string) int64 {
	if state == nil || key == "" {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.connections[key]
}

func (state *State) Close() {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.closed = true
	state.windows = cacheutil.NewBoundedTTLMap[fixedWindow](state.windowCap, state.now)
	state.requests = cacheutil.NewBoundedTTLMap[leakyBucket](state.windowCap, state.now)
	clear(state.connections)
	state.mu.Unlock()
}
