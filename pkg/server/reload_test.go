package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/store"
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
	const maxWait = 200 * time.Millisecond
	events := make(chan struct{}, 3)
	reloads := make(chan struct{}, 2)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, maxWait, func() { reloads <- struct{}{} })
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
	const maxWait = 200 * time.Millisecond
	events := make(chan struct{}, 1)
	firstReloadStarted := make(chan struct{})
	releaseFirstReload := make(chan struct{})
	secondReload := make(chan struct{})
	var reloadCount atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, maxWait, func() {
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
	const maxWait = 2 * time.Second
	events := make(chan struct{}, 1)
	reloaded := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, maxWait, func() { reloaded <- struct{}{} })
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

func TestReloadSchedulerContinuousEventsReloadAtMaximumWait(t *testing.T) {
	const quiet = 40 * time.Millisecond
	const maxWait = 100 * time.Millisecond
	events := make(chan struct{}, 1)
	reloads := make(chan time.Time, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, maxWait, func() { reloads <- time.Now() })
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	started := time.Now()
	stopEvents := make(chan struct{})
	defer close(stopEvents)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case events <- struct{}{}:
				default:
				}
			case <-stopEvents:
				return
			}
		}
	}()

	select {
	case reloadedAt := <-reloads:
		if elapsed := reloadedAt.Sub(started); elapsed > maxWait+75*time.Millisecond {
			t.Fatalf("continuous events delayed reload for %s, want at most %s", elapsed, maxWait+75*time.Millisecond)
		}
	case <-time.After(maxWait + 150*time.Millisecond):
		t.Fatal("continuous events starved reload past maximum wait")
	}
}

func TestBuilderResourceEtcdEventsScheduleHTTPReload(t *testing.T) {
	const quiet = 10 * time.Millisecond
	const maxWait = 100 * time.Millisecond
	events := make(chan struct{}, 1)
	reloads := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadScheduler(ctx, events, quiet, maxWait, func() { reloads <- struct{}{} })
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	for _, key := range []string{
		"/apisix/global_rules/global-1",
		"/apisix/plugin_configs/config-1",
	} {
		handleStoreEventUpdate(
			&store.Event{Key: []byte(key)},
			func() { events <- struct{}{} },
			func() { t.Fatalf("HTTP builder resource %q scheduled a stream reload", key) },
		)
		waitForReload(t, reloads)
	}
}

func TestFetchAndSyncInitialEtcdConfigWaitsForSuccessfulFetch(t *testing.T) {
	var calls []string
	err := fetchAndSyncInitialEtcdConfig(
		func() error {
			calls = append(calls, "fetch")
			return nil
		},
		func() {
			calls = append(calls, "sync")
		},
	)
	if err != nil {
		t.Fatalf("fetchAndSyncInitialEtcdConfig() error = %v", err)
	}
	if got, want := calls, []string{"fetch", "sync"}; !equalStrings(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}

	calls = nil
	wantErr := context.Canceled
	err = fetchAndSyncInitialEtcdConfig(
		func() error {
			calls = append(calls, "fetch")
			return wantErr
		},
		func() {
			calls = append(calls, "sync")
		},
	)
	if err != wantErr {
		t.Fatalf("fetchAndSyncInitialEtcdConfig() error = %v, want %v", err, wantErr)
	}
	if got, want := calls, []string{"fetch"}; !equalStrings(got, want) {
		t.Fatalf("failed fetch calls = %v, want %v", got, want)
	}
}

func TestReloadRetainsExistingHandlerForUndecodableSnapshot(t *testing.T) {
	events := make(chan *store.Event)
	storage := store.NewStore(t.TempDir()+"/reload.db", events)
	storage.Start()
	t.Cleanup(storage.Stop)

	put := func(bucket string, id string, value []byte) {
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/" + bucket + "/" + id)
		event.Value = value
		events <- event
	}
	remove := func(bucket string, id string) {
		event := store.NewEvent()
		event.Type = store.EventTypeDelete
		event.Key = []byte("/apisix/" + bucket + "/" + id)
		events <- event
	}
	put("routes", "valid-route", []byte(`{"id":"valid-route","uri":"/valid"}`))
	put("routes", "invalid-route", []byte(`{"id":"invalid-route","uri":"/invalid","plugins":[]}`))
	storage.Sync()

	oldHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Handler", "last-good")
		w.Header().Set("X-Global-Security", "enforced")
		w.WriteHeader(http.StatusUnauthorized)
	})
	server := &Server{
		addr:    "127.0.0.1:9080",
		storage: storage,
		routes:  newRouteHandler(oldHandler, nil),
	}

	server.reload(context.Background())
	response := httptest.NewRecorder()
	server.routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/valid", nil))
	if got, want := response.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status after invalid reload = %d, want retained handler status %d", got, want)
	}
	if got, want := response.Header().Get("X-Handler"), "last-good"; got != want {
		t.Fatalf("handler marker after invalid reload = %q, want %q", got, want)
	}

	remove("routes", "invalid-route")
	put("global_rules", "valid-global", []byte(`{"id":"valid-global","plugins":{}}`))
	put("global_rules", "invalid-global", []byte(`{"id":"invalid-global","plugins":[]}`))
	storage.Sync()

	server.reload(context.Background())
	response = httptest.NewRecorder()
	server.routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/valid", nil))
	if got, want := response.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status after invalid global-rule reload = %d, want retained handler status %d", got, want)
	}
	if got, want := response.Header().Get("X-Global-Security"), "enforced"; got != want {
		t.Fatalf("global security marker after invalid reload = %q, want %q", got, want)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
