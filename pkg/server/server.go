package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
)

type Server struct {
	addr            string
	server          *http.Server
	reloadEventChan chan struct{}

	events  chan *store.Event
	storage *store.Store
}

func NewServer() (*Server, error) {
	events := make(chan *store.Event)
	storage := store.NewStore("apisix-go-store.db", events)
	return &Server{
		// FIXME: listen to multiple address from global config
		addr:            ":8080",
		server:          &http.Server{},
		reloadEventChan: make(chan struct{}, 1),
		events:          events,
		storage:         storage,
	}, nil
}

func (s *Server) Start() {
	s.storage.AddEventUpdateHook(
		func(event *store.Event) {
			s.SendReloadEvent()
		},
	)

	ctx, cancelFunc := context.WithCancel(context.Background())
	s.registerSignalHandler(ctx, cancelFunc)

	logger.Info("Starting storage")
	s.storage.Start()
	s.startEtcdWatcher()

	logger.Info("build the routes")
	s.server.Handler = route.NewBuilder(s.storage).Build()

	// start the reloader
	reloadCheckInterval := 60 * time.Second
	go s.listenReloadEvent(ctx, reloadCheckInterval)

	// start prometheus at another port
	for _, plugin := range config.GlobalConfig.Plugins {
		// prometheus enabled
		if plugin == "prometheus" {
			metrics.Init()

			go func() {
				exportUri := "/apisix/prometheus/metrics"
				exportIP := "127.0.0.1"
				exportPort := 9091
				attr, ok := config.GlobalConfig.PluginAttr["prometheus"]
				if ok {
					if v, ok := attr["export_ip"]; ok {
						exportIP = v.(string)
					}
					if v, ok := attr["export_port"]; ok {
						exportPort = v.(int)
					}
					if v, ok := attr["export_uri"]; ok {
						exportUri = v.(string)
					}
				}

				// FIXME: not support `enable_export_server == false`

				mux := chi.NewRouter()
				mux.Get(exportUri, promhttp.Handler().ServeHTTP)
				address := fmt.Sprintf("%s:%d", exportIP, exportPort)
				http.ListenAndServe(address, mux)
			}()
		}
	}

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

func (s *Server) startEtcdWatcher() {
	// prefix := "/apisix"
	// endpoints := []string{"127.0.0.1:2379"}
	prefix := config.GlobalConfig.Deployment.Etcd.Prefix
	endpoints := config.GlobalConfig.Deployment.Etcd.Host
	username := config.GlobalConfig.Deployment.Etcd.User
	password := config.GlobalConfig.Deployment.Etcd.Password

	logger.Info("Starting etcd client")
	etcdClient, err := etcd.NewConfigClient(endpoints, username, password, prefix, s.events)
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
