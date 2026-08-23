package runtime

import (
	"context"
	"errors"
	goruntime "runtime"
	"testing"
	"time"
)

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
