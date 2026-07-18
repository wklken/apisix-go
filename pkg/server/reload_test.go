package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestReconcileInitialReloadEventDropsRepresentedSentinel(t *testing.T) {
	events := make(chan struct{}, 1)
	events <- struct{}{}
	var generation atomic.Uint64
	generation.Store(1)

	reconcileInitialReloadEvent(events, 1, generation.Load)

	if got := len(events); got != 0 {
		t.Fatalf("reload queue length = %d, want 0 after initial build", got)
	}
}

func TestReconcileInitialReloadEventPreservesUpdateDuringBuild(t *testing.T) {
	events := make(chan struct{}, 1)
	events <- struct{}{}
	var generation atomic.Uint64
	generation.Store(1)

	// The update increments its generation before its enqueue coalesces with
	// the initial sentinel, matching the server's store hook ordering.
	generation.Add(1)

	reconcileInitialReloadEvent(events, 1, generation.Load)

	if got := len(events); got != 1 {
		t.Fatalf("reload queue length = %d, want 1 for the update during build", got)
	}
}

func TestReloadSchedulerCoalescesBurstAfterQuietPeriod(t *testing.T) {
	const quiet = 20 * time.Millisecond
	events := make(chan struct{}, 3)
	reloads := make(chan struct{}, 2)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, func() { reloads <- struct{}{} })
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	events <- struct{}{}
	events <- struct{}{}
	events <- struct{}{}
	waitForReload(t, reloads)
	assertNoReload(t, reloads, 2*quiet)
}

func TestReloadSchedulerSchedulesEventArrivingDuringReload(t *testing.T) {
	const quiet = 20 * time.Millisecond
	events := make(chan struct{}, 1)
	firstReloadStarted := make(chan struct{})
	releaseFirstReload := make(chan struct{})
	secondReload := make(chan struct{})
	var reloadCount atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, func() {
			if reloadCount.Add(1) == 1 {
				close(firstReloadStarted)
				<-releaseFirstReload
				return
			}
			close(secondReload)
		})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-releaseFirstReload:
		default:
			close(releaseFirstReload)
		}
		<-done
	})

	events <- struct{}{}
	waitForReload(t, firstReloadStarted)
	events <- struct{}{}
	close(releaseFirstReload)
	waitForReload(t, secondReload)
	if got := reloadCount.Load(); got != 2 {
		t.Fatalf("reload count = %d, want 2", got)
	}
}

func TestReloadSchedulerCancellationStopsPendingTimer(t *testing.T) {
	const quiet = time.Second
	events := make(chan struct{}, 1)
	reloaded := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, func() { reloaded <- struct{}{} })
		close(done)
	}()

	events <- struct{}{}
	waitForReloadQueueDrain(t, events)
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("reload scheduler did not exit after context cancellation")
	}
	select {
	case <-reloaded:
		t.Fatal("pending reload ran after context cancellation")
	default:
	}
}

func waitForReloadQueueDrain(t *testing.T, events <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for len(events) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("reload scheduler did not consume the queued event")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForReload(t *testing.T, reloaded <-chan struct{}) {
	t.Helper()
	select {
	case <-reloaded:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for reload")
	}
}

func assertNoReload(t *testing.T, reloaded <-chan struct{}, duration time.Duration) {
	t.Helper()
	select {
	case <-reloaded:
		t.Fatal("unexpected extra reload")
	case <-time.After(duration):
	}
}
