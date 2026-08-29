package compiler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	api_breaker "github.com/wklken/apisix-go/pkg/plugin/api_breaker"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var httpAPIBreakerStateDigest = sha256.Sum256([]byte("apisix/http/api-breaker-state/v1"))

type httpAPIBreakerStateLeaseSlot struct {
	mu    sync.Mutex
	lease *runtime.ResourceLease[*api_breaker.State]
}

func (slot *httpAPIBreakerStateLeaseSlot) adopt(lease *runtime.ResourceLease[*api_breaker.State]) {
	slot.mu.Lock()
	slot.lease = lease
	slot.mu.Unlock()
}

func (slot *httpAPIBreakerStateLeaseSlot) release(ctx context.Context) error {
	slot.mu.Lock()
	lease := slot.lease
	slot.mu.Unlock()
	if lease == nil {
		return nil
	}
	return lease.Release(ctx)
}

func (prepared *PreparedGeneration) acquireHTTPAPIBreakerState(
	ctx context.Context,
) (*api_breaker.State, error) {
	if prepared == nil || ctx == nil || prepared.registry == nil || prepared.cleanup == nil {
		return nil, fmt.Errorf("%w: HTTP api-breaker state owner is incomplete", ErrInvalidInput)
	}
	slot := &httpAPIBreakerStateLeaseSlot{}
	if err := prepared.cleanup.Own(
		cleanupResourceFinalize,
		"http-api-breaker-state",
		slot.release,
	); err != nil {
		return nil, err
	}
	lease, err := runtime.Acquire(
		context.WithoutCancel(ctx),
		prepared.registry,
		runtime.ResourceKey{
			Kind: "plugin-api-breaker-state", Scope: "http-process/v1", Digest: httpAPIBreakerStateDigest,
		},
		func(context.Context) (*api_breaker.State, func(context.Context) error, error) {
			state := api_breaker.NewState()
			return state, func(context.Context) error {
				state.Close()
				return nil
			}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	slot.adopt(lease)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lease.Value(), nil
}
