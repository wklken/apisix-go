package server

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
)

func (s *Server) SendReloadEvent() {
	select {
	case s.reloadEventChan <- struct{}{}:
		logger.Info("ReloadProvider sent a reload event")
	default:
		logger.Info("ReloadProvider do nothing, already got a reload event")
	}
}

func (s *Server) listenReloadEvent(ctx context.Context, checkInterval time.Duration, storage *store.Store) {
	logger.Info("listen to the reload event")

	t := time.NewTicker(checkInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logger.Info("reload event check after 30s")

			// check the chan without block here
			select {
			case reloadEvent := <-s.reloadEventChan:
				logger.Infof("receive reload event: %+v", reloadEvent)
				// do reload
				s.reload(ctx, storage)
			default:
				logger.Debug("get nothing, will not do reload")
			}
		}
	}
}

var reloadMu sync.Mutex

// reload will do the reload
func (s *Server) reload(ctx context.Context, storage *store.Store) {
	reloadMu.Lock()
	defer reloadMu.Unlock()

	logger.Info("reloading")

	r := chi.NewRouter()

	// handler the panics if any
	defer func() {
		if err := recover(); err != nil {
			logger.Errorf("panic while reload, will not reset the handler: %v", err)
		} else {
			logger.Info("no errors, will reset the handler")

			// replace s.server.Handler
			// s.server.Handler = chi.ServerBaseContext(ctx, r)
			s.server.BaseContext = func(net.Listener) context.Context {
				return ctx
			}
			s.server.Handler = r
		}
	}()

	routes := storage.GetBucketData("routes")
	r = route.BuildRoute(routes)

	logger.Info("reload done")
}
