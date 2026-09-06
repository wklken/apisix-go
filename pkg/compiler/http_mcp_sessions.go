package compiler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/wklken/apisix-go/pkg/plugin/mcp_bridge"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var httpMCPSessionsDigest = sha256.Sum256([]byte("apisix/http/mcp-sessions/v1"))

type httpMCPSessionsLeaseSlot struct {
	mu    sync.Mutex
	lease *runtime.ResourceLease[*mcp_bridge.SessionRegistry]
}

func (slot *httpMCPSessionsLeaseSlot) adopt(lease *runtime.ResourceLease[*mcp_bridge.SessionRegistry]) {
	slot.mu.Lock()
	slot.lease = lease
	slot.mu.Unlock()
}

func (slot *httpMCPSessionsLeaseSlot) release(ctx context.Context) error {
	slot.mu.Lock()
	lease := slot.lease
	slot.mu.Unlock()
	if lease == nil {
		return nil
	}
	return lease.Release(ctx)
}

func (prepared *PreparedGeneration) acquireHTTPMCPSessions(
	ctx context.Context,
) (*mcp_bridge.SessionRegistry, error) {
	if prepared == nil || ctx == nil || prepared.registry == nil || prepared.cleanup == nil {
		return nil, fmt.Errorf("%w: HTTP MCP session registry owner is incomplete", ErrInvalidInput)
	}
	slot := &httpMCPSessionsLeaseSlot{}
	if err := prepared.cleanup.Own(
		cleanupResourceFinalize,
		"http-mcp-sessions",
		slot.release,
	); err != nil {
		return nil, err
	}
	lease, err := runtime.Acquire(
		context.WithoutCancel(ctx),
		prepared.registry,
		runtime.ResourceKey{
			Kind: "plugin-mcp-sessions", Scope: "http-process/v1", Digest: httpMCPSessionsDigest,
		},
		func(context.Context) (*mcp_bridge.SessionRegistry, func(context.Context) error, error) {
			state := mcp_bridge.NewSessionRegistry()
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
