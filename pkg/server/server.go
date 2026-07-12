package server

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
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
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

var ErrMissingStreamUpstream = errors.New("missing stream upstream")

type streamRuntimeOwner interface {
	Reload([]resource.StreamRoute) error
	Close(context.Context) error
}

type Server struct {
	addr            string
	server          *http.Server
	routes          *routeHandler
	streamRuntime   streamRuntimeOwner
	reloadEventChan chan struct{}

	events     chan *store.Event
	storage    *store.Store
	etcdClient *etcd.ConfigClient
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
	return slices.Contains(config.GlobalConfig.Plugins, name)
}

func (s *Server) Start() {
	s.storage.AddEventUpdateHook(
		func(event *store.Event) {
			s.SendReloadEvent()
			if s.streamRuntime != nil && isStreamRouteEvent(event) {
				if err := s.reloadStreamRoutes(); err != nil {
					logger.Errorf("reload stream routes fail: %s", err)
				}
			}
		},
	)

	ctx, cancelFunc := context.WithCancel(context.Background())
	s.registerSignalHandler(ctx, cancelFunc)

	logger.Info("Starting storage")
	s.storage.Start()
	s.startEtcdWatcher(ctx)

	logger.Info("build the routes")
	builder := route.NewBuilderWithServerAddr(s.storage, s.addr)
	s.routes.Replace(builder.Build(), builder.Stop)
	s.startStreamProxy(ctx)

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
	if s.streamRuntime != nil {
		if err := s.streamRuntime.Close(ctx); err != nil {
			return err
		}
	}
	s.routes.Close()
	if s.etcdClient != nil {
		if err := s.etcdClient.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) startStreamProxy(ctx context.Context) {
	if config.GlobalConfig == nil || !streamProxyModeEnabled(config.GlobalConfig) {
		return
	}
	if len(config.GlobalConfig.Apisix.StreamProxy.Tcp) == 0 {
		return
	}

	routes, err := s.loadStreamRoutes()
	if err != nil {
		logger.Errorf("load stream routes fail: %s", err)
		return
	}
	runtime, err := streamruntime.NewRuntime(
		ctx,
		config.GlobalConfig.Apisix.StreamProxy.Tcp,
		routes,
		config.GlobalConfig.StreamPlugins,
		logStreamResult,
	)
	if err != nil {
		logger.Errorf("start stream proxy fail: %s", err)
		return
	}
	s.streamRuntime = runtime
	logger.Infof("stream proxy listening on %v", runtime.Addresses())
}

func (s *Server) loadStreamRoutes() ([]resource.StreamRoute, error) {
	routes, err := store.ListStreamRoutes()
	if err != nil {
		return nil, err
	}
	return resolveStreamRoutes(routes, store.GetUpstream)
}

func (s *Server) reloadStreamRoutes() error {
	routes, err := s.loadStreamRoutes()
	if err != nil {
		return err
	}
	return s.streamRuntime.Reload(routes)
}

func resolveStreamRoutes(
	routes []resource.StreamRoute,
	lookup func(string) (resource.Upstream, error),
) ([]resource.StreamRoute, error) {
	resolved := make([]resource.StreamRoute, len(routes))
	copy(resolved, routes)
	for index := range resolved {
		route := &resolved[index]
		if route.UpstreamID == "" || len(route.Upstream.Nodes) > 0 {
			continue
		}
		if lookup == nil {
			return nil, fmt.Errorf(
				"stream route %q references upstream %q: %w",
				route.ID,
				route.UpstreamID,
				ErrMissingStreamUpstream,
			)
		}
		upstream, err := lookup(route.UpstreamID)
		if err != nil {
			return nil, fmt.Errorf("stream route %q references upstream %q: %w", route.ID, route.UpstreamID, err)
		}
		route.Upstream = upstream
	}
	return resolved, nil
}

func streamProxyModeEnabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	mode := strings.ToLower(strings.ReplaceAll(cfg.Apisix.ProxyMode, " ", ""))
	return mode == "stream" || mode == "http&stream" || mode == "stream&http"
}

func isStreamRouteEvent(event *store.Event) bool {
	if event == nil {
		return false
	}
	parts := bytes.Split(event.Key, []byte("/"))
	if len(parts) < 2 {
		return false
	}
	bucket := parts[len(parts)-2]
	return bytes.Equal(bucket, []byte("stream_routes")) || bytes.Equal(bucket, []byte("upstreams"))
}

func logStreamResult(result streamruntime.Result) {
	if result.Err != nil {
		logger.Errorf(
			"stream route %s ended with error: protocol=%s remote=%s err=%s",
			result.RouteID,
			result.Protocol,
			result.Remote,
			result.Err,
		)
		return
	}
	logger.Infof(
		"stream route %s connection ended: protocol=%s remote=%s client_id=%s",
		result.RouteID,
		result.Protocol,
		result.Remote,
		result.ClientID,
	)
}

func (s *Server) startEtcdWatcher(ctx context.Context) {
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
	s.etcdClient = etcdClient
	logger.Info("fetch full data from etcd")
	err = etcdClient.FetchAll()
	if err != nil {
		panic(err)
	}
	if serverInfoReportingEnabled() {
		nodeID := server_info.CurrentInfo().ID
		_, err := etcdClient.StartServerInfoReporter(
			ctx,
			nodeID,
			server_info.ReportTTL(),
			func() ([]byte, error) {
				return stdjson.Marshal(server_info.CurrentInfo())
			},
		)
		if err != nil {
			logger.Warnf("start server-info reporter fail: %s", err)
		}
	}
	logger.Info("watch etcd")
	go etcdClient.Watch(ctx)
}

func serverInfoReportingEnabled() bool {
	if !pluginConfigured("server-info") || config.GlobalConfig == nil {
		return false
	}
	if strings.EqualFold(config.GlobalConfig.Deployment.Role, "data_plane") {
		return false
	}
	return strings.EqualFold(config.GlobalConfig.Deployment.RoleTraditional.ConfigProvider, "etcd")
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

func newPrometheusExportServerConfig(attr map[string]any) prometheusExportServerConfig {
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
	if v, ok := attr["export_addr"].(map[string]any); ok {
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
