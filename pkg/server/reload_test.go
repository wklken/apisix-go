package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/store"
	bolt "go.etcd.io/bbolt"
)

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
		func() error {
			calls = append(calls, "sync")
			return nil
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
		func() error {
			calls = append(calls, "sync")
			return nil
		},
	)
	if err != wantErr {
		t.Fatalf("fetchAndSyncInitialEtcdConfig() error = %v, want %v", err, wantErr)
	}
	if got, want := calls, []string{"fetch"}; !equalStrings(got, want) {
		t.Fatalf("failed fetch calls = %v, want %v", got, want)
	}
}

func TestFetchAndSyncInitialEtcdConfigPropagatesSyncError(t *testing.T) {
	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_initial_sync_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_initial_sync_ready"})
	t.Cleanup(func() { metrics.ConfigApplyFailures, metrics.ConfigApplyReady = oldFailures, oldReady })

	wantErr := context.Canceled
	err := fetchAndSyncInitialEtcdConfig(
		func() error { return nil },
		func() error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("fetchAndSyncInitialEtcdConfig() error = %v, want %v", err, wantErr)
	}
	metric := &dto.Metric{}
	if err := metrics.ConfigApplyFailures.Write(metric); err != nil {
		t.Fatalf("write sync failure metric: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("sync failure count = %v, want 1", got)
	}
}

func TestApplyStandaloneSnapshotSkipsReloadWhenSyncFails(t *testing.T) {
	wantErr := context.Canceled
	var routes, streams int
	err := applyStandaloneSnapshot(
		config.StandaloneReloadResult{
			ChangedHTTPRouteBuckets: []string{"routes"},
			ChangedStreamBuckets:    []string{"stream_routes"},
		},
		nil,
		func() error { return wantErr },
		func() error { routes++; return nil },
		func() error { streams++; return nil },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("applyStandaloneSnapshot() error = %v, want %v", err, wantErr)
	}
	if routes != 0 || streams != 0 {
		t.Fatalf("reloads = routes:%d streams:%d, want none", routes, streams)
	}
}

func TestReloadPublishesValidGenerationForLegacyMalformedRowsAndKeepsReadinessBlocked(t *testing.T) {
	oldFailures, oldReady, oldQuarantine := metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_legacy_reload_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_legacy_reload_ready"})
	metrics.ConfigApplyQuarantined = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_legacy_reload_quarantine"})
	t.Cleanup(func() {
		metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.ConfigApplyQuarantined = oldFailures, oldReady, oldQuarantine
	})
	storage, events := openLegacyReloadStore(t, map[string]map[string][]byte{
		"routes": {
			"valid-route":   []byte(`{"id":"valid-route","uri":"/valid"}`),
			"invalid-route": []byte(`{"id":"invalid-route","uri":"/invalid","plugins":[]}`),
		},
		"global_rules": {
			"valid-global":   []byte(`{"id":"valid-global","plugins":{}}`),
			"invalid-global": []byte(`{"id":"invalid-global","plugins":[]}`),
		},
	})

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

	if err := server.reload(context.Background()); err != nil {
		t.Fatalf("reload() error = %v, want valid route generation", err)
	}
	response := httptest.NewRecorder()
	server.routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/valid", nil))
	if got := response.Header().Get("X-Handler"); got != "" {
		t.Fatalf("handler marker after legacy-row reload = %q, want newly published handler", got)
	}
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	if got := configApplyGaugeValue(t, metrics.ConfigApplyQuarantined); got != 2 {
		t.Fatalf("legacy quarantine gauge = %v, want two malformed rows", got)
	}
	if metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = true while legacy resources remain quarantined")
	}

	metrics.RecordConfigApplyQuarantine(4)
	replacement := store.NewEvent()
	replacement.Type = store.EventTypePut
	replacement.Key = []byte("/apisix/routes/invalid-route")
	replacement.Value = []byte(`{"id":"invalid-route","uri":"/recovered"}`)
	events <- replacement
	deletion := store.NewEvent()
	deletion.Type = store.EventTypeDelete
	deletion.Key = []byte("/apisix/global_rules/invalid-global")
	events <- deletion
	if err := storage.Sync(); err != nil {
		t.Fatalf("recover legacy rows: %v", err)
	}
	if err := server.reload(context.Background()); err != nil {
		t.Fatalf("reload() after legacy recovery = %v", err)
	}
	if got := configApplyGaugeValue(t, metrics.ConfigApplyQuarantined); got != 4 {
		t.Fatalf("quarantine gauge after store recovery = %v, want independent provider count 4", got)
	}
	if metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = true while provider quarantine remains")
	}
	metrics.RecordConfigApplyQuarantine(0)
	if !metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = false after provider and store quarantines clear")
	}
}

func openLegacyReloadStore(t *testing.T, seed map[string]map[string][]byte) (*store.Store, chan *store.Event) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy-reload.db")
	initial, err := store.Open(path, make(chan *store.Event, 1))
	if err != nil {
		t.Fatalf("open initial legacy store: %v", err)
	}
	if err := initial.Stop(); err != nil {
		t.Fatalf("stop initial legacy store: %v", err)
	}
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for bucketName, entries := range seed {
			bucket := tx.Bucket([]byte(bucketName))
			for id, value := range entries {
				if err := bucket.Put([]byte(id), value); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		t.Fatalf("seed legacy database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
	events := make(chan *store.Event, 1)
	storage, err := store.Open(path, events)
	if err != nil {
		t.Fatalf("reopen legacy store: %v", err)
	}
	storage.Start()
	previous := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previous)
		if err := storage.Stop(); err != nil {
			t.Errorf("stop legacy reload store: %v", err)
		}
	})
	return storage, events
}

func TestReloadRetainsLastGoodHandlerAndReportsDisabledPlugin(t *testing.T) {
	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	config.GlobalConfig = &config.Config{}

	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/reload-disabled-plugin.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	t.Cleanup(func() { _ = storage.Stop() })

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/routes/disabled-route")
	// Use an unregistered plugin as a deterministic route-build failure on the
	// pre-allowlist baseline; the combined WU-02 tests cover a known disabled
	// factory with the same reload transaction.
	event.Value = []byte(`{"id":"disabled-route","uri":"/disabled","plugins":{"disabled-test-plugin":{}}}`)
	events <- event
	if err := storage.Sync(); err != nil {
		t.Fatalf("store sync: %v", err)
	}

	oldHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Handler", "last-good")
		w.WriteHeader(http.StatusUnauthorized)
	})
	var oldStops atomic.Int32
	server := &Server{
		addr:    "127.0.0.1:9080",
		storage: storage,
		routes:  newRouteHandler(oldHandler, func() { oldStops.Add(1) }),
	}

	err = server.reload(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disabled-test-plugin") {
		t.Fatalf("reload() error = %v, want disabled-test-plugin error", err)
	}
	response := httptest.NewRecorder()
	server.routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/any", nil))
	if got, want := response.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status after failed reload = %d, want retained handler status %d", got, want)
	}
	if got, want := response.Header().Get("X-Handler"), "last-good"; got != want {
		t.Fatalf("handler marker after failed reload = %q, want %q", got, want)
	}
	if got := oldStops.Load(); got != 0 {
		t.Fatalf("last-good handler stopper calls = %d, want 0", got)
	}
}

func TestAcknowledgedHTTPRouteWaitsForSuccessfulPublication(t *testing.T) {
	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	config.GlobalConfig = &config.Config{}

	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/acknowledged-http.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	t.Cleanup(func() { _ = storage.Stop() })

	server := &Server{
		addr:            "127.0.0.1:9080",
		storage:         storage,
		routes:          newRouteHandler(http.NotFoundHandler(), nil),
		reloadEventChan: make(chan struct{}, 1),
	}
	server.registerAcknowledgedStoreUpdateHook(context.Background())

	event := store.NewAcknowledgedEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/routes/disabled-route")
	event.Value = []byte(`{"id":"disabled-route","uri":"/disabled","plugins":{"disabled-test-plugin":{}}}`)
	events <- event
	err = event.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disabled-test-plugin") {
		t.Fatalf("acknowledged route error = %v, want publication failure", err)
	}
}

func TestAcknowledgedHTTPPublicationRunsOncePerStoreGeneration(t *testing.T) {
	server := &Server{}
	var reloads atomic.Int32
	reload := func() error {
		reloads.Add(1)
		return nil
	}

	if err := server.publishAcknowledgedHTTPGeneration(7, reload); err != nil {
		t.Fatalf("first publication: %v", err)
	}
	if err := server.publishAcknowledgedHTTPGeneration(7, reload); err != nil {
		t.Fatalf("duplicate bucket publication: %v", err)
	}
	if got := reloads.Load(); got != 1 {
		t.Fatalf("reload calls for one store generation = %d, want 1", got)
	}
	if err := server.publishAcknowledgedHTTPGeneration(8, reload); err != nil {
		t.Fatalf("next generation publication: %v", err)
	}
	if got := reloads.Load(); got != 2 {
		t.Fatalf("reload calls after next store generation = %d, want 2", got)
	}

	wantErr := errors.New("publication failed")
	failedServer := &Server{}
	var failedReloads atomic.Int32
	failedReload := func() error {
		failedReloads.Add(1)
		return wantErr
	}
	for range 2 {
		if err := failedServer.publishAcknowledgedHTTPGeneration(9, failedReload); !errors.Is(err, wantErr) {
			t.Fatalf("cached publication error = %v, want %v", err, wantErr)
		}
	}
	if got := failedReloads.Load(); got != 1 {
		t.Fatalf("failed reload calls for one store generation = %d, want 1", got)
	}
}

func TestAcknowledgedHTTPRouteRejectsCanceledPublicationContext(t *testing.T) {
	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	config.GlobalConfig = &config.Config{}

	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/acknowledged-http-canceled.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })
	server := &Server{
		storage: storage,
		routes:  newRouteHandler(http.NotFoundHandler(), nil),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server.registerAcknowledgedStoreUpdateHook(ctx)

	event := store.NewAcknowledgedEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/routes/canceled-route")
	event.Value = []byte(`{"id":"canceled-route","uri":"/canceled"}`)
	events <- event
	if err := event.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("acknowledged route error = %v, want context.Canceled", err)
	}
}

func TestAcknowledgedHTTPPublicationRejectsContextCanceledWhileWaitingForReload(t *testing.T) {
	server := &Server{}
	server.reloadMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.reloadAcknowledgedHTTP(ctx)
	}()
	select {
	case err := <-done:
		server.reloadMu.Unlock()
		t.Fatalf("publication returned before reload lock was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	server.reloadMu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("acknowledged publication error = %v, want context.Canceled", err)
	}
}

func TestReloadSchedulerRecordsConfigApplyReadiness(t *testing.T) {
	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	config.GlobalConfig = &config.Config{}

	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_reload_config_apply_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_reload_config_apply_ready",
	})
	t.Cleanup(func() { metrics.ConfigApplyFailures, metrics.ConfigApplyReady = oldFailures, oldReady })

	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/reload-scheduler.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	t.Cleanup(func() { _ = storage.Stop() })

	putRoute := func(value []byte) {
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/routes/reload-route")
		event.Value = value
		events <- event
		if err := storage.Sync(); err != nil {
			t.Fatalf("store sync: %v", err)
		}
	}
	putRoute([]byte(`{"id":"reload-route","uri":"/reload","plugins":{"disabled-test-plugin":{}}}`))

	server := &Server{
		addr:            "127.0.0.1:9080",
		storage:         storage,
		routes:          newRouteHandler(http.NotFoundHandler(), nil),
		reloadEventChan: make(chan struct{}, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.listenReloadEvent(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	server.SendReloadEvent()
	waitForConfigApplyMetric(t, metrics.ConfigApplyFailures, 1)
	if got := configApplyGaugeValue(t, metrics.ConfigApplyReady); got != 0 {
		t.Fatalf("ready after failed reload = %v, want 0", got)
	}
	if got := configApplyCounterValue(t, metrics.ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count after failed reload = %v, want 1", got)
	}
	metrics.RecordConfigApplySuccess()
	if got := configApplyGaugeValue(t, metrics.ConfigApplyReady); got != 0 {
		t.Fatalf("ready after provider-only success = %v, want 0", got)
	}

	putRoute([]byte(`{"id":"reload-route","uri":"/reload"}`))
	server.SendReloadEvent()
	waitForConfigApplyMetric(t, metrics.ConfigApplyReady, 1)
	if got := configApplyCounterValue(t, metrics.ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count after successful reload = %v, want 1", got)
	}
}

func TestReloadSchedulerCancellationDoesNotRecordConfigFailure(t *testing.T) {
	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_cancelled_reload_config_apply_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_cancelled_reload_config_apply_ready",
	})
	metrics.ConfigApplyReady.Set(1)
	t.Cleanup(func() { metrics.ConfigApplyFailures, metrics.ConfigApplyReady = oldFailures, oldReady })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := &Server{reloadEventChan: make(chan struct{}, 1)}
	server.SendReloadEvent()
	server.listenReloadEvent(ctx)

	if got := configApplyCounterValue(t, metrics.ConfigApplyFailures); got != 0 {
		t.Fatalf("failure count after cancelled scheduler = %v, want 0", got)
	}
	if got := configApplyGaugeValue(t, metrics.ConfigApplyReady); got != 1 {
		t.Fatalf("ready after cancelled scheduler = %v, want unchanged 1", got)
	}
}

func TestReloadSkipsWhenContextCancelled(t *testing.T) {
	oldHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handler", "last-good")
		w.WriteHeader(http.StatusUnauthorized)
	})
	server := &Server{
		addr:    "127.0.0.1:9080",
		routes:  newRouteHandler(oldHandler, nil),
		storage: &store.Store{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = server.reload(ctx)

	response := httptest.NewRecorder()
	server.routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/any", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status after cancelled reload = %d, want the retained handler", response.Code)
	}
	if got := response.Header().Get("X-Handler"); got != "last-good" {
		t.Fatalf("handler marker after cancelled reload = %q, want last-good", got)
	}
}

func TestReloadConcurrentRebuildsKeepServingTraffic(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/concurrent-reload.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	server := &Server{
		addr:    "127.0.0.1:9080",
		storage: storage,
		routes:  newRouteHandler(nil, nil),
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = server.reload(context.Background())
				}
			}
		})
	}
	for range 500 {
		response := httptest.NewRecorder()
		server.routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/any", nil))
		if response.Code == http.StatusInternalServerError {
			t.Fatalf("request failed with %d while reloads were in flight", response.Code)
		}
	}
	close(stop)
	wg.Wait()
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

func waitForConfigApplyMetric(t *testing.T, metric prometheus.Collector, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var got float64
		switch typed := metric.(type) {
		case prometheus.Gauge:
			got = configApplyGaugeValue(t, typed)
		case prometheus.Counter:
			got = configApplyCounterValue(t, typed)
		default:
			t.Fatalf("unsupported config-apply metric type %T", metric)
		}
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for config-apply metric %T = %v", metric, want)
}

func configApplyCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write config-apply counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func configApplyGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write config-apply gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}
