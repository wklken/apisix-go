package limit_count

import (
	"context"
	"time"

	"github.com/ulule/limiter/v3"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
)

// defaultLocalStoreCapacity bounds the number of in-memory local-policy
// counters; the earliest expiring counters are evicted once the bound is hit.
var defaultLocalStoreCapacity = 10000

type localFixedCounter struct {
	value   int64
	resetAt time.Time
}

// localFixedWindowStore is the local-policy limiter store. It behaves like
// the upstream in-memory store but bounds the number of live counters and
// evicts expired counters on access instead of with a background cleaner
// goroutine.
type localFixedWindowStore struct {
	cache *cacheutil.BoundedTTLMap[localFixedCounter]
}

func newLocalFixedWindowStore(now func() time.Time, capacity int) *localFixedWindowStore {
	return &localFixedWindowStore{
		cache: cacheutil.NewBoundedTTLMap[localFixedCounter](capacity, now),
	}
}

func (s *localFixedWindowStore) Get(ctx context.Context, key string, rate limiter.Rate) (limiter.Context, error) {
	return s.increment(key, 1, rate)
}

func (s *localFixedWindowStore) Increment(
	ctx context.Context,
	key string,
	count int64,
	rate limiter.Rate,
) (limiter.Context, error) {
	return s.increment(key, count, rate)
}

func (s *localFixedWindowStore) increment(key string, delta int64, rate limiter.Rate) (limiter.Context, error) {
	var value int64
	var expiration time.Time
	s.cache.Mutate(key, func(current localFixedCounter, now time.Time) (localFixedCounter, time.Duration, bool) {
		value = current.value + delta
		expiration = now.Add(rate.Period)
		return localFixedCounter{value: value, resetAt: expiration}, rate.Period, true
	})
	return contextFromState(rate, expiration, value), nil
}

func (s *localFixedWindowStore) Peek(ctx context.Context, key string, rate limiter.Rate) (limiter.Context, error) {
	var value int64
	var expiration time.Time
	s.cache.Mutate(key, func(current localFixedCounter, now time.Time) (localFixedCounter, time.Duration, bool) {
		value = current.value
		expiration = current.resetAt
		if current.resetAt.IsZero() {
			expiration = now.Add(rate.Period)
		}
		return current, 0, false
	})
	return contextFromState(rate, expiration, value), nil
}

func (s *localFixedWindowStore) Reset(ctx context.Context, key string, rate limiter.Rate) (limiter.Context, error) {
	var expiration time.Time
	s.cache.Mutate(key, func(current localFixedCounter, now time.Time) (localFixedCounter, time.Duration, bool) {
		expiration = now.Add(rate.Period)
		return current, 0, false
	})
	s.cache.Delete(key)
	return contextFromState(rate, expiration, 0), nil
}

// contextFromState builds the limiter context the same way the upstream
// in-memory store does.
func contextFromState(rate limiter.Rate, expiration time.Time, count int64) limiter.Context {
	limit := rate.Limit
	remaining := int64(0)
	reached := true
	if count <= limit {
		remaining = limit - count
		reached = false
	}
	return limiter.Context{
		Limit:     limit,
		Remaining: remaining,
		Reset:     expiration.Unix(),
		Reached:   reached,
	}
}
