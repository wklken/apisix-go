package limit_count

import (
	"context"
	"errors"
	"time"

	limiter "github.com/ulule/limiter/v3"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
)

var errLimitCountStateUnavailable = errors.New("limit-count local state is unavailable")

type limitCountStateStore struct {
	state *limitbase.State
}

func newLimitCountStateStore(state *limitbase.State) limiter.Store {
	return &limitCountStateStore{state: state}
}

func (store *limitCountStateStore) Get(
	ctx context.Context,
	key string,
	rate limiter.Rate,
) (limiter.Context, error) {
	return store.Increment(ctx, key, 1, rate)
}

func (store *limitCountStateStore) Increment(
	_ context.Context,
	key string,
	cost int64,
	rate limiter.Rate,
) (limiter.Context, error) {
	if store == nil || store.state == nil {
		return limiter.Context{}, errLimitCountStateUnavailable
	}
	result := store.state.FixedWindow(key, rate.Limit, cost, rate.Period, true)
	if result.Reset <= 0 {
		return limiter.Context{}, errLimitCountStateUnavailable
	}
	return stateLimiterContext(rate, result.Remaining, result.Reset), nil
}

func (store *limitCountStateStore) Peek(
	_ context.Context,
	key string,
	rate limiter.Rate,
) (limiter.Context, error) {
	if store == nil || store.state == nil {
		return limiter.Context{}, errLimitCountStateUnavailable
	}
	result := store.state.FixedWindowSnapshot(key, rate.Limit, rate.Period)
	if result.Reset <= 0 {
		return limiter.Context{}, errLimitCountStateUnavailable
	}
	return stateLimiterContext(rate, result.Remaining, result.Reset), nil
}

func (store *limitCountStateStore) Reset(
	_ context.Context,
	key string,
	rate limiter.Rate,
) (limiter.Context, error) {
	if store == nil || store.state == nil {
		return limiter.Context{}, errLimitCountStateUnavailable
	}
	result := store.state.FixedWindowSnapshot(key, rate.Limit, rate.Period)
	if result.Reset <= 0 {
		return limiter.Context{}, errLimitCountStateUnavailable
	}
	if result.Exists {
		store.state.AdjustFixedWindow(key, rate.Limit, result.Remaining-rate.Limit, rate.Period, true)
	}
	return stateLimiterContext(rate, rate.Limit, result.Reset), nil
}

func stateLimiterContext(rate limiter.Rate, remaining int64, reset time.Duration) limiter.Context {
	return limiter.Context{
		Limit:     rate.Limit,
		Remaining: max(remaining, 0),
		Reset:     time.Now().Add(reset).Unix(),
		Reached:   remaining < 0,
	}
}
