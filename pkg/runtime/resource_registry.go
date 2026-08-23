// Package runtime owns generation runtime resources and their lifecycles.
package runtime

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrInvalidResourceIdentity is returned when a resource key cannot
	// unambiguously identify one reusable runtime resource.
	ErrInvalidResourceIdentity = errors.New("invalid resource identity")
	// ErrResourceRegistryClosed is returned when acquiring from a registry
	// after its terminal Close has started.
	ErrResourceRegistryClosed = errors.New("resource registry is closed")
	// ErrResourceTypeMismatch is returned when one key is acquired with a
	// different Go type than the resource already stored for that key.
	ErrResourceTypeMismatch = errors.New("resource type mismatch")
)

// ResourceKey is the complete identity of one reusable runtime resource.
// Scope prevents resources with equal configuration but different lifecycle
// owners from sharing, while Digest identifies the effective configuration.
type ResourceKey struct {
	Kind   string
	Scope  string
	Digest [32]byte
}

// ResourceFactory constructs a resource and returns its terminal close
// function. A failed factory is not retained by the registry.
type ResourceFactory[T any] func(context.Context) (T, func(context.Context) error, error)

type resourceEntry struct {
	ready           chan struct{}
	value           any
	closeResource   func(context.Context) error
	createErr       error
	creatorCanceled bool
	references      int
	closeOnce       sync.Once
	closeErr        error
}

func (e *resourceEntry) close(ctx context.Context) error {
	<-e.ready
	if e.createErr != nil {
		return nil
	}
	e.closeOnce.Do(func() {
		if e.closeResource != nil {
			e.closeErr = e.closeResource(ctx)
		}
	})
	return e.closeErr
}

// ResourceRegistry interns resources by identity and owns their terminal
// shutdown. A resource remains alive until its final lease is released or the
// registry is closed.
type ResourceRegistry struct {
	mu        sync.Mutex
	entries   map[ResourceKey]*resourceEntry
	closing   map[*resourceEntry]struct{}
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// NewResourceRegistry creates an empty resource registry.
func NewResourceRegistry() *ResourceRegistry {
	return &ResourceRegistry{
		entries: make(map[ResourceKey]*resourceEntry),
		closing: make(map[*resourceEntry]struct{}),
	}
}

// ResourceLease is a typed reference to one shared resource. Release is
// idempotent and replays the result of its first call.
type ResourceLease[T any] struct {
	value       T
	release     func(context.Context) error
	releaseOnce sync.Once
	releaseErr  error
}

// Value returns the leased resource.
func (l *ResourceLease[T]) Value() T {
	return l.value
}

// Release gives up this lease. The final release closes the resource.
func (l *ResourceLease[T]) Release(ctx context.Context) error {
	l.releaseOnce.Do(func() {
		l.releaseErr = l.release(ctx)
	})
	return l.releaseErr
}

// Acquire returns a typed lease for key. Concurrent acquisitions of the same
// key share one factory call and one resource until the final release.
func Acquire[T any](
	ctx context.Context,
	registry *ResourceRegistry,
	key ResourceKey,
	factory ResourceFactory[T],
) (*ResourceLease[T], error) {
	if registry == nil || key.Kind == "" || key.Scope == "" || key.Digest == ([32]byte{}) {
		return nil, ErrInvalidResourceIdentity
	}
	for {
		entry, creator, err := registry.reserve(ctx, key)
		if err != nil {
			return nil, err
		}
		if creator {
			value, closeResource, createErr := factory(ctx)
			registry.complete(key, entry, value, closeResource, createErr, ctx.Err())
		}
		value, release, retryableCreatorCancellation, err := registry.await(ctx, key, entry)
		if err != nil {
			if !creator && ctx.Err() == nil && retryableCreatorCancellation {
				continue
			}
			return nil, err
		}
		typed, ok := value.(T)
		if !ok {
			releaseErr := release(context.Background())
			return nil, joinReleaseError(ErrResourceTypeMismatch, releaseErr)
		}
		return &ResourceLease[T]{value: typed, release: release}, nil
	}
}

func (r *ResourceRegistry) reserve(
	ctx context.Context,
	key ResourceKey,
) (*resourceEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, false, ErrResourceRegistryClosed
	}
	if entry, ok := r.entries[key]; ok {
		entry.references++
		return entry, false, nil
	}
	entry := &resourceEntry{ready: make(chan struct{}), references: 1}
	r.entries[key] = entry
	return entry, true, nil
}

func (r *ResourceRegistry) complete(
	key ResourceKey,
	entry *resourceEntry,
	value any,
	closeResource func(context.Context) error,
	createErr error,
	creatorContextErr error,
) {
	r.mu.Lock()
	entry.value = value
	entry.closeResource = closeResource
	entry.createErr = createErr
	entry.creatorCanceled = isContextCancellation(creatorContextErr)
	if createErr != nil && r.entries[key] == entry {
		delete(r.entries, key)
	}
	close(entry.ready)
	r.mu.Unlock()
}

func (r *ResourceRegistry) await(
	ctx context.Context,
	key ResourceKey,
	entry *resourceEntry,
) (any, func(context.Context) error, bool, error) {
	if err := ctx.Err(); err != nil {
		releaseErr := r.release(key, entry, context.Background())
		return nil, nil, false, joinReleaseError(err, releaseErr)
	}
	select {
	case <-entry.ready:
	case <-ctx.Done():
		primaryErr := ctx.Err()
		releaseErr := r.release(key, entry, context.Background())
		return nil, nil, false, joinReleaseError(primaryErr, releaseErr)
	}
	if entry.createErr != nil {
		retryable := entry.creatorCanceled && isContextCancellation(entry.createErr)
		return nil, nil, retryable, entry.createErr
	}
	if err := ctx.Err(); err != nil {
		releaseErr := r.release(key, entry, context.Background())
		return nil, nil, false, joinReleaseError(err, releaseErr)
	}
	r.mu.Lock()
	if r.closed {
		if entry.references > 0 {
			entry.references--
		}
		r.mu.Unlock()
		return nil, nil, false, ErrResourceRegistryClosed
	}
	r.mu.Unlock()
	return entry.value, func(releaseCtx context.Context) error {
		return r.release(key, entry, releaseCtx)
	}, false, nil
}

func isContextCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func joinReleaseError(primaryErr, releaseErr error) error {
	if releaseErr == nil {
		return primaryErr
	}
	return errors.Join(primaryErr, releaseErr)
}

func (r *ResourceRegistry) release(key ResourceKey, entry *resourceEntry, ctx context.Context) error {
	r.mu.Lock()
	if entry.references > 0 {
		entry.references--
	}
	current := r.entries[key] == entry
	closeResource := false
	if current && entry.references == 0 {
		delete(r.entries, key)
		r.closing[entry] = struct{}{}
		closeResource = true
	} else if !current && r.closed {
		closeResource = true
	}
	r.mu.Unlock()
	if !closeResource {
		return nil
	}
	err := entry.close(ctx)
	r.mu.Lock()
	delete(r.closing, entry)
	r.mu.Unlock()
	return err
}

// Len returns the number of distinct resources accepting leases. Resources
// already detached for terminal close are not included.
func (r *ResourceRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Close prevents new acquisitions, waits for in-progress factories, and
// closes every remaining resource. Repeated calls replay the first result.
func (r *ResourceRegistry) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		entries := make([]*resourceEntry, 0, len(r.entries)+len(r.closing))
		for _, entry := range r.entries {
			entries = append(entries, entry)
		}
		for entry := range r.closing {
			entries = append(entries, entry)
		}
		r.entries = make(map[ResourceKey]*resourceEntry)
		r.mu.Unlock()

		errs := make([]error, 0, len(entries))
		for _, entry := range entries {
			if err := entry.close(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}
