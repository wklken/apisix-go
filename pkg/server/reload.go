package server

import (
	"context"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/route"
)

const (
	// reloadQuietInterval coalesces a short burst of etcd route events.
	reloadQuietInterval = 50 * time.Millisecond
	// reloadMaximumWait prevents a continuous etcd event stream from starving publication.
	reloadMaximumWait = 500 * time.Millisecond
)

func (s *Server) SendReloadEvent() {
	select {
	case s.reloadEventChan <- struct{}{}:
		logger.Info("ReloadProvider sent a reload event")
	default:
		logger.Info("ReloadProvider do nothing, already got a reload event")
	}
}

func (s *Server) listenReloadEvent(ctx context.Context) {
	logger.Info("listen to the reload event")
	runReloadScheduler(ctx, s.reloadEventChan, reloadQuietInterval, reloadMaximumWait, func() {
		s.reload(ctx)
	})
}

func reconcileInitialReloadEvent(events chan struct{}, builtGeneration uint64, currentGeneration func() uint64) {
	select {
	case <-events:
	default:
	}
	if currentGeneration() == builtGeneration {
		return
	}
	select {
	case events <- struct{}{}:
	default:
	}
}

func runReloadScheduler(
	ctx context.Context,
	events <-chan struct{},
	quietInterval time.Duration,
	maximumWait time.Duration,
	reload func(),
) {
	var quietTimer *time.Timer
	var quietTimerC <-chan time.Time
	var maximumTimer *time.Timer
	var maximumTimerC <-chan time.Time
	var maximumDeadline time.Time
	defer func() {
		if quietTimer != nil {
			stopAndDrainReloadTimer(quietTimer)
		}
		if maximumTimer != nil {
			stopAndDrainReloadTimer(maximumTimer)
		}
	}()
	finishBatch := func() {
		if quietTimer != nil {
			stopAndDrainReloadTimer(quietTimer)
			quietTimer = nil
			quietTimerC = nil
		}
		if maximumTimer != nil {
			stopAndDrainReloadTimer(maximumTimer)
			maximumTimer = nil
			maximumTimerC = nil
		}
		maximumDeadline = time.Time{}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			if !maximumDeadline.IsZero() && !time.Now().Before(maximumDeadline) {
				finishBatch()
				logger.Info("receive reload event")
				reload()
				continue
			}
			if quietTimer == nil {
				quietTimer = time.NewTimer(quietInterval)
				quietTimerC = quietTimer.C
				maximumTimer = time.NewTimer(maximumWait)
				maximumTimerC = maximumTimer.C
				maximumDeadline = time.Now().Add(maximumWait)
			} else {
				stopAndDrainReloadTimer(quietTimer)
				quietTimer.Reset(quietInterval)
			}
		case <-quietTimerC:
			finishBatch()
			logger.Info("receive reload event")
			reload()
		case <-maximumTimerC:
			finishBatch()
			logger.Info("receive reload event")
			reload()
		}
	}
}

func stopAndDrainReloadTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

var reloadMu sync.Mutex

// reload will do the reload
func (s *Server) reload(_ context.Context) {
	reloadMu.Lock()
	defer reloadMu.Unlock()

	logger.Info("reloading")

	builder := route.NewBuilderWithServerAddr(s.storage, s.addr)
	installed := false

	defer func() {
		if !installed {
			builder.Stop()
		}
		if err := recover(); err != nil {
			logger.Errorf("panic while reload, will not reset the handler: %v", err)
		}
	}()

	handler := builder.Build()
	if handler == nil {
		logger.Error("reload built a nil route handler; keeping the current handler")
		return
	}
	s.routes.Replace(handler, builder.Stop)
	installed = true

	logger.Info("reload done")
}
