package metrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExpirationRuntimeCalculatesBoundedScanInterval(t *testing.T) {
	tests := []struct {
		name     string
		expires  []time.Duration
		expected time.Duration
	}{
		{name: "disabled", expires: []time.Duration{0, 0}, expected: 0},
		{name: "minimum one second", expires: []time.Duration{time.Second, 10 * time.Second}, expected: time.Second},
		{
			name:     "half minimum expiration",
			expires:  []time.Duration{10 * time.Second, time.Minute},
			expected: 5 * time.Second,
		},
		{name: "maximum one minute", expires: []time.Duration{5 * time.Minute}, expected: time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trackers := make([]*metricSeriesTracker, 0, len(test.expires))
			for _, expire := range test.expires {
				trackers = append(trackers, newMetricSeriesTracker(1, 1, expire, nil, nil))
			}
			runtime := newExpirationRuntime(trackers...)
			if runtime.interval != test.expected {
				t.Fatalf("interval = %s, want %s", runtime.interval, test.expected)
			}
		})
	}
}

func TestExpirationRuntimeScanDeletesAtMostBatchPerFamily(t *testing.T) {
	now := time.Unix(3000, 0)
	expiring := newMetricSeriesTracker(300, 1, time.Minute, nil, nil)
	expiring.now = func() time.Time { return now }
	disabled := newMetricSeriesTracker(1, 1, 0, nil, nil)
	disabled.now = func() time.Time { return now }
	for index := range 300 {
		expiring.withSeries([]string{fmt.Sprintf("route-%d", index)}, func([]string) {})
	}
	disabled.withSeries([]string{"disabled"}, func([]string) {})
	runtime := newExpirationRuntime(expiring, disabled)

	runtime.scan(now.Add(time.Minute))

	if got := metricSeriesEntryCount(expiring); got != 44 {
		t.Fatalf("expiring entryCount() = %d, want 44 after one 256-entry batch", got)
	}
	if got := metricSeriesEntryCount(disabled); got != 1 {
		t.Fatalf("disabled entryCount() = %d, want 1", got)
	}
}

func TestExpirationRuntimeStartsOnceStopsIdempotentlyAndRestarts(t *testing.T) {
	tracker := newMetricSeriesTracker(1, 1, time.Second, nil, nil)
	runtime := newExpirationRuntime(tracker)
	var tickersMu sync.Mutex
	var tickers []*fakeExpirationTicker
	runtime.newTicker = func(interval time.Duration) expirationTicker {
		if interval != time.Second {
			t.Fatalf("newTicker interval = %s, want 1s", interval)
		}
		ticker := newFakeExpirationTicker()
		tickersMu.Lock()
		tickers = append(tickers, ticker)
		tickersMu.Unlock()
		return ticker
	}

	stop, err := runtime.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if stop == nil {
		t.Fatal("Start() stop = nil")
	}
	if _, err := runtime.Start(context.Background()); !errors.Is(err, errExpirationRuntimeRunning) {
		t.Fatalf("second Start() error = %v, want errExpirationRuntimeRunning", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := stop(waitCtx); err != nil {
		t.Fatalf("stop() error = %v", err)
	}
	if err := stop(waitCtx); err != nil {
		t.Fatalf("second stop() error = %v", err)
	}
	tickersMu.Lock()
	firstTicker := tickers[0]
	tickersMu.Unlock()
	if !firstTicker.wasStopped() {
		t.Fatal("first ticker was not stopped")
	}

	restartedStop, err := runtime.Start(context.Background())
	if err != nil {
		t.Fatalf("restart Start() error = %v", err)
	}
	if err := restartedStop(waitCtx); err != nil {
		t.Fatalf("restarted stop() error = %v", err)
	}
}

func TestExpirationRuntimeDoesNotStartWhenEveryTrackerIsDisabled(t *testing.T) {
	runtime := newExpirationRuntime(newMetricSeriesTracker(1, 1, 0, nil, nil))
	tickerCalls := 0
	runtime.newTicker = func(time.Duration) expirationTicker {
		tickerCalls++
		return newFakeExpirationTicker()
	}

	stop, err := runtime.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if stop != nil {
		t.Fatal("Start() stop is non-nil for disabled runtime")
	}
	if tickerCalls != 0 {
		t.Fatalf("newTicker calls = %d, want 0", tickerCalls)
	}
}

type fakeExpirationTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newFakeExpirationTicker() *fakeExpirationTicker {
	return &fakeExpirationTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
}

func (t *fakeExpirationTicker) Chan() <-chan time.Time {
	return t.ticks
}

func (t *fakeExpirationTicker) Stop() {
	t.once.Do(func() { close(t.stopped) })
}

func (t *fakeExpirationTicker) wasStopped() bool {
	select {
	case <-t.stopped:
		return true
	default:
		return false
	}
}
