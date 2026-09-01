package limit_count

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
)

func TestRedisAPISIX317WireContract(t *testing.T) {
	client := &recordingLimitCountRedisClient{
		evalShaResults: []redisResult{
			{err: errors.New("NOSCRIPT No matching script")},
			{value: []any{int64(-1), int64(60)}},
		},
	}
	store := newRedisLimitCountStore(client, func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})

	got, err := store.Increment(context.Background(), "route-key", 3, limiter.Rate{
		Limit:  2,
		Period: time.Minute,
	})
	if err != nil {
		t.Fatalf("Increment() error = %v", err)
	}
	if !got.Reached || got.Remaining != 0 || got.Reset != 1_700_000_060 {
		t.Fatalf("Increment() = %#v", got)
	}
	if !reflect.DeepEqual(client.keys, []string{"plugin-limit-countroute-key"}) {
		t.Fatalf("Redis keys = %#v", client.keys)
	}
	if !reflect.DeepEqual(client.args, []any{int64(2), int64(60), int64(3)}) {
		t.Fatalf("Redis args = %#v", client.args)
	}
	if client.evalShaCalls != 1 || client.evalCalls != 1 {
		t.Fatalf("script calls = EVALSHA %d, EVAL %d", client.evalShaCalls, client.evalCalls)
	}
}

func TestRedisAPISIX317DoesNotFallbackForOtherErrors(t *testing.T) {
	client := &recordingLimitCountRedisClient{
		evalShaResults: []redisResult{{err: errors.New("connection refused")}},
	}
	store := newRedisLimitCountStore(client, time.Now)

	if _, err := store.Get(context.Background(), "key", limiter.Rate{
		Limit: 1, Period: time.Second,
	}); err == nil {
		t.Fatal("Get() error = nil")
	}
	if client.evalCalls != 0 {
		t.Fatalf("EVAL calls = %d, want 0", client.evalCalls)
	}
}

type redisResult struct {
	value any
	err   error
}

type recordingLimitCountRedisClient struct {
	evalShaResults []redisResult
	keys           []string
	args           []any
	evalShaCalls   int
	evalCalls      int
}

func (client *recordingLimitCountRedisClient) EvalSha(
	_ context.Context, _ string, keys []string, args ...any,
) *redis.Cmd {
	client.evalShaCalls++
	client.keys = append([]string(nil), keys...)
	client.args = append([]any(nil), args...)
	result := client.evalShaResults[0]
	client.evalShaResults = client.evalShaResults[1:]
	command := redis.NewCmd(context.Background())
	if result.err != nil {
		command.SetErr(result.err)
	} else {
		command.SetVal(result.value)
	}
	return command
}

func (client *recordingLimitCountRedisClient) Eval(
	_ context.Context, _ string, keys []string, args ...any,
) *redis.Cmd {
	client.evalCalls++
	client.keys = append([]string(nil), keys...)
	client.args = append([]any(nil), args...)
	result := client.evalShaResults[0]
	client.evalShaResults = client.evalShaResults[1:]
	command := redis.NewCmd(context.Background())
	if result.err != nil {
		command.SetErr(result.err)
	} else {
		command.SetVal(result.value)
	}
	return command
}
