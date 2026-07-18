package server

import (
	"context"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/route"
)

// reloadQuietInterval coalesces the contiguous DELETE/PUT route events emitted
// for one standalone snapshot before rebuilding from the complete store state.
const reloadQuietInterval = 50 * time.Millisecond

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
	runReloadScheduler(ctx, s.reloadEventChan, reloadQuietInterval, func() {
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
	reload func(),
) {
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			stopAndDrainReloadTimer(timer)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			if timer == nil {
				timer = time.NewTimer(quietInterval)
			} else {
				stopAndDrainReloadTimer(timer)
				timer.Reset(quietInterval)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
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
