package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/observability/otel"
	"github.com/wklken/apisix-go/pkg/plugin/node_status"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
	"github.com/wklken/apisix-go/pkg/tlsconfig"
	"github.com/wklken/apisix-go/pkg/version"
	"golang.org/x/net/http2"
)

var ErrMissingStreamUpstream = errors.New("missing stream upstream")

var (
	errStandaloneHTTPRoutePublication = errors.New("standalone HTTP route publication")
	errStandaloneStreamPublication    = errors.New("standalone stream publication")
)

type configProducer interface {
	Stop() error
}

type streamRuntimeOwner interface {
	Reload([]resource.StreamRoute) error
	Close(context.Context) error
}

type etcdClientOwner interface {
	Watch(context.Context)
	Close() error
}

type etcdConfigProducer struct {
	client etcdClientOwner
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	stopOnce    sync.Once
	stopErr     error
}

func newEtcdConfigProducer(parent context.Context, client etcdClientOwner) *etcdConfigProducer {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &etcdConfigProducer{
		client: client,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

func (p *etcdConfigProducer) Start() {
	p.lifecycleMu.Lock()
	if p.started || p.stopped || p.client == nil {
		p.lifecycleMu.Unlock()
		return
	}
	p.started = true
	p.lifecycleMu.Unlock()
	go func() {
		defer close(p.done)
		p.client.Watch(p.ctx)
	}()
}

func (p *etcdConfigProducer) Stop() error {
	p.stopOnce.Do(func() {
		p.lifecycleMu.Lock()
		p.stopped = true
		started := p.started
		p.lifecycleMu.Unlock()

		p.cancel()
		if started {
			<-p.done
		}
		if p.client != nil {
			p.stopErr = p.client.Close()
		}
	})
	return p.stopErr
}

type Server struct {
	staticConfig   *config.EffectiveConfig
	dataEncryption data_encryption.Service

	addr            string
	addrs           []string
	server          *http.Server
	routes          *routeHandler
	clusters        *pxy.ClusterRegistry
	streamRuntime   streamRuntimeOwner
	streamReloadMu  sync.Mutex
	streamRoutes    []resource.StreamRoute
	reloadEventChan chan struct{}

	reloadMu                  sync.Mutex
	httpPublicationMu         sync.Mutex
	httpPublicationAttempted  bool
	httpPublicationGeneration uint64
	httpPublicationErr        error

	events            chan *store.Event
	storage           *store.Store
	etcdClient        *etcd.ConfigClient
	standaloneWatcher *config.StandaloneFileWatcher
	producer          configProducer

	lifecycleMu       sync.Mutex
	lifecycleCancel   context.CancelFunc
	startupDone       chan struct{}
	schedulerDone     chan struct{}
	startupInProgress bool
	shutdownRequested bool

	shutdownMu         sync.Mutex
	shutdownDone       chan struct{}
	shutdownInProgress bool
	shutdownComplete   bool
	shutdownErr        error

	listenerMu sync.Mutex
	listeners  []net.Listener

	prometheusServer         *http.Server
	stopPrometheusExpiration func(context.Context) error
	otelShutdown             func(context.Context) error
}

const startupCleanupTimeout = time.Second

func NewServer(effective *config.EffectiveConfig, encryption data_encryption.Service) (*Server, error) {
	if effective == nil {
		return nil, errors.New("effective config is required")
	}
	cfg := &effective.Config
	events := make(chan *store.Event)
	storage, err := store.GetStore(config.JournalPath(effective), events, encryption)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	routes := newRouteHandler(http.NotFoundHandler(), nil)
	handler := newConfiguredHTTPHandler(routes, cfg)
	addrs := configuredListenAddresses(cfg)
	otelShutdown, err := otel.Init("apisix-go")
	if err != nil {
		_ = storage.Stop()
		return nil, fmt.Errorf("initialize tracing: %w", err)
	}
	return &Server{
		staticConfig:    effective,
		dataEncryption:  encryption,
		addr:            addrs[0],
		addrs:           addrs,
		server:          newConfiguredHTTPServer(handler, cfg),
		routes:          routes,
		clusters:        pxy.NewClusterRegistry(newClusterObserver(cfg)),
		reloadEventChan: make(chan struct{}, 1),
		events:          events,
		storage:         storage,
		otelShutdown:    otelShutdown,
	}, nil
}

func newClusterObserver(cfg *config.Config) pxy.ClusterObserver {
	if !prometheusEnabled(cfg) {
		return pxy.NopClusterObserver{}
	}
	return metrics.NewProxyRuntimeObserver()
}

func newConfiguredHTTPHandler(routes http.Handler, cfg *config.Config) http.Handler {
	handler := newHealthHandler(routes, requiresEtcdReadiness(cfg))
	var trustedAddresses []string
	if cfg != nil {
		handler = limitRequestBody(handler, cfg.NginxConfig.HTTP.ClientMaxBodySize)
		trustedAddresses = cfg.Apisix.TrustedAddresses
	}
	handler = normalizeForwardedHeaders(handler, trustedAddresses)
	if cfg != nil && cfg.Apisix.NormalizeURILikeServlet {
		handler = normalizeRequestPath(handler)
	}
	if cfg != nil && cfg.Apisix.DeleteURITailSlash {
		handler = deleteURITailSlash(handler)
	}
	if pluginConfigured(cfg, "node-status") {
		handler = node_status.Track(handler)
	}
	if prometheusEnabled(cfg) {
		next := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metrics.RecordHTTPRequestTotal()
			next.ServeHTTP(w, r)
		})
	}
	return forceServerHeader(handler, cfg)
}

func newHealthHandler(next http.Handler, requireEtcd bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/livez":
			w.WriteHeader(http.StatusOK)
		case "/readyz":
			writeReadinessResponse(w, requireEtcd)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func writeReadinessResponse(w http.ResponseWriter, requireEtcd bool) {
	state := metrics.GetReadiness()
	status := http.StatusOK
	if !state.ConfigApplyReady || (requireEtcd && !state.EtcdReachable) {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"config_apply_ready":%t,"etcd_reachable":%t}`, state.ConfigApplyReady, state.EtcdReachable)
}

func requiresEtcdReadiness(cfg *config.Config) bool {
	return cfg != nil && standaloneConfigProvider(cfg) == ""
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

func deleteURITailSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL == nil || r.URL.Path == "/" || !strings.HasSuffix(r.URL.Path, "/") {
			next.ServeHTTP(w, r)
			return
		}

		request := r.Clone(r.Context())
		requestURL := *r.URL
		requestURL.Path = strings.TrimSuffix(requestURL.Path, "/")
		requestURL.RawPath = ""
		request.URL = &requestURL
		next.ServeHTTP(w, request)
	})
}

func configuredServerToken(cfg *config.Config) string {
	if cfg != nil && !cfg.Apisix.EnableServerTokens {
		return "APISIX"
	}
	return "APISIX/" + version.Version
}

func forceServerHeader(next http.Handler, cfg *config.Config) http.Handler {
	token := configuredServerToken(cfg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", token)
		wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(status int) {
					w.Header().Set("Server", token)
					writeHeader(status)
				}
			},
			Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(p []byte) (int, error) {
					w.Header().Set("Server", token)
					return write(p)
				}
			},
			Flush: func(flush httpsnoop.FlushFunc) httpsnoop.FlushFunc {
				return func() {
					w.Header().Set("Server", token)
					flush()
				}
			},
			FlushError: func(flushError httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
				return func() error {
					w.Header().Set("Server", token)
					return flushError()
				}
			},
			ReadFrom: func(readFrom httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(src io.Reader) (int64, error) {
					w.Header().Set("Server", token)
					return readFrom(src)
				}
			},
			WriteString: func(writeString httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
				return func(value string) (int, error) {
					w.Header().Set("Server", token)
					return writeString(value)
				}
			},
		})
		next.ServeHTTP(wrapped, r)
	})
}

func configuredListenAddresses(cfg *config.Config) []string {
	if cfg == nil {
		return []string{":8080"}
	}
	return cfg.Apisix.ListenAddresses()
}

func configuredTLSListenAddresses(cfg *config.Config) []string {
	if !tlsconfig.FrontendEnabled(cfg) {
		return nil
	}
	listeners := cfg.Apisix.Ssl.Listen
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

func newConfiguredHTTPServer(handler http.Handler, cfg *config.Config) *http.Server {
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	if frontendHTTP2Enabled(cfg) {
		protocols.SetHTTP2(true)
	}
	if frontendPlainHTTP2Enabled(cfg) {
		protocols.SetUnencryptedHTTP2(true)
	}
	server := &http.Server{
		Handler:           handler,
		Protocols:         protocols,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultHTTPIdleTimeout,
	}
	if prometheusEnabled(cfg) {
		server.ConnState = metrics.NewHTTPConnectionStateObserver()
	}
	if frontendHTTP2Enabled(cfg) {
		if err := http2.ConfigureServer(server, nil); err != nil {
			logger.Errorf("configure HTTP/2 server: %s", err)
		}
	}
	if cfg == nil {
		return server
	}

	httpConfig := cfg.NginxConfig.HTTP
	if httpConfig.KeepaliveTimeout > 0 {
		server.IdleTimeout = httpConfig.KeepaliveTimeout
	}
	if httpConfig.ClientHeaderTimeout > 0 {
		server.ReadHeaderTimeout = httpConfig.ClientHeaderTimeout
	}
	if httpConfig.ClientBodyTimeout > 0 {
		server.ReadTimeout = httpConfig.ClientBodyTimeout + httpConfig.ClientHeaderTimeout
	}
	return server
}

func pluginConfigured(cfg *config.Config, name string) bool {
	if cfg == nil {
		return false
	}
	return slices.Contains(cfg.Plugins, name)
}

func (s *Server) beginStart(parent context.Context) (context.Context, bool) {
	if parent == nil {
		parent = context.Background()
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.shutdownRequested || s.startupInProgress {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	s.lifecycleCancel = cancel
	s.startupDone = make(chan struct{})
	s.startupInProgress = true
	return ctx, true
}

func (s *Server) finishStart() {
	s.lifecycleMu.Lock()
	if s.startupInProgress {
		s.startupInProgress = false
		close(s.startupDone)
	}
	s.lifecycleMu.Unlock()
}

func (s *Server) retainProducer(producer configProducer) error {
	s.lifecycleMu.Lock()
	if s.shutdownRequested {
		s.lifecycleMu.Unlock()
		_ = producer.Stop()
		return context.Canceled
	}
	s.producer = producer
	s.lifecycleMu.Unlock()
	return nil
}

func (s *Server) startReloadScheduler(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	if s.shutdownRequested || s.schedulerDone != nil {
		s.lifecycleMu.Unlock()
		return
	}
	schedulerDone := make(chan struct{})
	s.schedulerDone = schedulerDone
	if s.lifecycleCancel == nil {
		ctx, s.lifecycleCancel = context.WithCancel(ctx)
	}
	s.lifecycleMu.Unlock()
	go func() {
		defer close(schedulerDone)
		s.listenReloadEvent(ctx)
	}()
}

func (s *Server) lifecycleResourcesForShutdown() (
	context.CancelFunc,
	configProducer,
	chan struct{},
	chan struct{},
) {
	s.lifecycleMu.Lock()
	s.shutdownRequested = true
	cancel := s.lifecycleCancel
	producer := s.producer
	startupDone := s.startupDone
	schedulerDone := s.schedulerDone
	s.lifecycleMu.Unlock()
	return cancel, producer, startupDone, schedulerDone
}

func (s *Server) cleanupAfterStart() error {
	ctx, cancel := context.WithTimeout(context.Background(), startupCleanupTimeout)
	err := s.shutdown(ctx)
	cancel()
	if err == nil {
		return nil
	}
	// Stop producers and reload work even when HTTP quiescence is not yet
	// possible. Route generations and Store remain owned until active handlers
	// have exited and a later shutdown attempt can safely release them.
	lifecycleCtx, lifecycleCancel := context.WithTimeout(context.Background(), startupCleanupTimeout)
	if lifecycleErr, _ := s.stopProducerAndScheduler(lifecycleCtx); lifecycleErr != nil {
		err = errors.Join(err, lifecycleErr)
	}
	lifecycleCancel()
	s.closeOwnedListeners()
	if s.server != nil {
		// A startup error must not be held hostage by a request that cannot
		// drain. Close listeners and active connections as a bounded fallback.
		// net/http does not terminate handler goroutines, so dependency cleanup
		// continues only after those handlers release their route generation.
		_ = s.server.Close()
	}
	go func() {
		if cleanupErr := s.shutdown(context.Background()); cleanupErr != nil {
			logger.Errorf("finish server cleanup after startup failure: %s", cleanupErr)
		}
	}()
	return err
}

func (s *Server) retainListener(listener net.Listener) {
	s.listenerMu.Lock()
	s.listeners = append(s.listeners, listener)
	s.listenerMu.Unlock()
}

func (s *Server) releaseListener(listener net.Listener) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	for index, owned := range s.listeners {
		if owned != listener {
			continue
		}
		s.listeners = append(s.listeners[:index], s.listeners[index+1:]...)
		return
	}
}

func (s *Server) closeOwnedListeners() {
	s.listenerMu.Lock()
	listeners := append([]net.Listener(nil), s.listeners...)
	s.listeners = nil
	s.listenerMu.Unlock()
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

// Start runs the server until ctx is cancelled or a listener fails. Startup
// failures are returned with operation context instead of panicking; the
// command owns the process shutdown path.
func (s *Server) Start(ctx context.Context) (startErr error) {
	cfg := &s.staticConfig.Config
	runCtx, ok := s.beginStart(ctx)
	if !ok {
		return context.Canceled
	}
	ctx = runCtx
	defer func() {
		s.finishStart()
		if err := s.cleanupAfterStart(); err != nil {
			startErr = errors.Join(startErr, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if prometheusEnabled(cfg) {
		if err := metrics.Init(cfg.PluginAttr["prometheus"]); err != nil {
			return fmt.Errorf("initialize prometheus metrics: %w", err)
		}
		if err := s.startPrometheusExpiration(ctx); err != nil {
			return err
		}
	}
	metrics.SetConfigApplyStreamRequired(streamProxyModeEnabled(cfg))
	if standaloneConfigProvider(cfg) == "" {
		s.registerAcknowledgedStoreUpdateHook(ctx)
	}

	logger.Info("Starting storage")
	s.storage.Start()
	if err := s.startConfigProvider(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if standaloneConfigProvider(cfg) != "" {
		logger.Info("build the routes")
		builder := route.NewBuilderWithClusterRegistry(
			s.storage, s.addr, s.clusters, s.staticConfig, s.dataEncryption.Resolver(),
		)
		if err := buildAndInstallInitialRoutes(s.routes, builder); err != nil {
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageHTTPRoutes)
			return err
		}
		metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	previousStreamRuntime := s.streamRuntime
	if err := s.startStreamProxy(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.standaloneWatcher != nil {
		if err := s.standaloneWatcher.StartAndReconcile(); err != nil {
			return fmt.Errorf("start standalone config watcher: %w", err)
		}
		provider := standaloneConfigProvider(cfg)
		logger.Infof("watch standalone config %s", config.StandaloneConfigFile(provider))
	}

	// start the reloader
	s.startReloadScheduler(ctx)

	return s.startServing(
		ctx,
		previousStreamRuntime,
		s.startPrometheusExportServer,
		s.startHTTPListeners,
	)
}

func (s *Server) startServing(
	ctx context.Context,
	previousStreamRuntime streamRuntimeOwner,
	startPrometheus func() error,
	startHTTP func(context.Context) error,
) error {
	var startedStreamRuntime streamRuntimeOwner
	if s.streamRuntime != previousStreamRuntime {
		startedStreamRuntime = s.streamRuntime
	}
	if err := startPrometheus(); err != nil {
		return errors.Join(err, s.closeStartedStreamRuntime(startedStreamRuntime))
	}
	if err := startHTTP(ctx); err != nil {
		return errors.Join(err, s.closeStartedStreamRuntime(startedStreamRuntime))
	}
	return nil
}

// Shutdown gracefully stops the HTTP listeners and the observability and
// config dependencies owned by the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

type runtimeRouteBuilder interface {
	BuildWithRouteQuarantine() (*chi.Mux, error)
	Stop()
}

func buildAndInstallInitialRoutes(routes *routeHandler, builder runtimeRouteBuilder) error {
	handler, err := builder.BuildWithRouteQuarantine()
	if err != nil {
		builder.Stop()
		return fmt.Errorf("build initial routes: %w", err)
	}
	routes.Replace(handler, builder.Stop)
	recordRouteBuildQuarantine(builder)
	return nil
}

type quarantineAwareRouteBuilder interface {
	QuarantinedResourceCount() int
}

func recordRouteBuildQuarantine(builder any) {
	if quarantineAware, ok := builder.(quarantineAwareRouteBuilder); ok {
		metrics.RecordConfigApplyStoreQuarantine(quarantineAware.QuarantinedResourceCount())
	}
}

func (s *Server) shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		s.shutdownMu.Lock()
		if s.shutdownComplete {
			err := s.shutdownErr
			s.shutdownMu.Unlock()
			return err
		}
		if s.shutdownInProgress {
			done := s.shutdownDone
			s.shutdownMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		s.shutdownInProgress = true
		s.shutdownDone = make(chan struct{})
		done := s.shutdownDone
		s.shutdownMu.Unlock()

		err, complete := s.shutdownAttempt(ctx)

		s.shutdownMu.Lock()
		s.shutdownErr = err
		s.shutdownComplete = complete
		s.shutdownInProgress = false
		close(done)
		s.shutdownMu.Unlock()
		return err
	}
}

func (s *Server) shutdownAttempt(ctx context.Context) (error, bool) {
	if s.server != nil {
		if err := s.server.Shutdown(ctx); err != nil {
			return fmt.Errorf("stop HTTP server: %w", err), false
		}
	}
	if err := s.stopPrometheusExpirationRuntime(ctx); err != nil {
		return err, false
	}

	producerErr, lifecycleComplete := s.stopProducerAndScheduler(ctx)
	if !lifecycleComplete {
		return producerErr, false
	}
	var errs []error
	if producerErr != nil {
		errs = append(errs, producerErr)
	}

	s.streamReloadMu.Lock()
	streamRuntime := s.streamRuntime
	if streamRuntime != nil {
		if err := streamRuntime.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop stream runtime: %w", err))
		}
		s.streamRuntime = nil
		s.streamRoutes = nil
	}
	s.streamReloadMu.Unlock()
	if streamRuntime != nil {
		metrics.SetStreamRoutes(nil)
	}
	if s.routes != nil {
		s.routes.Close()
	}
	if s.clusters != nil {
		s.clusters.Close()
	}
	if s.storage != nil {
		if err := s.storage.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop store: %w", err))
		}
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
	return errors.Join(errs...), true
}

func (s *Server) startPrometheusExpiration(ctx context.Context) error {
	stop, err := metrics.StartExpiration(ctx)
	if err != nil {
		return fmt.Errorf("start prometheus metric expiration: %w", err)
	}
	return s.retainPrometheusExpiration(stop)
}

func (s *Server) retainPrometheusExpiration(stop func(context.Context) error) error {
	if stop == nil {
		return nil
	}

	s.lifecycleMu.Lock()
	if !s.shutdownRequested {
		s.stopPrometheusExpiration = stop
		s.lifecycleMu.Unlock()
		return nil
	}
	s.lifecycleMu.Unlock()

	stopErr := stop(context.Background())
	return errors.Join(context.Canceled, stopErr)
}

func (s *Server) stopPrometheusExpirationRuntime(ctx context.Context) error {
	s.lifecycleMu.Lock()
	s.shutdownRequested = true
	stop := s.stopPrometheusExpiration
	s.lifecycleMu.Unlock()
	if stop == nil {
		return nil
	}
	if err := stop(ctx); err != nil {
		return fmt.Errorf("stop prometheus metric expiration: %w", err)
	}
	s.lifecycleMu.Lock()
	if s.stopPrometheusExpiration != nil {
		s.stopPrometheusExpiration = nil
	}
	s.lifecycleMu.Unlock()
	return nil
}

func (s *Server) stopProducerAndScheduler(ctx context.Context) (error, bool) {
	cancel, producer, startupDone, schedulerDone := s.lifecycleResourcesForShutdown()
	var errs []error
	if producer != nil {
		if err := producer.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("stop config producer: %w", err))
		}
	} else {
		s.lifecycleMu.Lock()
		standaloneWatcher := s.standaloneWatcher
		etcdClient := s.etcdClient
		s.lifecycleMu.Unlock()
		if standaloneWatcher != nil {
			if err := standaloneWatcher.Stop(); err != nil {
				errs = append(errs, fmt.Errorf("stop standalone config watcher: %w", err))
			}
		} else if etcdClient != nil {
			if err := etcdClient.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close etcd client: %w", err))
			}
		}
	}
	if cancel != nil {
		cancel()
	}
	if err := waitForLifecycle(ctx, startupDone); err != nil {
		errs = append(errs, fmt.Errorf("wait for startup: %w", err))
		return errors.Join(errs...), false
	}
	if err := waitForLifecycle(ctx, schedulerDone); err != nil {
		errs = append(errs, fmt.Errorf("wait for reload scheduler: %w", err))
		return errors.Join(errs...), false
	}
	return errors.Join(errs...), true
}

func waitForLifecycle(ctx context.Context, done chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) startStreamProxy(ctx context.Context) error {
	cfg := &s.staticConfig.Config
	if !streamProxyModeEnabled(cfg) {
		return nil
	}
	fail := func(err error) error {
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageStreams)
		return err
	}
	streamConfig := cfg.Apisix.StreamProxy
	if len(streamConfig.Tcp) == 0 {
		return fail(fmt.Errorf("stream mode requires at least one TCP listener"))
	}
	if len(streamConfig.Udp) > 0 {
		return fail(fmt.Errorf("UDP stream listeners are not supported"))
	}
	proxyProtocol := cfg.Apisix.ProxyProtocol
	if proxyProtocol.EnableTCPPP {
		return fail(fmt.Errorf("stream PROXY protocol is not supported"))
	}
	if proxyProtocol.EnableTCPPPToUpstream {
		return fail(fmt.Errorf("upstream PROXY protocol is not supported"))
	}

	// Serialize the initial load/publication with acknowledged dynamic reloads.
	// An event committed after the initial read either blocks here and reloads
	// the published runtime, or completes first and is included by this read.
	s.streamReloadMu.Lock()
	defer s.streamReloadMu.Unlock()
	routes, candidate, err := s.loadStreamRoutes()
	if err != nil {
		return fail(fmt.Errorf("load stream routes: %w", err))
	}
	runtime, err := streamruntime.NewRuntime(
		ctx,
		streamConfig.Tcp,
		routes,
		cfg.StreamPlugins,
		logStreamResult,
	)
	if err != nil {
		return fail(fmt.Errorf("start stream proxy: %w", err))
	}
	s.lifecycleMu.Lock()
	if s.shutdownRequested {
		s.lifecycleMu.Unlock()
		_ = runtime.Close(context.Background())
		return fail(context.Canceled)
	}
	s.streamRuntime = runtime
	s.lifecycleMu.Unlock()
	s.streamRoutes = routes
	store.CommitStreamRouteLastGood(candidate)
	metrics.SetStreamRoutes(streamRouteIDs(routes))
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageStreams)
	logger.Infof("stream proxy listening on %v", runtime.Addresses())
	return nil
}

func (s *Server) closeStartedStreamRuntime(runtime streamRuntimeOwner) error {
	s.streamReloadMu.Lock()
	defer s.streamReloadMu.Unlock()
	if runtime == nil || s.streamRuntime != runtime {
		return nil
	}
	err := runtime.Close(context.Background())
	s.streamRuntime = nil
	s.streamRoutes = nil
	metrics.SetStreamRoutes(nil)
	if err != nil {
		return fmt.Errorf("stop stream runtime after startup failure: %w", err)
	}
	return nil
}

// startPrometheusExportServer starts the prometheus export server when the
// plugin is enabled and retains it as an owned lifecycle resource.
func (s *Server) startPrometheusExportServer() error {
	cfg := &s.staticConfig.Config
	if !prometheusEnabled(cfg) {
		return nil
	}
	attr := cfg.PluginAttr["prometheus"]
	if err := metrics.Init(attr); err != nil {
		return fmt.Errorf("initialize prometheus metrics: %w", err)
	}
	exportConfig, err := metrics.ConfiguredExportServer(attr)
	if err != nil {
		return fmt.Errorf("configure prometheus export server: %w", err)
	}
	exporter, _, err := metrics.StartExportServer(exportConfig)
	if err != nil {
		return fmt.Errorf("start prometheus export server: %w", err)
	}
	s.lifecycleMu.Lock()
	if s.shutdownRequested {
		s.lifecycleMu.Unlock()
		if exporter != nil {
			_ = exporter.Close()
		}
		return context.Canceled
	}
	s.prometheusServer = exporter
	s.lifecycleMu.Unlock()
	return nil
}

func prometheusEnabled(cfg *config.Config) bool {
	return cfg != nil && slices.Contains(cfg.Plugins, "prometheus")
}

func (s *Server) loadStreamRoutes() ([]resource.StreamRoute, map[string]resource.StreamRoute, error) {
	routes, candidate, err := store.PrepareStreamRoutes()
	if err != nil {
		return nil, nil, err
	}
	resolved, err := resolveStreamRoutesWithServices(routes, store.GetUpstream, store.GetService)
	if err != nil {
		return nil, nil, err
	}
	return resolved, candidate, nil
}

func (s *Server) reloadStreamRoutes() error {
	_, err := s.reloadStreamRoutesIfStarted()
	return err
}

func (s *Server) reloadStreamRoutesIfStarted() (bool, error) {
	s.streamReloadMu.Lock()
	defer s.streamReloadMu.Unlock()
	if s.streamRuntime == nil {
		return false, nil
	}
	routes, candidate, err := s.loadStreamRoutes()
	if err != nil {
		return true, err
	}
	if reflect.DeepEqual(routes, s.streamRoutes) {
		store.CommitStreamRouteLastGood(candidate)
		return true, nil
	}
	if err := s.streamRuntime.Reload(routes); err != nil {
		return true, err
	}
	s.streamRoutes = routes
	store.CommitStreamRouteLastGood(candidate)
	metrics.SetStreamRoutes(streamRouteIDs(routes))
	return true, nil
}

func streamRouteIDs(routes []resource.StreamRoute) []string {
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.ID != "" {
			ids = append(ids, route.ID)
		}
	}
	return ids
}

func resolveStreamRoutes(
	routes []resource.StreamRoute,
	lookup func(string) (resource.Upstream, error),
) ([]resource.StreamRoute, error) {
	return resolveStreamRoutesWithServices(routes, lookup, nil)
}

func resolveStreamRoutesWithServices(
	routes []resource.StreamRoute,
	lookupUpstream func(string) (resource.Upstream, error),
	lookupService func(string) (resource.Service, error),
) ([]resource.StreamRoute, error) {
	resolved := make([]resource.StreamRoute, len(routes))
	copy(resolved, routes)
	for index := range resolved {
		route := &resolved[index]
		if route.ServiceID != "" && route.UpstreamID == "" {
			if lookupService == nil {
				return nil, fmt.Errorf(
					"stream route %q references service %q: service lookup is unavailable",
					route.ID,
					route.ServiceID,
				)
			}
			service, err := lookupService(route.ServiceID)
			if err != nil {
				return nil, fmt.Errorf("stream route %q references service %q: %w", route.ID, route.ServiceID, err)
			}
			mergeStreamService(route, service)
		}
		if route.UpstreamID == "" || len(route.Upstream.Nodes) > 0 {
			continue
		}
		if lookupUpstream == nil {
			return nil, fmt.Errorf(
				"stream route %q references upstream %q: %w",
				route.ID,
				route.UpstreamID,
				ErrMissingStreamUpstream,
			)
		}
		upstream, err := lookupUpstream(route.UpstreamID)
		if err != nil {
			return nil, fmt.Errorf("stream route %q references upstream %q: %w", route.ID, route.UpstreamID, err)
		}
		route.Upstream = upstream
	}
	return resolved, nil
}

func mergeStreamService(route *resource.StreamRoute, service resource.Service) {
	if route == nil {
		return
	}
	if len(service.Plugins) > 0 {
		plugins := make(map[string]resource.PluginConfig, len(service.Plugins)+len(route.Plugins))
		maps.Copy(plugins, service.Plugins)
		maps.Copy(plugins, route.Plugins)
		route.Plugins = plugins
	}
	if route.UpstreamID == "" && len(route.Upstream.Nodes) == 0 {
		route.Upstream = service.Upstream
		route.UpstreamID = service.UpstreamID
	}
}

func streamProxyModeEnabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	mode := strings.ToLower(strings.ReplaceAll(cfg.Apisix.ProxyMode, " ", ""))
	return slices.Contains(strings.Split(mode, "&"), "stream")
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

func (s *Server) registerAcknowledgedStoreUpdateHook(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.storage.AddAcknowledgedEventUpdateHook(func(event *store.Event) error {
		return s.handleAcknowledgedStoreEvent(event, func() error {
			return s.publishAcknowledgedHTTPGeneration(
				s.storage.HTTPConfigGeneration(),
				func() error { return s.reloadAcknowledgedHTTP(ctx) },
			)
		})
	})
}

func (s *Server) publishAcknowledgedHTTPGeneration(generation uint64, reload func() error) error {
	s.httpPublicationMu.Lock()
	defer s.httpPublicationMu.Unlock()
	if s.httpPublicationAttempted && s.httpPublicationGeneration == generation {
		return s.httpPublicationErr
	}
	err := reload()
	s.httpPublicationAttempted = true
	s.httpPublicationGeneration = generation
	s.httpPublicationErr = err
	return err
}

func (s *Server) handleAcknowledgedStoreEvent(event *store.Event, reloadHTTP func() error) error {
	if isHTTPRouteEvent(event) && reloadHTTP != nil {
		if err := reloadHTTP(); err != nil {
			return err
		}
	}
	if !isStreamRouteEvent(event) {
		return nil
	}
	started, err := s.reloadStreamRoutesIfStarted()
	if err != nil {
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageStreams)
		logger.Errorf("reload stream routes fail: %s", err)
		return err
	}
	if !started {
		return nil
	}
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageStreams)
	return nil
}

func (s *Server) reloadAcknowledgedHTTP(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := s.reload(ctx); err != nil {
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageHTTPRoutes)
		return err
	}
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	return nil
}

func routeEventBucket(event *store.Event) (string, bool) {
	return store.EventBucket(event)
}

func logStreamResult(result streamruntime.Result) {
	metrics.RecordStreamConnection(result.RouteID)
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
	provider := standaloneConfigProvider(&s.staticConfig.Config)
	if provider != "" {
		path := config.StandaloneConfigFile(provider)
		watcher := config.NewStandaloneFileWatcher(path, provider, s.events, s.dataEncryption)
		s.lifecycleMu.Lock()
		s.standaloneWatcher = watcher
		s.lifecycleMu.Unlock()
		if err := s.retainProducer(watcher); err != nil {
			return fmt.Errorf("retain standalone config watcher: %w", err)
		}
		snapshot, err := s.storage.SnapshotBuckets(config.StandaloneBuckets())
		if err != nil {
			return fmt.Errorf("snapshot standalone store: %w", err)
		}
		watcher.SeedCurrentSnapshot(snapshot)
		initialResult, err := watcher.ReloadSnapshot()
		if err != nil {
			return fmt.Errorf("load standalone config: %w", err)
		}
		if err := s.storage.Sync(); err != nil {
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return fmt.Errorf("sync initial standalone config: %w", err)
		}
		metrics.RecordConfigApplyQuarantine(initialResult.QuarantinedResourceCount())
		metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
		watcher.SetAcknowledgedReloadCallback(func(result config.StandaloneReloadResult, err error) error {
			if applyErr := applyStandaloneSnapshot(
				result,
				err,
				s.storage.Sync,
				func() error { return s.reload(ctx) },
				func() error {
					if s.streamRuntime == nil {
						return nil
					}
					return s.reloadStreamRoutes()
				},
			); applyErr != nil {
				if errors.Is(applyErr, errStandaloneHTTPRoutePublication) {
					metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
					metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageHTTPRoutes)
				} else if errors.Is(applyErr, errStandaloneStreamPublication) {
					metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
					metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageStreams)
				} else {
					metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
				}
				logger.Errorf("apply standalone config: %s", applyErr)
				return applyErr
			}
			metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
			if result.AffectsHTTPRoutes() {
				metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
			}
			if result.AffectsStreams() && s.streamRuntime != nil {
				metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageStreams)
			}
			return nil
		})
		return nil
	}
	return s.startEtcdWatcher(ctx)
}

func applyStandaloneSnapshot(
	result config.StandaloneReloadResult,
	err error,
	syncStore func() error,
	reloadRoutes func() error,
	reloadStreams func() error,
) error {
	if err != nil {
		return err
	}
	metrics.RecordConfigApplyQuarantine(result.QuarantinedResourceCount())
	if err := syncStore(); err != nil {
		return err
	}
	if result.AffectsHTTPRoutes() {
		if err := reloadRoutes(); err != nil {
			return fmt.Errorf("%w: %w", errStandaloneHTTPRoutePublication, err)
		}
	}
	if result.AffectsStreams() {
		if err := reloadStreams(); err != nil {
			return fmt.Errorf("%w: %w", errStandaloneStreamPublication, err)
		}
	}
	return nil
}

func standaloneConfigProvider(cfg *config.Config) string {
	provider, err := config.EffectiveConfigProvider(cfg)
	if err != nil {
		return ""
	}
	if provider != "yaml" && provider != "json" {
		return ""
	}
	return provider
}

func (s *Server) startEtcdWatcher(ctx context.Context) error {
	cfg := &s.staticConfig.Config
	etcdConfig := cfg.Deployment.Etcd
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
	logger.Info("Starting etcd client")
	etcdClient, err := etcd.NewConfigClientWithOptions(
		endpoints,
		username,
		password,
		prefix,
		s.events,
		etcdClientOptions(etcdConfig, tlsConfig),
	)
	if err != nil {
		return fmt.Errorf("start etcd client: %w", err)
	}
	s.lifecycleMu.Lock()
	s.etcdClient = etcdClient
	s.lifecycleMu.Unlock()
	producer := newEtcdConfigProducer(ctx, etcdClient)
	if err := s.retainProducer(producer); err != nil {
		return fmt.Errorf("retain etcd config watcher: %w", err)
	}
	logger.Info("fetch full data from etcd")
	err = fetchAndSyncInitialEtcdConfigContext(ctx, etcdClient.FetchAllContext, s.storage.Sync)
	if err != nil {
		return fmt.Errorf("fetch initial etcd config: %w", err)
	}
	if serverInfoReportingEnabled(cfg) {
		nodeID := server_info.CurrentInfo(cfg.Apisix.ID).ID
		attr := cfg.PluginAttr["server-info"]
		_, err := etcdClient.StartServerInfoReporter(
			ctx,
			nodeID,
			server_info.ReportTTL(attr),
			func() ([]byte, error) {
				return json.Marshal(server_info.CurrentInfo(cfg.Apisix.ID))
			},
		)
		if err != nil {
			logger.Warnf("start server-info reporter fail: %s", err)
		}
	}
	logger.Info("watch etcd")
	producer.Start()
	return nil
}

func etcdHealthCheckInterval(timeout int) time.Duration {
	if timeout <= 0 {
		return 10 * time.Second
	}
	return time.Duration(timeout) * time.Second
}

func etcdClientOptions(etcdConfig config.Etcd, tlsConfig *tls.Config) etcd.ClientOptions {
	requestTimeout := 5 * time.Second
	if etcdConfig.Timeout > 0 {
		requestTimeout = time.Duration(etcdConfig.Timeout) * time.Second
	}
	return etcd.ClientOptions{
		DialTimeout:         requestTimeout,
		RequestTimeout:      requestTimeout,
		HealthCheckInterval: etcdHealthCheckInterval(etcdConfig.HealthCheckTimeout),
		StartupRetry:        etcdConfig.StartupRetry,
		WatchTimeout:        time.Duration(etcdConfig.WatchTimeout) * time.Second,
		ResyncDelay:         time.Duration(etcdConfig.ResyncDelay) * time.Second,
		TLS:                 tlsConfig,
	}
}

func fetchAndSyncInitialEtcdConfig(fetch func() error, syncStore func() error) error {
	return fetchAndSyncInitialEtcdConfigContext(
		context.Background(),
		func(context.Context) error { return fetch() },
		syncStore,
	)
}

func fetchAndSyncInitialEtcdConfigContext(
	ctx context.Context,
	fetch func(context.Context) error,
	syncStore func() error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := fetch(ctx); err != nil {
		return err
	}
	if err := syncStore(); err != nil {
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
		return err
	}
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

func serverInfoReportingEnabled(cfg *config.Config) bool {
	if !pluginConfigured(cfg, "server-info") || cfg == nil {
		return false
	}
	if strings.EqualFold(cfg.Deployment.Role, "data_plane") {
		return false
	}
	return strings.EqualFold(cfg.Deployment.RoleTraditional.ConfigProvider, "etcd")
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
	cfg := &s.staticConfig.Config
	tlsAddrs := configuredTLSListenAddresses(cfg)
	var tlsConfig *tls.Config
	if len(tlsAddrs) > 0 {
		var err error
		tlsConfig, err = buildFrontendTLSConfig(cfg)
		if err != nil {
			return fmt.Errorf("build frontend TLS config: %w", err)
		}
	}
	serveErrors := make(chan error, len(addrs)+len(tlsAddrs))
	listeners := make([]net.Listener, 0, len(addrs)+len(tlsAddrs))
	for _, addr := range addrs {
		logger.Infof("listening on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			s.closeOwnedListeners()
			return fmt.Errorf("open listener %s: %w", addr, err)
		}
		s.retainListener(listener)
		listeners = append(listeners, listener)
	}
	for _, addr := range tlsAddrs {
		logger.Infof("listening with TLS on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			s.closeOwnedListeners()
			return fmt.Errorf("open TLS listener %s: %w", addr, err)
		}
		tlsListener := tls.NewListener(listener, tlsConfig)
		s.retainListener(tlsListener)
		listeners = append(listeners, tlsListener)
	}
	for _, listener := range listeners {
		go func(listener net.Listener) {
			defer s.releaseListener(listener)
			if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- err
			}
		}(listener)
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErrors:
		return err
	}
}

func frontendHTTP2Enabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Apisix.EnableHttp2 {
		return true
	}
	for _, listener := range cfg.Apisix.Ssl.Listen {
		if listener.EnableHttp2 {
			return true
		}
	}
	return false
}

func frontendPlainHTTP2Enabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Apisix.EnableHttp2 {
		return true
	}
	for _, listener := range cfg.Apisix.NodeListen {
		if listener.EnableHttp2 {
			return true
		}
	}
	return false
}
