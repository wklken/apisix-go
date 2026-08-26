package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

type observedDoneContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

type observedStagedCancelContext struct {
	*stagedCancelContext
	once     sync.Once
	observed chan struct{}
}

func (c *observedStagedCancelContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.stagedCancelContext.Done()
}

type triggeredDeadlineContext struct {
	context.Context
	once     sync.Once
	done     chan struct{}
	observed chan struct{}
}

func capturePanic(fn func()) (value any, panicked bool) {
	completed := false
	defer func() {
		if !completed {
			value = recover()
			panicked = true
		}
	}()
	fn()
	completed = true
	return nil, false
}

func (c *triggeredDeadlineContext) Done() <-chan struct{} {
	if c.observed != nil {
		c.once.Do(func() { close(c.observed) })
	}
	return c.done
}

func (c *triggeredDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
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

func TestResourceRegistryCanceledAcquireTempLeaseAuthorizesAcquireRetry(t *testing.T) {
	registry := NewResourceRegistry()
	key := testResourceKey()
	firstResidual := &TaskResidualError{cause: context.DeadlineExceeded}
	factoryStarted := make(chan struct{})
	allowFactoryReturn := make(chan struct{})
	secondCloseStarted := make(chan struct{})
	allowSecondClose := make(chan struct{})
	secondCloseReturned := make(chan struct{})
	replacementFactoryCalled := make(chan struct{})
	var creates atomic.Int32
	var closes atomic.Int32
	factory := func(ctx context.Context) (*testResource, func(context.Context) error, error) {
		switch creates.Add(1) {
		case 1:
			close(factoryStarted)
			<-allowFactoryReturn
			return &testResource{id: 1}, func(context.Context) error {
				switch closes.Add(1) {
				case 1:
					return firstResidual
				case 2:
					close(secondCloseStarted)
					<-allowSecondClose
					close(secondCloseReturned)
					return firstResidual
				default:
					return nil
				}
			}, nil
		default:
			close(replacementFactoryCalled)
			return &testResource{id: 2}, func(context.Context) error { return nil }, nil
		}
	}

	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorResult := make(chan error, 1)
	go func() {
		_, err := Acquire(creatorCtx, registry, key, factory)
		creatorResult <- err
	}()
	<-factoryStarted
	cancelCreator()
	close(allowFactoryReturn)
	if err := <-creatorResult; !errors.Is(err, context.Canceled) || !errors.Is(err, firstResidual) {
		t.Fatalf("canceled creator error = %v, want cancellation plus residual", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("initial close calls = %d, want 1", got)
	}

	retryBase, cancelRetry := context.WithCancel(context.Background())
	defer cancelRetry()
	retryCtx := &observedDoneContext{Context: retryBase, observed: make(chan struct{})}
	type acquireOutcome struct {
		lease *ResourceLease[*testResource]
		err   error
	}
	retryResult := make(chan acquireOutcome, 1)
	go func() {
		lease, err := Acquire(retryCtx, registry, key, factory)
		retryResult <- acquireOutcome{lease: lease, err: err}
	}()
	select {
	case <-secondCloseStarted:
	case <-retryCtx.observed:
		cancelRetry()
		outcome := <-retryResult
		t.Fatalf("Acquire() waited for terminal close without retrying: %v", outcome.err)
	}
	select {
	case <-replacementFactoryCalled:
		t.Fatal("replacement factory ran before retry finalizer returned")
	default:
	}
	close(allowSecondClose)
	<-secondCloseReturned
	outcome := <-retryResult
	if outcome.lease != nil || !errors.Is(outcome.err, firstResidual) {
		t.Fatalf("first retry Acquire() = (%#v, %v), want residual without replacement", outcome.lease, outcome.err)
	}
	select {
	case <-replacementFactoryCalled:
		t.Fatal("replacement factory ran after incomplete retry")
	default:
	}

	replacement, err := Acquire(context.Background(), registry, key, factory)
	if err != nil {
		t.Fatalf("terminal retry Acquire() error = %v", err)
	}
	if replacement == nil || replacement.Value().id != 2 {
		t.Fatalf("replacement lease = %#v, want resource id 2", replacement)
	}
	if err := replacement.Release(context.Background()); err != nil {
		t.Fatalf("replacement Release() error = %v", err)
	}
	if got := creates.Load(); got != 2 {
		t.Fatalf("factory calls = %d, want 2", got)
	}
	if got := closes.Load(); got != 3 {
		t.Fatalf("finalizer calls = %d, want 3", got)
	}
}

func TestResourceRegistryTypeMismatchTempLeaseAuthorizesAcquireRetry(t *testing.T) {
	registry := NewResourceRegistry()
	key := testResourceKey()
	firstResidual := &TaskResidualError{cause: context.DeadlineExceeded}
	secondCloseStarted := make(chan struct{})
	allowSecondClose := make(chan struct{})
	secondCloseReturned := make(chan struct{})
	replacementFactoryCalled := make(chan struct{})
	var closes atomic.Int32
	first, err := Acquire(context.Background(), registry, key, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{id: 1}, func(context.Context) error {
			switch closes.Add(1) {
			case 1:
				return firstResidual
			default:
				close(secondCloseStarted)
				<-allowSecondClose
				close(secondCloseReturned)
				return nil
			}
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
		_, mismatchErr := Acquire(mismatchCtx, registry, key, func(context.Context) (
			string, func(context.Context) error, error,
		) {
			t.Error("type-mismatched acquisition called the factory")
			return "wrong", nil, nil
		})
		mismatchResult <- mismatchErr
	}()
	<-mismatchCtx.blocked
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(mismatchCtx.proceed)
	if err := <-mismatchResult; !errors.Is(err, ErrResourceTypeMismatch) || !errors.Is(err, firstResidual) {
		t.Fatalf("type mismatch error = %v, want mismatch plus residual", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("initial close calls = %d, want 1", got)
	}

	retryBase, cancelRetry := context.WithCancel(context.Background())
	defer cancelRetry()
	retryCtx := &observedDoneContext{Context: retryBase, observed: make(chan struct{})}
	type acquireOutcome struct {
		lease *ResourceLease[*testResource]
		err   error
	}
	retryResult := make(chan acquireOutcome, 1)
	go func() {
		lease, acquireErr := Acquire(retryCtx, registry, key, func(context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			close(replacementFactoryCalled)
			return &testResource{id: 2}, func(context.Context) error { return nil }, nil
		})
		retryResult <- acquireOutcome{lease: lease, err: acquireErr}
	}()
	select {
	case <-secondCloseStarted:
	case <-retryCtx.observed:
		cancelRetry()
		outcome := <-retryResult
		t.Fatalf("Acquire() waited for terminal close without retrying: %v", outcome.err)
	}
	select {
	case <-replacementFactoryCalled:
		t.Fatal("replacement factory ran before retry finalizer returned")
	default:
	}
	close(allowSecondClose)
	<-secondCloseReturned
	outcome := <-retryResult
	if outcome.err != nil {
		t.Fatalf("replacement Acquire() error = %v", outcome.err)
	}
	if outcome.lease == nil || outcome.lease.Value().id != 2 {
		t.Fatalf("replacement lease = %#v, want resource id 2", outcome.lease)
	}
	if err := outcome.lease.Release(context.Background()); err != nil {
		t.Fatalf("replacement Release() error = %v", err)
	}
	if got := closes.Load(); got != 2 {
		t.Fatalf("original finalizer calls = %d, want 2", got)
	}
}

func TestResourceRegistryAcquireCapsOrphanRetryAcrossReplacementInterleave(t *testing.T) {
	registry := NewResourceRegistry()
	key := testResourceKey()
	aResidual := &TaskResidualError{cause: context.DeadlineExceeded}
	bResidual := &TaskResidualError{cause: context.DeadlineExceeded}
	aFactoryStarted := make(chan struct{})
	allowAFactoryReturn := make(chan struct{})
	var aCreates atomic.Int32
	var aCloses atomic.Int32
	aFactory := func(context.Context) (*testResource, func(context.Context) error, error) {
		if aCreates.Add(1) == 1 {
			close(aFactoryStarted)
			<-allowAFactoryReturn
		}
		return &testResource{id: aCreates.Load()}, func(context.Context) error {
			switch aCloses.Add(1) {
			case 1:
				return aResidual
			default:
				return nil
			}
		}, nil
	}

	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorResult := make(chan error, 1)
	go func() {
		_, err := Acquire(creatorCtx, registry, key, aFactory)
		creatorResult <- err
	}()
	<-aFactoryStarted
	cancelCreator()
	close(allowAFactoryReturn)
	if err := <-creatorResult; !errors.Is(err, context.Canceled) || !errors.Is(err, aResidual) {
		t.Fatalf("orphan A creation error = %v, want cancellation plus residual", err)
	}
	if got := aCloses.Load(); got != 1 {
		t.Fatalf("orphan A finalizer calls = %d, want 1", got)
	}

	aCtx := &observedStagedCancelContext{
		stagedCancelContext: &stagedCancelContext{
			Context: context.Background(),
			blockAt: 2,
			blocked: make(chan struct{}),
			proceed: make(chan struct{}),
			done:    make(chan struct{}),
		},
		observed: make(chan struct{}),
	}
	type acquireOutcome struct {
		lease *ResourceLease[*testResource]
		err   error
	}
	aResult := make(chan acquireOutcome, 1)
	go func() {
		lease, err := Acquire(aCtx, registry, key, aFactory)
		aResult <- acquireOutcome{lease: lease, err: err}
	}()
	<-aCtx.blocked
	if got := aCloses.Load(); got != 2 {
		t.Fatalf("orphan A finalizer calls before interleave = %d, want 2", got)
	}
	if got := aCreates.Load(); got != 1 {
		t.Fatalf("orphan A factory calls before interleave = %d, want 1", got)
	}

	bFactoryStarted := make(chan struct{})
	allowBFactoryReturn := make(chan struct{})
	allowBThirdClose := make(chan struct{})
	bThirdCloseStarted := make(chan struct{})
	allowBThirdCloseReturn := make(chan struct{})
	bFourthCloseStarted := make(chan struct{})
	bReplacementFactoryCalled := make(chan struct{})
	var bCreates atomic.Int32
	var bCloses atomic.Int32
	bFactory := func(context.Context) (*testResource, func(context.Context) error, error) {
		createNumber := bCreates.Add(1)
		switch createNumber {
		case 1:
			close(bFactoryStarted)
			<-allowBFactoryReturn
		default:
			close(bReplacementFactoryCalled)
			return &testResource{id: createNumber}, func(context.Context) error { return nil }, nil
		}
		return &testResource{id: createNumber}, func(context.Context) error {
			switch bCloses.Add(1) {
			case 1, 2:
				return bResidual
			case 3:
				close(bThirdCloseStarted)
				<-allowBThirdClose
				return bResidual
			default:
				close(bFourthCloseStarted)
				<-allowBThirdCloseReturn
				return nil
			}
		}, nil
	}

	bCreatorCtx, cancelBCreator := context.WithCancel(context.Background())
	bCreatorResult := make(chan error, 1)
	go func() {
		_, err := Acquire(bCreatorCtx, registry, key, bFactory)
		bCreatorResult <- err
	}()
	<-bFactoryStarted
	cancelBCreator()
	close(allowBFactoryReturn)
	if err := <-bCreatorResult; !errors.Is(err, context.Canceled) || !errors.Is(err, bResidual) {
		t.Fatalf("orphan B creation error = %v, want cancellation plus residual", err)
	}
	if got := bCloses.Load(); got != 1 {
		t.Fatalf("orphan B finalizer calls = %d, want 1", got)
	}

	bRetryResult := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), registry, key, bFactory)
		bRetryResult <- err
	}()
	if err := <-bRetryResult; err != bResidual {
		t.Fatalf("B retry Acquire() error = %v, want exact residual", err)
	}
	if got := bCloses.Load(); got != 2 {
		t.Fatalf("B retry finalizer calls = %d, want 2", got)
	}

	bConcurrentRetryResult := make(chan error, 1)
	go func() {
		_, err := Acquire(context.Background(), registry, key, bFactory)
		bConcurrentRetryResult <- err
	}()
	<-bThirdCloseStarted

	close(aCtx.proceed)
	select {
	case <-aCtx.observed:
		close(allowBThirdClose)
		outcome := <-aResult
		t.Fatalf(
			"A joined B's newer close attempt instead of replaying its orphan residual: lease=%#v err=%v",
			outcome.lease,
			outcome.err,
		)
	case outcome := <-aResult:
		if outcome.lease != nil || outcome.err != bResidual {
			t.Fatalf("A interleave outcome = (%#v, %v), want exact B residual", outcome.lease, outcome.err)
		}
	}
	if got := bCloses.Load(); got != 3 {
		t.Fatalf("B finalizer calls after capped A = %d, want 3", got)
	}
	close(allowBThirdClose)
	if err := <-bConcurrentRetryResult; err != bResidual {
		t.Fatalf("B concurrent retry error = %v, want exact residual", err)
	}

	futureResult := make(chan acquireOutcome, 1)
	go func() {
		lease, err := Acquire(context.Background(), registry, key, bFactory)
		futureResult <- acquireOutcome{lease: lease, err: err}
	}()
	<-bFourthCloseStarted
	select {
	case <-bReplacementFactoryCalled:
		t.Fatal("replacement B factory ran before B finalizer returned")
	default:
	}
	close(allowBThirdCloseReturn)
	future := <-futureResult
	if future.err != nil {
		t.Fatalf("future B Acquire() error = %v", future.err)
	}
	if future.lease == nil || future.lease.Value().id != 2 {
		t.Fatalf("future B replacement = %#v, want resource id 2", future.lease)
	}
	if err := future.lease.Release(context.Background()); err != nil {
		t.Fatalf("future B replacement Release() error = %v", err)
	}
	if got := bCloses.Load(); got != 4 {
		t.Fatalf("B finalizer calls after future retry = %d, want 4", got)
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

	retryRegistry := NewResourceRegistry()
	residual := &TaskResidualError{
		residuals: []TaskResidual{{Owner: "core/resource/test"}},
		cause:     context.DeadlineExceeded,
	}
	entered := make(chan struct{})
	publish := make(chan struct{})
	var retryCalls atomic.Int32
	retryLease, err := Acquire(context.Background(), retryRegistry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			if retryCalls.Add(1) == 1 {
				close(entered)
				<-publish
				return residual
			}
			return nil
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	retryResults := make(chan error, 8)
	go func() { retryResults <- retryLease.Release(context.Background()) }()
	<-entered
	for range 7 {
		waitCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
		go func() { retryResults <- retryLease.Release(waitCtx) }()
		<-waitCtx.observed
	}
	close(publish)
	for range 8 {
		if err := <-retryResults; err != residual {
			t.Fatalf("concurrent retryable Release() error = %v, want exact residual", err)
		}
	}
	if got := retryCalls.Load(); got != 1 {
		t.Fatalf("retryable attempt calls = %d, want 1", got)
	}
	if err := retryLease.Release(context.Background()); err != nil {
		t.Fatalf("later retry Release() error = %v", err)
	}
	if got := retryCalls.Load(); got != 2 {
		t.Fatalf("retryable calls after later retry = %d, want 2", got)
	}
}

func TestResourceRegistryFinalReleaseResidualRetainsResourceForRetry(t *testing.T) {
	registry := NewResourceRegistry()
	tasks := NewTaskRegistry(context.Background(), nil)
	releaseTask := make(chan struct{})
	started := make(chan struct{})
	if err := tasks.Go(TaskSpec{Owner: "core/resource/test", Criticality: TaskCore}, func(context.Context) error {
		close(started)
		<-releaseTask
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started

	var resourceReleased atomic.Bool
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(ctx context.Context) error {
			residuals, stopErr := tasks.Stop(ctx)
			if stopErr != nil || len(residuals) != 0 {
				return stopErr
			}
			resourceReleased.Store(true)
			return nil
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := make(chan struct{})
	close(deadline)
	short := &triggeredDeadlineContext{Context: context.Background(), done: deadline}
	first := lease.Release(short)
	var residual *TaskResidualError
	if !errors.As(first, &residual) || resourceReleased.Load() {
		t.Fatalf("first release = %v, resourceReleased = %t", first, resourceReleased.Load())
	}
	if registry.Len() != 0 {
		t.Fatalf("accepting Len = %d, want 0 while closing", registry.Len())
	}
	close(releaseTask)
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("retry release = %v", err)
	}
	if !resourceReleased.Load() {
		t.Fatal("terminal retry did not release resource")
	}
}

func TestResourceRegistryAcquireWaitsForClosingIdentityBeforeReplacement(t *testing.T) {
	registry := NewResourceRegistry()
	key := testResourceKey()
	allowTerminal := make(chan struct{})
	residual := &TaskResidualError{
		residuals: []TaskResidual{{Owner: "core/resource/test"}},
		cause:     context.DeadlineExceeded,
	}
	var factories atomic.Int32
	lease, err := Acquire(context.Background(), registry, key, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		factories.Add(1)
		return &testResource{id: 1}, func(context.Context) error {
			select {
			case <-allowTerminal:
				return nil
			default:
				return residual
			}
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(context.Background()); err != residual {
		t.Fatalf("first Release() error = %v, want exact residual", err)
	}

	waitCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	type acquireResult struct {
		lease *ResourceLease[*testResource]
		err   error
	}
	result := make(chan acquireResult, 1)
	factoryCalled := make(chan struct{})
	go func() {
		replacement, acquireErr := Acquire(waitCtx, registry, key, func(context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			factories.Add(1)
			close(factoryCalled)
			return &testResource{id: 2}, func(context.Context) error { return nil }, nil
		})
		result <- acquireResult{lease: replacement, err: acquireErr}
	}()
	select {
	case <-waitCtx.observed:
	case <-factoryCalled:
		t.Fatal("replacement factory ran before the closing resource reached terminal completion")
	}
	if got := factories.Load(); got != 1 {
		t.Fatalf("factory calls while closing = %d, want 1", got)
	}

	close(allowTerminal)
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("terminal Release() error = %v", err)
	}
	outcome := <-result
	if outcome.err != nil {
		t.Fatalf("replacement Acquire() error = %v", outcome.err)
	}
	if outcome.lease.Value().id != 2 || factories.Load() != 2 {
		t.Fatalf("replacement = %+v, factory calls = %d", outcome.lease.Value(), factories.Load())
	}
	if err := outcome.lease.Release(context.Background()); err != nil {
		t.Fatalf("replacement Release() error = %v", err)
	}
}

func TestResourceRegistryAcquireClosingIdentityHonorsContext(t *testing.T) {
	registry := NewResourceRegistry()
	key := testResourceKey()
	residual := &TaskResidualError{cause: context.DeadlineExceeded}
	var factories atomic.Int32
	lease, err := Acquire(context.Background(), registry, key, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		factories.Add(1)
		return &testResource{}, func(context.Context) error { return residual }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(context.Background()); err != residual {
		t.Fatalf("Release() error = %v, want exact residual", err)
	}

	deadline := make(chan struct{})
	waitCtx := &triggeredDeadlineContext{
		Context: context.Background(), done: deadline, observed: make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		_, acquireErr := Acquire(waitCtx, registry, key, func(context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			factories.Add(1)
			return &testResource{}, nil, nil
		})
		result <- acquireErr
	}()
	<-waitCtx.observed
	close(deadline)
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want context.DeadlineExceeded", err)
	}
	if got := factories.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
}

func TestResourceRegistryAcquireClosingIdentityReturnsRegistryClosed(t *testing.T) {
	registry := NewResourceRegistry()
	key := testResourceKey()
	residual := &TaskResidualError{cause: context.DeadlineExceeded}
	var factories atomic.Int32
	lease, err := Acquire(context.Background(), registry, key, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		factories.Add(1)
		return &testResource{}, func(context.Context) error { return residual }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(context.Background()); err != residual {
		t.Fatalf("Release() error = %v, want exact residual", err)
	}

	waitCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, acquireErr := Acquire(waitCtx, registry, key, func(context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			factories.Add(1)
			return &testResource{}, nil, nil
		})
		result <- acquireErr
	}()
	<-waitCtx.observed
	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := registry.Close(closeCtx); err != residual {
		t.Fatalf("Close() error = %v, want exact residual", err)
	}
	if err := <-result; !errors.Is(err, ErrResourceRegistryClosed) {
		t.Fatalf("Acquire() error = %v, want ErrResourceRegistryClosed", err)
	}
	if got := factories.Load(); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
}

func TestResourceRegistryCloseResidualRetainsEntriesAndRetries(t *testing.T) {
	registry := NewResourceRegistry()
	tasks := NewTaskRegistry(context.Background(), nil)
	releaseTask := make(chan struct{})
	started := make(chan struct{})
	if err := tasks.Go(TaskSpec{Owner: "core/resource/test", Criticality: TaskCore}, func(context.Context) error {
		close(started)
		<-releaseTask
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	var released atomic.Bool
	_, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(ctx context.Context) error {
			residuals, stopErr := tasks.Stop(ctx)
			if stopErr != nil || len(residuals) != 0 {
				return stopErr
			}
			released.Store(true)
			return nil
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithCancel(context.Background())
	cancel()
	first := registry.Close(short)
	var residual *TaskResidualError
	if !errors.As(first, &residual) || released.Load() {
		t.Fatalf("first Close() = %v, released = %t", first, released.Load())
	}
	if _, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, nil, nil
	}); !errors.Is(err, ErrResourceRegistryClosed) {
		t.Fatalf("Acquire() after Close error = %v, want ErrResourceRegistryClosed", err)
	}
	close(releaseTask)
	if err := registry.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
	if !released.Load() {
		t.Fatal("retry Close() did not terminally release resource")
	}
}

func TestResourceRegistryCloseRetriesResidualButNotTerminalReleaseError(t *testing.T) {
	registry := NewResourceRegistry()
	residual := &TaskResidualError{cause: context.DeadlineExceeded}
	allowTerminal := make(chan struct{})
	var retryCalls atomic.Int32
	var terminalCalls atomic.Int32
	_, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			retryCalls.Add(1)
			select {
			case <-allowTerminal:
				return nil
			default:
				return residual
			}
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	other := testResourceKey()
	other.Scope = "upstream/u2"
	_, err = Acquire(context.Background(), registry, other, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			terminalCalls.Add(1)
			return errCloseFixture
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := registry.Close(context.Background())
	var residualError *TaskResidualError
	if !errors.As(first, &residualError) || !errors.Is(first, errCloseFixture) {
		t.Fatalf("first Close() error = %v, want residual and errCloseFixture", first)
	}
	close(allowTerminal)
	if err := registry.Close(context.Background()); !errors.Is(err, errCloseFixture) || errors.Is(err, residual) {
		t.Fatalf("retry Close() error = %v, want terminal errCloseFixture only", err)
	}
	if retryCalls.Load() != 2 || terminalCalls.Load() != 1 {
		t.Fatalf("close calls = retry %d, terminal %d; want 2 and 1", retryCalls.Load(), terminalCalls.Load())
	}
}

func TestResourceRegistryConcurrentFinalReleaseAndCloseSerializeAttempts(t *testing.T) {
	registry := NewResourceRegistry()
	entered := make(chan struct{})
	allowClose := make(chan struct{})
	var calls atomic.Int32
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-allowClose
			return nil
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseResult := make(chan error, 1)
	go func() { releaseResult <- lease.Release(context.Background()) }()
	<-entered
	closeCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	closeResult := make(chan error, 1)
	go func() { closeResult <- registry.Close(closeCtx) }()
	select {
	case <-closeCtx.observed:
	case <-time.After(time.Second):
		close(allowClose)
		t.Fatal("concurrent Close did not join the active finalizer with a context-selectable wait")
	}
	close(allowClose)
	if err := <-releaseResult; err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("finalizer calls = %d, want 1", got)
	}
}

func TestResourceEntryConcurrentJoinersReplayResidualBeforeLaterRetry(t *testing.T) {
	registry := NewResourceRegistry()
	entered := make(chan struct{})
	publishResidual := make(chan struct{})
	allowTerminal := make(chan struct{})
	residual := &TaskResidualError{
		residuals: []TaskResidual{{Owner: "core/resource/test"}},
		cause:     context.DeadlineExceeded,
	}
	var calls atomic.Int32
	var released atomic.Bool
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			switch calls.Add(1) {
			case 1:
				close(entered)
				<-publishResidual
				return residual
			default:
				<-allowTerminal
				released.Store(true)
				return nil
			}
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseResult := make(chan error, 1)
	go func() { releaseResult <- lease.Release(context.Background()) }()
	<-entered
	closeCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	closeResult := make(chan error, 1)
	go func() { closeResult <- registry.Close(closeCtx) }()
	select {
	case <-closeCtx.observed:
	case <-time.After(time.Second):
		close(publishResidual)
		close(allowTerminal)
		t.Fatal("concurrent Close did not join the exact active resource close attempt")
	}
	close(publishResidual)
	if err := <-releaseResult; err != residual {
		t.Fatalf("Release() error = %v, want exact first attempt residual", err)
	}
	if err := <-closeResult; err != residual {
		t.Fatalf("Close() error = %v, want exact first attempt residual", err)
	}
	if calls.Load() != 1 || released.Load() {
		t.Fatalf("after first attempt calls = %d, released = %t", calls.Load(), released.Load())
	}
	close(allowTerminal)
	if err := registry.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
	if calls.Load() != 2 || !released.Load() {
		t.Fatalf("after retry calls = %d, released = %t", calls.Load(), released.Load())
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

func TestResourceRegistryFinalizerPanicPublishesAndReplaysExactValue(t *testing.T) {
	registry := NewResourceRegistry()
	started := make(chan struct{})
	allowPanic := make(chan struct{})
	panicValue := &struct{ name string }{name: "resource finalizer"}
	var closes atomic.Int32
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			closes.Add(1)
			close(started)
			<-allowPanic
			panic(panicValue)
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		value    any
		panicked bool
	}
	releaseResult := make(chan outcome, 1)
	go func() {
		value, panicked := capturePanic(func() {
			_ = lease.Release(context.Background())
		})
		releaseResult <- outcome{value: value, panicked: panicked}
	}()
	<-started

	closeLeaderCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	closeLeaderResult := make(chan outcome, 1)
	go func() {
		value, panicked := capturePanic(func() {
			_ = registry.Close(closeLeaderCtx)
		})
		closeLeaderResult <- outcome{value: value, panicked: panicked}
	}()
	<-closeLeaderCtx.observed

	registryJoinCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	registryJoinResult := make(chan outcome, 1)
	go func() {
		value, panicked := capturePanic(func() {
			_ = registry.Close(registryJoinCtx)
		})
		registryJoinResult <- outcome{value: value, panicked: panicked}
	}()
	<-registryJoinCtx.observed
	close(allowPanic)

	var releaseOutcome, closeLeaderOutcome, registryJoinOutcome outcome
	select {
	case releaseOutcome = <-releaseResult:
	case <-time.After(time.Second):
		t.Fatal("finalizer panic leader did not publish its attempt")
	}
	select {
	case closeLeaderOutcome = <-closeLeaderResult:
	case <-time.After(time.Second):
		t.Fatal("registry Close leader remained blocked after finalizer panic")
	}
	select {
	case registryJoinOutcome = <-registryJoinResult:
	case <-time.After(time.Second):
		t.Fatal("registry Close joiner remained blocked after finalizer panic")
	}
	if !releaseOutcome.panicked || releaseOutcome.value != panicValue {
		t.Fatalf(
			"leader panic = (%v, %t), want exact value %p",
			releaseOutcome.value,
			releaseOutcome.panicked,
			panicValue,
		)
	}
	if !closeLeaderOutcome.panicked || closeLeaderOutcome.value != panicValue {
		t.Fatalf(
			"registry Close leader panic = (%v, %t), want exact value %p",
			closeLeaderOutcome.value,
			closeLeaderOutcome.panicked,
			panicValue,
		)
	}
	if !registryJoinOutcome.panicked || registryJoinOutcome.value != panicValue {
		t.Fatalf(
			"registry Close joiner panic = (%v, %t), want exact value %p",
			registryJoinOutcome.value,
			registryJoinOutcome.panicked,
			panicValue,
		)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("finalizer calls = %d, want 1", got)
	}
	select {
	case <-lease.entry.terminalDone:
	default:
		t.Fatal("terminal barrier remained open after panic cleanup")
	}
	registry.mu.Lock()
	closing := len(registry.closing)
	terminal := registry.closeTerminal
	registry.mu.Unlock()
	if closing != 0 || !terminal || registry.Len() != 0 {
		t.Fatalf("panic cleanup left registry state closing=%d terminal=%t len=%d", closing, terminal, registry.Len())
	}

	repeatedReleaseValue, repeatedReleasePanicked := capturePanic(func() {
		_ = lease.Release(context.Background())
	})
	if !repeatedReleasePanicked || repeatedReleaseValue != panicValue {
		t.Fatalf(
			"repeated Release() panic = (%v, %t), want exact value %p",
			repeatedReleaseValue,
			repeatedReleasePanicked,
			panicValue,
		)
	}
	repeatedCloseValue, repeatedClosePanicked := capturePanic(func() {
		_ = registry.Close(context.Background())
	})
	if !repeatedClosePanicked || repeatedCloseValue != panicValue {
		t.Fatalf(
			"repeated Close() panic = (%v, %t), want exact value %p",
			repeatedCloseValue,
			repeatedClosePanicked,
			panicValue,
		)
	}
}

func TestResourceRegistryTerminalBarrierPublishesAfterClosingMappingDetach(t *testing.T) {
	registry := NewResourceRegistry()
	key := testResourceKey()
	lease, err := Acquire(context.Background(), registry, key, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error { return nil }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entry := lease.entry
	if !registry.releaseReference(key, entry, false) {
		t.Fatal("final reference did not enter closing state")
	}

	waitCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	type acquireOutcome struct {
		lease *ResourceLease[*testResource]
		err   error
	}
	factoryCalled := make(chan struct{})
	acquireResult := make(chan acquireOutcome, 1)
	go func() {
		replacement, acquireErr := Acquire(waitCtx, registry, key, func(context.Context) (
			*testResource, func(context.Context) error, error,
		) {
			close(factoryCalled)
			return &testResource{}, func(context.Context) error { return nil }, nil
		})
		acquireResult <- acquireOutcome{lease: replacement, err: acquireErr}
	}()
	<-waitCtx.observed

	registry.mu.Lock()
	closeResult := make(chan resourceCloseResult, 1)
	go func() { closeResult <- entry.close(context.Background()) }()
	result := <-closeResult
	if result.err != nil || result.panicked || !result.terminal {
		t.Fatalf("entry.close() result = %+v, want terminal nil result", result)
	}
	select {
	case <-entry.terminalDone:
		t.Fatal("terminal barrier published before closing mapping was detached")
	default:
	}
	select {
	case <-factoryCalled:
		t.Fatal("replacement factory ran while the closing mapping was retained")
	default:
	}
	if registry.closing[key] != entry {
		t.Fatal("closing mapping changed before explicit completion")
	}
	registry.mu.Unlock()

	registry.completeEntryResult(key, entry, result)
	outcome := <-acquireResult
	if outcome.err != nil {
		t.Fatalf("replacement Acquire() error = %v", outcome.err)
	}
	if outcome.lease == nil {
		t.Fatal("replacement Acquire() returned a nil lease")
	}
	if err := outcome.lease.Release(context.Background()); err != nil {
		t.Fatalf("replacement Release() error = %v", err)
	}
}

func TestResourceRegistryClosedRetryRecordsLaterReleasePanic(t *testing.T) {
	registry := NewResourceRegistry()
	residual := &TaskResidualError{cause: context.DeadlineExceeded}
	panicValue := &struct{ name string }{name: "late resource finalizer"}
	var closes atomic.Int32
	lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			switch closes.Add(1) {
			case 1, 2:
				return residual
			default:
				panic(panicValue)
			}
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(context.Background()); err != residual {
		t.Fatalf("first Release() error = %v, want exact residual", err)
	}
	if err := registry.Close(context.Background()); err != residual {
		t.Fatalf("first Close() error = %v, want exact residual", err)
	}

	releasePanic, releasePanicked := capturePanic(func() {
		_ = lease.Release(context.Background())
	})
	if !releasePanicked || releasePanic != panicValue {
		t.Fatalf("late Release() panic = (%v, %t), want exact value %p", releasePanic, releasePanicked, panicValue)
	}
	closePanic, closePanicked := capturePanic(func() {
		_ = registry.Close(context.Background())
	})
	if !closePanicked || closePanic != panicValue {
		t.Fatalf(
			"Close() after late Release() panic = (%v, %t), want exact value %p",
			closePanic,
			closePanicked,
			panicValue,
		)
	}
	repeatedClosePanic, repeatedClosePanicked := capturePanic(func() {
		_ = registry.Close(context.Background())
	})
	if !repeatedClosePanicked || repeatedClosePanic != panicValue {
		t.Fatalf(
			"repeated Close() panic = (%v, %t), want exact value %p",
			repeatedClosePanic,
			repeatedClosePanicked,
			panicValue,
		)
	}
	if got := closes.Load(); got != 3 {
		t.Fatalf("finalizer calls = %d, want 3", got)
	}
	if registry.Len() != 0 {
		t.Fatalf("Len() after terminal panic = %d, want 0", registry.Len())
	}
}

func TestResourceLeaseReleaseReplaysCallLocalResidualBeforeLaterTerminal(t *testing.T) {
	for name, terminal := range map[string]struct {
		err        error
		panicValue any
	}{
		"error": {err: errCloseFixture},
		"panic": {panicValue: &struct{ name string }{name: "later release panic"}},
	} {
		t.Run(name, func(t *testing.T) {
			registry := NewResourceRegistry()
			residual := &TaskResidualError{cause: context.DeadlineExceeded}
			firstStarted := make(chan struct{})
			allowFirst := make(chan struct{})
			var closes atomic.Int32
			lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
				*testResource, func(context.Context) error, error,
			) {
				return &testResource{}, func(context.Context) error {
					switch closes.Add(1) {
					case 1:
						close(firstStarted)
						<-allowFirst
						return residual
					default:
						if terminal.panicValue != nil {
							panic(terminal.panicValue)
						}
						return terminal.err
					}
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			firstAfterClose := make(chan struct{})
			allowFirstReturn := make(chan struct{})
			var hookCalls atomic.Int32
			lease.afterCloseHook = func() {
				if hookCalls.Add(1) == 1 {
					close(firstAfterClose)
					<-allowFirstReturn
				}
			}

			type outcome struct {
				err        error
				panicValue any
				panicked   bool
			}
			firstResult := make(chan outcome, 1)
			go func() {
				panicValue, panicked := capturePanic(func() {
					firstResult <- outcome{err: lease.Release(context.Background())}
				})
				if panicked {
					firstResult <- outcome{panicValue: panicValue, panicked: true}
				}
			}()
			<-firstStarted
			close(allowFirst)
			<-firstAfterClose

			secondResult := make(chan outcome, 1)
			go func() {
				panicValue, panicked := capturePanic(func() {
					secondResult <- outcome{err: lease.Release(context.Background())}
				})
				if panicked {
					secondResult <- outcome{panicValue: panicValue, panicked: true}
				}
			}()
			second := <-secondResult
			close(allowFirstReturn)
			first := <-firstResult
			if first.panicked || first.err != residual {
				t.Fatalf(
					"first Release() outcome = (%v, %t), want exact first-attempt residual",
					first.err,
					first.panicked,
				)
			}
			if terminal.panicValue != nil {
				if !second.panicked || second.panicValue != terminal.panicValue {
					t.Fatalf(
						"second Release() panic = (%v, %t), want exact value %p",
						second.panicValue,
						second.panicked,
						terminal.panicValue,
					)
				}
			} else if second.panicked || second.err != terminal.err {
				t.Fatalf(
					"second Release() outcome = (%v, %t), want exact error %v",
					second.err,
					second.panicked,
					terminal.err,
				)
			}
			if got := closes.Load(); got != 2 {
				t.Fatalf("finalizer calls = %d, want 2", got)
			}
		})
	}
}

func TestResourceRegistryCloseReplaysCallLocalResidualBeforeLaterReleaseTerminal(t *testing.T) {
	for name, terminal := range map[string]struct {
		err        error
		panicValue any
	}{
		"error": {err: errCloseFixture},
		"panic": {panicValue: &struct{ name string }{name: "later registry panic"}},
	} {
		t.Run(name, func(t *testing.T) {
			registry := NewResourceRegistry()
			residual := &TaskResidualError{cause: context.DeadlineExceeded}
			firstStarted := make(chan struct{})
			allowFirst := make(chan struct{})
			var closes atomic.Int32
			lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
				*testResource, func(context.Context) error, error,
			) {
				return &testResource{}, func(context.Context) error {
					switch closes.Add(1) {
					case 1:
						close(firstStarted)
						<-allowFirst
						return residual
					default:
						if terminal.panicValue != nil {
							panic(terminal.panicValue)
						}
						return terminal.err
					}
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			beforeCommit := make(chan struct{})
			allowCommit := make(chan struct{})
			var hookOnce sync.Once
			registry.beforeCloseCommitHook = func() {
				hookOnce.Do(func() {
					close(beforeCommit)
					<-allowCommit
				})
			}

			type outcome struct {
				err        error
				panicValue any
				panicked   bool
			}
			closeResult := make(chan outcome, 1)
			go func() {
				panicValue, panicked := capturePanic(func() {
					closeResult <- outcome{err: registry.Close(context.Background())}
				})
				if panicked {
					closeResult <- outcome{panicValue: panicValue, panicked: true}
				}
			}()
			<-firstStarted
			close(allowFirst)
			<-beforeCommit

			releaseResult := make(chan outcome, 1)
			go func() {
				panicValue, panicked := capturePanic(func() {
					releaseResult <- outcome{err: lease.Release(context.Background())}
				})
				if panicked {
					releaseResult <- outcome{panicValue: panicValue, panicked: true}
				}
			}()
			release := <-releaseResult
			close(allowCommit)
			first := <-closeResult
			if first.panicked || first.err != residual {
				t.Fatalf(
					"first Close() outcome = (%v, %t), want exact first-attempt residual",
					first.err,
					first.panicked,
				)
			}
			if terminal.panicValue != nil {
				if !release.panicked || release.panicValue != terminal.panicValue {
					t.Fatalf(
						"late Release() panic = (%v, %t), want exact value %p",
						release.panicValue,
						release.panicked,
						terminal.panicValue,
					)
				}
			} else if release.panicked || release.err != terminal.err {
				t.Fatalf(
					"late Release() outcome = (%v, %t), want exact error %v",
					release.err,
					release.panicked,
					terminal.err,
				)
			}

			laterCloseValue, laterClosePanicked := capturePanic(func() {
				if laterErr := registry.Close(
					context.Background(),
				); terminal.panicValue == nil &&
					laterErr != terminal.err {
					t.Fatalf("later Close() error = %v, want exact late terminal error", laterErr)
				}
			})
			if terminal.panicValue != nil && (!laterClosePanicked || laterCloseValue != terminal.panicValue) {
				t.Fatalf(
					"later Close() panic = (%v, %t), want exact value %p",
					laterCloseValue,
					laterClosePanicked,
					terminal.panicValue,
				)
			}
			if got := closes.Load(); got != 2 {
				t.Fatalf("finalizer calls = %d, want 2", got)
			}
		})
	}
}

func TestResourceRegistryStaleReleaseCompletionCannotChangeTerminalClose(t *testing.T) {
	for name, terminal := range map[string]struct {
		err        error
		panicValue any
	}{
		"error": {err: errCloseFixture},
		"panic": {panicValue: &struct{ name string }{name: "stale release panic"}},
	} {
		t.Run(name, func(t *testing.T) {
			registry := NewResourceRegistry()
			started := make(chan struct{})
			allowClose := make(chan struct{})
			var closes atomic.Int32
			lease, err := Acquire(context.Background(), registry, testResourceKey(), func(context.Context) (
				*testResource, func(context.Context) error, error,
			) {
				return &testResource{}, func(context.Context) error {
					closes.Add(1)
					close(started)
					<-allowClose
					if terminal.panicValue != nil {
						panic(terminal.panicValue)
					}
					return terminal.err
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			secondAtHook := make(chan struct{})
			allowSecondCompletion := make(chan struct{})
			var hookCalls atomic.Int32
			lease.afterCloseHook = func() {
				if hookCalls.Add(1) == 2 {
					close(secondAtHook)
					<-allowSecondCompletion
				}
			}

			type releaseOutcome struct {
				err        error
				panicValue any
				panicked   bool
			}
			results := make(chan releaseOutcome, 2)
			release := func(ctx context.Context) {
				go func() {
					var outcome releaseOutcome
					outcome.panicValue, outcome.panicked = capturePanic(func() {
						outcome.err = lease.Release(ctx)
					})
					results <- outcome
				}()
			}

			release(context.Background())
			<-started
			joinCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
			release(joinCtx)
			<-joinCtx.observed
			close(allowClose)
			<-secondAtHook
			first := <-results
			if terminal.panicValue != nil {
				if !first.panicked || first.panicValue != terminal.panicValue {
					t.Fatalf(
						"first Release() panic = (%v, %t), want exact value %p",
						first.panicValue,
						first.panicked,
						terminal.panicValue,
					)
				}
			} else if first.panicked || first.err != terminal.err {
				t.Fatalf(
					"first Release() outcome = (%v, %t), want exact error %v",
					first.err,
					first.panicked,
					terminal.err,
				)
			}

			if err := registry.Close(context.Background()); err != nil {
				t.Fatalf("Close() after resource detached while open error = %v, want nil", err)
			}
			close(allowSecondCompletion)
			<-results

			panicValue, panicked := capturePanic(func() {
				if repeatedErr := registry.Close(context.Background()); repeatedErr != nil {
					t.Fatalf("repeated Close() error = %v, want frozen nil", repeatedErr)
				}
			})
			if panicked {
				t.Fatalf("repeated Close() panic = %v, want frozen nil", panicValue)
			}
			if got := closes.Load(); got != 1 {
				t.Fatalf("finalizer calls = %d, want 1", got)
			}
		})
	}
}

func TestResourceRegistryTerminalCloseReplaysSameJoinedError(t *testing.T) {
	registry := NewResourceRegistry()
	errFirst := errors.New("first close error")
	errSecond := errors.New("second close error")
	started := make(chan struct{})
	allowClose := make(chan struct{})
	firstKey := testResourceKey()
	secondKey := testResourceKey()
	secondKey.Scope = "upstream/u2"
	secondKey.Digest = sha256.Sum256([]byte("second"))

	if _, err := Acquire(context.Background(), registry, firstKey, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error {
			close(started)
			<-allowClose
			return errFirst
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(context.Background(), registry, secondKey, func(context.Context) (
		*testResource, func(context.Context) error, error,
	) {
		return &testResource{}, func(context.Context) error { return errSecond }, nil
	}); err != nil {
		t.Fatal(err)
	}

	leaderResult := make(chan error, 1)
	go func() { leaderResult <- registry.Close(context.Background()) }()
	<-started
	joinCtx := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	joinerResult := make(chan error, 1)
	go func() { joinerResult <- registry.Close(joinCtx) }()
	<-joinCtx.observed
	close(allowClose)
	leaderErr := <-leaderResult
	joinerErr := <-joinerResult
	repeatedErr := registry.Close(context.Background())
	if !errors.Is(leaderErr, errFirst) || !errors.Is(leaderErr, errSecond) {
		t.Fatalf("leader Close() error = %v, want both terminal errors", leaderErr)
	}
	if joinerErr != leaderErr {
		t.Fatalf("joiner Close() error = %v, want exact leader object %v", joinerErr, leaderErr)
	}
	if repeatedErr != leaderErr {
		t.Fatalf("repeated Close() error = %v, want exact leader object %v", repeatedErr, leaderErr)
	}
}
