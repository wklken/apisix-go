package compiler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestCleanupStackQuiescesThenReleasesInReverseOrder(t *testing.T) {
	var stack cleanupStack
	var order []string
	own := func(phase cleanupPhase, name string) {
		t.Helper()
		if err := stack.Own(phase, name, func(context.Context) error {
			order = append(order, name)
			return nil
		}); err != nil {
			t.Fatalf("Own(%q) error = %v", name, err)
		}
	}

	own(cleanupRelease, "registration")
	own(cleanupQuiesce, "tasks")
	own(cleanupRelease, "consumers")
	own(cleanupRelease, "plugin-1")
	own(cleanupRelease, "plugin-2")

	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	want := []string{"tasks", "plugin-2", "plugin-1", "consumers", "registration"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
}

func TestCleanupStackConcurrentCloseRunsEachStepOnce(t *testing.T) {
	var stack cleanupStack
	transient := errors.New("tasks still running")
	var attempts atomic.Int64
	var releases atomic.Int64
	entered := make(chan struct{})
	allowReturn := make(chan struct{})
	if err := stack.Own(cleanupRelease, "registration", func(context.Context) error {
		releases.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Own(registration) error = %v", err)
	}
	if err := stack.Own(cleanupQuiesce, "tasks", func(context.Context) error {
		attempt := attempts.Add(1)
		if attempt == 1 {
			close(entered)
			<-allowReturn
			return transient
		}
		return nil
	}); err != nil {
		t.Fatalf("Own(tasks) error = %v", err)
	}

	const callers = 32
	errs := make(chan error, callers)
	go func() {
		errs <- stack.Close(context.Background())
	}()
	<-entered

	waiting := make(chan struct{}, callers-1)
	for range callers - 1 {
		go func() {
			errs <- stack.Close(&cleanupWaiterContext{
				Context: context.Background(),
				waiting: waiting,
			})
		}()
	}
	for range callers - 1 {
		<-waiting
	}
	close(allowReturn)

	var first error
	for range callers {
		err := <-errs
		if !errors.Is(err, transient) {
			t.Fatalf("concurrent Close() error = %v, want %v", err, transient)
		}
		if first == nil {
			first = err
		} else if err != first {
			t.Fatalf("concurrent Close() result = %p, want attempt result %p", err, first)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("task cleanup calls in shared attempt = %d, want 1", got)
	}
	if got := releases.Load(); got != 0 {
		t.Fatalf("registration cleanup calls before retry = %d, want 0", got)
	}

	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("task cleanup calls after retry = %d, want 2", got)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("registration cleanup calls after retry = %d, want 1", got)
	}
}

type cleanupWaiterContext struct {
	context.Context
	waiting chan<- struct{}
	once    sync.Once
}

func (ctx *cleanupWaiterContext) Done() <-chan struct{} {
	ctx.once.Do(func() {
		ctx.waiting <- struct{}{}
	})
	return ctx.Context.Done()
}

func TestCleanupStackQuiesceFailureDefersEveryReleaseAndRetriesOnlyPendingQuiescer(t *testing.T) {
	var stack cleanupStack
	var calls []string
	var quiesceAttempts int
	transient := errors.New("tasks still running")
	if err := stack.Own(cleanupRelease, "registration", func(context.Context) error {
		calls = append(calls, "release-registration")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupQuiesce, "already-done", func(context.Context) error {
		calls = append(calls, "quiesce-already-done")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupQuiesce, "tasks", func(context.Context) error {
		quiesceAttempts++
		calls = append(calls, fmt.Sprintf("quiesce-tasks-%d", quiesceAttempts))
		if quiesceAttempts == 1 {
			return transient
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := stack.Close(context.Background()); !errors.Is(err, transient) {
		t.Fatalf("first Close error = %v, want %v", err, transient)
	}
	if slices.Contains(calls, "release-registration") {
		t.Fatalf("release crossed failed quiesce: %v", calls)
	}
	if stack.terminallyClosed() {
		t.Fatal("failed quiesce terminally closed cleanup")
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("retry Close error = %v", err)
	}
	want := []string{"quiesce-tasks-1", "quiesce-already-done", "quiesce-tasks-2", "release-registration"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if err := stack.Close(context.Background()); err != nil || quiesceAttempts != 2 {
		t.Fatalf("terminal replay = %v, attempts = %d", err, quiesceAttempts)
	}
	if !stack.terminallyClosed() {
		t.Fatal("successful retry did not terminally close cleanup")
	}
}

func TestCleanupStackTerminalReleaseErrorsJoinAndNeverRetry(t *testing.T) {
	first := errors.New("first release")
	second := errors.New("second release")
	var stack cleanupStack
	var calls atomic.Int32
	for _, item := range []struct {
		name string
		err  error
	}{
		{name: "first", err: first}, {name: "second", err: second},
	} {
		if err := stack.Own(cleanupRelease, item.name, func(context.Context) error {
			calls.Add(1)
			return item.err
		}); err != nil {
			t.Fatal(err)
		}
	}
	closeErr := stack.Close(context.Background())
	if !errors.Is(closeErr, first) || !errors.Is(closeErr, second) {
		t.Fatalf("Close error = %v, want both releases", closeErr)
	}
	if replay := stack.Close(context.Background()); replay != closeErr || calls.Load() != 2 {
		t.Fatalf("terminal replay = %v, calls = %d", replay, calls.Load())
	}
}

func TestCleanupStackRejectsLateOwnership(t *testing.T) {
	var stack cleanupStack
	noop := func(context.Context) error { return nil }
	for _, test := range []struct {
		name  string
		phase cleanupPhase
		owner string
		run   func(context.Context) error
	}{
		{name: "unknown phase", phase: cleanupPhase(255), owner: "unknown", run: noop},
		{name: "blank name", phase: cleanupRelease, owner: "  ", run: noop},
		{name: "nil callback", phase: cleanupRelease, owner: "registration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := stack.Own(test.phase, test.owner, test.run); !errors.Is(
				err,
				ErrInvalidInput,
			) {
				t.Fatalf("Own() error = %v, want ErrInvalidInput", err)
			}
		})
	}
	if err := stack.Own(cleanupRelease, "registration", noop); err != nil {
		t.Fatalf("Own(registration) error = %v", err)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	var lateCalls atomic.Int64
	late := func(context.Context) error {
		lateCalls.Add(1)
		return nil
	}
	if err := stack.Own(cleanupRelease, "late", late); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("late Own() error = %v, want ErrInvalidInput", err)
	}
	if got := lateCalls.Load(); got != 0 {
		t.Fatalf("late cleanup calls = %d, want 0", got)
	}
}

func TestCleanupStackRejectsOwnershipAfterSealWhileCleanupBlocked(t *testing.T) {
	var stack cleanupStack
	var originalCalls atomic.Int64
	var lateCalls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBlockedCleanup := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	t.Cleanup(releaseBlockedCleanup)

	if err := stack.Own(cleanupRelease, "original", func(context.Context) error {
		originalCalls.Add(1)
		close(entered)
		<-release
		return nil
	}); err != nil {
		t.Fatalf("Own(original) error = %v", err)
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- stack.Close(context.Background())
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cleanup to start")
	}

	lateOwnDone := make(chan error, 1)
	go func() {
		lateOwnDone <- stack.Own(cleanupRelease, "late", func(context.Context) error {
			lateCalls.Add(1)
			return nil
		})
	}()
	select {
	case err := <-lateOwnDone:
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("late Own() error = %v, want ErrInvalidInput", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late Own() blocked while cleanup callback was running")
	}
	if got := lateCalls.Load(); got != 0 {
		t.Fatalf("late cleanup calls before release = %d, want 0", got)
	}

	releaseBlockedCleanup()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Close()")
	}
	if got := originalCalls.Load(); got != 1 {
		t.Fatalf("original cleanup calls = %d, want 1", got)
	}
	if got := lateCalls.Load(); got != 0 {
		t.Fatalf("late cleanup calls = %d, want 0", got)
	}
}

func TestCleanupStackRollbackRunsOnlyLaterStepsInReversePhaseOrder(t *testing.T) {
	var stack cleanupStack
	var order []string
	own := func(phase cleanupPhase, name string) {
		t.Helper()
		if err := stack.Own(phase, name, func(context.Context) error {
			order = append(order, name)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	own(cleanupRelease, "base-release")
	own(cleanupQuiesce, "base-quiesce")
	checkpoint, err := stack.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	own(cleanupRelease, "route-release-1")
	own(cleanupQuiesce, "route-quiesce")
	own(cleanupRelease, "route-release-2")

	if err := stack.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	want := []string{"route-quiesce", "route-release-2", "route-release-1"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("rollback order = %v, want %v", order, want)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	want = append(want, "base-quiesce", "base-release")
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("close after rollback order = %v, want %v", order, want)
	}
}

func TestCleanupStackRollbackRetainsSuffixWhenQuiesceFails(t *testing.T) {
	var stack cleanupStack
	var calls []string
	if err := stack.Own(cleanupRelease, "base-release", func(context.Context) error {
		calls = append(calls, "base-release")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := stack.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	quiesceAttempts := 0
	transient := errors.New("suffix tasks still running")
	if err := stack.Own(cleanupQuiesce, "suffix-quiesce", func(context.Context) error {
		quiesceAttempts++
		calls = append(calls, fmt.Sprintf("suffix-quiesce-%d", quiesceAttempts))
		if quiesceAttempts == 1 {
			return transient
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupRelease, "suffix-release", func(context.Context) error {
		calls = append(calls, "suffix-release")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := stack.Rollback(context.Background(), checkpoint); !errors.Is(err, transient) {
		t.Fatalf("first Rollback error = %v, want %v", err, transient)
	}
	if slices.Contains(calls, "suffix-release") {
		t.Fatalf("release crossed failed rollback quiesce: %v", calls)
	}
	if err := stack.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatalf("retry Rollback error = %v", err)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("Close error = %v", err)
	}
	want := []string{"suffix-quiesce-1", "suffix-quiesce-2", "suffix-release", "base-release"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestCleanupStackRollbackPreservesOwnershipAddedBySuffixCallback(t *testing.T) {
	var stack cleanupStack
	var calls []string
	if err := stack.Own(cleanupRelease, "base", func(context.Context) error {
		calls = append(calls, "base")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := stack.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupRelease, "suffix", func(context.Context) error {
		calls = append(calls, "suffix")
		return stack.Own(cleanupRelease, "late", func(context.Context) error {
			calls = append(calls, "late")
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}

	if err := stack.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatalf("Rollback error = %v", err)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("Close error = %v", err)
	}
	want := []string{"suffix", "late", "base"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestCleanupStackRollbackSerializesWithConcurrentCleanup(t *testing.T) {
	for _, test := range []struct {
		name  string
		start func(*cleanupStack, context.Context, cleanupCheckpoint) error
	}{
		{
			name: "second rollback",
			start: func(stack *cleanupStack, ctx context.Context, checkpoint cleanupCheckpoint) error {
				return stack.Rollback(ctx, checkpoint)
			},
		},
		{
			name: "close",
			start: func(stack *cleanupStack, ctx context.Context, _ cleanupCheckpoint) error {
				return stack.Close(ctx)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stack cleanupStack
			var suffixCalls atomic.Int32
			var baseCalls atomic.Int32
			if err := stack.Own(cleanupRelease, "base", func(context.Context) error {
				baseCalls.Add(1)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := stack.Checkpoint()
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			secondEntered := make(chan struct{})
			allowReturn := make(chan struct{})
			if err := stack.Own(cleanupRelease, "suffix", func(context.Context) error {
				attempt := suffixCalls.Add(1)
				if attempt == 1 {
					close(entered)
				}
				if attempt == 2 {
					close(secondEntered)
				}
				<-allowReturn
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			firstDone := make(chan error, 1)
			go func() {
				firstDone <- stack.Rollback(context.Background(), checkpoint)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for first rollback callback")
			}

			waiting := make(chan struct{}, 1)
			concurrentDone := make(chan error, 1)
			go func() {
				concurrentDone <- test.start(&stack, &cleanupWaiterContext{
					Context: context.Background(),
					waiting: waiting,
				}, checkpoint)
			}()
			select {
			case <-waiting:
			case <-secondEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent cleanup neither waited nor entered callback")
			}
			close(allowReturn)

			for name, result := range map[string]<-chan error{
				"first rollback":     firstDone,
				"concurrent cleanup": concurrentDone,
			} {
				select {
				case err := <-result:
					if err != nil {
						t.Fatalf("%s error = %v", name, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for %s", name)
				}
			}
			if test.name == "second rollback" {
				if err := stack.Close(context.Background()); err != nil {
					t.Fatalf("Close error = %v", err)
				}
			}
			if suffixCalls.Load() != 1 || baseCalls.Load() != 1 {
				t.Fatalf(
					"cleanup calls = suffix:%d base:%d, want 1/1",
					suffixCalls.Load(),
					baseCalls.Load(),
				)
			}
		})
	}
}

func TestCleanupStackResourceFinalizationResidualDefersReleaseAndRetries(t *testing.T) {
	releaseResidual, residualErr := runtimeResidualFixture(t)
	defer releaseResidual()

	var stack cleanupStack
	var calls []string
	if err := stack.Own(cleanupRelease, "registration", func(context.Context) error {
		calls = append(calls, "release-registration")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupResourceFinalize, "already-done", func(context.Context) error {
		calls = append(calls, "finalize-already-done")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupQuiesce, "tasks", func(context.Context) error {
		calls = append(calls, "quiesce-tasks")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	finalizeAttempts := 0
	if err := stack.Own(cleanupResourceFinalize, "runtime", func(context.Context) error {
		finalizeAttempts++
		calls = append(calls, fmt.Sprintf("finalize-runtime-%d", finalizeAttempts))
		if finalizeAttempts == 1 {
			return errors.Join(residualErr, context.DeadlineExceeded)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	first := stack.Close(context.Background())
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want task residual and deadline", first)
	}
	if slices.Contains(calls, "release-registration") {
		t.Fatalf("release crossed incomplete finalization: %v", calls)
	}
	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("retry Close error = %v", err)
	}
	want := []string{
		"quiesce-tasks",
		"finalize-runtime-1",
		"finalize-already-done",
		"finalize-runtime-2",
		"release-registration",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestCleanupStackResourceFinalizationTerminalErrorIsRecordedOnceAndAllowsRelease(t *testing.T) {
	terminalFinalizeErr := errors.New("terminal finalizer")
	releaseErr := errors.New("release")
	var stack cleanupStack
	var finalizerCalls atomic.Int32
	var releaseCalls atomic.Int32
	if err := stack.Own(cleanupRelease, "registration", func(context.Context) error {
		releaseCalls.Add(1)
		return releaseErr
	}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupResourceFinalize, "successful", func(context.Context) error {
		finalizerCalls.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := stack.Own(cleanupResourceFinalize, "terminal", func(context.Context) error {
		finalizerCalls.Add(1)
		return terminalFinalizeErr
	}); err != nil {
		t.Fatal(err)
	}

	closeErr := stack.Close(context.Background())
	if !errors.Is(closeErr, terminalFinalizeErr) || !errors.Is(closeErr, releaseErr) {
		t.Fatalf("Close error = %v, want finalizer and release errors", closeErr)
	}
	if replay := stack.Close(context.Background()); replay != closeErr ||
		finalizerCalls.Load() != 2 || releaseCalls.Load() != 1 {
		t.Fatalf(
			"terminal replay = %v, finalizers = %d, releases = %d",
			replay,
			finalizerCalls.Load(),
			releaseCalls.Load(),
		)
	}
}

func TestCleanupStackResourceFinalizationIncompleteClassifiers(t *testing.T) {
	releaseResidual, residualErr := runtimeResidualFixture(t)
	defer releaseResidual()

	for _, test := range []struct {
		name       string
		err        error
		incomplete bool
	}{
		{name: "task residual", err: residualErr, incomplete: true},
		{name: "canceled", err: context.Canceled, incomplete: true},
		{name: "deadline exceeded", err: context.DeadlineExceeded, incomplete: true},
		{name: "cleanup incomplete", err: ErrPreparedGenerationCleanupIncomplete, incomplete: true},
		{name: "ordinary terminal error", err: errors.New("terminal")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stack cleanupStack
			var finalizerCalls atomic.Int32
			var releaseCalls atomic.Int32
			if err := stack.Own(cleanupRelease, "release", func(context.Context) error {
				releaseCalls.Add(1)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := stack.Own(cleanupResourceFinalize, "finalizer", func(context.Context) error {
				finalizerCalls.Add(1)
				return test.err
			}); err != nil {
				t.Fatal(err)
			}

			first := stack.Close(context.Background())
			if !errors.Is(first, test.err) {
				t.Fatalf("first Close error = %v, want %v", first, test.err)
			}
			second := stack.Close(context.Background())
			if !errors.Is(second, test.err) {
				t.Fatalf("second Close error = %v, want %v", second, test.err)
			}
			if test.incomplete {
				if finalizerCalls.Load() != 2 || releaseCalls.Load() != 0 || stack.terminallyClosed() {
					t.Fatalf(
						"incomplete calls = finalizer:%d release:%d terminal:%v",
						finalizerCalls.Load(),
						releaseCalls.Load(),
						stack.terminallyClosed(),
					)
				}
				return
			}
			if second != first || finalizerCalls.Load() != 1 || releaseCalls.Load() != 1 ||
				!stack.terminallyClosed() {
				t.Fatalf(
					"terminal replay = %v, finalizer:%d release:%d terminal:%v",
					second,
					finalizerCalls.Load(),
					releaseCalls.Load(),
					stack.terminallyClosed(),
				)
			}
		})
	}
}

func runtimeResidualFixture(t *testing.T) (func(), error) {
	t.Helper()
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	started := make(chan struct{})
	allowExit := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(allowExit)
		})
	}
	t.Cleanup(release)
	if err := registry.Go(runtime.TaskSpec{
		Owner:       "compiler/cleanup/residual",
		Criticality: runtime.TaskCore,
	}, func(context.Context) error {
		close(started)
		<-allowExit
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	stopCtx, cancel := context.WithCancel(context.Background())
	cancel()
	residuals, stopErr := registry.Stop(stopCtx)
	if len(residuals) != 1 || stopErr == nil {
		t.Fatalf("TaskRegistry.Stop() = (%v, %v), want one residual", residuals, stopErr)
	}
	return release, stopErr
}

func TestCleanupStackRollbackRejectsForeignOrSealedCheckpoint(t *testing.T) {
	var first cleanupStack
	var second cleanupStack
	checkpoint, err := first.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Rollback(context.Background(), checkpoint); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("foreign checkpoint error = %v, want ErrInvalidInput", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Checkpoint(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sealed checkpoint error = %v, want ErrInvalidInput", err)
	}
	if err := first.Rollback(context.Background(), checkpoint); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sealed rollback error = %v, want ErrInvalidInput", err)
	}
}
