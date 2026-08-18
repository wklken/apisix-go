package server

import (
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
	"sync"
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
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
	"golang.org/x/net/http2"
)

var ErrMissingStreamUpstream = errors.New("missing stream upstream")

var errStandaloneHTTPRoutePublication = errors.New("standalone HTTP route publication")

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
	addr            string
	addrs           []string
	server          *http.Server
	routes          *routeHandler
	clusters        *pxy.ClusterRegistry
	streamRuntime   streamRuntimeOwner
	reloadEventChan chan struct{}

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

func NewServer() (*Server, error) {
	events := make(chan *store.Event)
	storage, err := store.GetStore("apisix-go-store.db", events)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	routes := newRouteHandler(http.NotFoundHandler(), nil)
	handler := newConfiguredHTTPHandler(routes, config.GlobalConfig)
	addrs := configuredListenAddresses()
	otelShutdown, err := otel.Init("apisix-go")
	if err != nil {
		_ = storage.Stop()
		return nil, fmt.Errorf("initialize tracing: %w", err)
	}
	return &Server{
		addr:            addrs[0],
		addrs:           addrs,
		server:          newConfiguredHTTPServer(handler),
		routes:          routes,
		clusters:        pxy.NewClusterRegistry(metrics.NewProxyRuntimeObserver()),
		reloadEventChan: make(chan struct{}, 1),
		events:          events,
		storage:         storage,
		otelShutdown:    otelShutdown,
	}, nil
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
	if pluginConfigured("node-status") {
		handler = node_status.Track(handler)
	}
	return handler
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
		ConnState:         metrics.NewHTTPConnectionStateObserver(),
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
	if err := metrics.Init(); err != nil {
		return fmt.Errorf("initialize prometheus metrics: %w", err)
	}
	if err := s.startPrometheusExpiration(ctx); err != nil {
		return err
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
	if err := ctx.Err(); err != nil {
		return err
	}

	logger.Info("build the routes")
	initialReloadGeneration := reloadGeneration.Load()
	builder := route.NewBuilderWithClusterRegistry(s.storage, s.addr, s.clusters)
	if err := buildAndInstallInitialRoutes(s.routes, builder); err != nil {
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageHTTPRoutes)
		return err
	}
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	if err := ctx.Err(); err != nil {
		return err
	}
	reconcileInitialReloadEvent(s.reloadEventChan, initialReloadGeneration, reloadGeneration.Load)
	previousStreamRuntime := s.streamRuntime
	if err := s.startStreamProxy(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.standaloneWatcher != nil {
		if err := s.standaloneWatcher.Start(); err != nil {
			return fmt.Errorf("start standalone config watcher: %w", err)
		}
		provider := standaloneConfigProvider(config.GlobalConfig)
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

	if s.streamRuntime != nil {
		if err := s.streamRuntime.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop stream runtime: %w", err))
		}
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
	if config.GlobalConfig == nil || !streamProxyModeEnabled(config.GlobalConfig) {
		return nil
	}
	streamConfig := config.GlobalConfig.Apisix.StreamProxy
	if len(streamConfig.Tcp) == 0 {
		return fmt.Errorf("stream mode requires at least one TCP listener")
	}
	if len(streamConfig.Udp) > 0 {
		return fmt.Errorf("UDP stream listeners are not supported")
	}
	proxyProtocol := config.GlobalConfig.Apisix.ProxyProtocol
	if proxyProtocol.EnableTCPPP {
		return fmt.Errorf("stream PROXY protocol is not supported")
	}
	if proxyProtocol.EnableTCPPPToUpstream {
		return fmt.Errorf("upstream PROXY protocol is not supported")
	}

	routes, err := s.loadStreamRoutes()
	if err != nil {
		return fmt.Errorf("load stream routes: %w", err)
	}
	runtime, err := streamruntime.NewRuntime(
		ctx,
		streamConfig.Tcp,
		routes,
		config.GlobalConfig.StreamPlugins,
		logStreamResult,
	)
	if err != nil {
		return fmt.Errorf("start stream proxy: %w", err)
	}
	s.lifecycleMu.Lock()
	if s.shutdownRequested {
		s.lifecycleMu.Unlock()
		_ = runtime.Close(context.Background())
		return context.Canceled
	}
	s.streamRuntime = runtime
	s.lifecycleMu.Unlock()
	logger.Infof("stream proxy listening on %v", runtime.Addresses())
	return nil
}

func (s *Server) closeStartedStreamRuntime(runtime streamRuntimeOwner) error {
	if runtime == nil || s.streamRuntime != runtime {
		return nil
	}
	err := runtime.Close(context.Background())
	s.streamRuntime = nil
	if err != nil {
		return fmt.Errorf("stop stream runtime after startup failure: %w", err)
	}
	return nil
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
	if err := metrics.Init(); err != nil {
		return fmt.Errorf("initialize prometheus metrics: %w", err)
	}
	exportConfig := newPrometheusExportServerConfig(config.GlobalConfig.PluginAttr["prometheus"])
	exporter, _, err := metrics.StartExportServer(metrics.ExportServerConfig{
		Enabled: exportConfig.Enabled,
		URI:     exportConfig.ExportURI,
		Address: exportConfig.Address(),
	})
	if err != nil {
		return fmt.Errorf("start prometheus export server: %w", err)
	}
	s.lifecycleMu.Lock()
	if s.shutdownRequested {
		s.lifecycleMu.Unlock()
		_ = exporter.Close()
		return context.Canceled
	}
	s.prometheusServer = exporter
	s.lifecycleMu.Unlock()
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

func routeEventBucket(event *store.Event) (string, bool) {
	return store.EventBucket(event)
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
		if err := watcher.Reload(); err != nil {
			return fmt.Errorf("load standalone config: %w", err)
		}
		if err := s.storage.Sync(); err != nil {
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return fmt.Errorf("sync initial standalone config: %w", err)
		}
		metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
		watcher.SetAcknowledgedReloadCallback(func(result config.StandaloneReloadResult, err error) error {
			if applyErr := applyStandaloneSnapshot(
				result,
				err,
				s.storage.Sync,
				func() error { return s.reload(ctx) },
				func() {
					if s.streamRuntime == nil {
						return
					}
					if err := s.reloadStreamRoutes(); err != nil {
						logger.Errorf("reload stream routes fail: %s", err)
					}
				},
			); applyErr != nil {
				if errors.Is(applyErr, errStandaloneHTTPRoutePublication) {
					metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
					metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageHTTPRoutes)
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
	reloadStreams func(),
) error {
	if err != nil {
		return err
	}
	if err := syncStore(); err != nil {
		return err
	}
	if result.AffectsHTTPRoutes() {
		if err := reloadRoutes(); err != nil {
			return fmt.Errorf("%w: %w", errStandaloneHTTPRoutePublication, err)
		}
	}
	if result.AffectsStreams() {
		reloadStreams()
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
			DialTimeout:         requestTimeout,
			RequestTimeout:      requestTimeout,
			HealthCheckInterval: etcdHealthCheckInterval(etcdConfig.HealthCheckTimeout),
			StartupRetry:        etcdConfig.StartupRetry,
			TLS:                 tlsConfig,
		},
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
	producer.Start()
	return nil
}

func etcdHealthCheckInterval(timeout int) time.Duration {
	if timeout <= 0 {
		return 10 * time.Second
	}
	return time.Duration(timeout) * time.Second
}

func fetchAndSyncInitialEtcdConfig(fetch func() error, syncStore func() error) error {
	if err := fetch(); err != nil {
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
	var tlsConfig *tls.Config
	if len(tlsAddrs) > 0 {
		var err error
		tlsConfig, err = buildFrontendTLSConfig()
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
