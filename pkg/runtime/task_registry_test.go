package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTaskOwnerUsesExactPrefixComponentAndPluginFailureIsolation(t *testing.T) {
	failures := make(chan TaskFailure, 1)
	registry := NewTaskRegistry(context.Background(), func(f TaskFailure) { failures <- f })
	owner, err := NewTaskOwner(registry, "plugin/request-id/attempt/scope/route/digest", TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("health failed")
	if err := owner.Go("health-refresh", func(context.Context) error { return wantErr }); err != nil {
		t.Fatal(err)
	}
	failure := <-failures
	if failure.Owner != "plugin/request-id/attempt/scope/route/digest/health-refresh" ||
		!errors.Is(failure.Err, wantErr) {
		t.Fatalf("failure = %#v", failure)
	}
	if err := owner.Go(
		"health-refresh",
		func(context.Context) error { return nil },
	); !errors.Is(
		err,
		ErrTaskOwnerFailed,
	) {
		t.Fatalf("failed component admission = %v", err)
	}
	done := make(chan struct{})
	if err := owner.Go("disk-cleanup", func(context.Context) error { close(done); return nil }); err != nil {
		t.Fatalf("sibling component admission = %v", err)
	}
	<-done
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskOwnerDelegatesTaskCoreWithoutPoisoningComponent(t *testing.T) {
	failures := make(chan TaskFailure, 1)
	registry := NewTaskRegistry(context.Background(), func(f TaskFailure) { failures <- f })
	owner, err := NewTaskOwner(registry, "core/runtime/key", TaskCore)
	if err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("core task failed")
	if err := owner.Go("health-refresh", func(context.Context) error { return wantErr }); err != nil {
		t.Fatal(err)
	}
	failure := <-failures
	if failure.Owner != "core/runtime/key/health-refresh" || !errors.Is(failure.Err, wantErr) {
		t.Fatalf("failure = %#v", failure)
	}
	done := make(chan struct{})
	if err := owner.Go("health-refresh", func(context.Context) error {
		close(done)
		return nil
	}); err != nil {
		t.Fatalf("second core component admission = %v", err)
	}
	<-done
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskOwnerAcceptsComponentBoundaries(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	owner, err := NewTaskOwner(registry, "plugin/test/key", TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 2)
	for _, component := range []string{"a0", strings.Repeat("a", 64)} {
		if err := owner.Go(component, func(context.Context) error {
			done <- component
			return nil
		}); err != nil {
			t.Fatalf("Go(%q) error = %v", component, err)
		}
	}
	for range 2 {
		<-done
	}
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskOwnerStopReportsExactDeduplicatedResidual(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	owner, err := NewTaskOwner(registry, "plugin/logger/key", TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	for range 2 {
		if err := owner.Go("batch-worker", func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	<-started
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	residuals, stopErr := registry.Stop(ctx)
	if !errors.Is(stopErr, context.DeadlineExceeded) || !reflect.DeepEqual(
		residuals, []TaskResidual{{Owner: "plugin/logger/key/batch-worker"}},
	) {
		t.Fatalf("Stop() = (%v, %v)", residuals, stopErr)
	}
	close(release)
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("retry Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskOwnerValidatesBeforeAdmission(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	constructorTests := []struct {
		name        string
		registry    *TaskRegistry
		prefix      string
		criticality TaskCriticality
		want        error
	}{
		{
			name:        "nil registry",
			prefix:      "plugin/test/key",
			criticality: TaskPlugin,
			want:        ErrTaskRegistryRequired,
		},
		{
			name:        "blank prefix",
			registry:    registry,
			prefix:      " \t",
			criticality: TaskPlugin,
			want:        ErrTaskOwnerRequired,
		},
		{
			name:        "padded prefix",
			registry:    registry,
			prefix:      " plugin/test/key ",
			criticality: TaskPlugin,
			want:        ErrTaskOwnerRequired,
		},
		{
			name:        "empty prefix",
			registry:    registry,
			criticality: TaskPlugin,
			want:        ErrTaskOwnerRequired,
		},
		{
			name:        "invalid criticality",
			registry:    registry,
			prefix:      "plugin/test/key",
			criticality: "unknown",
			want:        ErrTaskCriticalityInvalid,
		},
	}
	for _, tt := range constructorTests {
		t.Run(tt.name, func(t *testing.T) {
			owner, err := NewTaskOwner(tt.registry, tt.prefix, tt.criticality)
			if owner != nil || !errors.Is(err, tt.want) {
				t.Fatalf("NewTaskOwner() = (%#v, %v), want (nil, %v)", owner, err, tt.want)
			}
			if got := registry.Active(); len(got) != 0 {
				t.Fatalf("Active() = %v", got)
			}
		})
	}

	owner, err := NewTaskOwner(registry, "plugin/test/key", TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	componentTests := []struct {
		name      string
		component string
		run       func(context.Context) error
		want      error
	}{
		{name: "empty component", run: func(context.Context) error { return nil }, want: ErrTaskComponentInvalid},
		{
			name:      "uppercase component",
			component: "Health",
			run:       func(context.Context) error { return nil },
			want:      ErrTaskComponentInvalid,
		},
		{
			name:      "slashed component",
			component: "health/refresh",
			run:       func(context.Context) error { return nil },
			want:      ErrTaskComponentInvalid,
		},
		{
			name:      "whitespace component",
			component: "health refresh",
			run:       func(context.Context) error { return nil },
			want:      ErrTaskComponentInvalid,
		},
		{
			name:      "leading hyphen",
			component: "-health",
			run:       func(context.Context) error { return nil },
			want:      ErrTaskComponentInvalid,
		},
		{
			name:      "trailing hyphen",
			component: "health-",
			run:       func(context.Context) error { return nil },
			want:      ErrTaskComponentInvalid,
		},
		{
			name:      "65-byte component",
			component: strings.Repeat("a", 65),
			run:       func(context.Context) error { return nil },
			want:      ErrTaskComponentInvalid,
		},
		{name: "nil callback", component: "health-refresh", want: ErrTaskCallbackRequired},
	}
	for _, tt := range componentTests {
		t.Run(tt.name, func(t *testing.T) {
			if err := owner.Go(tt.component, tt.run); !errors.Is(err, tt.want) {
				t.Fatalf("Go() error = %v, want %v", err, tt.want)
			}
			if got := registry.Active(); len(got) != 0 {
				t.Fatalf("Active() = %v", got)
			}
		})
	}
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestNewTaskOwnerTreatsPrefixAsOpaqueProducerIdentity(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	prefix := "Custom Owner/" + strings.Repeat("segment/", 512) + "tail"
	owner, err := NewTaskOwner(registry, prefix, TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	if err := owner.Go("health-refresh", func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	if got, want := registry.Active(), []string{prefix + "/health-refresh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Active() = %v, want %v", got, want)
	}
	close(release)
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskRegistryReportsPluginPanicAndJoinsOwner(t *testing.T) {
	failures := make(chan TaskFailure, 1)
	registry := NewTaskRegistry(context.Background(), func(f TaskFailure) { failures <- f })

	if err := registry.Go(TaskSpec{Owner: "plugin/http-logger/r1", Criticality: TaskPlugin},
		func(context.Context) error { panic("boom") }); err != nil {
		t.Fatal(err)
	}

	failure := <-failures
	if failure.Owner != "plugin/http-logger/r1" || failure.PanicValue != "boom" || len(failure.Stack) == 0 {
		t.Fatalf("failure = %#v", failure)
	}
	if err := registry.Go(TaskSpec{Owner: "plugin/http-logger/r1", Criticality: TaskPlugin},
		func(context.Context) error { return nil }); !errors.Is(err, ErrTaskOwnerFailed) {
		t.Fatalf("Go() after owner panic error = %v, want %v", err, ErrTaskOwnerFailed)
	}

	residuals, err := registry.Stop(context.Background())
	if err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskRegistryReportsPluginErrorAndFailsOnlyThatOwner(t *testing.T) {
	wantErr := errors.New("delivery failed")
	failures := make(chan TaskFailure, 1)
	registry := NewTaskRegistry(context.Background(), func(f TaskFailure) { failures <- f })

	if err := registry.Go(TaskSpec{Owner: "plugin/logger/r1", Criticality: TaskPlugin},
		func(context.Context) error { return wantErr }); err != nil {
		t.Fatal(err)
	}
	failure := <-failures
	if failure.Owner != "plugin/logger/r1" || !errors.Is(failure.Err, wantErr) || failure.PanicValue != nil ||
		failure.Stack != nil {
		t.Fatalf("failure = %#v", failure)
	}
	if err := registry.Go(TaskSpec{Owner: "plugin/logger/r1", Criticality: TaskPlugin},
		func(context.Context) error { return nil }); !errors.Is(err, ErrTaskOwnerFailed) {
		t.Fatalf("failed owner Go() error = %v, want %v", err, ErrTaskOwnerFailed)
	}
	done := make(chan struct{})
	if err := registry.Go(TaskSpec{Owner: "plugin/other/r1", Criticality: TaskPlugin}, func(context.Context) error {
		close(done)
		return nil
	}); err != nil {
		t.Fatalf("other owner Go() error = %v", err)
	}
	<-done
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskRegistryCoreErrorReportsWithoutDisablingOwner(t *testing.T) {
	wantErr := errors.New("core task stopped")
	failures := make(chan TaskFailure, 1)
	registry := NewTaskRegistry(context.Background(), func(f TaskFailure) { failures <- f })

	if err := registry.Go(TaskSpec{Owner: "runtime.probe", Criticality: TaskCore},
		func(context.Context) error { return wantErr }); err != nil {
		t.Fatal(err)
	}
	if failure := <-failures; failure.Owner != "runtime.probe" || !errors.Is(failure.Err, wantErr) {
		t.Fatalf("failure = %#v", failure)
	}
	done := make(chan struct{})
	if err := registry.Go(TaskSpec{Owner: "runtime.probe", Criticality: TaskCore}, func(context.Context) error {
		close(done)
		return nil
	}); err != nil {
		t.Fatalf("second Go() error = %v", err)
	}
	<-done
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskRegistryCorePanicIsFatal(t *testing.T) {
	if os.Getenv("APISIX_GO_TEST_CORE_TASK_PANIC") == "1" {
		registry := NewTaskRegistry(context.Background(), nil)
		if err := registry.Go(TaskSpec{Owner: "runtime.invariant", Criticality: TaskCore},
			func(context.Context) error { panic("core boom") }); err != nil {
			panic(err)
		}
		select {}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTaskRegistryCorePanicIsFatal$")
	cmd.Env = append(os.Environ(), "APISIX_GO_TEST_CORE_TASK_PANIC=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("core panic subprocess exited successfully")
	}
	if !strings.Contains(string(output), "core boom") {
		t.Fatalf("core panic output = %q", output)
	}
}

func TestTaskRegistryStopReportsSortedCancellationIgnoringOwners(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	for _, owner := range []string{"plugin/zeta/r1", "plugin/alpha/r1"} {
		if err := registry.Go(TaskSpec{Owner: owner, Criticality: TaskPlugin}, func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	<-started
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	residuals, err := registry.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(residuals, []TaskResidual{
		{Owner: "plugin/alpha/r1"},
		{Owner: "plugin/zeta/r1"},
	}) {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
	if got := registry.Active(); !reflect.DeepEqual(got, []string{"plugin/alpha/r1", "plugin/zeta/r1"}) {
		t.Fatalf("Active() = %v", got)
	}
	if err := registry.Go(TaskSpec{Owner: "plugin/new/r1", Criticality: TaskPlugin},
		func(context.Context) error { return nil }); !errors.Is(err, ErrTaskRegistryStopped) {
		t.Fatalf("Go() after Stop error = %v, want %v", err, ErrTaskRegistryStopped)
	}

	close(release)
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("second Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskRegistryStopCancelsAcceptedTask(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	finished := make(chan struct{})
	if err := registry.Go(
		TaskSpec{Owner: "plugin/cancellable/r1", Criticality: TaskPlugin},
		func(ctx context.Context) error {
			<-ctx.Done()
			close(finished)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}

	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
	<-finished
}

func TestTaskRegistryStopPrefersCompletedEmptyRegistryOverCanceledContext(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for range 100 {
		if residuals, err := registry.Stop(canceled); err != nil || len(residuals) != 0 {
			t.Fatalf("completed Stop() = (%v, %v)", residuals, err)
		}
	}
}

func TestTaskRegistryStopPrefersCompletedTaskWhenDeadlineIsAlsoReady(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	finished := make(chan struct{})
	if err := registry.Go(TaskSpec{Owner: "plugin/completed/r1", Criticality: TaskPlugin}, func(context.Context) error {
		close(finished)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-finished
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("initial Stop() = (%v, %v)", residuals, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	for range 100 {
		if residuals, err := registry.Stop(canceled); err != nil || len(residuals) != 0 {
			t.Fatalf("completed Stop() = (%v, %v)", residuals, err)
		}
	}
}

func TestTaskRegistryStopRetryWithCanceledContextSucceedsAfterCompletion(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	release := make(chan struct{})
	if err := registry.Go(TaskSpec{Owner: "plugin/retry/r1", Criticality: TaskPlugin}, func(context.Context) error {
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	timeout, timeoutCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer timeoutCancel()
	if residuals, err := registry.Stop(timeout); !errors.Is(err, context.DeadlineExceeded) ||
		!reflect.DeepEqual(residuals, []TaskResidual{{Owner: "plugin/retry/r1"}}) {
		t.Fatalf("first Stop() = (%v, %v)", residuals, err)
	}

	close(release)
	<-registry.waitDone
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		if residuals, err := registry.Stop(canceled); err != nil || len(residuals) != 0 {
			t.Fatalf("completed retry Stop() = (%v, %v)", residuals, err)
		}
	}
}

func TestTaskRegistryConcurrentStopsHonorIndependentDeadlines(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	release := make(chan struct{})
	started := make(chan struct{})
	if err := registry.Go(
		TaskSpec{Owner: "plugin/concurrent-stop/r1", Criticality: TaskPlugin},
		func(context.Context) error {
			close(started)
			<-release
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	<-started

	timedOut := make(chan struct {
		residuals []TaskResidual
		err       error
	}, 1)
	completed := make(chan struct {
		residuals []TaskResidual
		err       error
	}, 1)
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	go func() {
		residuals, err := registry.Stop(short)
		timedOut <- struct {
			residuals []TaskResidual
			err       error
		}{residuals: residuals, err: err}
	}()
	go func() {
		residuals, err := registry.Stop(context.Background())
		completed <- struct {
			residuals []TaskResidual
			err       error
		}{residuals: residuals, err: err}
	}()

	first := <-timedOut
	if !errors.Is(first.err, context.DeadlineExceeded) ||
		!reflect.DeepEqual(first.residuals, []TaskResidual{{Owner: "plugin/concurrent-stop/r1"}}) {
		t.Fatalf("short Stop() = (%v, %v)", first.residuals, first.err)
	}
	select {
	case result := <-completed:
		t.Fatalf("long Stop() returned before task completion: (%v, %v)", result.residuals, result.err)
	default:
	}
	close(release)
	result := <-completed
	if result.err != nil || len(result.residuals) != 0 {
		t.Fatalf("long Stop() = (%v, %v)", result.residuals, result.err)
	}
}

func TestTaskRegistryActiveSortsAndDeduplicatesOwners(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	for _, owner := range []string{"plugin/zeta/r1", "plugin/alpha/r1", "plugin/zeta/r1"} {
		if err := registry.Go(TaskSpec{Owner: owner, Criticality: TaskPlugin}, func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		<-started
	}
	if got := registry.Active(); !reflect.DeepEqual(got, []string{"plugin/alpha/r1", "plugin/zeta/r1"}) {
		t.Fatalf("Active() = %v", got)
	}
	close(release)
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskRegistryValidatesBeforeAdmission(t *testing.T) {
	registry := NewTaskRegistry(context.Background(), nil)
	tests := []struct {
		name string
		spec TaskSpec
		run  func(context.Context) error
		want error
	}{
		{
			name: "empty owner",
			spec: TaskSpec{Criticality: TaskPlugin},
			run:  func(context.Context) error { return nil },
			want: ErrTaskOwnerRequired,
		},
		{
			name: "unknown criticality",
			spec: TaskSpec{Owner: "plugin/test/r1", Criticality: "unknown"},
			run:  func(context.Context) error { return nil },
			want: ErrTaskCriticalityInvalid,
		},
		{
			name: "nil callback",
			spec: TaskSpec{Owner: "plugin/test/r1", Criticality: TaskPlugin},
			want: ErrTaskCallbackRequired,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := registry.Go(tt.spec, tt.run); !errors.Is(err, tt.want) {
				t.Fatalf("Go() error = %v, want %v", err, tt.want)
			}
		})
	}
	if got := registry.Active(); len(got) != 0 {
		t.Fatalf("Active() = %v", got)
	}
	if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("Stop() = (%v, %v)", residuals, err)
	}
}

func TestTaskRegistryConcurrentAdmissionAndStop(t *testing.T) {
	for range 100 {
		registry := NewTaskRegistry(context.Background(), nil)
		start := make(chan struct{})
		var callers sync.WaitGroup
		for range 8 {
			callers.Go(func() {
				<-start
				err := registry.Go(TaskSpec{Owner: "plugin/race/r1", Criticality: TaskPlugin},
					func(context.Context) error { return nil })
				if err != nil && !errors.Is(err, ErrTaskRegistryStopped) {
					t.Errorf("Go() error = %v", err)
				}
			})
		}
		close(start)
		if residuals, err := registry.Stop(context.Background()); err != nil || len(residuals) != 0 {
			t.Fatalf("Stop() = (%v, %v)", residuals, err)
		}
		callers.Wait()
	}
}
