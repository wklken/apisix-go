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
	"github.com/spf13/cast"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/node_status"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
)

type Server struct {
	addr            string
	server          *http.Server
	routes          *routeHandler
	reloadEventChan chan struct{}

	events  chan *store.Event
	storage *store.Store
}

func NewServer() (*Server, error) {
	events := make(chan *store.Event)
	storage := store.NewStore("apisix-go-store.db", events)
	routes := newRouteHandler(http.NotFoundHandler(), nil)
	var handler http.Handler = routes
	if pluginConfigured("node-status") {
		handler = node_status.Track(handler)
	}
	return &Server{
		// FIXME: listen to multiple address from global config
		addr:            ":8080",
		server:          &http.Server{Handler: handler},
		routes:          routes,
		reloadEventChan: make(chan struct{}, 1),
		events:          events,
		storage:         storage,
	}, nil
}

func pluginConfigured(name string) bool {
	if config.GlobalConfig == nil {
		return false
	}
	for _, configured := range config.GlobalConfig.Plugins {
		if configured == name {
			return true
		}
	}
	return false
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
	builder := route.NewBuilderWithServerAddr(s.storage, s.addr)
	s.routes.Replace(builder.Build(), builder.Stop)

	// start the reloader
	reloadCheckInterval := 60 * time.Second
	go s.listenReloadEvent(ctx, reloadCheckInterval)

	// start prometheus at another port
	for _, plugin := range config.GlobalConfig.Plugins {
		// prometheus enabled
		if plugin == "prometheus" {
			metrics.Init()

			exportConfig := newPrometheusExportServerConfig(config.GlobalConfig.PluginAttr["prometheus"])
			if !exportConfig.Enabled {
				continue
			}

			go func(exportConfig prometheusExportServerConfig) {
				mux := chi.NewRouter()
				mux.Get(exportConfig.ExportURI, promhttp.Handler().ServeHTTP)
				http.ListenAndServe(exportConfig.Address(), mux)
			}(exportConfig)
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
		err := s.shutdown(shutdownCtx)
		if err != nil {
			logger.Fatal(err.Error())
		}
		cancelFunc()
	}()
}

func (s *Server) shutdown(ctx context.Context) error {
	if err := s.server.Shutdown(ctx); err != nil {
		return err
	}
	s.routes.Close()
	return nil
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

type prometheusExportServerConfig struct {
	Enabled    bool
	ExportURI  string
	ExportIP   string
	ExportPort int
}

func newPrometheusExportServerConfig(attr map[string]interface{}) prometheusExportServerConfig {
	cfg := prometheusExportServerConfig{
		Enabled:    true,
		ExportURI:  "/apisix/prometheus/metrics",
		ExportIP:   "127.0.0.1",
		ExportPort: 9091,
	}

	if attr == nil {
		return cfg
	}

	if v, ok := attr["enable_export_server"].(bool); ok {
		cfg.Enabled = v
	}
	if v, ok := attr["export_uri"].(string); ok && v != "" {
		cfg.ExportURI = v
	}
	if v, ok := attr["export_ip"].(string); ok && v != "" {
		cfg.ExportIP = v
	}
	if v, ok := attr["export_port"]; ok {
		cfg.ExportPort = cast.ToInt(v)
	}
	if v, ok := attr["export_addr"].(map[string]interface{}); ok {
		if ip, ok := v["ip"].(string); ok && ip != "" {
			cfg.ExportIP = ip
		}
		if port, ok := v["port"]; ok {
			cfg.ExportPort = cast.ToInt(port)
		}
	}

	return cfg
}

func (c prometheusExportServerConfig) Address() string {
	return fmt.Sprintf("%s:%d", c.ExportIP, c.ExportPort)
}
