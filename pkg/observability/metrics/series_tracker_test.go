package metrics

import (
	"fmt"
	"sync"
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

	if got := tracker.entryCount(); got != 2 {
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
	if got := tracker.entryCount(); got != 0 {
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
	if got := tracker.entryCount(); got != 1 {
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
	if got := tracker.entryCount(); got != 2 {
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

	if got := tracker.entryCount(); got > 10 {
		t.Fatalf("entryCount() = %d, want at most 10", got)
	}
	if got := gatheredMetricCountFromRegistry(t, registry); got > 11 {
		t.Fatalf("gathered children = %d, want at most 11", got)
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
