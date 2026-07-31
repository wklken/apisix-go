package limit_count

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var errSlidingWindowRejected = errors.New("sliding-window request rejected")

type slidingWindowStore interface {
	checkAndIncrement(
		ctx context.Context,
		currentKey string,
		lastKey string,
		cost int64,
		limit int64,
		window time.Duration,
		remaining time.Duration,
		expiry time.Duration,
		now time.Time,
	) (accepted bool, current int64, last int64, err error)
	increment(
		ctx context.Context,
		key string,
		delta int64,
		expiry time.Duration,
		now time.Time,
	) (int64, error)
}

type slidingWindowLimiter struct {
	store      slidingWindowStore
	pluginName string
	limit      int64
	window     time.Duration
}

func newSlidingWindowLimiter(
	store slidingWindowStore,
	pluginName string,
	limit int64,
	windowSeconds int64,
) *slidingWindowLimiter {
	return &slidingWindowLimiter{
		store:      store,
		pluginName: pluginName,
		limit:      limit,
		window:     time.Duration(windowSeconds) * time.Second,
	}
}

func (l *slidingWindowLimiter) incoming(
	ctx context.Context,
	key string,
	cost int64,
	now time.Time,
) (remaining int64, reset float64, err error) {
	currentKey, lastKey := l.counterKeys(key, now)
	remainingWindow := l.remainingWindow(now)
	accepted, current, last, err := l.store.checkAndIncrement(
		ctx,
		currentKey,
		lastKey,
		cost,
		l.limit,
		l.window,
		remainingWindow,
		2*l.window,
		now,
	)
	if err != nil {
		return 0, 0, err
	}

	lastRate := float64(last) / l.window.Seconds()
	estimatedLastWindowCount := lastRate * remainingWindow.Seconds()
	if !accepted {
		if current >= l.limit || lastRate == 0 {
			return 0, roundUpHundredths(remainingWindow.Seconds()), errSlidingWindowRejected
		}
		desiredDelay := remainingWindow.Seconds() -
			(float64(l.limit)-float64(current))/lastRate
		if desiredDelay < 0 {
			desiredDelay = 0
		}
		return 0, roundUpHundredths(desiredDelay), errSlidingWindowRejected
	}

	return int64(math.Floor(float64(l.limit) - float64(current) - estimatedLastWindowCount)),
		roundUpHundredths(remainingWindow.Seconds()),
		nil
}

func (l *slidingWindowLimiter) commit(
	ctx context.Context,
	key string,
	cost int64,
	now time.Time,
) (remaining int64, reset float64, err error) {
	currentKey, _ := l.counterKeys(key, now)
	current, err := l.store.increment(ctx, currentKey, cost, 2*l.window, now)
	if err != nil {
		return 0, 0, err
	}
	return l.limit - current, roundUpHundredths(l.remainingWindow(now).Seconds()), nil
}

func (l *slidingWindowLimiter) counterKeys(key string, now time.Time) (current string, last string) {
	windowID := now.UnixNano() / l.window.Nanoseconds()
	prefix := l.pluginName + ":" + key
	return fmt.Sprintf("%s.%d.counter", prefix, windowID),
		fmt.Sprintf("%s.%d.counter", prefix, windowID-1)
}

func (l *slidingWindowLimiter) remainingWindow(now time.Time) time.Duration {
	elapsed := time.Duration(now.UnixNano() % l.window.Nanoseconds())
	return l.window - elapsed
}

func roundUpHundredths(value float64) float64 {
	return math.Ceil(value*100) / 100
}

type memorySlidingWindowStore struct {
	mu          sync.Mutex
	counters    map[string]slidingWindowCounter
	expirations slidingWindowExpiryHeap
}

type slidingWindowCounter struct {
	value     int64
	expiresAt time.Time
}

type slidingWindowExpiry struct {
	key       string
	expiresAt time.Time
}

type slidingWindowExpiryHeap []slidingWindowExpiry

func (h slidingWindowExpiryHeap) Len() int {
	return len(h)
}

func (h slidingWindowExpiryHeap) Less(i int, j int) bool {
	return h[i].expiresAt.Before(h[j].expiresAt)
}

func (h slidingWindowExpiryHeap) Swap(i int, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *slidingWindowExpiryHeap) Push(value any) {
	*h = append(*h, value.(slidingWindowExpiry))
}

func (h *slidingWindowExpiryHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}

func newMemorySlidingWindowStore() *memorySlidingWindowStore {
	return &memorySlidingWindowStore{counters: make(map[string]slidingWindowCounter)}
}

func (s *memorySlidingWindowStore) checkAndIncrement(
	_ context.Context,
	currentKey string,
	lastKey string,
	cost int64,
	limit int64,
	window time.Duration,
	remaining time.Duration,
	expiry time.Duration,
	now time.Time,
) (bool, int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(now)

	current := s.countLocked(currentKey, now)
	last := min(s.countLocked(lastKey, now), limit)
	estimated := float64(last)/window.Seconds()*remaining.Seconds() + float64(current)
	if estimated+float64(cost) > float64(limit) {
		return false, current, last, nil
	}

	current += cost
	s.setLocked(currentKey, current, expiry, now)
	return true, current, last, nil
}

func (s *memorySlidingWindowStore) increment(
	_ context.Context,
	key string,
	delta int64,
	expiry time.Duration,
	now time.Time,
) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(now)

	current := s.countLocked(key, now) + delta
	s.setLocked(key, current, expiry, now)
	return current, nil
}

func (s *memorySlidingWindowStore) count(key string, now time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(now)
	return s.countLocked(key, now)
}

func (s *memorySlidingWindowStore) setLocked(
	key string,
	value int64,
	expiry time.Duration,
	now time.Time,
) {
	if counter, ok := s.counters[key]; ok {
		counter.value = value
		s.counters[key] = counter
		return
	}

	expiresAt := now.Add(expiry)
	s.counters[key] = slidingWindowCounter{value: value, expiresAt: expiresAt}
	heap.Push(&s.expirations, slidingWindowExpiry{key: key, expiresAt: expiresAt})
}

func (s *memorySlidingWindowStore) cleanupExpiredLocked(now time.Time) {
	for s.expirations.Len() > 0 && !now.Before(s.expirations[0].expiresAt) {
		expiry := heap.Pop(&s.expirations).(slidingWindowExpiry)
		counter, ok := s.counters[expiry.key]
		if ok && counter.expiresAt.Equal(expiry.expiresAt) && !now.Before(counter.expiresAt) {
			delete(s.counters, expiry.key)
		}
	}
}

func (s *memorySlidingWindowStore) countLocked(key string, now time.Time) int64 {
	counter, ok := s.counters[key]
	if !ok {
		return 0
	}
	if !counter.expiresAt.IsZero() && !now.Before(counter.expiresAt) {
		delete(s.counters, key)
		return 0
	}
	return counter.value
}

const redisSlidingCheckAndIncrementScript = `
-- apisix-go sliding-window check-and-increment
local cost = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window_size = tonumber(ARGV[3])
local remaining_time = tonumber(ARGV[4])
local expiry = ARGV[5]
local last = tonumber(ARGV[6])
if last > limit then
    last = limit
end

local cur_ttl = redis.call('pttl', KEYS[1])
local cur = 0
if cur_ttl >= 0 then
    cur = tonumber(redis.call('get', KEYS[1]) or 0)
end

local estimated = last / window_size * remaining_time + cur
if estimated + cost > limit then
    return {0, cur, last}
end

local new
if cur_ttl < 0 then
    redis.call('set', KEYS[1], cost, 'EX', expiry)
    new = cost
else
    new = redis.call('incrby', KEYS[1], cost)
end
return {1, new, last}
`

const redisSlidingIncrementScript = `
local ttl = redis.call('pttl', KEYS[1])
if ttl < 0 then
    redis.call('set', KEYS[1], ARGV[1], 'EX', ARGV[2])
    return tonumber(ARGV[1])
end
return redis.call('incrby', KEYS[1], ARGV[1])
`

type slidingRedisClient interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
}

type redisSlidingWindowStore struct {
	client slidingRedisClient
}

func newRedisSlidingWindowStore(client slidingRedisClient) *redisSlidingWindowStore {
	return &redisSlidingWindowStore{client: client}
}

func (s *redisSlidingWindowStore) checkAndIncrement(
	ctx context.Context,
	currentKey string,
	lastKey string,
	cost int64,
	limit int64,
	window time.Duration,
	remaining time.Duration,
	expiry time.Duration,
	_ time.Time,
) (bool, int64, int64, error) {
	last, err := s.client.Get(ctx, lastKey).Int64()
	if errors.Is(err, redis.Nil) {
		last = 0
	} else if err != nil {
		return false, 0, 0, err
	}

	result, err := s.client.Eval(
		ctx,
		redisSlidingCheckAndIncrementScript,
		[]string{currentKey},
		cost,
		limit,
		window.Seconds(),
		remaining.Seconds(),
		int64(expiry/time.Second),
		last,
	).Slice()
	if err != nil {
		return false, 0, 0, err
	}
	if len(result) != 3 {
		return false, 0, 0, fmt.Errorf("sliding-window Redis response has %d elements, want 3", len(result))
	}

	accepted, err := redisInteger(result[0])
	if err != nil {
		return false, 0, 0, fmt.Errorf("decode accepted flag: %w", err)
	}
	current, err := redisInteger(result[1])
	if err != nil {
		return false, 0, 0, fmt.Errorf("decode current count: %w", err)
	}
	last, err = redisInteger(result[2])
	if err != nil {
		return false, 0, 0, fmt.Errorf("decode previous count: %w", err)
	}
	return accepted == 1, current, last, nil
}

func (s *redisSlidingWindowStore) increment(
	ctx context.Context,
	key string,
	delta int64,
	expiry time.Duration,
	_ time.Time,
) (int64, error) {
	return s.client.Eval(
		ctx,
		redisSlidingIncrementScript,
		[]string{key},
		delta,
		int64(expiry/time.Second),
	).Int64()
}

func redisInteger(value any) (int64, error) {
	switch value := value.(type) {
	case int64:
		return value, nil
	case string:
		return strconv.ParseInt(value, 10, 64)
	case []byte:
		return strconv.ParseInt(string(value), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected integer type %T", value)
	}
}
