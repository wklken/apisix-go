package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

var errCloseFixture = errors.New("close fixture")

type testResource struct {
	id     int32
	closed atomic.Bool
}

type stagedCancelContext struct {
	context.Context
	calls   atomic.Int32
	blockAt int32
	blocked chan struct{}
	proceed chan struct{}
	done    chan struct{}
}

func (c *stagedCancelContext) Done() <-chan struct{} {
	return c.done
}

func (c *stagedCancelContext) Err() error {
	if c.calls.Add(1) == c.blockAt {
		close(c.blocked)
		<-c.proceed
	}
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func testResourceKey() ResourceKey {
	return ResourceKey{
		Kind:   "upstream-cluster",
		Scope:  "upstream/u1",
		Digest: sha256.Sum256([]byte("same")),
	}
}

func TestResourceRegistrySharesEqualIdentityUntilFinalRelease(t *testing.T) {
	registry := NewResourceRegistry()
	var creates atomic.Int32
	factory := func(context.Context) (*testResource, func(context.Context) error, error) {
		resource := &testResource{id: creates.Add(1)}
		return resource, func(context.Context) error {
			resource.closed.Store(true)
			return nil
		}, nil
	}

	first, err := Acquire(context.Background(), registry, testResourceKey(), factory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(context.Background(), registry, testResourceKey(), factory)
	if err != nil {
		t.Fatal(err)
	}
	if first.Value() != second.Value() || creates.Load() != 1 {
		t.Fatal("equal identity did not share one resource")
	}
	if got := registry.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.Value().closed.Load() {
		t.Fatal("resource closed before final release")
	}
	if err := second.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !second.Value().closed.Load() {
		t.Fatal("resource remained open after final release")
	}
	if got := registry.Len(); got != 0 {
		t.Fatalf("Len() after final release = %d, want 0", got)
	}
}

func TestResourceRegistrySeparatesDifferentScopeAndDigest(t *testing.T) {
	registry := NewResourceRegistry()
	var creates atomic.Int32
	factory := func(context.Context) (*testResource, func(context.Context) error, error) {
		resource := &testResource{id: creates.Add(1)}
		return resource, func(context.Context) error { return nil }, nil
	}
	base := testResourceKey()
	differentScope := base
	differentScope.Scope = "upstream/u2"
	differentDigest := base
	differentDigest.Digest = sha256.Sum256([]byte("changed"))

	first, err := Acquire(context.Background(), registry, base, factory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(context.Background(), registry, differentScope, factory)
	if err != nil {
		t.Fatal(err)
	}
	third, err := Acquire(context.Background(), registry, differentDigest, factory)
	if err != nil {
		t.Fatal(err)
	}
	if first.Value() == second.Value() || first.Value() == third.Value() || second.Value() == third.Value() {
		t.Fatal("different resource identities shared a resource")
	}
	if got := creates.Load(); got != 3 {
		t.Fatalf("factory calls = %d, want 3", got)
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := third.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResourceRegistryConcurrentFirstAcquireHasSingleCreator(t *testing.T) {
	registry := NewResourceRegistry()
	started := make(chan struct{})
	continueFactory := make(chan struct{})
	var creates atomic.Int32
	factory := func(context.Context) (*testResource, func(context.Context) error, error) {
		if creates.Add(1) == 1 {
			close(started)
		}
		<-continueFactory
		return &testResource{}, func(context.Context) error { return nil }, nil
	}

	const acquisitions = 16
	leases := make(chan *ResourceLease[*testResource], acquisitions)
	errs := make(chan error, acquisitions)
	var wg sync.WaitGroup
	for range acquisitions {
		wg.Go(func() {
			lease, err := Acquire(context.Background(), registry, testResourceKey(), factory)
			if err != nil {
				errs <- err
				return
			}
			leases <- lease
		})
	}
	<-started
	close(continueFactory)
	wg.Wait()
	close(leases)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var shared *testResource
	for lease := range leases {
		if shared == nil {
			shared = lease.Value()
		} else if lease.Value() != shared {
			t.Fatal("concurrent equal identity created different resources")
		}
		if err := lease.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
}

func TestResourceRegistryDoesNotCacheFactoryFailure(t *testing.T) {
	registry := NewResourceRegistry()
	var attempts atomic.Int32
	factory := func(context.Context) (*testResource, func(context.Context) error, error) {
		if attempts.Add(1) == 1 {
			return nil, nil, errors.New("create fixture")
		}
		return &testResource{}, func(context.Context) error { return nil }, nil
	}

	if _, err := Acquire(context.Background(), registry, testResourceKey(), factory); err == nil {
		t.Fatal("first Acquire() error = nil, want factory failure")
	}
	lease, err := Acquire(context.Background(), registry, testResourceKey(), factory)
	if err != nil {
		t.Fatalf("second Acquire() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("factory calls = %d, want 2", got)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResourceRegistryDoesNotCacheCanceledFactory(t *testing.T) {
	registry := NewResourceRegistry()
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := Acquire(ctx, registry, testResourceKey(), func(ctx context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			close(started)
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Acquire() error = %v, want context.Canceled", err)
	}

	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error { return nil }, nil
	})
	if err != nil {
		t.Fatalf("Acquire() after canceled factory error = %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResourceRegistryHealthyWaitersShareReplacementAfterCanceledCreator(t *testing.T) {
	registry := NewResourceRegistry()
	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorStarted := make(chan struct{})
	var creates atomic.Int32
	var closes atomic.Int32
	resource := &testResource{}
	factory := func(ctx context.Context) (*testResource, func(context.Context) error, error) {
		if creates.Add(1) == 1 {
			close(creatorStarted)
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return resource, func(context.Context) error {
			closes.Add(1)
			resource.closed.Store(true)
			return nil
		}, nil
	}
	creatorResult := make(chan error, 1)
	go func() {
		_, err := Acquire(creatorCtx, registry, testResourceKey(), factory)
		creatorResult <- err
	}()
	<-creatorStarted

	const waiterCount = 8
	type waiterOutcome struct {
		lease *ResourceLease[*testResource]
		err   error
	}
	waiterContexts := make([]*stagedCancelContext, 0, waiterCount)
	waiterResults := make(chan waiterOutcome, waiterCount)
	for range waiterCount {
		waiterCtx := &stagedCancelContext{
			Context: context.Background(),
			blockAt: 2,
			blocked: make(chan struct{}),
			proceed: make(chan struct{}),
			done:    make(chan struct{}),
		}
		waiterContexts = append(waiterContexts, waiterCtx)
		go func() {
			lease, err := Acquire(waiterCtx, registry, testResourceKey(), factory)
			waiterResults <- waiterOutcome{lease: lease, err: err}
		}()
	}
	for _, waiterCtx := range waiterContexts {
		<-waiterCtx.blocked
	}
	cancelCreator()
	if err := <-creatorResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("creator Acquire() error = %v, want context.Canceled", err)
	}
	for _, waiterCtx := range waiterContexts {
		close(waiterCtx.proceed)
	}
	leases := make([]*ResourceLease[*testResource], 0, waiterCount)
	for range waiterCount {
		outcome := <-waiterResults
		if outcome.err != nil {
			t.Fatalf("healthy waiter Acquire() error = %v", outcome.err)
		}
		if outcome.lease.Value() != resource {
			t.Fatal("healthy waiters did not share the replacement resource")
		}
		leases = append(leases, outcome.lease)
	}
	if got := creates.Load(); got != 2 {
		t.Fatalf("factory calls = %d, want canceled creator plus one replacement", got)
	}
	for _, lease := range leases {
		if err := lease.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("replacement close calls = %d, want 1", got)
	}
	if !resource.closed.Load() || registry.Len() != 0 {
		t.Fatal("replacement resource leaked after final release")
	}
}

func TestResourceRegistryHealthyCreatorDeadlineFailureIsSharedWithoutRetry(t *testing.T) {
	registry := NewResourceRegistry()
	creatorStarted := make(chan struct{})
	finishCreator := make(chan struct{})
	var creates atomic.Int32
	factory := func(context.Context) (*testResource, func(context.Context) error, error) {
		if creates.Add(1) == 1 {
			close(creatorStarted)
			<-finishCreator
		}
		return nil, nil, context.DeadlineExceeded
	}
	creatorResult := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), registry, testResourceKey(), factory)
		creatorResult <- err
	}()
	<-creatorStarted

	const waiterCount = 8
	waiterContexts := make([]*stagedCancelContext, 0, waiterCount)
	waiterResults := make(chan error, waiterCount)
	for range waiterCount {
		waiterCtx := &stagedCancelContext{
			Context: context.Background(),
			blockAt: 2,
			blocked: make(chan struct{}),
			proceed: make(chan struct{}),
			done:    make(chan struct{}),
		}
		waiterContexts = append(waiterContexts, waiterCtx)
		go func() {
			_, err := Acquire(waiterCtx, registry, testResourceKey(), factory)
			waiterResults <- err
		}()
	}
	for _, waiterCtx := range waiterContexts {
		<-waiterCtx.blocked
	}
	close(finishCreator)
	if err := <-creatorResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("creator Acquire() error = %v, want context.DeadlineExceeded", err)
	}
	for _, waiterCtx := range waiterContexts {
		close(waiterCtx.proceed)
	}
	for range waiterCount {
		if err := <-waiterResults; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter Acquire() error = %v, want context.DeadlineExceeded", err)
		}
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want one shared business deadline failure", got)
	}
	if got := registry.Len(); got != 0 {
		t.Fatalf("Len() after shared factory failure = %d, want 0", got)
	}
}

func TestResourceRegistryCanceledFinalReferenceClosesResource(t *testing.T) {
	registry := NewResourceRegistry()
	resource := &testResource{}
	var closes atomic.Int32
	first, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return resource, func(context.Context) error {
			closes.Add(1)
			resource.closed.Store(true)
			return errCloseFixture
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	waiterCtx := &stagedCancelContext{
		Context: context.Background(),
		blockAt: 2,
		blocked: make(chan struct{}),
		proceed: make(chan struct{}),
		done:    make(chan struct{}),
	}
	waiterResult := make(chan error, 1)
	go func() {
		_, acquireErr := Acquire(waiterCtx, registry, testResourceKey(), func(context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			t.Error("shared acquisition called the factory")
			return nil, nil, nil
		})
		waiterResult <- acquireErr
	}()
	<-waiterCtx.blocked
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resource.closed.Load() {
		t.Fatal("resource closed while the waiting acquisition retained a reference")
	}
	close(waiterCtx.done)
	close(waiterCtx.proceed)
	if err := <-waiterResult; !errors.Is(err, context.Canceled) || !errors.Is(err, errCloseFixture) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled and errCloseFixture", err)
	}
	if !resource.closed.Load() {
		t.Fatal("resource remained open after the canceled final reference")
	}
	if got := registry.Len(); got != 0 {
		t.Fatalf("Len() after canceled final reference = %d, want 0", got)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

func TestResourceRegistryPostReadyCancellationJoinsCloseError(t *testing.T) {
	registry := NewResourceRegistry()
	resource := &testResource{}
	var closes atomic.Int32
	ctx := &stagedCancelContext{
		Context: context.Background(),
		blockAt: 3,
		blocked: make(chan struct{}),
		proceed: make(chan struct{}),
		done:    make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		_, err := Acquire(ctx, registry, testResourceKey(), func(context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			return resource, func(context.Context) error {
				closes.Add(1)
				resource.closed.Store(true)
				return errCloseFixture
			}, nil
		})
		result <- err
	}()
	<-ctx.blocked
	close(ctx.done)
	close(ctx.proceed)
	if err := <-result; !errors.Is(err, context.Canceled) || !errors.Is(err, errCloseFixture) {
		t.Fatalf("Acquire() error = %v, want context.Canceled and errCloseFixture", err)
	}
	if !resource.closed.Load() || registry.Len() != 0 {
		t.Fatal("post-ready cancellation leaked the final resource")
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

func TestResourceRegistryTypeMismatchJoinsFinalCloseError(t *testing.T) {
	registry := NewResourceRegistry()
	resource := &testResource{}
	var closes atomic.Int32
	first, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return resource, func(context.Context) error {
			closes.Add(1)
			resource.closed.Store(true)
			return errCloseFixture
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	mismatchCtx := &stagedCancelContext{
		Context: context.Background(),
		blockAt: 3,
		blocked: make(chan struct{}),
		proceed: make(chan struct{}),
		done:    make(chan struct{}),
	}
	mismatchResult := make(chan error, 1)
	go func() {
		_, acquireErr := Acquire(mismatchCtx, registry, testResourceKey(), func(context.Context) (
			string, func(context.Context) error, error,
		) {
			t.Error("type-mismatched shared acquisition called the factory")
			return "wrong", nil, nil
		})
		mismatchResult <- acquireErr
	}()
	<-mismatchCtx.blocked
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(mismatchCtx.proceed)
	if err := <-mismatchResult; !errors.Is(err, ErrResourceTypeMismatch) || !errors.Is(err, errCloseFixture) {
		t.Fatalf("type mismatch error = %v, want ErrResourceTypeMismatch and errCloseFixture", err)
	}
	if !resource.closed.Load() || registry.Len() != 0 {
		t.Fatal("type mismatch leaked the final resource")
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

func TestResourceRegistryRejectsInvalidIdentityAndTypeMismatch(t *testing.T) {
	registry := NewResourceRegistry()
	factory := func(context.Context) (*testResource, func(context.Context) error, error) {
		return &testResource{}, func(context.Context) error { return nil }, nil
	}
	for name, key := range map[string]ResourceKey{
		"empty kind":   {Scope: "scope", Digest: sha256.Sum256([]byte("digest"))},
		"empty scope":  {Kind: "kind", Digest: sha256.Sum256([]byte("digest"))},
		"empty digest": {Kind: "kind", Scope: "scope"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Acquire(
				context.Background(),
				registry,
				key,
				factory,
			); !errors.Is(
				err,
				ErrInvalidResourceIdentity,
			) {
				t.Fatalf("Acquire() error = %v, want ErrInvalidResourceIdentity", err)
			}
		})
	}
	if _, err := Acquire(
		context.Background(),
		nil,
		testResourceKey(),
		factory,
	); !errors.Is(
		err,
		ErrInvalidResourceIdentity,
	) {
		t.Fatalf("Acquire() with nil registry error = %v, want ErrInvalidResourceIdentity", err)
	}

	lease, err := Acquire(context.Background(), registry, testResourceKey(), factory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		string, func(context.Context) error, error,
	) {
		return "wrong", func(context.Context) error { return nil }, nil
	}); !errors.Is(err, ErrResourceTypeMismatch) {
		t.Fatalf("Acquire() type mismatch error = %v, want ErrResourceTypeMismatch", err)
	}
	if got := registry.Len(); got != 1 {
		t.Fatalf("Len() after type mismatch = %d, want 1", got)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResourceRegistryLeaseConcurrentReleaseRunsCloseOnce(t *testing.T) {
	registry := NewResourceRegistry()
	var closes atomic.Int32
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			closes.Add(1)
			return errCloseFixture
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			errs <- lease.Release(context.Background())
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, errCloseFixture) {
			t.Fatalf("Release() error = %v, want errCloseFixture", err)
		}
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

func TestResourceRegistryCloseIsTerminalAndReplaysCloseError(t *testing.T) {
	registry := NewResourceRegistry()
	var closes atomic.Int32
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			closes.Add(1)
			return errCloseFixture
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := registry.Close(context.Background()); !errors.Is(err, errCloseFixture) {
			t.Fatalf("Close() error = %v, want errCloseFixture", err)
		}
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
	if got := registry.Len(); got != 0 {
		t.Fatalf("Len() after Close = %d, want 0", got)
	}
	if _, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, nil, nil
	}); !errors.Is(err, ErrResourceRegistryClosed) {
		t.Fatalf("Acquire() after Close error = %v, want ErrResourceRegistryClosed", err)
	}
	if err := lease.Release(context.Background()); !errors.Is(err, errCloseFixture) {
		t.Fatalf("Release() after Close error = %v, want errCloseFixture", err)
	}
}
