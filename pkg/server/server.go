package server

import (
	"bytes"
	"context"
	"crypto/tls"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"slices"
	"strings"
	"sync/atomic"
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
	"golang.org/x/net/http2"
)

var ErrMissingStreamUpstream = errors.New("missing stream upstream")

type streamRuntimeOwner interface {
	Reload([]resource.StreamRoute) error
	Close(context.Context) error
}

type Server struct {
	addr            string
	addrs           []string
	server          *http.Server
	routes          *routeHandler
	streamRuntime   streamRuntimeOwner
	reloadEventChan chan struct{}

	events            chan *store.Event
	storage           *store.Store
	etcdClient        *etcd.ConfigClient
	standaloneWatcher *config.StandaloneFileWatcher
}

func NewServer() (*Server, error) {
	events := make(chan *store.Event)
	storage := store.NewStore("apisix-go-store.db", events)
	routes := newRouteHandler(http.NotFoundHandler(), nil)
	var handler http.Handler = routes
	if config.GlobalConfig != nil && len(config.GlobalConfig.Apisix.TrustedAddresses) > 0 {
		handler = stripUntrustedForwardedFor(handler, config.GlobalConfig.Apisix.TrustedAddresses)
	}
	if config.GlobalConfig != nil && config.GlobalConfig.Apisix.NormalizeURILikeServlet {
		handler = normalizeRequestPath(handler)
	}
	if pluginConfigured("node-status") {
		handler = node_status.Track(handler)
	}
	addrs := configuredListenAddresses()
	return &Server{
		addr:            addrs[0],
		addrs:           addrs,
		server:          newConfiguredHTTPServer(handler),
		routes:          routes,
		reloadEventChan: make(chan struct{}, 1),
		events:          events,
		storage:         storage,
	}, nil
}

func stripUntrustedForwardedFor(next http.Handler, addresses []string) http.Handler {
	trustedNetworks := make([]*net.IPNet, 0, len(addresses))
	for _, address := range addresses {
		if _, network, err := net.ParseCIDR(address); err == nil {
			trustedNetworks = append(trustedNetworks, network)
			continue
		}
		ip := net.ParseIP(address)
		if ip == nil {
			continue
		}
		bits := 128
		if ip.To4() != nil {
			ip = ip.To4()
			bits = 32
		}
		trustedNetworks = append(trustedNetworks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	if len(trustedNetworks) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		remoteIP := net.ParseIP(strings.Trim(host, "[]"))
		trusted := false
		for _, network := range trustedNetworks {
			if network.Contains(remoteIP) {
				trusted = true
				break
			}
		}
		if !trusted {
			r.Header.Del("X-Forwarded-For")
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeRequestPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned := path.Clean(r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/") && cleaned != "/" {
			cleaned += "/"
		}
		if cleaned == r.URL.Path {
			next.ServeHTTP(w, r)
			return
		}

		request := r.Clone(r.Context())
		requestURL := *r.URL
		requestURL.Path = cleaned
		requestURL.RawPath = ""
		request.URL = &requestURL
		next.ServeHTTP(w, request)
	})
}

func configuredListenAddresses() []string {
	if config.GlobalConfig == nil {
		return []string{":8080"}
	}
	return config.GlobalConfig.Apisix.ListenAddresses()
}

func configuredTLSListenAddresses() []string {
	if config.GlobalConfig == nil || !config.GlobalConfig.Apisix.Ssl.Enable {
		return nil
	}
	listeners := config.GlobalConfig.Apisix.Ssl.Listen
	addresses := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		if listener.Port < 1 || listener.Port > 65535 {
			continue
		}
		host := strings.TrimSpace(listener.Ip)
		if host == "" {
			host = "0.0.0.0"
		}
		addresses = append(addresses, net.JoinHostPort(host, fmt.Sprintf("%d", listener.Port)))
	}
	return addresses
}

func newConfiguredHTTPServer(handler http.Handler) *http.Server {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	if frontendHTTP2Enabled() {
		protocols.SetHTTP2(true)
	}
	if frontendPlainHTTP2Enabled() {
		protocols.SetUnencryptedHTTP2(true)
	}
	server := &http.Server{Handler: handler, Protocols: protocols}
	if frontendHTTP2Enabled() {
		if err := http2.ConfigureServer(server, nil); err != nil {
			logger.Errorf("configure HTTP/2 server: %s", err)
		}
	}
	if config.GlobalConfig == nil {
		return server
	}

	httpConfig := config.GlobalConfig.NginxConfig.HTTP
	server.IdleTimeout = httpConfig.KeepaliveTimeout
	server.ReadHeaderTimeout = httpConfig.ClientHeaderTimeout
	server.WriteTimeout = httpConfig.SendTimeout
	if httpConfig.ClientBodyTimeout > 0 {
		server.ReadTimeout = httpConfig.ClientBodyTimeout + httpConfig.ClientHeaderTimeout
	}
	return server
}

func pluginConfigured(name string) bool {
	if config.GlobalConfig == nil {
		return false
	}
	return slices.Contains(config.GlobalConfig.Plugins, name)
}

func (s *Server) Start() {
	var reloadGeneration atomic.Uint64
	if standaloneConfigProvider(config.GlobalConfig) == "" {
		s.storage.AddEventUpdateHook(
			func(event *store.Event) {
				handleStoreEventUpdate(
					event,
					func() {
						reloadGeneration.Add(1)
						s.SendReloadEvent()
					},
					func() {
						if s.streamRuntime == nil {
							return
						}
						if err := s.reloadStreamRoutes(); err != nil {
							logger.Errorf("reload stream routes fail: %s", err)
						}
					},
				)
			},
		)
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	s.registerSignalHandler(ctx, cancelFunc)

	logger.Info("Starting storage")
	s.storage.Start()
	s.startConfigProvider(ctx)

	logger.Info("build the routes")
	initialReloadGeneration := reloadGeneration.Load()
	builder := route.NewBuilderWithServerAddr(s.storage, s.addr)
	s.routes.Replace(initialRouteHandler(builder.Build()), builder.Stop)
	reconcileInitialReloadEvent(s.reloadEventChan, initialReloadGeneration, reloadGeneration.Load)
	s.startStreamProxy(ctx)
	if s.standaloneWatcher != nil {
		s.standaloneWatcher.Watch()
		provider := standaloneConfigProvider(config.GlobalConfig)
		logger.Infof("watch standalone config %s", config.StandaloneConfigFile(provider))
	}

	// start the reloader
	go s.listenReloadEvent(ctx)

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
				if err := http.ListenAndServe(exportConfig.Address(), mux); err != nil {
					logger.Errorf("prometheus export server stopped: %s", err)
				}
			}(exportConfig)
		}
	}

	s.startServer(ctx)
}

func initialRouteHandler(handler *chi.Mux) http.Handler {
	if handler == nil {
		return http.NotFoundHandler()
	}
	return handler
}

func (s *Server) registerSignalHandler(ctx context.Context, cancelFunc context.CancelFunc) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sig
		shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
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
	bucket, ok := routeEventBucket(event)
	return ok && store.IsStreamReloadBucket(bucket)
}

func isHTTPRouteEvent(event *store.Event) bool {
	bucket, ok := routeEventBucket(event)
	return ok && store.IsHTTPRouteReloadBucket(bucket)
}

func handleStoreEventUpdate(event *store.Event, reloadHTTP func(), reloadStream func()) {
	if isHTTPRouteEvent(event) && reloadHTTP != nil {
		reloadHTTP()
	}
	if isStreamRouteEvent(event) && reloadStream != nil {
		reloadStream()
	}
}

func routeEventBucket(event *store.Event) (string, bool) {
	if event == nil {
		return "", false
	}
	parts := bytes.Split(event.Key, []byte("/"))
	if len(parts) < 2 {
		return "", false
	}
	return string(parts[len(parts)-2]), true
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

func (s *Server) startConfigProvider(ctx context.Context) {
	provider := standaloneConfigProvider(config.GlobalConfig)
	if provider != "" {
		path := config.StandaloneConfigFile(provider)
		watcher := config.NewStandaloneFileWatcher(path, provider, s.events)
		if err := watcher.Reload(); err != nil {
			panic(fmt.Errorf("load standalone config: %w", err))
		}
		s.storage.Sync()
		watcher.SetReloadCallback(func(result config.StandaloneReloadResult, err error) {
			applyStandaloneSnapshot(
				result,
				err,
				s.storage.Sync,
				func() { s.reload(ctx) },
				func() {
					if s.streamRuntime == nil {
						return
					}
					if err := s.reloadStreamRoutes(); err != nil {
						logger.Errorf("reload stream routes fail: %s", err)
					}
				},
			)
		})
		s.standaloneWatcher = watcher
		return
	}
	s.startEtcdWatcher(ctx)
}

func applyStandaloneSnapshot(
	result config.StandaloneReloadResult,
	err error,
	syncStore func(),
	reloadRoutes func(),
	reloadStreams func(),
) {
	if err != nil {
		return
	}
	syncStore()
	if result.AffectsHTTPRoutes() {
		reloadRoutes()
	}
	if result.AffectsStreams() {
		reloadStreams()
	}
}

func standaloneConfigProvider(cfg *config.Config) string {
	if cfg == nil || !strings.EqualFold(cfg.Deployment.Role, "data_plane") {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Deployment.RoleDataPlane.ConfigProvider))
	if provider != "yaml" && provider != "json" {
		return ""
	}
	return provider
}

func (s *Server) startEtcdWatcher(ctx context.Context) {
	etcdConfig := config.GlobalConfig.Deployment.Etcd
	prefix := etcdConfig.Prefix
	endpoints := etcdConfig.Host
	username := etcdConfig.User
	password := etcdConfig.Password

	var tlsConfig *tls.Config
	var err error
	if etcdTLSRequired(endpoints, etcdConfig.TLS) {
		tlsConfig, err = etcd.NewTLSConfig(
			etcdConfig.TLS.Cert,
			etcdConfig.TLS.Key,
			etcdConfig.TLS.SNI,
			etcdConfig.TLS.Verify,
		)
		if err != nil {
			logger.Errorf("build etcd TLS config fail: %s", err)
			return
		}
	}
	requestTimeout := 5 * time.Second
	if etcdConfig.Timeout > 0 {
		requestTimeout = time.Duration(etcdConfig.Timeout) * time.Second
	}

	logger.Info("Starting etcd client")
	etcdClient, err := etcd.NewConfigClientWithOptions(
		endpoints,
		username,
		password,
		prefix,
		s.events,
		etcd.ClientOptions{
			DialTimeout:    requestTimeout,
			RequestTimeout: requestTimeout,
			StartupRetry:   etcdConfig.StartupRetry,
			TLS:            tlsConfig,
		},
	)
	if err != nil {
		panic(err)
	}
	s.etcdClient = etcdClient
	logger.Info("fetch full data from etcd")
	err = fetchAndSyncInitialEtcdConfig(etcdClient.FetchAll, s.storage.Sync)
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

func fetchAndSyncInitialEtcdConfig(fetch func() error, syncStore func()) error {
	if err := fetch(); err != nil {
		return err
	}
	syncStore()
	return nil
}

func etcdTLSRequired(endpoints []string, tlsConfig config.EtcdTLS) bool {
	if tlsConfig.Cert != "" || tlsConfig.Key != "" || tlsConfig.SNI != "" {
		return true
	}
	for _, endpoint := range endpoints {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(endpoint)), "https://") {
			return true
		}
	}
	return false
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
	addrs := s.addrs
	if len(addrs) == 0 {
		addrs = []string{s.addr}
	}
	for _, addr := range addrs {
		logger.Infof("listening on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Fatalf("error opening listener: %w", err)
		}
		go func(listener net.Listener) {
			if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
				logger.Errorf("error serve: %s", err)
			}
		}(listener)
	}
	for _, addr := range configuredTLSListenAddresses() {
		logger.Infof("listening with TLS on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			logger.Fatalf("error opening TLS listener: %w", err)
		}
		tlsListener := tls.NewListener(listener, frontendTLSConfig())
		go func(listener net.Listener) {
			if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
				logger.Errorf("error serve TLS: %s", err)
			}
		}(tlsListener)
	}

	<-ctx.Done()
}

func frontendTLSConfig() *tls.Config {
	protocols := []string{"http/1.1"}
	if frontendHTTP2Enabled() {
		protocols = append([]string{"h2"}, protocols...)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: protocols,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			serverName := strings.TrimSpace(hello.ServerName)
			if serverName == "" && config.GlobalConfig != nil {
				serverName = strings.TrimSpace(config.GlobalConfig.Apisix.Ssl.FallbackSNI)
			}
			ssls, err := store.ListSSLs()
			if err != nil {
				return nil, err
			}
			for _, sslResource := range ssls {
				if sslResource.Status == 0 || !matchesSNI(sslResource.Snis, serverName) {
					continue
				}
				certificate, err := tls.X509KeyPair([]byte(sslResource.Cert), []byte(sslResource.Key))
				if err != nil {
					return nil, fmt.Errorf("load SSL resource %q: %w", sslResource.ID, err)
				}
				return &certificate, nil
			}
			return nil, fmt.Errorf("no SSL certificate for SNI %q", serverName)
		},
	}
}

func frontendHTTP2Enabled() bool {
	if config.GlobalConfig == nil {
		return false
	}
	if config.GlobalConfig.Apisix.EnableHttp2 {
		return true
	}
	for _, listener := range config.GlobalConfig.Apisix.Ssl.Listen {
		if listener.EnableHttp2 {
			return true
		}
	}
	return false
}

func frontendPlainHTTP2Enabled() bool {
	if config.GlobalConfig == nil {
		return false
	}
	if config.GlobalConfig.Apisix.EnableHttp2 {
		return true
	}
	for _, listener := range config.GlobalConfig.Apisix.NodeListen {
		if listener.EnableHttp2 {
			return true
		}
	}
	return false
}

func matchesSNI(snis []string, serverName string) bool {
	for _, sni := range snis {
		sni = strings.TrimSpace(sni)
		if strings.EqualFold(sni, serverName) {
			return true
		}
		if strings.HasPrefix(sni, "*.") && strings.HasSuffix(strings.ToLower(serverName), strings.ToLower(sni[1:])) {
			return true
		}
	}
	return false
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
