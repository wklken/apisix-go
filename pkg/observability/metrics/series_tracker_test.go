package metrics

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricSeriesTrackerBoundsVectorChildrenAndReusesExistingSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_metric_series_tracker_total"},
		[]string{"route", "code"},
	)
	registry.MustRegister(vector)
	overflow := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_metric_series_tracker_overflow_total"})
	tracker := newMetricSeriesTracker(2, 2, 0, overflow, vector.DeleteLabelValues)
	record := func(labels ...string) {
		tracker.withSeries(labels, func(actual []string) {
			vector.WithLabelValues(actual...).Inc()
		})
	}

	record("route-a", "200")
	record("route-b", "201")
	record("route-a", "200")
	record("route-c", "202")

	if got := metricSeriesEntryCount(tracker); got != 2 {
		t.Fatalf("entryCount() = %d, want 2", got)
	}
	if got := gatheredMetricCountFromRegistry(t, registry); got != 3 {
		t.Fatalf("gathered metric children = %d, want 3", got)
	}
	if got := counterValue(t, vector.WithLabelValues("route-a", "200")); got != 2 {
		t.Fatalf("existing series value = %v, want 2", got)
	}
	if got := counterValue(t, vector.WithLabelValues(overflowLabel, overflowLabel)); got != 1 {
		t.Fatalf("overflow series value = %v, want 1", got)
	}
	if got := counterValue(t, overflow); got != 1 {
		t.Fatalf("overflow counter = %v, want 1", got)
	}
}

func TestMetricSeriesTrackerExpirationDeletesVectorChildAndReleasesCapacity(t *testing.T) {
	registry := prometheus.NewRegistry()
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_metric_series_expiration_total"},
		[]string{"route"},
	)
	registry.MustRegister(vector)
	now := time.Unix(100, 0)
	tracker := newMetricSeriesTracker(1, 1, time.Minute, nil, vector.DeleteLabelValues)
	tracker.now = func() time.Time { return now }
	record := func(route string) {
		tracker.withSeries([]string{route}, func(actual []string) {
			vector.WithLabelValues(actual...).Inc()
		})
	}

	record("route-a")
	now = now.Add(time.Minute)
	if got := tracker.expireSeries(now, 256); got != 1 {
		t.Fatalf("expireSeries() = %d, want 1", got)
	}
	if got := metricSeriesEntryCount(tracker); got != 0 {
		t.Fatalf("entryCount() after expiration = %d, want 0", got)
	}
	if got := gatheredMetricCountFromRegistry(t, registry); got != 0 {
		t.Fatalf("gathered children after expiration = %d, want 0", got)
	}

	record("route-b")
	if got := counterValue(t, vector.WithLabelValues("route-b")); got != 1 {
		t.Fatalf("replacement series value = %v, want 1", got)
	}
	if got := counterValue(t, vector.WithLabelValues(overflowLabel)); got != 0 {
		t.Fatalf("overflow series value = %v, want 0", got)
	}

	now = now.Add(time.Minute)
	tracker.expireSeries(now, 256)
	record("route-a")
	if got := counterValue(t, vector.WithLabelValues("route-a")); got != 1 {
		t.Fatalf("recreated series value = %v, want reset value 1", got)
	}
}

func TestMetricSeriesTrackerRechecksStaleExpirationCandidate(t *testing.T) {
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_metric_series_stale_candidate_total"},
		[]string{"route"},
	)
	now := time.Unix(200, 0)
	tracker := newMetricSeriesTracker(1, 1, time.Minute, nil, vector.DeleteLabelValues)
	tracker.now = func() time.Time { return now }
	tracker.withSeries([]string{"route-a"}, func(actual []string) {
		vector.WithLabelValues(actual...).Inc()
	})

	now = now.Add(time.Minute)
	candidates := tracker.expiredCandidates(now, 256)
	if len(candidates) != 1 {
		t.Fatalf("expiredCandidates() returned %d candidates, want 1", len(candidates))
	}
	tracker.withSeries([]string{"route-a"}, func(actual []string) {
		vector.WithLabelValues(actual...).Inc()
	})
	if got := tracker.deleteExpired(candidates, now); got != 0 {
		t.Fatalf("deleteExpired() = %d, want refreshed series preserved", got)
	}
	if got := counterValue(t, vector.WithLabelValues("route-a")); got != 2 {
		t.Fatalf("refreshed series value = %v, want 2", got)
	}
}

func TestMetricSeriesTrackerDisablesExpirationAtZero(t *testing.T) {
	deleteCalls := 0
	tracker := newMetricSeriesTracker(1, 1, 0, nil, func(...string) bool {
		deleteCalls++
		return true
	})
	tracker.withSeries([]string{"route-a"}, func([]string) {})

	if got := tracker.expireSeries(time.Now().Add(time.Hour), 256); got != 0 {
		t.Fatalf("expireSeries() = %d, want disabled", got)
	}
	if deleteCalls != 0 {
		t.Fatalf("delete callback calls = %d, want 0", deleteCalls)
	}
	if got := metricSeriesEntryCount(tracker); got != 1 {
		t.Fatalf("entryCount() = %d, want 1", got)
	}
}

func TestMetricSeriesTrackerPinsActiveSeriesUntilRelease(t *testing.T) {
	registry := prometheus.NewRegistry()
	vector := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_metric_series_active"},
		[]string{"route"},
	)
	registry.MustRegister(vector)
	now := time.Unix(300, 0)
	tracker := newMetricSeriesTracker(1, 1, time.Minute, nil, vector.DeleteLabelValues)
	tracker.now = func() time.Time { return now }
	release := tracker.acquireSeries(
		[]string{"route-a"},
		func(actual []string) { vector.WithLabelValues(actual...).Inc() },
		func(actual []string) { vector.WithLabelValues(actual...).Dec() },
	)

	now = now.Add(time.Minute)
	if got := tracker.expireSeries(now, 256); got != 0 {
		t.Fatalf("expireSeries() with active reference = %d, want 0", got)
	}
	if got := gaugeValue(t, vector.WithLabelValues("route-a")); got != 1 {
		t.Fatalf("active gauge = %v, want 1", got)
	}
	release()
	if got := gaugeValue(t, vector.WithLabelValues("route-a")); got != 0 {
		t.Fatalf("released gauge = %v, want 0", got)
	}

	now = now.Add(time.Minute)
	if got := tracker.expireSeries(now, 256); got != 1 {
		t.Fatalf("expireSeries() after release = %d, want 1", got)
	}
	if got := gatheredMetricCountFromRegistry(t, registry); got != 0 {
		t.Fatalf("gathered children after released expiration = %d, want 0", got)
	}
}

func TestMetricSeriesTrackerTupleKeyDoesNotCollide(t *testing.T) {
	tracker := newMetricSeriesTracker(2, 2, 0, nil, func(...string) bool { return true })
	tracker.withSeries([]string{"a", "bc"}, func([]string) {})
	tracker.withSeries([]string{"ab", "c"}, func([]string) {})
	if got := metricSeriesEntryCount(tracker); got != 2 {
		t.Fatalf("entryCount() = %d, want collision-free two entries", got)
	}
}

func TestMetricSeriesTrackerConcurrentUpdatesAndExpirationStayBounded(t *testing.T) {
	registry := prometheus.NewRegistry()
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_metric_series_concurrent_total"},
		[]string{"route"},
	)
	registry.MustRegister(vector)
	tracker := newMetricSeriesTracker(10, 1, time.Nanosecond, nil, vector.DeleteLabelValues)

	var wait sync.WaitGroup
	for worker := range 20 {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := range 100 {
				labels := []string{fmt.Sprintf("route-%d-%d", worker, iteration%20)}
				tracker.withSeries(labels, func(actual []string) {
					vector.WithLabelValues(actual...).Inc()
				})
				tracker.expireSeries(time.Now().Add(time.Hour), 3)
			}
		}(worker)
	}
	wait.Wait()

	if got := metricSeriesEntryCount(tracker); got > 10 {
		t.Fatalf("entryCount() = %d, want at most 10", got)
	}
	if got := gatheredMetricCountFromRegistry(t, registry); got > 11 {
		t.Fatalf("gathered children = %d, want at most 11", got)
	}
}

func TestMetricSeriesTrackerConcurrentRefreshAndExpirationKeepTrackedChild(t *testing.T) {
	registry := prometheus.NewRegistry()
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_metric_series_refresh_expiration_total"},
		[]string{"route"},
	)
	registry.MustRegister(vector)
	tracker := newMetricSeriesTracker(1, 1, time.Second, nil, vector.DeleteLabelValues)
	var clock atomic.Int64
	clock.Store(time.Second.Nanoseconds())
	tracker.now = func() time.Time { return time.Unix(0, clock.Load()) }
	record := func() {
		tracker.withSeries([]string{"route-a"}, func(actual []string) {
			vector.WithLabelValues(actual...).Inc()
		})
	}
	record()

	for range 100 {
		scanAt := time.Unix(0, clock.Add(time.Second.Nanoseconds()))
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			record()
		}()
		go func() {
			defer wait.Done()
			<-start
			tracker.expireSeries(scanAt, 1)
		}()
		close(start)
		wait.Wait()

		if got := metricSeriesEntryCount(tracker); got != 1 {
			t.Fatalf("entry count after refresh/expiration race = %d, want 1", got)
		}
		if got := gatheredMetricCountFromRegistry(t, registry); got != 1 {
			t.Fatalf("vector children after refresh/expiration race = %d, want 1", got)
		}
	}
}

func TestMetricSeriesTrackerConcurrentReleaseAndExpirationKeepTrackedGaugeChild(t *testing.T) {
	registry := prometheus.NewRegistry()
	vector := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_metric_series_release_expiration"},
		[]string{"route"},
	)
	registry.MustRegister(vector)
	tracker := newMetricSeriesTracker(1, 1, time.Second, nil, vector.DeleteLabelValues)
	var clock atomic.Int64
	clock.Store(time.Second.Nanoseconds())
	tracker.now = func() time.Time { return time.Unix(0, clock.Load()) }

	for range 100 {
		release := tracker.acquireSeries(
			[]string{"route-a"},
			func(actual []string) { vector.WithLabelValues(actual...).Inc() },
			func(actual []string) { vector.WithLabelValues(actual...).Dec() },
		)
		scanAt := time.Unix(0, clock.Add(time.Second.Nanoseconds()))
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			release()
		}()
		go func() {
			defer wait.Done()
			<-start
			tracker.expireSeries(scanAt, 1)
		}()
		close(start)
		wait.Wait()

		if got := metricSeriesEntryCount(tracker); got != 1 {
			t.Fatalf("entry count after release/expiration race = %d, want 1", got)
		}
		if got := gatheredMetricCountFromRegistry(t, registry); got != 1 {
			t.Fatalf("vector children after release/expiration race = %d, want 1", got)
		}
		if got := gaugeValue(t, vector.WithLabelValues("route-a")); got != 0 {
			t.Fatalf("gauge after release/expiration race = %v, want 0", got)
		}
	}
}

func TestMetricSeriesTrackerOverflowUpdateDoesNotBlockExistingSeries(t *testing.T) {
	tracker := newMetricSeriesTracker(1, 1, 0, nil, nil)
	tracker.withSeries([]string{"route-a"}, func([]string) {})

	overflowStarted := make(chan struct{})
	releaseOverflow := make(chan struct{})
	overflowDone := make(chan struct{})
	go func() {
		defer close(overflowDone)
		tracker.withSeries([]string{"route-b"}, func([]string) {
			close(overflowStarted)
			<-releaseOverflow
		})
	}()
	<-overflowStarted

	existingDone := make(chan struct{})
	go tracker.withSeries([]string{"route-a"}, func([]string) { close(existingDone) })
	select {
	case <-existingDone:
		close(releaseOverflow)
		<-overflowDone
	case <-time.After(time.Second):
		close(releaseOverflow)
		<-overflowDone
		<-existingDone
		t.Fatal("overflow metric update held the tracker lock and blocked an existing series")
	}
}

func TestMetricSeriesTrackerOverflowAcquireDoesNotBlockExistingSeries(t *testing.T) {
	tracker := newMetricSeriesTracker(1, 1, 0, nil, nil)
	firstRelease := tracker.acquireSeries([]string{"route-a"}, func([]string) {}, func([]string) {})
	firstRelease()

	overflowStarted := make(chan struct{})
	releaseOverflow := make(chan struct{})
	overflowRelease := make(chan func(), 1)
	go func() {
		overflowRelease <- tracker.acquireSeries(
			[]string{"route-b"},
			func([]string) {
				close(overflowStarted)
				<-releaseOverflow
			},
			func([]string) {},
		)
	}()
	<-overflowStarted

	existingRelease := make(chan func(), 1)
	go func() {
		existingRelease <- tracker.acquireSeries([]string{"route-a"}, func([]string) {}, func([]string) {})
	}()
	select {
	case release := <-existingRelease:
		release()
		close(releaseOverflow)
		(<-overflowRelease)()
	case <-time.After(time.Second):
		close(releaseOverflow)
		(<-overflowRelease)()
		(<-existingRelease)()
		t.Fatal("overflow gauge acquire held the tracker lock and blocked an existing series")
	}
}

func gatheredMetricCountFromRegistry(t *testing.T, registry *prometheus.Registry) int {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	count := 0
	for _, family := range families {
		count += len(family.Metric)
	}
	return count
}

func metricSeriesEntryCount(tracker *metricSeriesTracker) int {
	if tracker == nil {
		return 0
	}
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	return len(tracker.entries)
}
