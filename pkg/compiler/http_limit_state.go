package compiler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var httpRateLimitStateDigest = sha256.Sum256([]byte("apisix/http/rate-limit-state/v1"))

type httpRateLimitStateLeaseSlot struct {
	mu    sync.Mutex
	lease *runtime.ResourceLease[*limitbase.State]
}

func (slot *httpRateLimitStateLeaseSlot) adopt(lease *runtime.ResourceLease[*limitbase.State]) {
	slot.mu.Lock()
	slot.lease = lease
	slot.mu.Unlock()
}

func (slot *httpRateLimitStateLeaseSlot) release(ctx context.Context) error {
	slot.mu.Lock()
	lease := slot.lease
	slot.mu.Unlock()
	if lease == nil {
		return nil
	}
	return lease.Release(ctx)
}

func (prepared *PreparedGeneration) acquireHTTPRateLimitState(
	ctx context.Context,
) (*limitbase.State, error) {
	if prepared == nil || ctx == nil || prepared.registry == nil || prepared.cleanup == nil {
		return nil, fmt.Errorf("%w: HTTP rate-limit state owner is incomplete", ErrInvalidInput)
	}
	slot := &httpRateLimitStateLeaseSlot{}
	if err := prepared.cleanup.Own(
		cleanupResourceFinalize,
		"http-rate-limit-state",
		slot.release,
	); err != nil {
		return nil, err
	}
	lease, err := runtime.Acquire(
		context.WithoutCancel(ctx),
		prepared.registry,
		runtime.ResourceKey{
			Kind: "plugin-rate-limit-state", Scope: "http-process/v1", Digest: httpRateLimitStateDigest,
		},
		func(context.Context) (*limitbase.State, func(context.Context) error, error) {
			state := limitbase.NewState()
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
