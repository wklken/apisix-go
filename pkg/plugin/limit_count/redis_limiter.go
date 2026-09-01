package limit_count

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
)

const redisLimitCountScript = `
assert(tonumber(ARGV[3]) >= 1, "cost must be at least 1")
local ttl = redis.call('ttl', KEYS[1])
if ttl < 0 then
    redis.call('set', KEYS[1], ARGV[1] - ARGV[3], 'EX', ARGV[2])
    return {ARGV[1] - ARGV[3], ARGV[2]}
end
return {redis.call('incrby', KEYS[1], 0 - ARGV[3]), ttl}
`

var redisLimitCountLua = redis.NewScript(redisLimitCountScript)

type redisLimitCountClient interface {
	EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *redis.Cmd
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
}

type redisLimitCountStore struct {
	client redisLimitCountClient
	now    func() time.Time
}

func newRedisLimitCountStore(client redisLimitCountClient, now func() time.Time) limiter.Store {
	return &redisLimitCountStore{client: client, now: now}
}

func (store *redisLimitCountStore) Get(
	ctx context.Context,
	key string,
	rate limiter.Rate,
) (limiter.Context, error) {
	return store.Increment(ctx, key, 1, rate)
}

func (store *redisLimitCountStore) Increment(
	ctx context.Context,
	key string,
	cost int64,
	rate limiter.Rate,
) (limiter.Context, error) {
	if cost < 1 {
		return limiter.Context{}, fmt.Errorf("limit-count Redis cost must be at least 1")
	}
	window := int64(rate.Period / time.Second)
	if window < 1 {
		return limiter.Context{}, fmt.Errorf("limit-count Redis time window must be at least one second")
	}
	result, err := store.run(ctx, []string{"plugin-limit-count" + key}, rate.Limit, window, cost)
	if err != nil {
		return limiter.Context{}, err
	}
	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return limiter.Context{}, fmt.Errorf("unexpected Redis limit-count result: %v", result)
	}
	remaining, ok := limitbase.RedisInt(values[0])
	if !ok {
		return limiter.Context{}, fmt.Errorf("unexpected Redis limit-count remaining value: %v", values[0])
	}
	ttl, ok := limitbase.RedisInt(values[1])
	if !ok {
		return limiter.Context{}, fmt.Errorf("unexpected Redis limit-count TTL value: %v", values[1])
	}
	now := store.now
	if now == nil {
		now = time.Now
	}
	return limiter.Context{
		Limit:     rate.Limit,
		Remaining: max(remaining, 0),
		Reset:     now().Add(time.Duration(ttl) * time.Second).Unix(),
		Reached:   remaining < 0,
	}, nil
}

func (store *redisLimitCountStore) Peek(
	context.Context,
	string,
	limiter.Rate,
) (limiter.Context, error) {
	return limiter.Context{}, errors.New("limit-count Redis does not support dry-run reads")
}

func (store *redisLimitCountStore) Reset(
	context.Context,
	string,
	limiter.Rate,
) (limiter.Context, error) {
	return limiter.Context{}, errors.New("limit-count Redis does not support counter reset")
}

func (store *redisLimitCountStore) run(
	ctx context.Context,
	keys []string,
	args ...any,
) (any, error) {
	result, err := store.client.EvalSha(ctx, redisLimitCountLua.Hash(), keys, args...).Result()
	if err == nil || !strings.HasPrefix(strings.ToUpper(err.Error()), "NOSCRIPT") {
		return result, err
	}
	return store.client.Eval(ctx, redisLimitCountScript, keys, args...).Result()
}
