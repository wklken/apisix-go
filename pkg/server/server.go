package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cast"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/observability/otel"
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

	prometheusServer *http.Server
	otelShutdown     func(context.Context) error
}

func NewServer() (*Server, error) {
	events := make(chan *store.Event)
	storage, err := store.GetStore("apisix-go-store.db", events)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	routes := newRouteHandler(http.NotFoundHandler(), nil)
	var handler http.Handler = routes
	var trustedAddresses []string
	if config.GlobalConfig != nil {
		trustedAddresses = config.GlobalConfig.Apisix.TrustedAddresses
	}
	handler = normalizeForwardedHeaders(handler, trustedAddresses)
	if config.GlobalConfig != nil && config.GlobalConfig.Apisix.NormalizeURILikeServlet {
		handler = normalizeRequestPath(handler)
	}
	if pluginConfigured("node-status") {
		handler = node_status.Track(handler)
	}
	addrs := configuredListenAddresses()
	otelShutdown, err := otel.Init("apisix-go")
	if err != nil {
		return nil, fmt.Errorf("initialize tracing: %w", err)
	}
	return &Server{
		addr:            addrs[0],
		addrs:           addrs,
		server:          newConfiguredHTTPServer(handler),
		routes:          routes,
		reloadEventChan: make(chan struct{}, 1),
		events:          events,
		storage:         storage,
		otelShutdown:    otelShutdown,
	}, nil
}

func normalizeForwardedHeaders(next http.Handler, addresses []string) http.Handler {
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
		if trusted {
			r = apisixctx.WithTrustedProxy(r)
			if r.Header.Get("X-Forwarded-Proto") == "" {
				r.Header.Set("X-Forwarded-Proto", scheme(r))
			}
			if r.Header.Get("X-Forwarded-Host") == "" {
				r.Header.Set("X-Forwarded-Host", r.Host)
			}
			if r.Header.Get("X-Forwarded-Port") == "" {
				r.Header.Set("X-Forwarded-Port", listenPort(r))
			}
		} else {
			// Untrusted peers cannot forge the observed values: overwrite with
			// the trusted ones, mirroring APISIX handle_x_forwarded_headers.
			r.Header.Set("X-Forwarded-Proto", scheme(r))
			r.Header.Set("X-Forwarded-Host", r.Host)
			r.Header.Set("X-Forwarded-Port", listenPort(r))
			r.Header.Del("Forwarded")
			// When a trust boundary is configured, drop the spoofable inbound
			// chain; otherwise preserve the compatible default.
			if len(trustedNetworks) > 0 {
				r.Header.Del("X-Forwarded-For")
			}
		}
		next.ServeHTTP(w, r)
	})
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func listenPort(r *http.Request) string {
	_, port, err := net.SplitHostPort(r.Host)
	if err == nil {
		return port
	}
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, port, err := net.SplitHostPort(local.String()); err == nil {
			return port
		}
	}
	if r.TLS != nil {
		return "443"
	}
	return "80"
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
		addresses = append(addresses, net.JoinHostPort(host, strconv.Itoa(listener.Port)))
	}
	return addresses
}

const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultHTTPIdleTimeout   = 90 * time.Second
)

func newConfiguredHTTPServer(handler http.Handler) *http.Server {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	if frontendHTTP2Enabled() {
		protocols.SetHTTP2(true)
	}
	if frontendPlainHTTP2Enabled() {
		protocols.SetUnencryptedHTTP2(true)
	}
	server := &http.Server{
		Handler:           handler,
		Protocols:         protocols,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultHTTPIdleTimeout,
	}
	if frontendHTTP2Enabled() {
		if err := http2.ConfigureServer(server, nil); err != nil {
			logger.Errorf("configure HTTP/2 server: %s", err)
		}
	}
	if config.GlobalConfig == nil {
		return server
	}

	httpConfig := config.GlobalConfig.NginxConfig.HTTP
	if httpConfig.KeepaliveTimeout > 0 {
		server.IdleTimeout = httpConfig.KeepaliveTimeout
	}
	if httpConfig.ClientHeaderTimeout > 0 {
		server.ReadHeaderTimeout = httpConfig.ClientHeaderTimeout
	}
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

// Start runs the server until ctx is cancelled or a listener fails. Startup
// failures are returned with operation context instead of panicking; the
// command owns the process shutdown path.
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	logger.Info("Starting storage")
	s.storage.Start()
	if err := s.startConfigProvider(ctx); err != nil {
		return err
	}

	logger.Info("build the routes")
	initialReloadGeneration := reloadGeneration.Load()
	builder := route.NewBuilderWithServerAddr(s.storage, s.addr)
	if err := buildAndInstallInitialRoutes(s.routes, builder); err != nil {
		return err
	}
	reconcileInitialReloadEvent(s.reloadEventChan, initialReloadGeneration, reloadGeneration.Load)
	s.startStreamProxy(ctx)
	if s.standaloneWatcher != nil {
		s.standaloneWatcher.Watch()
		provider := standaloneConfigProvider(config.GlobalConfig)
		logger.Infof("watch standalone config %s", config.StandaloneConfigFile(provider))
	}

	// start the reloader
	go s.listenReloadEvent(ctx)

	if err := s.startPrometheusExportServer(); err != nil {
		return err
	}

	return s.startHTTPListeners(ctx)
}

// Shutdown gracefully stops the HTTP listeners and the observability and
// config dependencies owned by the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

type strictRouteBuilder interface {
	BuildStrict() (*chi.Mux, error)
	Stop()
}

func buildAndInstallInitialRoutes(routes *routeHandler, builder strictRouteBuilder) error {
	handler, err := builder.BuildStrict()
	if err != nil {
		builder.Stop()
		return fmt.Errorf("build initial routes: %w", err)
	}
	routes.Replace(handler, builder.Stop)
	return nil
}

func (s *Server) shutdown(ctx context.Context) error {
	var errs []error
	httpErr := s.server.Shutdown(ctx)
	if httpErr != nil {
		errs = append(errs, fmt.Errorf("stop HTTP server: %w", httpErr))
	} else {
		// HTTP quiescence reached, so no in-flight request can block the
		// graceful route drain; a timed-out HTTP shutdown skips the drain and
		// the command's exit releases stragglers.
		s.routes.Close()
	}
	if s.streamRuntime != nil {
		if err := s.streamRuntime.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop stream runtime: %w", err))
		}
	}
	if s.standaloneWatcher != nil {
		s.standaloneWatcher.Stop()
	}
	if s.prometheusServer != nil {
		if err := s.prometheusServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop prometheus export server: %w", err))
		}
	}
	if s.otelShutdown != nil {
		if err := s.otelShutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop tracing: %w", err))
		}
	}
	if s.etcdClient != nil {
		if err := s.etcdClient.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close etcd client: %w", err))
		}
	}
	return errors.Join(errs...)
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

// startPrometheusExportServer starts the prometheus export server when the
// plugin is enabled and retains it as an owned lifecycle resource.
func (s *Server) startPrometheusExportServer() error {
	if config.GlobalConfig == nil {
		return nil
	}
	if !slices.Contains(config.GlobalConfig.Plugins, "prometheus") {
		return nil
	}
	metrics.Init()
	exportConfig := newPrometheusExportServerConfig(config.GlobalConfig.PluginAttr["prometheus"])
	exporter, _, err := metrics.StartExportServer(metrics.ExportServerConfig{
		Enabled: exportConfig.Enabled,
		URI:     exportConfig.ExportURI,
		Address: exportConfig.Address(),
	})
	if err != nil {
		return fmt.Errorf("start prometheus export server: %w", err)
	}
	s.prometheusServer = exporter
	return nil
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

func (s *Server) startConfigProvider(ctx context.Context) error {
	provider := standaloneConfigProvider(config.GlobalConfig)
	if provider != "" {
		path := config.StandaloneConfigFile(provider)
		watcher := config.NewStandaloneFileWatcher(path, provider, s.events)
		if err := watcher.Reload(); err != nil {
			return fmt.Errorf("load standalone config: %w", err)
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
		return nil
	}
	return s.startEtcdWatcher(ctx)
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

func (s *Server) startEtcdWatcher(ctx context.Context) error {
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
			return fmt.Errorf("build etcd TLS config: %w", err)
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
		return fmt.Errorf("start etcd client: %w", err)
	}
	s.etcdClient = etcdClient
	logger.Info("fetch full data from etcd")
	err = fetchAndSyncInitialEtcdConfig(etcdClient.FetchAll, s.storage.Sync)
	if err != nil {
		return fmt.Errorf("fetch initial etcd config: %w", err)
	}
	if serverInfoReportingEnabled() {
		nodeID := server_info.CurrentInfo().ID
		_, err := etcdClient.StartServerInfoReporter(
			ctx,
			nodeID,
			server_info.ReportTTL(),
			func() ([]byte, error) {
				return json.Marshal(server_info.CurrentInfo())
			},
		)
		if err != nil {
			logger.Warnf("start server-info reporter fail: %s", err)
		}
	}
	logger.Info("watch etcd")
	go etcdClient.Watch(ctx)
	return nil
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

// startHTTPListeners binds every configured HTTP and TLS listener and blocks
// until ctx is cancelled or a listener fails. Bind and serve failures are
// returned so the command can cancel the root context and enter the normal
// shutdown path.
func (s *Server) startHTTPListeners(ctx context.Context) error {
	addrs := s.addrs
	if len(addrs) == 0 {
		addrs = []string{s.addr}
	}
	tlsAddrs := configuredTLSListenAddresses()
	serveErrors := make(chan error, len(addrs)+len(tlsAddrs))
	for _, addr := range addrs {
		logger.Infof("listening on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("open listener %s: %w", addr, err)
		}
		go func(listener net.Listener) {
			if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- err
			}
		}(listener)
	}
	for _, addr := range tlsAddrs {
		logger.Infof("listening with TLS on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("open TLS listener %s: %w", addr, err)
		}
		tlsListener := tls.NewListener(listener, frontendTLSConfig())
		go func(listener net.Listener) {
			if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- err
			}
		}(tlsListener)
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErrors:
		return err
	}
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
			return store.GetSSLCertificateForSNI(serverName)
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
	return net.JoinHostPort(c.ExportIP, strconv.Itoa(c.ExportPort))
}
