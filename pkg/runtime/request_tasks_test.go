package runtime

import (
	"context"
	"errors"
	goruntime "runtime"
	"testing"
	"time"
)

type requestTaskWaitResult struct {
	err        error
	panicValue any
}

func waitRequestTaskGroup(group *RequestTaskGroup) (result requestTaskWaitResult) {
	defer func() { result.panicValue = recover() }()
	result.err = group.Wait()
	return result
}

func waitForRequestTaskGroupWaiting(t *testing.T, group *RequestTaskGroup) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		group.mu.Lock()
		waiting := group.waiting
		group.mu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("RequestTaskGroup.Wait did not close admission")
		}
		goruntime.Gosched()
	}
}

func waitForRequestTaskGroupPanic(t *testing.T, group *RequestTaskGroup, want any) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		group.mu.Lock()
		panicked := group.panicked
		panicValue := group.panicValue
		group.mu.Unlock()
		if panicked {
			if panicValue != want {
				t.Fatalf("recorded panic = %#v, want exact %#v", panicValue, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("RequestTaskGroup did not record panic")
		}
		goruntime.Gosched()
	}
}

func TestRequestTaskGroupJoinsAcceptedTaskErrors(t *testing.T) {
	wantA := errors.New("first failed")
	wantB := errors.New("second failed")
	group := NewRequestTaskGroup(context.Background(), "request/batch/r1")
	for _, taskErr := range []error{wantA, nil, wantB} {
		if err := group.Go(func(context.Context) error { return taskErr }); err != nil {
			t.Fatal(err)
		}
	}

	err := group.Wait()
	if !errors.Is(err, wantA) || !errors.Is(err, wantB) {
		t.Fatalf("Wait() error = %v", err)
	}
	if err := group.Go(func(context.Context) error { return nil }); !errors.Is(err, ErrTaskGroupWaiting) {
		t.Fatalf("Go() after Wait error = %v, want %v", err, ErrTaskGroupWaiting)
	}
}

func TestRequestTaskGroupWaitJoinsSiblingsBeforeRepanickingExactValue(t *testing.T) {
	previousProcs := goruntime.GOMAXPROCS(1)
	t.Cleanup(func() { goruntime.GOMAXPROCS(previousProcs) })

	group := NewRequestTaskGroup(context.Background(), "request/batch-requests")
	wantPanic := &struct{ marker string }{marker: "core-invariant"}
	releaseSibling := make(chan struct{})
	siblingDone := make(chan struct{})
	if err := group.Go(func(context.Context) error { panic(wantPanic) }); err != nil {
		t.Fatal(err)
	}
	if err := group.Go(func(context.Context) error {
		<-releaseSibling
		close(siblingDone)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitForRequestTaskGroupPanic(t, group, wantPanic)

	type panicResult struct {
		value         any
		siblingJoined bool
	}
	recovered := make(chan panicResult, 1)
	go func() {
		defer func() {
			result := panicResult{value: recover()}
			select {
			case <-siblingDone:
				result.siblingJoined = true
			default:
			}
			recovered <- result
		}()
		_ = group.Wait()
	}()
	waitForRequestTaskGroupWaiting(t, group)
	close(releaseSibling)
	result := <-recovered
	if !result.siblingJoined || result.value != wantPanic {
		t.Fatalf("recovered panic = %#v, sibling joined = %t, want exact %#v after join",
			result.value, result.siblingJoined, wantPanic)
	}
}

func TestRequestTaskGroupRepeatedAndConcurrentWaitReplaySamePanic(t *testing.T) {
	group := NewRequestTaskGroup(context.Background(), "connection/stream-bridge")
	firstPanic := &struct{ marker string }{marker: "first-invariant"}
	secondPanic := &struct{ marker string }{marker: "second-invariant"}
	triggerFirst := make(chan struct{})
	triggerSecond := make(chan struct{})
	if err := group.Go(func(context.Context) error {
		<-triggerFirst
		panic(firstPanic)
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Go(func(context.Context) error {
		<-triggerSecond
		panic(secondPanic)
	}); err != nil {
		t.Fatal(err)
	}
	close(triggerFirst)
	waitForRequestTaskGroupPanic(t, group, firstPanic)
	close(triggerSecond)

	const waiters = 4
	results := make(chan requestTaskWaitResult, waiters)
	for range waiters {
		go func() { results <- waitRequestTaskGroup(group) }()
	}
	for range waiters {
		result := <-results
		if result.err != nil || result.panicValue != firstPanic {
			t.Fatalf("concurrent Wait() = (%v, %#v), want exact panic %#v", result.err, result.panicValue, firstPanic)
		}
	}
	for range 2 {
		result := waitRequestTaskGroup(group)
		if result.err != nil || result.panicValue != firstPanic {
			t.Fatalf("repeated Wait() = (%v, %#v), want exact panic %#v", result.err, result.panicValue, firstPanic)
		}
	}
}

func TestRequestTaskGroupPanicTakesPriorityOverJoinedErrors(t *testing.T) {
	wantErr := errors.New("ordinary task failed")
	wantPanic := &struct{ marker string }{marker: "panic-wins"}
	group := NewRequestTaskGroup(context.Background(), "request/mixed-results")
	if err := group.Go(func(context.Context) error { return wantErr }); err != nil {
		t.Fatal(err)
	}
	if err := group.Go(func(context.Context) error { panic(wantPanic) }); err != nil {
		t.Fatal(err)
	}

	result := waitRequestTaskGroup(group)
	if result.err != nil || result.panicValue != wantPanic {
		t.Fatalf("Wait() = (%v, %#v), want exact panic %#v instead of joined error %v",
			result.err, result.panicValue, wantPanic, wantErr)
	}
}

func TestRequestTaskGroupWaitDoesNotDetachCompletion(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	group := NewRequestTaskGroup(parent, "request/mirror/r1")
	release := make(chan struct{})
	taskDone := make(chan struct{})
	if err := group.Go(func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		close(taskDone)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- group.Wait() }()
	cancel()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait() returned before accepted task completed: %v", err)
	default:
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := group.Go(func(context.Context) error { return nil })
		if errors.Is(err, ErrTaskGroupWaiting) {
			break
		}
		if err != nil {
			t.Fatalf("Go() while Wait is starting error = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("Wait() did not close task admission")
		}
		goruntime.Gosched()
	}

	close(release)
	<-taskDone
	if err := <-waitDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want %v", err, context.Canceled)
	}
}

func TestRequestTaskGroupValidatesConstructorAndCallbackBeforeAdmission(t *testing.T) {
	var missingParent context.Context
	tests := []struct {
		name  string
		group *RequestTaskGroup
		run   func(context.Context) error
		want  error
	}{
		{
			name: "nil parent",
			group: NewRequestTaskGroup(
				missingParent,
				"request/test/r1",
			),
			run:  func(context.Context) error { return nil },
			want: ErrTaskGroupContextRequired,
		},
		{
			name:  "empty owner",
			group: NewRequestTaskGroup(context.Background(), ""),
			run:   func(context.Context) error { return nil },
			want:  ErrTaskGroupOwnerRequired,
		},
		{
			name:  "nil callback",
			group: NewRequestTaskGroup(context.Background(), "request/test/r1"),
			want:  ErrTaskCallbackRequired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.group.Go(tt.run); !errors.Is(err, tt.want) {
				t.Fatalf("Go() error = %v, want %v", err, tt.want)
			}
			if err := tt.group.Wait(); err != nil {
				t.Fatalf("Wait() error = %v", err)
			}
		})
	}
}

func TestRequestTaskGroupWaitIsRepeatable(t *testing.T) {
	wantErr := errors.New("task failed")
	group := NewRequestTaskGroup(context.Background(), "request/repeat/r1")
	if err := group.Go(func(context.Context) error { return wantErr }); err != nil {
		t.Fatal(err)
	}
	if err := group.Wait(); !errors.Is(err, wantErr) {
		t.Fatalf("first Wait() error = %v", err)
	}
	if err := group.Wait(); !errors.Is(err, wantErr) {
		t.Fatalf("second Wait() error = %v", err)
	}
}
