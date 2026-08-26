package compiler

import (
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/generation"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

// StreamSnapshot is the authority-free stream observation surface of one
// prepared generation. Listener installation and cleanup remain generation-owned.
type StreamSnapshot struct {
	artifact generation.GenerationArtifact
	router   *streamruntime.Router
	closed   atomic.Bool
}

func (snapshot *StreamSnapshot) Revision() uint64 {
	if snapshot == nil || snapshot.closed.Load() {
		return 0
	}
	return snapshot.artifact.Revision
}

func (snapshot *StreamSnapshot) Router() *streamruntime.Router {
	if snapshot == nil || snapshot.closed.Load() {
		return nil
	}
	return snapshot.router
}

func (snapshot *StreamSnapshot) revoke() {
	if snapshot != nil {
		snapshot.closed.Store(true)
	}
}
