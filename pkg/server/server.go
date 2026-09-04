package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/observability/otel"
	"github.com/wklken/apisix-go/pkg/plugin/node_status"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
	"github.com/wklken/apisix-go/pkg/tlsconfig"
	"github.com/wklken/apisix-go/pkg/version"
	"golang.org/x/net/http2"
)

type configProducer interface {
	Start(context.Context) error
	Stop() error
}

type streamRuntimeOwner interface {
	Close(context.Context) error
}

type etcdClientOwner interface {
	FetchAllContext(context.Context) error
	Watch(context.Context)
	Close() error
}

type etcdConfigProducer struct {
	client       etcdClientOwner
	afterInitial func(context.Context)
	ctx          context.Context
	cancel       context.CancelFunc
	startDone    chan struct{}
	watchDone    chan struct{}

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	stopOnce    sync.Once
	startOnce   sync.Once
	startErr    error
	stopErr     error
}

func newEtcdConfigProducer(client etcdClientOwner) *etcdConfigProducer {
	return &etcdConfigProducer{
		client:    client,
		startDone: make(chan struct{}),
		watchDone: make(chan struct{}),
	}
}

func (p *etcdConfigProducer) Start(parent context.Context) error {
	p.startOnce.Do(func() {
		defer close(p.startDone)
		if parent == nil {
			parent = context.Background()
		}
		p.lifecycleMu.Lock()
		if p.stopped || p.client == nil {
			p.startErr = context.Canceled
			p.lifecycleMu.Unlock()
			close(p.watchDone)
			return
		}
		p.ctx, p.cancel = context.WithCancel(parent)
		p.started = true
		ctx := p.ctx
		p.lifecycleMu.Unlock()

		if err := p.client.FetchAllContext(ctx); err != nil {
			if ctx.Err() != nil {
				p.startErr = ctx.Err()
				close(p.watchDone)
				return
			}
			metrics.RecordEtcdReachable(false)
			p.startErr = fmt.Errorf("initial etcd reconciliation: %w", err)
			close(p.watchDone)
			return
		}
		if p.afterInitial != nil {
			p.afterInitial(ctx)
		}
		go func() {
			defer close(p.watchDone)
			p.client.Watch(ctx)
		}()
	})
	<-p.startDone
	return p.startErr
}

func (p *etcdConfigProducer) Stop() error {
	p.stopOnce.Do(func() {
		p.lifecycleMu.Lock()
		p.stopped = true
		started := p.started
		p.lifecycleMu.Unlock()

		if p.cancel != nil {
			p.cancel()
		}
		if started {
			<-p.startDone
			<-p.watchDone
		}
		if p.client != nil {
			p.stopErr = p.client.Close()
		}
	})
	return p.stopErr
}

type standaloneConfigProducer struct {
	watcher *config.StandaloneFileWatcher
}

func (p *standaloneConfigProducer) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.watcher == nil {
		return errors.New("standalone config watcher is required")
	}
	startDone := make(chan error, 1)
	go func() {
		startDone <- p.watcher.StartAndReconcile()
	}()
	select {
	case err := <-startDone:
		return err
	case <-ctx.Done():
		return errors.Join(ctx.Err(), p.watcher.Stop(), <-startDone)
	}
}

func (p *standaloneConfigProducer) Stop() error {
	if p == nil || p.watcher == nil {
		return nil
	}
	return p.watcher.Stop()
}

type generationEngineOwner interface {
	generation.PublicationEngine
	Close(context.Context) error
	acquireHTTP() (httpGenerationLease, bool)
	acquireStream() (streamGenerationLease, bool)
	refreshStreamMetrics()
}

type newServerFactories struct {
	initObservability func(string) (func(context.Context) error, error)
	newFactory        func(
		*config.EffectiveConfig,
		secret.Materializer,
		compiler.WorkerRuntimeObservers,
	) (*compiler.WorkerCompilerFactory, error)
	newEngine      func(*Server, *compiler.WorkerCompilerFactory) (generationEngineOwner, error)
	newCoordinator func(generation.PublicationEngine) *generation.Coordinator
	closeFactory   func(*compiler.WorkerCompilerFactory, context.Context) error
	closeResolver  func(*secret.GenerationSecretResolver, context.Context) error
}

type serverRuntimeFactories struct {
	newStream func(
		context.Context,
		[]config.TcpListen,
		streamruntime.RouterSource,
	) (streamRuntimeOwner, error)
	startHTTP   func(*Server, context.Context) (<-chan error, error)
	newProducer func(*Server, context.Context) (configProducer, error)
}

func defaultServerRuntimeFactories() serverRuntimeFactories {
	return serverRuntimeFactories{
		newStream: func(
			ctx context.Context,
			listeners []config.TcpListen,
			source streamruntime.RouterSource,
		) (streamRuntimeOwner, error) {
			return streamruntime.NewRuntime(ctx, listeners, source)
		},
		startHTTP: func(server *Server, ctx context.Context) (<-chan error, error) {
			return server.startHTTPListenerRuntime(ctx)
		},
		newProducer: func(server *Server, ctx context.Context) (configProducer, error) {
			return server.constructConfigProducer(ctx)
		},
	}
}

func (factories serverRuntimeFactories) withDefaults() serverRuntimeFactories {
	defaults := defaultServerRuntimeFactories()
	if factories.newStream == nil {
		factories.newStream = defaults.newStream
	}
	if factories.startHTTP == nil {
		factories.startHTTP = defaults.startHTTP
	}
	if factories.newProducer == nil {
		factories.newProducer = defaults.newProducer
	}
	return factories
}

func defaultNewServerFactories() newServerFactories {
	return newServerFactories{
		initObservability: otel.Init,
		newFactory:        compiler.NewWorkerCompilerFactory,
		newEngine: func(server *Server, factory *compiler.WorkerCompilerFactory) (generationEngineOwner, error) {
			return NewGenerationEngine(server, factory)
		},
		newCoordinator: generation.NewCoordinator,
		closeFactory: func(factory *compiler.WorkerCompilerFactory, ctx context.Context) error {
			return factory.Close(ctx)
		},
		closeResolver: func(resolver *secret.GenerationSecretResolver, ctx context.Context) error {
			return resolver.Close(ctx)
		},
	}
}

func (factories newServerFactories) withDefaults() newServerFactories {
	defaults := defaultNewServerFactories()
	if factories.initObservability == nil {
		factories.initObservability = defaults.initObservability
	}
	if factories.newFactory == nil {
		factories.newFactory = defaults.newFactory
	}
	if factories.newEngine == nil {
		factories.newEngine = defaults.newEngine
	}
	if factories.newCoordinator == nil {
		factories.newCoordinator = defaults.newCoordinator
	}
	if factories.closeFactory == nil {
		factories.closeFactory = defaults.closeFactory
	}
	if factories.closeResolver == nil {
		factories.closeResolver = defaults.closeResolver
	}
	return factories
}

type Server struct {
	staticConfig     *config.EffectiveConfig
	dataEncryption   data_encryption.Service
	resolver         *secret.GenerationSecretResolver
	serverInfo       *server_info.View
	engine           generationEngineOwner
	coordinator      *generation.Coordinator
	closeResolver    func(*secret.GenerationSecretResolver, context.Context) error
	runtimeFactories serverRuntimeFactories
	shutdownHTTP     func(context.Context) error
	drainRoutes      func(context.Context) error

	addrs           []string
	server          *http.Server
	statusServer    *http.Server
	controlServer   *http.Server
	routes          *routeHandler
	streamRuntime   streamRuntimeOwner
	streamRuntimeMu sync.Mutex

	producer configProducer

	lifecycleMu         sync.Mutex
	lifecycleCancel     context.CancelFunc
	startupDone         chan struct{}
	startupInProgress   bool
	shutdownRequested   bool
	listenersRejected   bool
	lateProducerStopErr error

	shutdownMu        sync.Mutex
	activeAttempt     *shutdownAttemptResult
	shutdownComplete  bool
	shutdownErr       error
	shutdownErrors    []error
	shutdownPhase     uint8
	httpDrained       bool
	routesDrained     bool
	streamDrained     bool
	engineClosed      bool
	resolverClosed    bool
	expirationStopped bool
	exporterStopped   bool
	tracingStopped    bool

	producerStopOnce sync.Once
	producerStopDone chan struct{}
	producerStopErr  error

	listenerMu sync.Mutex
	listeners  []net.Listener

	prometheusServer         *http.Server
	stopPrometheusExpiration func(context.Context) error
	otelShutdown             func(context.Context) error
}

type shutdownAttemptResult struct {
	done chan struct{}
	err  error
}

const startupCleanupTimeout = time.Second

const (
	shutdownPhaseInitial uint8 = iota
	shutdownPhaseProducerStopped
	shutdownPhaseLeasesRejected
	shutdownPhaseDrained
	shutdownPhaseEngineClosed
	shutdownPhaseResolverClosed
	shutdownPhaseObservabilityClosed
)

func NewServer(
	effective *config.EffectiveConfig,
	encryption data_encryption.Service,
	resolver *secret.GenerationSecretResolver,
) (*Server, error) {
	return newServerWithFactories(
		effective,
		encryption,
		resolver,
		defaultNewServerFactories(),
	)
}

func newServerWithFactories(
	effective *config.EffectiveConfig,
	encryption data_encryption.Service,
	resolver *secret.GenerationSecretResolver,
	factories newServerFactories,
) (*Server, error) {
	if err := validateNewServerDependencies(effective, encryption, resolver); err != nil {
		return nil, err
	}
	factories = factories.withDefaults()
	cfg := &effective.Config
	addrs := configuredListenAddresses(cfg)
	server := &Server{
		staticConfig:     effective,
		dataEncryption:   encryption,
		resolver:         resolver,
		closeResolver:    factories.closeResolver,
		runtimeFactories: defaultServerRuntimeFactories(),
		addrs:            addrs,
		serverInfo:       server_info.NewView(cfg.Apisix.ID),
	}

	otelShutdown, err := factories.initObservability("apisix-go")
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("initialize tracing: %w", err),
			factories.closeResolver(resolver, context.Background()),
		)
	}
	server.otelShutdown = otelShutdown
	cleanupTransferred := func(primary error) error {
		return errors.Join(
			primary,
			factories.closeResolver(resolver, context.Background()),
			otelShutdown(context.Background()),
		)
	}
	observers := compiler.WorkerRuntimeObservers{Cluster: newClusterObserver(cfg)}
	observers.ServerInfo = server.serverInfo
	if streamProxyModeEnabled(cfg) {
		observers.Stream = logStreamResult
	}
	factory, err := factories.newFactory(
		effective,
		secret.NewMaterializer(encryption, resolver),
		observers,
	)
	if err != nil {
		return nil, cleanupTransferred(fmt.Errorf("create worker compiler factory: %w", err))
	}
	engine, err := factories.newEngine(server, factory)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("create generation engine: %w", err),
			factories.closeFactory(factory, context.Background()),
			factories.closeResolver(resolver, context.Background()),
			otelShutdown(context.Background()),
		)
	}
	server.engine = engine
	server.coordinator = factories.newCoordinator(engine)
	if server.coordinator == nil {
		return nil, errors.Join(
			errors.New("create generation coordinator: nil coordinator"),
			engine.Close(context.Background()),
			factories.closeResolver(resolver, context.Background()),
			otelShutdown(context.Background()),
		)
	}
	if server.routes == nil {
		return nil, errors.Join(
			errors.New("generation engine did not bind HTTP runtime"),
			engine.Close(context.Background()),
			factories.closeResolver(resolver, context.Background()),
			otelShutdown(context.Background()),
		)
	}
	server.server = newConfiguredHTTPServer(newConfiguredHTTPHandler(server.routes, cfg), cfg)
	server.statusServer = newConfiguredHTTPServer(newStatusHandler(server.httpGenerationReady), nil)
	server.controlServer = newConfiguredHTTPServer(newControlHandler(cfg, server.serverInfo), nil)
	server.shutdownHTTP = func(ctx context.Context) error {
		return errors.Join(
			server.server.Shutdown(ctx),
			server.statusServer.Shutdown(ctx),
			server.controlServer.Shutdown(ctx),
		)
	}
	return server, nil
}

func validateNewServerDependencies(
	effective *config.EffectiveConfig,
	encryption data_encryption.Service,
	resolver *secret.GenerationSecretResolver,
) error {
	switch {
	case effective == nil:
		return errors.New("effective config is required")
	case !encryption.Configured():
		return errors.New("data encryption service is required")
	case resolver == nil:
		return errors.New("generation secret resolver is required")
	default:
		return nil
	}
}

func newClusterObserver(cfg *config.Config) pxy.ClusterObserver {
	if !prometheusEnabled(cfg) {
		return pxy.NopClusterObserver{}
	}
	return metrics.NewProxyRuntimeObserver()
}

func newConfiguredHTTPHandler(routes http.Handler, cfg *config.Config) http.Handler {
	handler := routes
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

func newStatusHandler(serviceable func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			writeStatusResponse(w, http.StatusOK, "ok")
		case "/status/ready":
			if metrics.GetReadiness().ConfigApplyReady && serviceable != nil && serviceable() {
				writeStatusResponse(w, http.StatusOK, "ok")
				return
			}
			writeStatusResponse(w, http.StatusServiceUnavailable, "not ready")
		default:
			http.NotFound(w, r)
		}
	})
}

func newControlHandler(cfg *config.Config, view *server_info.View) http.Handler {
	if cfg == nil || !cfg.Apisix.EnableControl || !pluginConfigured(cfg, "server-info") || view == nil {
		return http.NotFoundHandler()
	}
	serverInfoHandler := view.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/server_info" {
			http.NotFound(w, r)
			return
		}
		serverInfoHandler.ServeHTTP(w, r)
	})
}

func (s *Server) httpGenerationReady() bool {
	if s == nil || s.engine == nil {
		return false
	}
	lease, ok := s.engine.acquireHTTP()
	if !ok || lease.Snapshot == nil || lease.Snapshot.Handler() == nil || lease.Release == nil {
		if ok && lease.Release != nil {
			lease.Release()
		}
		return false
	}
	lease.Release()
	return true
}

func writeStatusResponse(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"status":%q}`, value)
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
		if requestLength, known := apisixlog.EstimateHTTP1RequestLength(r); known {
			r = apisixctx.WithRequestVars(r)
			apisixctx.RegisterRequestVar(r, "$request_length", requestLength)
		}
		forwardedFor := r.Header.Values("X-Forwarded-For")
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
			// When no global policy exists, retain the raw X-Forwarded-For values
			// privately so real-ip can apply its own route-scoped peer policy.
			if len(addresses) == 0 {
				r = apisixctx.WithForwardedForCandidate(r, forwardedFor)
			}
			r.Header.Set("X-Forwarded-Proto", scheme(r))
			r.Header.Set("X-Forwarded-Host", r.Host)
			r.Header.Set("X-Forwarded-Port", listenPort(r))
			r.Header.Del("Forwarded")
			r.Header.Del("X-Forwarded-For")
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

func configuredStatusAddress(cfg *config.Config) string {
	return net.JoinHostPort(cfg.Apisix.Status.IP, strconv.Itoa(cfg.Apisix.Status.Port))
}

func configuredControlAddress(cfg *config.Config) (string, bool) {
	if cfg == nil || !cfg.Apisix.EnableControl {
		return "", false
	}
	host := strings.TrimSpace(cfg.Apisix.Control.Ip)
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Apisix.Control.Port
	if port == 0 {
		port = 9090
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), true
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
		stopErr := producer.Stop()
		s.lifecycleMu.Lock()
		s.lateProducerStopErr = errors.Join(
			s.lateProducerStopErr,
			wrapCleanupError("stop late config producer", stopErr),
		)
		s.lifecycleMu.Unlock()
		return errors.Join(context.Canceled, stopErr)
	}
	s.producer = producer
	s.lifecycleMu.Unlock()
	return nil
}

func (s *Server) cleanupAfterStart() error {
	ctx, cancel := context.WithTimeout(context.Background(), startupCleanupTimeout)
	err := s.shutdown(ctx)
	cancel()
	if err == nil {
		return nil
	}
	go func() {
		if cleanupErr := s.shutdown(context.Background()); cleanupErr != nil {
			logger.Errorf("finish server cleanup after startup failure: %s", cleanupErr)
		}
	}()
	return err
}

func (s *Server) retainListener(listener net.Listener) {
	s.lifecycleMu.Lock()
	if s.listenersRejected {
		s.lifecycleMu.Unlock()
		_ = listener.Close()
		return
	}
	s.listenerMu.Lock()
	s.listeners = append(s.listeners, listener)
	s.listenerMu.Unlock()
	s.lifecycleMu.Unlock()
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
		s.refreshGenerationStreamMetrics()
		if err := s.startPrometheusExpiration(ctx); err != nil {
			return err
		}
	}
	metrics.SetConfigApplyStreamRequired(streamProxyModeEnabled(cfg))
	factories := s.runtimeFactories.withDefaults()
	s.runtimeFactories = factories
	producer, err := factories.newProducer(s, ctx)
	if err != nil {
		return fmt.Errorf("construct config producer: %w", err)
	}
	if err := s.retainProducer(producer); err != nil {
		return fmt.Errorf("retain config producer: %w", err)
	}
	if err := producer.Start(ctx); err != nil {
		return fmt.Errorf("start config producer: %w", err)
	}
	if err := s.startImmutableStreamRuntime(ctx, factories.newStream); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.startPrometheusExportServer(); err != nil {
		return err
	}
	serveErrors, err := factories.startHTTP(s, ctx)
	if err != nil {
		return fmt.Errorf("start HTTP listeners: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveErrors:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) startServing(
	ctx context.Context,
	previousStreamRuntime streamRuntimeOwner,
	startPrometheus func() error,
	startHTTP func(context.Context) error,
) error {
	var startedStreamRuntime streamRuntimeOwner
	if current := s.currentStreamRuntime(); current != previousStreamRuntime {
		startedStreamRuntime = current
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

func (s *Server) shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.shutdownMu.Lock()
	if s.shutdownComplete {
		err := s.shutdownErr
		s.shutdownMu.Unlock()
		return err
	}
	if attempt := s.activeAttempt; attempt != nil {
		s.shutdownMu.Unlock()
		return waitShutdownAttempt(ctx, attempt)
	}
	attempt := &shutdownAttemptResult{done: make(chan struct{})}
	s.activeAttempt = attempt
	s.shutdownMu.Unlock()

	err, complete := s.shutdownAttempt(ctx)

	s.shutdownMu.Lock()
	attempt.err = err
	s.shutdownErr = err
	s.shutdownComplete = complete
	s.activeAttempt = nil
	close(attempt.done)
	s.shutdownMu.Unlock()
	return err
}

func waitShutdownAttempt(ctx context.Context, attempt *shutdownAttemptResult) error {
	select {
	case <-attempt.done:
		return attempt.err
	default:
	}
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		select {
		case <-attempt.done:
			return attempt.err
		default:
			return ctx.Err()
		}
	}
}

func (s *Server) shutdownAttempt(ctx context.Context) (error, bool) {
	if s.shutdownPhase < shutdownPhaseProducerStopped {
		cancel, startupDone := s.requestShutdown()
		producerErr, complete := s.stopProducerBarrier(ctx)
		if !complete {
			return producerErr, false
		}
		if cancel != nil {
			cancel()
		}
		if err := waitForLifecycle(ctx, startupDone); err != nil {
			return errors.Join(producerErr, fmt.Errorf("wait for server startup: %w", err)), false
		}
		s.lifecycleMu.Lock()
		producerErr = errors.Join(producerErr, s.lateProducerStopErr)
		s.lifecycleMu.Unlock()
		s.appendShutdownError(producerErr)
		s.shutdownPhase = shutdownPhaseProducerStopped
	}
	if s.shutdownPhase < shutdownPhaseLeasesRejected {
		s.lifecycleMu.Lock()
		s.listenersRejected = true
		s.lifecycleMu.Unlock()
		s.closeOwnedListeners()
		if s.routes != nil {
			s.routes.RejectNew()
		}
		s.initiateStreamRuntimeClose()
		s.shutdownPhase = shutdownPhaseLeasesRejected
	}
	if s.shutdownPhase < shutdownPhaseDrained {
		drainErr, complete := s.drainRuntimeOwners(ctx)
		if !complete {
			return errors.Join(errors.Join(s.shutdownErrors...), drainErr), false
		}
		s.appendShutdownError(drainErr)
		s.shutdownPhase = shutdownPhaseDrained
	}
	if s.shutdownPhase < shutdownPhaseEngineClosed {
		if s.engine != nil && !s.engineClosed {
			engineErr := wrapCleanupError("close generation engine", s.engine.Close(ctx))
			var residual *runtime.TaskResidualError
			if errors.As(engineErr, &residual) ||
				errors.Is(engineErr, compiler.ErrPreparedGenerationCleanupIncomplete) ||
				contextError(engineErr) {
				return errors.Join(errors.Join(s.shutdownErrors...), engineErr), false
			}
			s.appendShutdownError(engineErr)
			s.engineClosed = true
		}
		s.shutdownPhase = shutdownPhaseEngineClosed
	}
	if s.shutdownPhase < shutdownPhaseResolverClosed {
		if s.resolver != nil && !s.resolverClosed {
			closer := s.closeResolver
			if closer == nil {
				closer = func(resolver *secret.GenerationSecretResolver, ctx context.Context) error {
					return resolver.Close(ctx)
				}
			}
			s.appendShutdownError(wrapCleanupError("close generation secret resolver", closer(s.resolver, ctx)))
			s.resolverClosed = true
		} else if s.closeResolver != nil && !s.resolverClosed {
			s.appendShutdownError(wrapCleanupError("close generation secret resolver", s.closeResolver(nil, ctx)))
			s.resolverClosed = true
		}
		s.shutdownPhase = shutdownPhaseResolverClosed
	}
	if s.shutdownPhase < shutdownPhaseObservabilityClosed {
		observabilityErr, complete := s.closeObservability(ctx)
		if !complete {
			return errors.Join(errors.Join(s.shutdownErrors...), observabilityErr), false
		}
		s.appendShutdownError(observabilityErr)
		s.shutdownPhase = shutdownPhaseObservabilityClosed
	}
	return errors.Join(s.shutdownErrors...), true
}

func (s *Server) requestShutdown() (context.CancelFunc, chan struct{}) {
	s.lifecycleMu.Lock()
	s.shutdownRequested = true
	cancel := s.lifecycleCancel
	startupDone := s.startupDone
	startupInProgress := s.startupInProgress
	s.lifecycleMu.Unlock()
	if !startupInProgress {
		startupDone = nil
	}
	return cancel, startupDone
}

func (s *Server) appendShutdownError(err error) {
	if err != nil {
		s.shutdownErrors = append(s.shutdownErrors, err)
	}
}

func wrapCleanupError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (s *Server) stopProducerBarrier(ctx context.Context) (error, bool) {
	s.producerStopOnce.Do(func() {
		s.lifecycleMu.Lock()
		producer := s.producer
		s.lifecycleMu.Unlock()
		s.producerStopDone = make(chan struct{})
		go func() {
			defer close(s.producerStopDone)
			if producer != nil {
				s.producerStopErr = wrapCleanupError("stop config producer", producer.Stop())
			}
		}()
	})
	select {
	case <-s.producerStopDone:
		return s.producerStopErr, true
	case <-ctx.Done():
		return ctx.Err(), false
	}
}

func (s *Server) initiateStreamRuntimeClose() {
	if s.streamDrained {
		return
	}
	s.streamRuntimeMu.Lock()
	streamRuntime := s.streamRuntime
	s.streamRuntimeMu.Unlock()
	if streamRuntime == nil {
		s.streamDrained = true
		return
	}

	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := streamRuntime.Close(closeCtx); err != nil {
		if contextError(err) {
			return
		}
		s.appendShutdownError(wrapCleanupError("drain stream runtime", err))
	}
	s.streamDrained = true
	s.clearStreamRuntime(streamRuntime)
}

func (s *Server) drainRuntimeOwners(ctx context.Context) (error, bool) {
	var errs []error
	incomplete := false
	if !s.httpDrained {
		shutdownHTTP := s.shutdownHTTP
		if shutdownHTTP == nil && s.server != nil {
			shutdownHTTP = s.server.Shutdown
		}
		if shutdownHTTP == nil {
			s.httpDrained = true
		} else if err := shutdownHTTP(ctx); errors.Is(err, net.ErrClosed) {
			s.httpDrained = true
		} else if err != nil {
			wrapped := wrapCleanupError("drain HTTP server", err)
			if contextError(err) {
				errs = append(errs, wrapped)
				incomplete = true
			} else {
				s.appendShutdownError(wrapped)
				s.httpDrained = true
			}
		} else {
			s.httpDrained = true
		}
	}
	if !s.routesDrained {
		drainRoutes := s.drainRoutes
		if drainRoutes == nil && s.routes != nil {
			drainRoutes = s.routes.Drain
		}
		if drainRoutes == nil {
			s.routesDrained = true
		} else if err := drainRoutes(ctx); err != nil {
			wrapped := wrapCleanupError("drain HTTP generation leases", err)
			if contextError(err) {
				errs = append(errs, wrapped)
				incomplete = true
			} else {
				s.appendShutdownError(wrapped)
				s.routesDrained = true
			}
		} else {
			s.routesDrained = true
		}
	}
	if !s.streamDrained {
		s.streamRuntimeMu.Lock()
		streamRuntime := s.streamRuntime
		s.streamRuntimeMu.Unlock()
		if streamRuntime == nil {
			s.streamDrained = true
		} else if err := streamRuntime.Close(ctx); err != nil {
			wrapped := wrapCleanupError("drain stream runtime", err)
			if contextError(err) {
				errs = append(errs, wrapped)
				incomplete = true
			} else {
				s.appendShutdownError(wrapped)
				s.streamDrained = true
				s.clearStreamRuntime(streamRuntime)
			}
		} else {
			s.streamDrained = true
			s.clearStreamRuntime(streamRuntime)
		}
	}
	if ctx.Err() != nil {
		incomplete = true
		if len(errs) == 0 {
			errs = append(errs, ctx.Err())
		}
	}
	return errors.Join(errs...), !incomplete && s.httpDrained && s.routesDrained && s.streamDrained
}

func (s *Server) clearStreamRuntime(streamRuntime streamRuntimeOwner) {
	s.streamRuntimeMu.Lock()
	if s.streamRuntime == streamRuntime {
		s.streamRuntime = nil
	}
	s.streamRuntimeMu.Unlock()
}

func (s *Server) currentStreamRuntime() streamRuntimeOwner {
	s.streamRuntimeMu.Lock()
	defer s.streamRuntimeMu.Unlock()
	return s.streamRuntime
}

func contextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Server) closeObservability(ctx context.Context) (error, bool) {
	var errs []error
	incomplete := false
	if !s.expirationStopped {
		if err := s.stopPrometheusExpirationRuntime(ctx); err != nil {
			if contextError(err) {
				errs = append(errs, err)
				incomplete = true
			} else {
				s.appendShutdownError(err)
				s.expirationStopped = true
			}
		} else {
			s.expirationStopped = true
		}
	}
	if !s.exporterStopped {
		if s.prometheusServer == nil {
			s.exporterStopped = true
		} else if err := s.prometheusServer.Shutdown(ctx); err != nil {
			wrapped := wrapCleanupError("stop prometheus export server", err)
			if contextError(err) {
				errs = append(errs, wrapped)
				incomplete = true
			} else {
				s.appendShutdownError(wrapped)
				s.exporterStopped = true
			}
		} else {
			s.exporterStopped = true
		}
	}
	if !s.tracingStopped && !incomplete {
		if s.otelShutdown != nil {
			s.appendShutdownError(wrapCleanupError("stop tracing", s.otelShutdown(ctx)))
		}
		s.tracingStopped = true
	}
	if ctx.Err() != nil {
		incomplete = true
		if len(errs) == 0 {
			errs = append(errs, ctx.Err())
		}
	}
	return errors.Join(errs...), !incomplete && s.expirationStopped && s.exporterStopped && s.tracingStopped
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

func (s *Server) startImmutableStreamRuntime(
	ctx context.Context,
	newRuntime func(
		context.Context,
		[]config.TcpListen,
		streamruntime.RouterSource,
	) (streamRuntimeOwner, error),
) error {
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

	if s.engine == nil {
		return fail(errors.New("generation engine is required for stream runtime"))
	}
	runtime, err := newRuntime(
		ctx,
		streamConfig.Tcp,
		func() (streamruntime.RouterLease, bool) {
			lease, ok := s.engine.acquireStream()
			if !ok {
				return streamruntime.RouterLease{}, false
			}
			return streamruntime.RouterLease{Router: lease.Router, Release: lease.Release}, true
		},
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
	s.streamRuntimeMu.Lock()
	s.streamRuntime = runtime
	s.streamRuntimeMu.Unlock()
	s.lifecycleMu.Unlock()
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageStreams)
	if addressOwner, ok := runtime.(interface{ Addresses() []string }); ok {
		logger.Infof("stream proxy listening on %v", addressOwner.Addresses())
	}
	return nil
}

func (s *Server) closeStartedStreamRuntime(runtime streamRuntimeOwner) error {
	s.streamRuntimeMu.Lock()
	if runtime == nil || s.streamRuntime != runtime {
		s.streamRuntimeMu.Unlock()
		return nil
	}
	s.streamRuntime = nil
	s.streamRuntimeMu.Unlock()
	err := runtime.Close(context.Background())
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
	s.refreshGenerationStreamMetrics()
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

func (s *Server) refreshGenerationStreamMetrics() {
	if s != nil && s.engine != nil {
		s.engine.refreshStreamMetrics()
	}
}

func prometheusEnabled(cfg *config.Config) bool {
	return cfg != nil && slices.Contains(cfg.Plugins, "prometheus")
}

func streamProxyModeEnabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	mode := strings.ToLower(strings.ReplaceAll(cfg.Apisix.ProxyMode, " ", ""))
	return slices.Contains(strings.Split(mode, "&"), "stream")
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

func (s *Server) constructConfigProducer(ctx context.Context) (configProducer, error) {
	provider := standaloneConfigProvider(&s.staticConfig.Config)
	if provider != "" {
		watcher := config.NewStandaloneFileWatcher(
			config.StandaloneConfigFile(provider),
			provider,
			s.coordinator,
			s.dataEncryption,
		)
		producer := &standaloneConfigProducer{watcher: watcher}
		return producer, nil
	}
	return s.constructEtcdConfigProducer(ctx)
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

func (s *Server) constructEtcdConfigProducer(ctx context.Context) (configProducer, error) {
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
			cfg.Apisix.Ssl.SslTrustedCertificate,
		)
		if err != nil {
			return nil, fmt.Errorf("build etcd TLS config: %w", err)
		}
	}
	logger.Info("Starting etcd client")
	etcdClient, err := etcd.NewConfigClientWithOptions(
		endpoints,
		username,
		password,
		prefix,
		s.coordinator,
		etcdClientOptions(etcdConfig, tlsConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("start etcd client: %w", err)
	}
	producer := newEtcdConfigProducer(etcdClient)
	if serverInfoReportingEnabled(cfg) {
		producer.afterInitial = func(ctx context.Context) {
			if etcdVersion, versionErr := etcdClient.ServerVersion(ctx); versionErr != nil {
				logger.Warnf("resolve server-info etcd version fail: %s", versionErr)
			} else {
				s.serverInfo.SetEtcdVersion(etcdVersion)
			}
			nodeID := s.serverInfo.Current().ID
			attr := cfg.PluginAttr["server-info"]
			_, reporterErr := etcdClient.StartServerInfoReporter(
				ctx,
				nodeID,
				server_info.ReportTTL(attr),
				func() ([]byte, error) {
					return json.Marshal(s.serverInfo.Current())
				},
			)
			if reporterErr != nil {
				logger.Warnf("start server-info reporter fail: %s", reporterErr)
			}
		}
	}
	return producer, nil
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

func (s *Server) startHTTPListenerRuntime(ctx context.Context) (<-chan error, error) {
	cfg := &s.staticConfig.Config
	tlsAddrs := configuredTLSListenAddresses(cfg)
	var tlsConfig *tls.Config
	if len(tlsAddrs) > 0 {
		if s.engine == nil {
			return nil, errors.New("generation engine is required for TLS listeners")
		}
		var err error
		tlsConfig, err = buildGenerationFrontendTLSConfig(cfg, s.engine.acquireHTTP)
		if err != nil {
			return nil, fmt.Errorf("build frontend TLS config: %w", err)
		}
	}
	return s.serveHTTPListenerRuntime(tlsAddrs, tlsConfig)
}

func (s *Server) serveHTTPListenerRuntime(
	tlsAddrs []string,
	tlsConfig *tls.Config,
) (<-chan error, error) {
	addrs := s.addrs
	controlAddr, controlEnabled := configuredControlAddress(&s.staticConfig.Config)
	listenerCount := len(addrs) + len(tlsAddrs) + 1
	if controlEnabled {
		listenerCount++
	}
	serveErrors := make(chan error, listenerCount)
	listeners := make([]net.Listener, 0, len(addrs)+len(tlsAddrs))
	for _, addr := range addrs {
		logger.Infof("listening on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			s.closeOwnedListeners()
			return nil, fmt.Errorf("open listener %s: %w", addr, err)
		}
		s.retainListener(listener)
		listeners = append(listeners, listener)
	}
	for _, addr := range tlsAddrs {
		logger.Infof("listening with TLS on %s", addr)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			s.closeOwnedListeners()
			return nil, fmt.Errorf("open TLS listener %s: %w", addr, err)
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
	statusAddr := configuredStatusAddress(&s.staticConfig.Config)
	logger.Infof("status API listening on %s", statusAddr)
	statusListener, err := net.Listen("tcp", statusAddr)
	if err != nil {
		s.closeOwnedListeners()
		return nil, fmt.Errorf("open status listener %s: %w", statusAddr, err)
	}
	s.retainListener(statusListener)
	go func() {
		defer s.releaseListener(statusListener)
		if err := s.statusServer.Serve(statusListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrors <- err
		}
	}()
	if controlEnabled {
		if s.controlServer == nil {
			s.closeOwnedListeners()
			return nil, errors.New("control HTTP server is required when apisix.enable_control is true")
		}
		logger.Infof("control API listening on %s", controlAddr)
		controlListener, err := net.Listen("tcp", controlAddr)
		if err != nil {
			s.closeOwnedListeners()
			return nil, fmt.Errorf("open control listener %s: %w", controlAddr, err)
		}
		s.retainListener(controlListener)
		go func() {
			defer s.releaseListener(controlListener)
			if err := s.controlServer.Serve(controlListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- err
			}
		}()
	}

	return serveErrors, nil
}

func frontendHTTP2Enabled(cfg *config.Config) bool {
	return cfg != nil && cfg.Apisix.EnableHttp2
}
