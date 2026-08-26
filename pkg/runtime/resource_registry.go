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

type resourceCloseAttempt struct {
	done       chan struct{}
	err        error
	panicValue any
	panicked   bool
	terminal   bool
}

type resourceCloseResult struct {
	err        error
	panicValue any
	panicked   bool
	terminal   bool
}

type resourceEntry struct {
	ready           chan struct{}
	value           any
	closeResource   func(context.Context) error
	createErr       error
	creatorCanceled bool
	references      int
	// retryOnAcquire is guarded by ResourceRegistry.mu and is set only when
	// the final reference belonged to an internal lease that cannot be retried.
	retryOnAcquire bool

	closeMu               sync.Mutex
	closeAttempt          *resourceCloseAttempt
	incompleteAttempt     *resourceCloseAttempt
	closeAttemptNotify    chan struct{}
	terminal              bool
	terminalErr           error
	terminalPanic         any
	terminalPanicked      bool
	terminalDone          chan struct{}
	terminalDonePublished bool
}

func (e *resourceEntry) close(ctx context.Context) resourceCloseResult {
	if ctx == nil {
		ctx = context.Background()
	}

	e.closeMu.Lock()
	if e.terminal {
		err := e.terminalErr
		panicValue := e.terminalPanic
		panicked := e.terminalPanicked
		e.closeMu.Unlock()
		return resourceCloseResult{
			err:        err,
			panicValue: panicValue,
			panicked:   panicked,
			terminal:   true,
		}
	}
	if attempt := e.closeAttempt; attempt != nil {
		e.closeMu.Unlock()
		return waitResourceCloseAttempt(ctx, attempt)
	}
	attempt := &resourceCloseAttempt{done: make(chan struct{})}
	e.closeAttempt = attempt
	if e.closeAttemptNotify == nil {
		e.closeAttemptNotify = make(chan struct{})
	}
	closeAttemptNotify := e.closeAttemptNotify
	e.closeAttemptNotify = make(chan struct{})
	close(closeAttemptNotify)
	e.closeMu.Unlock()

	ready := false
	select {
	case <-e.ready:
		ready = true
	default:
	}
	var err error
	var panicValue any
	var panicked bool
	func() {
		completed := false
		defer func() {
			if !completed {
				panicValue = recover()
				panicked = true
			}
		}()
		if ready {
			if e.createErr == nil && e.closeResource != nil {
				err = e.closeResource(ctx)
			}
		} else {
			select {
			case <-e.ready:
				if e.createErr == nil && e.closeResource != nil {
					err = e.closeResource(ctx)
				}
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		completed = true
	}()

	e.closeMu.Lock()
	attempt.err = err
	attempt.panicValue = panicValue
	attempt.panicked = panicked
	if !panicked && incompleteResourceClose(err) {
		if e.closeAttempt == attempt {
			e.closeAttempt = nil
		}
		e.incompleteAttempt = attempt
		close(attempt.done)
		e.closeMu.Unlock()
		return resourceCloseResult{err: err}
	}
	e.terminal = true
	e.terminalErr = err
	e.terminalPanic = panicValue
	e.terminalPanicked = panicked
	e.closeAttempt = nil
	e.incompleteAttempt = nil
	attempt.terminal = true
	close(attempt.done)
	e.closeMu.Unlock()
	return resourceCloseResult{
		err:        err,
		panicValue: panicValue,
		panicked:   panicked,
		terminal:   true,
	}
}

func waitResourceCloseAttempt(ctx context.Context, attempt *resourceCloseAttempt) resourceCloseResult {
	select {
	case <-attempt.done:
		return replayResourceCloseAttempt(attempt)
	default:
	}
	select {
	case <-attempt.done:
		return replayResourceCloseAttempt(attempt)
	case <-ctx.Done():
		select {
		case <-attempt.done:
			return replayResourceCloseAttempt(attempt)
		default:
			return resourceCloseResult{err: ctx.Err()}
		}
	}
}

func replayResourceCloseAttempt(attempt *resourceCloseAttempt) resourceCloseResult {
	return resourceCloseResult{
		err:        attempt.err,
		panicValue: attempt.panicValue,
		panicked:   attempt.panicked,
		terminal:   attempt.terminal,
	}
}

func incompleteResourceClose(err error) bool {
	if isContextCancellation(err) {
		return true
	}
	var residual *TaskResidualError
	return errors.As(err, &residual)
}

func (e *resourceEntry) terminalResult() (bool, error) {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	return e.terminal, e.terminalErr
}

func (e *resourceEntry) waitForCloseAttempt(ctx context.Context) resourceCloseResult {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		e.closeMu.Lock()
		if e.terminal {
			result := resourceCloseResult{
				err:        e.terminalErr,
				panicValue: e.terminalPanic,
				panicked:   e.terminalPanicked,
				terminal:   true,
			}
			e.closeMu.Unlock()
			return result
		}
		attempt := e.incompleteAttempt
		if attempt == nil {
			attempt = e.closeAttempt
		}
		attemptNotify := e.closeAttemptNotify
		e.closeMu.Unlock()
		if attempt != nil {
			return waitResourceCloseAttempt(ctx, attempt)
		}
		select {
		case <-attemptNotify:
		case <-e.terminalDone:
		case <-ctx.Done():
			return resourceCloseResult{err: ctx.Err()}
		}
	}
}

func (e *resourceEntry) publishTerminalDone() {
	e.closeMu.Lock()
	if !e.terminalDonePublished {
		e.terminalDonePublished = true
		close(e.terminalDone)
	}
	e.closeMu.Unlock()
}

type resourceRegistryCloseAttempt struct {
	done       chan struct{}
	err        error
	panicValue any
	panicked   bool
}

// ResourceRegistry interns resources by identity and owns their terminal
// shutdown. A resource remains alive until its final lease is released or the
// registry is closed.
type ResourceRegistry struct {
	mu      sync.Mutex
	entries map[ResourceKey]*resourceEntry
	closing map[ResourceKey]*resourceEntry
	closed  bool

	closedDone    chan struct{}
	closeAttempt  *resourceRegistryCloseAttempt
	closeRecorded map[*resourceEntry]struct{}
	closeErrors   []error
	closeTerminal bool
	closeErr      error
	closePanic    any
	closePanicked bool

	beforeCloseCommitHook func()
}

// NewResourceRegistry creates an empty resource registry.
func NewResourceRegistry() *ResourceRegistry {
	return &ResourceRegistry{
		entries:       make(map[ResourceKey]*resourceEntry),
		closing:       make(map[ResourceKey]*resourceEntry),
		closedDone:    make(chan struct{}),
		closeRecorded: make(map[*resourceEntry]struct{}),
	}
}

// ResourceLease is a typed reference to one shared resource. Release is
// idempotent. A final lease retains authority to retry an incomplete close.
type ResourceLease[T any] struct {
	value    T
	registry *ResourceRegistry
	key      ResourceKey
	entry    *resourceEntry

	afterCloseHook func()

	mu                sync.Mutex
	referenceReleased bool
	finalReference    bool
	// retryOnAcquire is true only for temporary leases never returned to callers.
	retryOnAcquire   bool
	terminal         bool
	terminalErr      error
	terminalPanic    any
	terminalPanicked bool
}

// Value returns the leased resource.
func (l *ResourceLease[T]) Value() T {
	return l.value
}

// Release gives up this lease. The final release closes the resource.
func (l *ResourceLease[T]) Release(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	if l.terminal {
		err := l.terminalErr
		panicValue := l.terminalPanic
		panicked := l.terminalPanicked
		l.mu.Unlock()
		if panicked {
			panic(panicValue)
		}
		return err
	}
	if !l.referenceReleased {
		l.referenceReleased = true
		l.finalReference = l.registry.releaseReference(l.key, l.entry, l.retryOnAcquire)
		if !l.finalReference {
			l.terminal = true
			l.mu.Unlock()
			return nil
		}
	}
	l.mu.Unlock()

	result := l.entry.close(ctx)
	if l.afterCloseHook != nil {
		l.afterCloseHook()
	}
	if result.terminal {
		l.registry.completeEntryResult(l.key, l.entry, result)
		l.mu.Lock()
		l.terminal = true
		l.terminalErr = result.err
		l.terminalPanic = result.panicValue
		l.terminalPanicked = result.panicked
		l.mu.Unlock()
		if result.panicked {
			panic(result.panicValue)
		}
		return result.err
	}
	return result.err
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
	orphanRetryUsed := false
	for {
		entry, closing, retryOnAcquire, creator, err := registry.reserve(ctx, key)
		if err != nil {
			return nil, err
		}
		if closing != nil {
			if retryOnAcquire {
				if orphanRetryUsed {
					result := closing.waitForCloseAttempt(ctx)
					if result.terminal {
						registry.completeEntryResult(key, closing, result)
						if result.panicked {
							panic(result.panicValue)
						}
						continue
					}
					return nil, result.err
				}
				orphanRetryUsed = true
				result := closing.close(ctx)
				if result.terminal {
					registry.completeEntryResult(key, closing, result)
					if result.panicked {
						panic(result.panicValue)
					}
					// This entry is terminal and detached. A following loop
					// iteration may observe a distinct replacement lifecycle.
					continue
				}
				// Never retry the same incomplete entry from one Acquire call.
				return nil, result.err
			}
			select {
			case <-closing.terminalDone:
				continue
			case <-registry.closedDone:
				return nil, ErrResourceRegistryClosed
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if creator {
			value, closeResource, createErr := factory(ctx)
			registry.complete(key, entry, value, closeResource, createErr, ctx.Err())
		}
		value, retryableCreatorCancellation, err := registry.await(ctx, key, entry)
		if err != nil {
			if !creator && ctx.Err() == nil && retryableCreatorCancellation {
				continue
			}
			return nil, err
		}
		typed, ok := value.(T)
		if !ok {
			mismatched := &ResourceLease[any]{
				value: value, registry: registry, key: key, entry: entry,
				retryOnAcquire: true,
			}
			releaseErr := mismatched.Release(context.Background())
			return nil, joinReleaseError(ErrResourceTypeMismatch, releaseErr)
		}
		return &ResourceLease[T]{value: typed, registry: registry, key: key, entry: entry}, nil
	}
}

func (r *ResourceRegistry) reserve(
	ctx context.Context,
	key ResourceKey,
) (*resourceEntry, *resourceEntry, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, false, false, ErrResourceRegistryClosed
	}
	if entry, ok := r.closing[key]; ok {
		return nil, entry, entry.retryOnAcquire, false, nil
	}
	if entry, ok := r.entries[key]; ok {
		entry.references++
		return entry, nil, false, false, nil
	}
	entry := &resourceEntry{
		ready: make(chan struct{}), references: 1, terminalDone: make(chan struct{}),
		closeAttemptNotify: make(chan struct{}),
	}
	r.entries[key] = entry
	return entry, nil, false, true, nil
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
) (any, bool, error) {
	if err := ctx.Err(); err != nil {
		lease := &ResourceLease[any]{
			registry: r, key: key, entry: entry, retryOnAcquire: true,
		}
		releaseErr := lease.Release(context.Background())
		return nil, false, joinReleaseError(err, releaseErr)
	}
	select {
	case <-entry.ready:
	case <-ctx.Done():
		primaryErr := ctx.Err()
		lease := &ResourceLease[any]{
			registry: r, key: key, entry: entry, retryOnAcquire: true,
		}
		releaseErr := lease.Release(context.Background())
		return nil, false, joinReleaseError(primaryErr, releaseErr)
	}
	if entry.createErr != nil {
		retryable := entry.creatorCanceled && isContextCancellation(entry.createErr)
		return nil, retryable, entry.createErr
	}
	if err := ctx.Err(); err != nil {
		lease := &ResourceLease[any]{
			registry: r, key: key, entry: entry, retryOnAcquire: true,
		}
		releaseErr := lease.Release(context.Background())
		return nil, false, joinReleaseError(err, releaseErr)
	}
	r.mu.Lock()
	if r.closed {
		if entry.references > 0 {
			entry.references--
		}
		r.mu.Unlock()
		return nil, false, ErrResourceRegistryClosed
	}
	r.mu.Unlock()
	return entry.value, false, nil
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

func (r *ResourceRegistry) releaseReference(
	key ResourceKey,
	entry *resourceEntry,
	retryOnAcquire bool,
) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry.references > 0 {
		entry.references--
	}
	if r.entries[key] == entry && entry.references == 0 {
		delete(r.entries, key)
		r.closing[key] = entry
		entry.retryOnAcquire = retryOnAcquire
		return true
	}
	if r.closing[key] == entry {
		return true
	}
	terminal, _ := entry.terminalResult()
	return terminal
}

func (r *ResourceRegistry) completeEntryResult(
	key ResourceKey,
	entry *resourceEntry,
	terminalResult resourceCloseResult,
) {
	if !terminalResult.terminal {
		return
	}
	r.mu.Lock()
	detached := false
	if r.closing[key] == entry {
		delete(r.closing, key)
		entry.retryOnAcquire = false
		detached = true
	}
	if detached && r.closed {
		r.recordTerminalEntryLocked(entry, terminalResult)
	}
	r.mu.Unlock()
	entry.publishTerminalDone()
}

func (r *ResourceRegistry) recordTerminalEntryLocked(entry *resourceEntry, result resourceCloseResult) {
	if _, recorded := r.closeRecorded[entry]; recorded {
		return
	}
	r.closeRecorded[entry] = struct{}{}
	if result.err != nil {
		r.closeErrors = append(r.closeErrors, result.err)
	}
	if result.panicked && !r.closePanicked {
		r.closePanicked = true
		r.closePanic = result.panicValue
	}
}

// Len returns the number of distinct resources accepting leases. Resources
// already detached for terminal close are not included.
func (r *ResourceRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Close prevents new acquisitions and advances all resources toward terminal
// close. A later call retries entries whose previous close was incomplete.
func (r *ResourceRegistry) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.closeTerminal {
		err := r.closeErr
		panicValue := r.closePanic
		panicked := r.closePanicked
		r.mu.Unlock()
		if panicked {
			panic(panicValue)
		}
		return err
	}
	if attempt := r.closeAttempt; attempt != nil {
		r.mu.Unlock()
		return waitResourceRegistryCloseAttempt(ctx, attempt)
	}
	if !r.closed {
		r.closed = true
		close(r.closedDone)
	}
	for key, entry := range r.entries {
		r.closing[key] = entry
		delete(r.entries, key)
	}
	type keyedEntry struct {
		key   ResourceKey
		entry *resourceEntry
	}
	entries := make([]keyedEntry, 0, len(r.closing))
	for key, entry := range r.closing {
		entries = append(entries, keyedEntry{key: key, entry: entry})
	}
	attempt := &resourceRegistryCloseAttempt{done: make(chan struct{})}
	r.closeAttempt = attempt
	attempt.panicked = r.closePanicked
	attempt.panicValue = r.closePanic
	attemptErrors := append([]error(nil), r.closeErrors...)
	r.mu.Unlock()

	attemptIncomplete := false
	for _, item := range entries {
		result := item.entry.close(ctx)
		if result.terminal {
			r.completeEntryResult(item.key, item.entry, result)
			if result.panicked && !attempt.panicked {
				attempt.panicked = true
				attempt.panicValue = result.panicValue
			}
		}
		if result.err != nil {
			attemptErrors = append(attemptErrors, result.err)
		}
		if !result.terminal {
			attemptIncomplete = true
		}
	}

	if r.beforeCloseCommitHook != nil {
		r.beforeCloseCommitHook()
	}
	r.mu.Lock()
	if attempt.panicked && !r.closePanicked {
		r.closePanicked = true
		r.closePanic = attempt.panicValue
	}
	attempt.err = joinErrors(attemptErrors)
	if len(r.closing) == 0 {
		r.closeTerminal = true
		if attemptIncomplete {
			r.closeErr = joinErrors(append([]error(nil), r.closeErrors...))
		} else {
			r.closeErr = attempt.err
		}
	}
	if r.closeAttempt == attempt {
		r.closeAttempt = nil
	}
	close(attempt.done)
	err := attempt.err
	panicValue := attempt.panicValue
	panicked := attempt.panicked
	r.mu.Unlock()
	if panicked {
		panic(panicValue)
	}
	return err
}

func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return errors.Join(errs...)
	}
}

func waitResourceRegistryCloseAttempt(ctx context.Context, attempt *resourceRegistryCloseAttempt) error {
	select {
	case <-attempt.done:
		return replayResourceRegistryCloseAttempt(attempt)
	default:
	}
	select {
	case <-attempt.done:
		return replayResourceRegistryCloseAttempt(attempt)
	case <-ctx.Done():
		select {
		case <-attempt.done:
			return replayResourceRegistryCloseAttempt(attempt)
		default:
			return ctx.Err()
		}
	}
}

func replayResourceRegistryCloseAttempt(attempt *resourceRegistryCloseAttempt) error {
	if attempt.panicked {
		panic(attempt.panicValue)
	}
	return attempt.err
}
