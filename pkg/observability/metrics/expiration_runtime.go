package metrics

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	minExpirationScanInterval = time.Second
	maxExpirationScanInterval = time.Minute
	expirationDeleteBatchSize = 256
)

var errExpirationRuntimeRunning = errors.New("prometheus metric expiration runtime is already running")

type expirationTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type realExpirationTicker struct {
	ticker *time.Ticker
}

func (t *realExpirationTicker) Chan() <-chan time.Time {
	return t.ticker.C
}

func (t *realExpirationTicker) Stop() {
	t.ticker.Stop()
}

type expirationRuntime struct {
	mu        sync.Mutex
	trackers  []*metricSeriesTracker
	interval  time.Duration
	running   bool
	newTicker func(time.Duration) expirationTicker
}

func newExpirationRuntime(trackers ...*metricSeriesTracker) *expirationRuntime {
	return &expirationRuntime{
		trackers: append([]*metricSeriesTracker(nil), trackers...),
		interval: expirationScanInterval(trackers),
		newTicker: func(interval time.Duration) expirationTicker {
			return &realExpirationTicker{ticker: time.NewTicker(interval)}
		},
	}
}

func expirationScanInterval(trackers []*metricSeriesTracker) time.Duration {
	var interval time.Duration
	for _, tracker := range trackers {
		if tracker == nil || tracker.expire <= 0 {
			continue
		}
		candidate := max(tracker.expire/2, minExpirationScanInterval)
		if interval == 0 || candidate < interval {
			interval = candidate
		}
	}
	if interval > maxExpirationScanInterval {
		return maxExpirationScanInterval
	}
	return interval
}

func (r *expirationRuntime) Start(parent context.Context) (func(context.Context) error, error) {
	if r == nil || r.interval <= 0 {
		return nil, nil
	}
	if parent == nil {
		parent = context.Background()
	}

	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil, errExpirationRuntimeRunning
	}
	runCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	r.running = true
	r.mu.Unlock()

	go r.run(runCtx, done)
	var cancelOnce sync.Once
	return func(waitCtx context.Context) error {
		cancelOnce.Do(cancel)
		if waitCtx == nil {
			waitCtx = context.Background()
		}
		select {
		case <-done:
			return nil
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}, nil
}

func (r *expirationRuntime) run(ctx context.Context, done chan<- struct{}) {
	ticker := r.newTicker(r.interval)
	defer close(done)
	defer r.markStopped()
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.Chan():
			r.scan(now)
		case <-ctx.Done():
			return
		}
	}
}

func (r *expirationRuntime) markStopped() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

func (r *expirationRuntime) scan(now time.Time) {
	for _, tracker := range r.trackers {
		tracker.expireSeries(now, expirationDeleteBatchSize)
	}
}
