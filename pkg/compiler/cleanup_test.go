package compiler

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	var tasks atomic.Int64
	var registration atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := stack.Own(cleanupRelease, "registration", func(context.Context) error {
		registration.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Own(registration) error = %v", err)
	}
	if err := stack.Own(cleanupQuiesce, "tasks", func(context.Context) error {
		tasks.Add(1)
		close(entered)
		<-release
		return nil
	}); err != nil {
		t.Fatalf("Own(tasks) error = %v", err)
	}

	const callers = 32
	errs := make(chan error, callers)
	var callersReady sync.WaitGroup
	callersReady.Add(callers)
	for range callers {
		go func() {
			callersReady.Done()
			errs <- stack.Close(context.Background())
		}()
	}
	callersReady.Wait()
	<-entered
	close(release)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Close() error = %v", err)
		}
	}
	if got := tasks.Load(); got != 1 {
		t.Fatalf("task cleanup calls = %d, want 1", got)
	}
	if got := registration.Load(); got != 1 {
		t.Fatalf("registration cleanup calls = %d, want 1", got)
	}
}

func TestCleanupStackReplaysJoinedErrors(t *testing.T) {
	quiesceErr := errors.New("tasks did not quiesce")
	releaseErr := errors.New("registration did not release")
	var stack cleanupStack
	var calls atomic.Int64
	for _, test := range []struct {
		phase cleanupPhase
		name  string
		err   error
	}{
		{phase: cleanupRelease, name: "registration", err: releaseErr},
		{phase: cleanupRelease, name: "consumers"},
		{phase: cleanupQuiesce, name: "tasks", err: quiesceErr},
	} {
		if err := stack.Own(test.phase, test.name, func(ctx context.Context) error {
			if ctx == nil {
				t.Errorf("%s cleanup received nil context", test.name)
			}
			calls.Add(1)
			return test.err
		}); err != nil {
			t.Fatalf("Own(%q) error = %v", test.name, err)
		}
	}

	first := stack.Close(nil) //nolint:staticcheck // Close explicitly normalizes a nil cleanup context.
	if !errors.Is(first, quiesceErr) || !errors.Is(first, releaseErr) {
		t.Fatalf("Close() error = %v, want both cleanup errors", first)
	}
	if !strings.Contains(first.Error(), "tasks") || !strings.Contains(first.Error(), "registration") {
		t.Fatalf("Close() error = %v, want failing owner names", first)
	}
	replayed := stack.Close(context.Background())
	if replayed != first {
		t.Fatalf("replayed Close() error = %v, want cached result %v", replayed, first)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("cleanup calls after replay = %d, want 3", got)
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
			if err := stack.Own(test.phase, test.owner, test.run); !errors.Is(err, ErrInvalidInput) {
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
