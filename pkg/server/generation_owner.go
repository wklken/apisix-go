package server

import (
	"context"
	"sync"

	"github.com/wklken/apisix-go/pkg/compiler"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

type activeBundle struct {
	http   *generationOwner
	stream *generationOwner
}

func (bundle activeBundle) withDomains(owner *generationOwner, domains ownerDomain) activeBundle {
	requireOwnerDomains(domains)
	if owner == nil {
		panic("replace active bundle with nil generation owner")
	}
	next := bundle
	if domains&ownerDomainHTTP != 0 {
		next.http = owner
	}
	if domains&ownerDomainStream != 0 {
		next.stream = owner
	}
	return next
}

type ownerDomain uint8

const (
	ownerDomainHTTP ownerDomain = 1 << iota
	ownerDomainStream
	ownerDomainAll = ownerDomainHTTP | ownerDomainStream
)

type generationOwner struct {
	prepared *compiler.PreparedGeneration

	mu            sync.Mutex
	activeDomains ownerDomain
	leases        uint64
	retiring      bool
	drained       chan struct{}
	drainOnce     sync.Once

	closeOnce sync.Once
	closeErr  error
}

func newGenerationOwner(prepared *compiler.PreparedGeneration) *generationOwner {
	if prepared == nil {
		return nil
	}
	return &generationOwner{
		prepared: prepared,
		drained:  make(chan struct{}),
	}
}

func (owner *generationOwner) activateDomains(domains ownerDomain) {
	requireOwnerDomains(domains)
	if owner == nil || owner.prepared == nil {
		panic("activate nil generation owner")
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.retiring {
		panic("activate retired generation owner")
	}
	if owner.activeDomains&domains != 0 {
		panic("activate already-active generation owner domain")
	}
	if domains&ownerDomainHTTP != 0 && owner.prepared.HTTP() == nil {
		panic("activate generation owner without HTTP snapshot")
	}
	if domains&ownerDomainStream != 0 && owner.prepared.Stream() == nil {
		panic("activate generation owner without stream snapshot")
	}
	owner.activeDomains |= domains
}

// deactivateDomains returns true only when this transition removes the
// owner's final active domain. The caller uses that transition to enqueue
// retirement even while leases are still outstanding.
func (owner *generationOwner) deactivateDomains(domains ownerDomain) bool {
	requireOwnerDomains(domains)
	if owner == nil {
		panic("deactivate nil generation owner")
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.activeDomains&domains != domains {
		panic("deactivate inactive generation owner domain")
	}
	owner.activeDomains &^= domains
	if owner.activeDomains != 0 {
		return false
	}
	owner.retiring = true
	owner.signalDrainedLocked()
	return true
}

func (owner *generationOwner) acquireHTTP() (httpGenerationLease, bool) {
	if owner == nil {
		return httpGenerationLease{}, false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.activeDomains&ownerDomainHTTP == 0 || owner.prepared == nil {
		return httpGenerationLease{}, false
	}
	snapshot := owner.prepared.HTTP()
	if snapshot == nil {
		return httpGenerationLease{}, false
	}
	owner.leases++
	return owner.newHTTPLeaseLocked(snapshot), true
}

func (owner *generationOwner) acquireStream() (streamGenerationLease, bool) {
	if owner == nil {
		return streamGenerationLease{}, false
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.activeDomains&ownerDomainStream == 0 || owner.prepared == nil {
		return streamGenerationLease{}, false
	}
	snapshot := owner.prepared.Stream()
	if snapshot == nil || snapshot.Router() == nil {
		return streamGenerationLease{}, false
	}
	owner.leases++
	var releaseOnce sync.Once
	return streamGenerationLease{
		Router: snapshot.Router(),
		Release: func() {
			releaseOnce.Do(owner.release)
		},
	}, true
}

func (owner *generationOwner) newHTTPLeaseLocked(snapshot *compiler.HTTPSnapshot) httpGenerationLease {
	handle := &httpLeaseHandle{owner: owner}
	return httpGenerationLease{
		Snapshot: snapshot,
		Release:  handle.release,
		retain:   handle.retain,
	}
}

// retainHTTP is authorized only through a still-live HTTP lease handle. It
// deliberately does not require the HTTP slot to remain active: a request may
// retain a batch or hijack child after a newer bundle has replaced its slot.
func (owner *generationOwner) retainHTTP() (httpGenerationLease, bool) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.prepared == nil || owner.leases == 0 {
		return httpGenerationLease{}, false
	}
	snapshot := owner.prepared.HTTP()
	if snapshot == nil {
		return httpGenerationLease{}, false
	}
	owner.leases++
	return owner.newHTTPLeaseLocked(snapshot), true
}

func (owner *generationOwner) release() {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.leases == 0 {
		panic("release generation owner without lease")
	}
	owner.leases--
	owner.signalDrainedLocked()
}

func (owner *generationOwner) signalDrainedLocked() {
	if owner.retiring && owner.activeDomains == 0 && owner.leases == 0 {
		owner.drainOnce.Do(func() { close(owner.drained) })
	}
}

func (owner *generationOwner) closePrepared(ctx context.Context) error {
	if owner == nil || owner.prepared == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-owner.drained:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	owner.closeOnce.Do(func() {
		owner.closeErr = owner.prepared.Close(ctx)
	})
	return owner.closeErr
}

type httpGenerationLease struct {
	Snapshot *compiler.HTTPSnapshot
	Release  func()
	retain   func() (httpGenerationLease, bool)
}

type streamGenerationLease struct {
	Router  *streamruntime.Router
	Release func()
}

type (
	httpLeaseSource   func() (httpGenerationLease, bool)
	streamLeaseSource func() (streamGenerationLease, bool)
)

type httpLeaseHandle struct {
	mu       sync.Mutex
	owner    *generationOwner
	released bool
}

func (handle *httpLeaseHandle) release() {
	handle.mu.Lock()
	if handle.released {
		handle.mu.Unlock()
		return
	}
	handle.released = true
	owner := handle.owner
	handle.mu.Unlock()
	owner.release()
}

func (handle *httpLeaseHandle) retain() (httpGenerationLease, bool) {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.released {
		return httpGenerationLease{}, false
	}
	return handle.owner.retainHTTP()
}

func requireOwnerDomains(domains ownerDomain) {
	if domains == 0 || domains&^ownerDomainAll != 0 {
		panic("invalid generation owner domain mask")
	}
}
