package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
)

type Server struct {
	addr   string
	server *http.Server

	reloadEventChan chan struct{}
}

func NewServer() (*Server, error) {
	return &Server{
		addr:            ":9080",
		server:          &http.Server{},
		reloadEventChan: make(chan struct{}, 1),
	}, nil
}

func (s *Server) Start() {
	ctx, cancelFunc := context.WithCancel(context.Background())
	s.registerSignalHandler(ctx, cancelFunc)

	events := make(chan *store.Event)
	storage := s.startStorage(events)
	s.startEtcdClient(events)

	s.buildRoutes(storage)

	// start the reloader
	reloadCheckInterval := 30 * time.Second
	go s.listenReloadEvent(ctx, reloadCheckInterval, storage)

	// FIXME: port and path should be configurable
	// start prometheus at another port
	go func() {
		mux := chi.NewRouter()
		mux.Get("/apisix/prometheus/metrics", promhttp.Handler().ServeHTTP)
		// server := &http.Server{}
		// server.Handler = mux
		http.ListenAndServe(":9091", mux)
	}()

	s.startServer(ctx)
}

func (s *Server) registerSignalHandler(ctx context.Context, cancelFunc context.CancelFunc) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sig
		shutdownCtx, _ := context.WithTimeout(ctx, 30*time.Second)
		go func() {
			<-shutdownCtx.Done()
			if shutdownCtx.Err() == context.DeadlineExceeded {
				logger.Fatal("graceful shutdown timed out.. forcing exit.")
			}
		}()
		err := s.server.Shutdown(shutdownCtx)
		if err != nil {
			logger.Fatal(err.Error())
		}
		cancelFunc()
	}()
}

func (s *Server) startStorage(events chan *store.Event) *store.Store {
	logger.Info("Starting storage")
	storage := store.NewStore("my.db", events, []store.EventUpdateHook{
		func(event *store.Event) {
			s.SendReloadEvent()
		},
	})
	go storage.Start()
	return storage
}

func (s *Server) startEtcdClient(events chan *store.Event) {
	prefix := "/apisix"
	endpoints := []string{"127.0.0.1:2379"}
	logger.Info("Starting etcd client")
	etcdClient, err := etcd.NewConfigClient(endpoints, prefix, events)
	if err != nil {
		panic(err)
	}
	logger.Info("fetch full data from etcd")
	err = etcdClient.FetchAll()
	if err != nil {
		panic(err)
	}
	logger.Info("watch etcd")
	go etcdClient.Watch()
}

func (s *Server) buildRoutes(storage *store.Store) {
	logger.Info("build the routes")
	routes := storage.GetBucketData("routes")
	s.server.Handler = route.BuildRoute(routes)
}

func (s *Server) startServer(ctx context.Context) {
	logger.Infof("listening on %s", s.addr)
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		logger.Fatalf("error opening listener: %w", err)
	}
	err = s.server.Serve(listener)
	if err != nil && err != http.ErrServerClosed {
		logger.Fatalf("error serve: %w", err)
	}
	<-ctx.Done()
}
