package server

import (
	"context"
	"fmt"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
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
		if ctx != nil && ctx.Err() != nil {
			return
		}
		if err := s.reload(ctx); err != nil {
			if ctx != nil && ctx.Err() != nil {
				return
			}
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageHTTPRoutes)
			logger.Errorf("reload routes fail: %s", err)
			return
		}
		if ctx == nil || ctx.Err() == nil {
			metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
		}
	})
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

// reload rebuilds the route handler from the store. A cancelled context
// skips the rebuild so a shutting-down server does not install a handler it
// cannot serve.
func (s *Server) reload(ctx context.Context) (reloadErr error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		logger.Info("skip reload: context cancelled")
		return ctx.Err()
	}

	logger.Info("reloading")

	builder := route.NewBuilderWithClusterRegistry(s.storage, s.addr, s.clusters)
	installed := false

	defer func() {
		if !installed {
			builder.Stop()
		}
		if recovered := recover(); recovered != nil {
			switch panicErr := recovered.(type) {
			case error:
				reloadErr = fmt.Errorf("reload routes panic: %w", panicErr)
			default:
				reloadErr = fmt.Errorf("reload routes panic: %v", panicErr)
			}
			logger.Errorf("panic while reload, will not reset the handler: %v", recovered)
		}
	}()

	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil {
		reloadErr = fmt.Errorf("reload routes: %w", err)
		logger.Errorf("reload routes fail, keeping the current handler: %s", reloadErr)
		return reloadErr
	}
	s.routes.Replace(handler, builder.Stop)
	recordRouteBuildQuarantine(builder)
	installed = true

	logger.Info("reload done")
	return nil
}
