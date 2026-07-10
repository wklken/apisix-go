package server

import (
	"context"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/route"
)

func (s *Server) SendReloadEvent() {
	select {
	case s.reloadEventChan <- struct{}{}:
		logger.Info("ReloadProvider sent a reload event")
	default:
		logger.Info("ReloadProvider do nothing, already got a reload event")
	}
}

func (s *Server) listenReloadEvent(ctx context.Context, checkInterval time.Duration) {
	logger.Info("listen to the reload event")

	t := time.NewTicker(checkInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logger.Debug("reload event check after 30s")

			// check the chan without block here
			select {
			case reloadEvent := <-s.reloadEventChan:
				logger.Infof("receive reload event: %+v", reloadEvent)
				// do reload
				s.reload(ctx)
			default:
				logger.Debug("get nothing, will not do reload")
			}
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
