package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestPrometheusInitErrorsPropagateToServerCallers(t *testing.T) {
	const childEnv = "APISIX_GO_SERVER_PROMETHEUS_INIT_CHILD"
	if os.Getenv(childEnv) == "1" {
		effective := &config.EffectiveConfig{Config: config.Config{
			Plugins: []string{"prometheus"},
			PluginAttr: map[string]map[string]any{
				"prometheus": {"max_http_series": "not-an-integer"},
			},
		}}
		if err := (&Server{staticConfig: effective}).startPrometheusExportServer(); err == nil ||
			!strings.HasPrefix(err.Error(), "initialize prometheus metrics: ") {
			t.Fatalf("startPrometheusExportServer() error = %v, want metrics init prefix", err)
		}

		// A nil store makes the ordering contract observable: invalid metrics
		// configuration must return before storage hooks or Start are touched.
		server := &Server{staticConfig: effective, server: &http.Server{}}
		if err := server.Start(context.Background()); err == nil ||
			!strings.HasPrefix(err.Error(), "initialize prometheus metrics: ") {
			t.Fatalf("Server.Start() error = %v, want metrics init prefix", err)
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestPrometheusInitErrorsPropagateToServerCallers$")
	command.Env = append(os.Environ(), childEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("prometheus init propagation child failed: %v\n%s", err, output)
	}
}

func TestPrometheusInitPublishesActiveStreamRouteMetrics(t *testing.T) {
	const childEnv = "APISIX_GO_SERVER_ACTIVE_METRICS_CHILD"
	if os.Getenv(childEnv) == "1" {
		if metrics.StreamConnections != nil {
			t.Fatal("stream metrics initialized before publication ordering test")
		}
		fixture := newGenerationEngineFixture(t)
		prepareEngineStreamGeneration(t, fixture.engine, 86, "active-before-metrics")
		if metrics.StreamConnections != nil {
			t.Fatal("publication unexpectedly initialized stream metrics")
		}

		server := &Server{
			staticConfig: &config.EffectiveConfig{Config: config.Config{
				Plugins: []string{"prometheus"},
				PluginAttr: map[string]map[string]any{
					"prometheus": {"enable_export_server": false},
				},
			}},
			engine: fixture.engine,
		}
		if err := server.startPrometheusExportServer(); err != nil {
			t.Fatal(err)
		}
		assertGenerationStreamMetricDelta(t, "active-before-metrics", 1)
		return
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestPrometheusInitPublishesActiveStreamRouteMetrics$")
	command.Env = append(os.Environ(), childEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recovery metrics child failed: %v\n%s", err, output)
	}
}

func TestNewServerRejectsNilEffectiveConfigBeforeCreatingRuntimeFiles(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })
	runtimeDir := t.TempDir()
	if err := os.Chdir(runtimeDir); err != nil {
		t.Fatalf("change working directory: %v", err)
	}

	server, err := NewServer(
		nil,
		nil,
		testutil.DataEncryptionService(false, nil),
		nil,
	)
	if server != nil || err == nil || err.Error() != "effective config is required" {
		t.Fatalf("NewServer(nil) = (%#v, %v)", server, err)
	}
	entries, readErr := os.ReadDir(runtimeDir)
	if readErr != nil {
		t.Fatalf("read runtime directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("NewServer(nil) created runtime files: %v", entries)
	}
}

func TestServerInstancesRetainIndependentStaticConfig(t *testing.T) {
	firstConfig := &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
		ID: "node-a", NodeListen: []config.NodeListen{{Ip: "127.0.0.1", Port: 9080}},
	}}}
	secondConfig := &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
		ID: "node-b", NodeListen: []config.NodeListen{{Ip: "127.0.0.2", Port: 9081}},
	}}}
	first := &Server{staticConfig: firstConfig}
	second := &Server{staticConfig: secondConfig}

	if first.staticConfig.Config.Apisix.ID != "node-a" || second.staticConfig.Config.Apisix.ID != "node-b" {
		t.Fatalf("server IDs crossed: first=%q second=%q",
			first.staticConfig.Config.Apisix.ID, second.staticConfig.Config.Apisix.ID)
	}
	if got := configuredListenAddresses(
		&first.staticConfig.Config,
	); !reflect.DeepEqual(
		got,
		[]string{"127.0.0.1:9080"},
	) {
		t.Fatalf("first addresses = %#v", got)
	}
	if got := configuredListenAddresses(
		&second.staticConfig.Config,
	); !reflect.DeepEqual(
		got,
		[]string{"127.0.0.2:9081"},
	) {
		t.Fatalf("second addresses = %#v", got)
	}
}

func TestServerShutdownStopsPrometheusExpiration(t *testing.T) {
	stopCalls := 0
	server := &Server{
		stopPrometheusExpiration: func(context.Context) error {
			stopCalls++
			return nil
		},
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if stopCalls != 1 {
		t.Fatalf("expiration stop calls = %d, want 1", stopCalls)
	}
	if server.stopPrometheusExpiration != nil {
		t.Fatal("successful Shutdown() retained expiration stop callback")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if stopCalls != 1 {
		t.Fatalf("second Shutdown() expiration stop calls = %d, want 1", stopCalls)
	}
}

func TestServerShutdownDoesNotReachPrometheusExpirationBeforeDrainCompletes(t *testing.T) {
	release := make(chan struct{})
	stopCalls := 0
	server := &Server{
		stopPrometheusExpiration: func(ctx context.Context) error {
			stopCalls++
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	firstCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := server.Shutdown(firstCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Shutdown() error = %v, want context.Canceled", err)
	}
	if server.shutdownComplete {
		t.Fatal("timed-out expiration wait marked shutdown complete")
	}
	if server.stopPrometheusExpiration == nil {
		t.Fatal("timed-out expiration wait cleared stop callback")
	}

	close(release)
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Shutdown() error = %v", err)
	}
	if stopCalls != 1 {
		t.Fatalf("expiration stop calls = %d, want 1 after earlier drain timeout", stopCalls)
	}
}

func TestStartFailureCleanupStopsPrometheusExpiration(t *testing.T) {
	stopCalls := 0
	server := &Server{
		stopPrometheusExpiration: func(context.Context) error {
			stopCalls++
			return nil
		},
	}

	if err := server.cleanupAfterStart(); err != nil {
		t.Fatalf("cleanupAfterStart() error = %v", err)
	}
	if stopCalls != 1 {
		t.Fatalf("expiration stop calls = %d, want 1", stopCalls)
	}
}

func TestDrainRuntimeOwnersTreatsClosedHTTPListenerAsDrained(t *testing.T) {
	server := &Server{
		shutdownHTTP: func(context.Context) error {
			return errors.Join(errors.New("close listener"), net.ErrClosed)
		},
		routesDrained: true,
		streamDrained: true,
	}

	err, complete := server.drainRuntimeOwners(context.Background())
	if err != nil || !complete || !server.httpDrained {
		t.Fatalf("drain closed HTTP listener = (%v, %v, %v), want nil/true/true", err, complete, server.httpDrained)
	}
	if len(server.shutdownErrors) != 0 {
		t.Fatalf("closed HTTP listener recorded shutdown errors: %v", server.shutdownErrors)
	}
}

func TestServerShutdownRejectsLatePrometheusExpirationRetention(t *testing.T) {
	server := &Server{}
	if err := server.stopPrometheusExpirationRuntime(context.Background()); err != nil {
		t.Fatalf("stopPrometheusExpirationRuntime() error = %v", err)
	}

	stopCalls := 0
	deadlineSeen := false
	err := server.retainPrometheusExpiration(func(ctx context.Context) error {
		stopCalls++
		_, deadlineSeen = ctx.Deadline()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retainPrometheusExpiration() error = %v, want context.Canceled", err)
	}
	if stopCalls != 1 {
		t.Fatalf("late expiration stop calls = %d, want 1", stopCalls)
	}
	if deadlineSeen {
		t.Fatal("late expiration cleanup used a bounded wait and could lose the stop handle on timeout")
	}
	if server.stopPrometheusExpiration != nil {
		t.Fatal("late expiration handle was retained after shutdown began")
	}
}

type fakeConfigProducer struct {
	mu       sync.Mutex
	stopped  int
	stopErr  error
	stopSeen chan struct{}
	onStop   func()
}

func (*fakeConfigProducer) Start(context.Context) error { return nil }

func (p *fakeConfigProducer) Stop() error {
	p.mu.Lock()
	p.stopped++
	p.mu.Unlock()
	if p.stopSeen != nil {
		close(p.stopSeen)
		p.stopSeen = nil
	}
	if p.onStop != nil {
		p.onStop()
	}
	return p.stopErr
}

func TestServerShutdownStopsProducerBeforeEngineAndJoinsErrors(t *testing.T) {
	producerErr := errors.New("producer stop failed")
	streamErr := errors.New("stream stop failed")
	traceErr := errors.New("trace stop failed")
	producer := &fakeConfigProducer{stopErr: producerErr, stopSeen: make(chan struct{})}
	producerStopped := false
	producer.onStop = func() { producerStopped = true }
	engineClosed := false
	server := newShutdownLifecycleServer(t, func(context.Context) error {
		if !producerStopped {
			t.Error("generation engine closed before provider Stop() completed")
		}
		engineClosed = true
		return nil
	})
	stream := &fakeStreamRuntime{closeErr: streamErr}
	server.producer = producer
	server.streamRuntime = stream
	server.otelShutdown = func(context.Context) error { return traceErr }

	err := server.Shutdown(context.Background())
	for _, want := range []error{producerErr, streamErr, traceErr} {
		if !errors.Is(err, want) {
			t.Fatalf("Shutdown() error = %v, want joined %v", err, want)
		}
	}
	if producer.stopped != 1 {
		t.Fatalf("producer Stop calls = %d, want 1", producer.stopped)
	}
	if !engineClosed {
		t.Fatal("Shutdown() did not close the generation engine")
	}
	if err := server.Shutdown(context.Background()); !errors.Is(err, producerErr) {
		t.Fatalf("repeated Shutdown() error = %v, want original joined error", err)
	}
	if producer.stopped != 1 {
		t.Fatalf("repeated Shutdown() called producer Stop %d times, want 1", producer.stopped)
	}
}

func TestStartFailureCleanupIsBoundedWithActiveHTTPRequest(t *testing.T) {
	t.Setenv("TASK9_STARTUP_LOGIN", "login")
	t.Setenv("TASK9_STARTUP_PASSWORD", "password")
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	fixture := newGenerationContractFixture(t, false)
	prepareGenerationContract(t, fixture.engine, 301, generationContractResources(
		t, backend.URL, "startup-cleanup", "TASK9_STARTUP_LOGIN", "TASK9_STARTUP_PASSWORD", "", nil,
	))
	owner := fixture.engine.active.Load().http
	routes := newGenerationRouteHandler(fixture.engine.acquireHTTP)
	httpServer := &http.Server{Handler: routes}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() {
		release()
		_ = httpServer.Close()
	})
	requestDone := make(chan struct{})
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			_ = response.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("active request did not start")
	}

	server := &Server{server: httpServer, routes: routes, engine: fixture.engine}
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- server.cleanupAfterStart() }()
	select {
	case err := <-cleanupDone:
		if err == nil {
			t.Fatal("cleanupAfterStart() error = nil with an active request")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup cleanup remained blocked behind an active request")
	}
	select {
	case <-owner.closeDone:
		t.Fatal("startup cleanup closed the generation while the active request retained its lease")
	default:
	}
	release()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("active request did not exit after release")
	}
	select {
	case <-owner.closeDone:
	case <-time.After(time.Second):
		t.Fatal("startup cleanup did not close the generation after the active request exited")
	}
}

type fakeEtcdClient struct {
	watchStarted chan struct{}
	releaseWatch chan struct{}
	closeCalled  chan struct{}
	mu           sync.Mutex
	order        []string
}

func (*fakeEtcdClient) FetchAllContext(context.Context) error { return nil }

func (c *fakeEtcdClient) Watch(context.Context) {
	close(c.watchStarted)
	<-c.releaseWatch
	c.mu.Lock()
	c.order = append(c.order, "watch-exit")
	c.mu.Unlock()
}

func (c *fakeEtcdClient) Close() error {
	c.mu.Lock()
	c.order = append(c.order, "client-close")
	c.mu.Unlock()
	close(c.closeCalled)
	return nil
}

type failingInitialEtcdClient struct {
	watchStarted chan struct{}
	closeCalled  chan struct{}
	fetchErr     error
}

func (c *failingInitialEtcdClient) FetchAllContext(context.Context) error { return c.fetchErr }

func (c *failingInitialEtcdClient) Watch(ctx context.Context) {
	close(c.watchStarted)
	<-ctx.Done()
}

func (c *failingInitialEtcdClient) Close() error {
	close(c.closeCalled)
	return nil
}

func TestEtcdWatchExitIsJoinedBeforeClientClose(t *testing.T) {
	client := &fakeEtcdClient{
		watchStarted: make(chan struct{}),
		releaseWatch: make(chan struct{}),
		closeCalled:  make(chan struct{}),
	}
	producer := newEtcdConfigProducer(client)
	if err := producer.Start(context.Background()); err != nil {
		t.Fatalf("producer Start() error = %v", err)
	}
	select {
	case <-client.watchStarted:
	case <-time.After(time.Second):
		t.Fatal("etcd Watch() did not start")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- producer.Stop() }()
	select {
	case <-client.closeCalled:
		t.Fatal("client Close() ran before Watch() exited")
	case <-time.After(50 * time.Millisecond):
	}
	close(client.releaseWatch)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("producer Stop() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer Stop() did not wait for Watch() exit")
	}
	client.mu.Lock()
	gotOrder := append([]string(nil), client.order...)
	client.mu.Unlock()
	if !slices.Equal(gotOrder, []string{"watch-exit", "client-close"}) {
		t.Fatalf("shutdown order = %v, want watch-exit then client-close", gotOrder)
	}
}

func TestNormalizeRequestPathCleansDotSegments(t *testing.T) {
	var gotPath string
	var gotRequestURI string
	handler := normalizeRequestPath(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRequestURI = r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/./internal/x?aa=1", nil)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotPath != "/internal/x" {
		t.Fatalf("URL.Path = %q, want /internal/x", gotPath)
	}
	if gotRequestURI != "/./internal/x?aa=1" {
		t.Fatalf("RequestURI = %q, want original request target preserved", gotRequestURI)
	}
}

func TestStripUntrustedForwardedForDropsForgedHeader(t *testing.T) {
	var gotForwardedFor string
	var gotCandidate []string
	handler := normalizeForwardedHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		gotCandidate = apisixctx.ForwardedForCandidate(r)
		w.WriteHeader(http.StatusNoContent)
	}), []string{"192.128.0.0/16"})
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotForwardedFor != "" {
		t.Fatalf("X-Forwarded-For = %q, want forged header removed", gotForwardedFor)
	}
	if len(gotCandidate) != 0 {
		t.Fatalf("forwarded-for candidate = %q, want absent under configured global policy", gotCandidate)
	}
}

func TestStripUntrustedForwardedForDropsForgedHeaderWithoutTrustedAddresses(t *testing.T) {
	var gotForwardedFor string
	var gotCandidate []string
	handler := normalizeForwardedHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		gotCandidate = apisixctx.ForwardedForCandidate(r)
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotForwardedFor != "" {
		t.Fatalf("X-Forwarded-For = %q, want forged header removed", gotForwardedFor)
	}
	if !slices.Equal(gotCandidate, []string{"1.1.1.1"}) {
		t.Fatalf("forwarded-for candidate = %q, want ingress value retained privately", gotCandidate)
	}
}

func TestStripUntrustedForwardedForPreservesTrustedHeader(t *testing.T) {
	var gotForwardedFor string
	var trustedProxy bool
	handler := normalizeForwardedHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		trustedProxy = apisixctx.IsTrustedProxy(r)
		w.WriteHeader(http.StatusNoContent)
	}), []string{"127.0.0.0/24"})
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotForwardedFor != "1.1.1.1" {
		t.Fatalf("X-Forwarded-For = %q, want trusted header preserved", gotForwardedFor)
	}
	if !trustedProxy {
		t.Fatal("trusted proxy context = false, want true")
	}
}

func TestNormalizeForwardedHeadersSetsObservedHostAndPort(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		local    net.Addr
		tls      bool
		wantHost string
		wantPort string
	}{
		{
			name:     "explicit host port",
			host:     "api.example.com:8443",
			local:    &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9080},
			wantHost: "api.example.com:8443",
			wantPort: "8443",
		},
		{
			name:     "listener port",
			host:     "api.example.com",
			local:    &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9080},
			wantHost: "api.example.com",
			wantPort: "9080",
		},
		{
			name:     "TLS listener port",
			host:     "api.example.com",
			local:    &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9443},
			tls:      true,
			wantHost: "api.example.com",
			wantPort: "9443",
		},
		{
			name:     "HTTP fallback",
			host:     "api.example.com",
			wantHost: "api.example.com",
			wantPort: "80",
		},
		{
			name:     "TLS fallback",
			host:     "api.example.com",
			tls:      true,
			wantHost: "api.example.com",
			wantPort: "443",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotHost string
			var gotPort string
			handler := normalizeForwardedHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHost = r.Header.Get("X-Forwarded-Host")
				gotPort = r.Header.Get("X-Forwarded-Port")
			}), nil)
			req := httptest.NewRequest(http.MethodGet, "http://api.example.com/hello", nil)
			req.Host = test.host
			req.RemoteAddr = "127.0.0.1:12345"
			if test.local != nil {
				req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, test.local))
			}
			if test.tls {
				req.TLS = &tls.ConnectionState{}
			}

			handler.ServeHTTP(httptest.NewRecorder(), req)

			if gotHost != test.wantHost {
				t.Fatalf("X-Forwarded-Host = %q, want %q", gotHost, test.wantHost)
			}
			if gotPort != test.wantPort {
				t.Fatalf("X-Forwarded-Port = %q, want %q", gotPort, test.wantPort)
			}
		})
	}
}

func TestNormalizeForwardedHeadersCapturesOriginalRequestLengthBeforeMutation(t *testing.T) {
	var gotLength any
	handler := normalizeForwardedHeaders(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotLength = apisixctx.GetRequestVar(r, "$request_length")
		if r.Header.Get("X-Forwarded-Proto") != "http" ||
			r.Header.Get("X-Forwarded-Host") != "gateway.example.test" {
			t.Fatalf("normalized forwarded headers = %#v", r.Header)
		}
	}), nil)
	request := httptest.NewRequest(http.MethodGet, "http://gateway.example.test/opentracing", nil)
	request.RequestURI = "/opentracing"
	request.Header.Set("User-Agent", "Go-http-client/1.1")
	request.RemoteAddr = "127.0.0.1:12345"

	handler.ServeHTTP(httptest.NewRecorder(), request)

	if gotLength != int64(89) {
		t.Fatalf("$request_length = %#v, want original 89-byte request before forwarded headers", gotLength)
	}
}

func TestConfiguredHTTPServerUsesSafeHeaderAndIdleDefaults(t *testing.T) {
	cfg := &config.Config{Plugins: []string{"prometheus"}}
	server := newConfiguredHTTPServer(http.NotFoundHandler(), cfg)
	if server.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want 10s", server.ReadHeaderTimeout)
	}
	if server.IdleTimeout != 90*time.Second {
		t.Fatalf("IdleTimeout = %s, want 90s", server.IdleTimeout)
	}
	if server.ReadTimeout != 0 || server.WriteTimeout != 0 {
		t.Fatalf("stream-sensitive total timeouts = %s/%s, want zero", server.ReadTimeout, server.WriteTimeout)
	}
	if server.ConnState == nil {
		t.Fatal("configured HTTP server has no connection lifecycle observer")
	}
}

func TestConfiguredHTTPServerSkipsConnectionObserverWithoutPrometheus(t *testing.T) {
	cfg := &config.Config{Plugins: []string{"limit-req"}}

	if observer := newConfiguredHTTPServer(http.NotFoundHandler(), cfg).ConnState; observer != nil {
		t.Fatal("configured HTTP server installed a Prometheus connection observer while metrics are disabled")
	}
}

func TestStatusEndpointsUseSeparateHandlerAndIgnoreEtcdLoss(t *testing.T) {
	restoreMetrics := installHealthMetrics(t)
	defer restoreMetrics()

	serviceable := false
	handler := newStatusHandler(func() bool { return serviceable })

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("/status code = %d, want %d", response.Code, http.StatusOK)
	}
	if got, want := strings.TrimSpace(response.Body.String()), `{"status":"ok"}`; got != want {
		t.Fatalf("/status body = %q, want %q", got, want)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("/status/ready before config apply = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}

	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"/status/ready without active generation = %d, want %d",
			response.Code,
			http.StatusServiceUnavailable,
		)
	}

	serviceable = true
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("/status/ready after config apply = %d, want %d", response.Code, http.StatusOK)
	}

	metrics.RecordEtcdReachable(false)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status/ready", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("/status/ready after etcd loss = %d, want last-good ready", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown status path = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestConfiguredHTTPHandlerLeavesLegacyProbePathsToRoutes(t *testing.T) {
	handler := newConfiguredHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), &config.Config{})

	for _, requestPath := range []string{"/livez", "/readyz"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want route-owned %d", requestPath, response.Code, http.StatusNoContent)
		}
	}
}

func TestConfiguredHTTPHandlerCountsAllRequestsOnlyWhenPrometheusEnabled(t *testing.T) {
	previousRequests := metrics.Requests
	t.Cleanup(func() {
		metrics.Requests = previousRequests
	})

	for _, test := range []struct {
		name    string
		plugins []string
		want    float64
	}{
		{name: "enabled", plugins: []string{"prometheus"}, want: 2},
		{name: "disabled", plugins: []string{"limit-req"}, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{Plugins: test.plugins}
			metrics.Requests = prometheus.NewGauge(prometheus.GaugeOpts{
				Name: "test_http_handler_requests_" + test.name,
			})
			handler := newConfiguredHTTPHandler(http.NotFoundHandler(), cfg)
			for _, path := range []string{"/livez", "/ordinary"} {
				handler.ServeHTTP(
					httptest.NewRecorder(),
					httptest.NewRequest(http.MethodGet, path, nil),
				)
			}
			if got := configApplyGaugeValue(t, metrics.Requests); got != test.want {
				t.Fatalf("request total = %v, want %v", got, test.want)
			}
		})
	}
}

func configApplyGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}

func installHealthMetrics(t *testing.T) func() {
	t.Helper()
	oldFailures, oldReady, oldReachable := metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.EtcdReachable
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_server_health_config_apply_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_server_health_config_apply_ready"})
	metrics.EtcdReachable = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_server_health_etcd_reachable"})
	return func() {
		metrics.ConfigApplyFailures, metrics.ConfigApplyReady, metrics.EtcdReachable = oldFailures, oldReady, oldReachable
	}
}

func TestConfiguredServerUsesNodeListenAndHTTPTimeouts(t *testing.T) {
	cfg := &config.Config{
		Apisix: config.Apisix{NodeListen: []config.NodeListen{
			{Port: 9080},
			{Ip: "127.0.0.2", Port: 9081},
		}},
		NginxConfig: config.NginxConfig{HTTP: config.NginxHTTP{
			KeepaliveTimeout:    60 * time.Second,
			ClientHeaderTimeout: 5 * time.Second,
			ClientBodyTimeout:   10 * time.Second,
		}},
	}

	if got, want := configuredListenAddresses(cfg), []string{
		"0.0.0.0:9080",
		"127.0.0.2:9081",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredListenAddresses() = %#v, want %#v", got, want)
	}

	server := newConfiguredHTTPServer(http.NotFoundHandler(), cfg)
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %s, want 1m0s", server.IdleTimeout)
	}
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want 5s", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 15*time.Second {
		t.Fatalf("ReadTimeout = %s, want 15s", server.ReadTimeout)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want zero because send_timeout has no Go equivalent", server.WriteTimeout)
	}
}

func TestConfiguredTLSListenAddresses(t *testing.T) {
	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable: true,
		Listen: []config.Listen{
			{Port: 9443},
			{Ip: "127.0.0.2", Port: 9444},
		},
	}}}

	if got, want := configuredTLSListenAddresses(cfg), []string{
		"0.0.0.0:9443",
		"127.0.0.2:9444",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredTLSListenAddresses() = %#v, want %#v", got, want)
	}

	cfg.Apisix.Ssl.Enable = false
	if got := configuredTLSListenAddresses(cfg); len(got) != 0 {
		t.Fatalf("configuredTLSListenAddresses() = %#v, want no disabled listeners", got)
	}
}

func TestConfiguredHTTPServerAndFrontendTLSAdvertiseHTTP2(t *testing.T) {
	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		Listen:       []config.Listen{{Port: 9443, EnableHttp2: true}},
		SslProtocols: "TLSv1.2 TLSv1.3",
		SslCiphers:   "ECDHE-RSA-AES128-GCM-SHA256",
	}}}

	server := newConfiguredHTTPServer(http.NotFoundHandler(), cfg)
	if _, ok := server.TLSNextProto["h2"]; !ok {
		t.Fatal("configured HTTP server does not install an HTTP/2 handler")
	}
	if server.Protocols.UnencryptedHTTP2() {
		t.Fatal("TLS-only HTTP/2 configuration enabled plaintext h2c")
	}

	tlsConfig := mustGenerationFrontendTLSConfig(t, cfg)
	if !slices.Contains(tlsConfig.NextProtos, "h2") {
		t.Fatalf("frontend TLS protocols = %v, want h2", tlsConfig.NextProtos)
	}

	cfg.Apisix.Ssl.Listen[0].EnableHttp2 = false
	if protocols := mustGenerationFrontendTLSConfig(t, cfg).NextProtos; slices.Contains(protocols, "h2") {
		t.Fatalf("disabled frontend TLS protocols = %v, must not advertise h2", protocols)
	}
}

func TestConfiguredHTTPServerEnablesH2COnlyForPlaintextListener(t *testing.T) {
	cfg := &config.Config{Apisix: config.Apisix{NodeListen: []config.NodeListen{{
		Port: 9080, EnableHttp2: true,
	}}}}

	server := newConfiguredHTTPServer(http.NotFoundHandler(), cfg)
	if !server.Protocols.UnencryptedHTTP2() {
		t.Fatal("explicit plaintext HTTP/2 listener did not enable h2c")
	}
}

func TestEtcdTLSIsNotEnabledForHTTPEndpoints(t *testing.T) {
	verify := true
	settings := config.EtcdTLS{Verify: &verify}
	if etcdTLSRequired([]string{"http://127.0.0.1:2379"}, settings) {
		t.Fatal("etcdTLSRequired() = true for an HTTP endpoint")
	}
	if !etcdTLSRequired([]string{"https://127.0.0.1:2379"}, settings) {
		t.Fatal("etcdTLSRequired() = false for an HTTPS endpoint")
	}
}

func TestEtcdHealthCheckIntervalMapping(t *testing.T) {
	for _, test := range []struct {
		name   string
		config int
		want   time.Duration
	}{
		{name: "default", config: 0, want: 10 * time.Second},
		{name: "negative default", config: -1, want: 10 * time.Second},
		{name: "configured", config: 7, want: 7 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := etcdHealthCheckInterval(test.config); got != test.want {
				t.Fatalf("etcdHealthCheckInterval(%d) = %s, want %s", test.config, got, test.want)
			}
		})
	}
}

func TestStandaloneConfigProviderSelection(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		provider string
		want     bool
	}{
		{name: "yaml data plane", role: "data_plane", provider: "yaml", want: true},
		{name: "json data plane", role: "data_plane", provider: "json", want: true},
		{name: "etcd data plane", role: "data_plane", provider: "etcd", want: false},
		{name: "yaml traditional", role: "traditional", provider: "yaml", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Deployment: config.Deployment{
				Role:          tt.role,
				RoleDataPlane: config.RoleConfig{ConfigProvider: tt.provider},
			}}
			if got := standaloneConfigProvider(cfg) != ""; got != tt.want {
				t.Fatalf("standaloneConfigProvider() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEtcdClientOptionsUseConfiguredWatchAndResyncDelay(t *testing.T) {
	options := etcdClientOptions(config.Etcd{
		Timeout:            7,
		WatchTimeout:       11,
		ResyncDelay:        13,
		HealthCheckTimeout: 17,
		StartupRetry:       19,
	}, nil)
	if options.DialTimeout != 7*time.Second || options.RequestTimeout != 7*time.Second {
		t.Fatalf("etcd request timeouts = %s/%s, want 7s/7s", options.DialTimeout, options.RequestTimeout)
	}
	if options.WatchTimeout != 11*time.Second {
		t.Fatalf("etcd watch timeout = %s, want 11s", options.WatchTimeout)
	}
	if options.ResyncDelay != 13*time.Second {
		t.Fatalf("etcd resync delay = %s, want 13s", options.ResyncDelay)
	}
	if options.HealthCheckInterval != 17*time.Second || options.StartupRetry != 19 {
		t.Fatalf("etcd health/retry options = %s/%d, want 17s/19", options.HealthCheckInterval, options.StartupRetry)
	}
}

func TestFrontendHTTP2DefaultsWithoutConfig(t *testing.T) {
	if frontendHTTP2Enabled(nil) {
		t.Fatal("frontendHTTP2Enabled() = true without config")
	}
	if frontendPlainHTTP2Enabled(nil) {
		t.Fatal("frontendPlainHTTP2Enabled() = true without config")
	}
	if got := configuredTLSListenAddresses(nil); got != nil {
		t.Fatalf("configuredTLSListenAddresses() = %#v, want nil without config", got)
	}

	cfg := &config.Config{}
	if frontendHTTP2Enabled(cfg) {
		t.Fatal("frontendHTTP2Enabled() = true with default config")
	}
	if frontendPlainHTTP2Enabled(cfg) {
		t.Fatal("frontendPlainHTTP2Enabled() = true with default config")
	}
}

func TestEtcdTLSRequiredForCertKeyAndSNI(t *testing.T) {
	verify := true
	settings := config.EtcdTLS{Verify: &verify}
	endpoints := []string{"http://127.0.0.1:2379"}
	if etcdTLSRequired(endpoints, settings) {
		t.Fatal("etcdTLSRequired() = true with only verification configured")
	}

	if !etcdTLSRequired(endpoints, config.EtcdTLS{Cert: "cert.pem"}) {
		t.Fatal("etcdTLSRequired() = false with a client certificate")
	}
	if !etcdTLSRequired(endpoints, config.EtcdTLS{Key: "key.pem"}) {
		t.Fatal("etcdTLSRequired() = false with a client key")
	}
	if !etcdTLSRequired(endpoints, config.EtcdTLS{SNI: "etcd.example.test"}) {
		t.Fatal("etcdTLSRequired() = false with an SNI override")
	}
}

func TestStartHTTPListenerRuntimeReturnsListenError(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listener: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	address := occupied.Addr().String()

	server := &Server{
		staticConfig: &config.EffectiveConfig{},
		addrs:        []string{address},
		server:       newConfiguredHTTPServer(http.NotFoundHandler(), nil),
	}
	_, err = server.startHTTPListenerRuntime(t.Context())
	if err == nil {
		t.Fatal("startHTTPListenerRuntime() error = nil for an occupied listener address")
	}
	if !strings.Contains(err.Error(), occupied.Addr().String()) {
		t.Fatalf("startHTTPListenerRuntime() error = %v, want the occupied address in the error", err)
	}
}

func TestServeHTTPListenerRuntimeReturnsControlListenErrorAndClosesOwnedListeners(t *testing.T) {
	occupiedControl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy control listener: %v", err)
	}
	defer func() { _ = occupiedControl.Close() }()
	controlAddress := occupiedControl.Addr().(*net.TCPAddr)

	dataProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve data listener: %v", err)
	}
	dataAddress := dataProbe.Addr().String()
	_ = dataProbe.Close()

	statusProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve status listener: %v", err)
	}
	statusAddress := statusProbe.Addr().(*net.TCPAddr)
	_ = statusProbe.Close()

	cfg := config.Config{Apisix: config.Apisix{
		EnableControl: true,
		Control:       config.Control{Ip: "127.0.0.1", Port: controlAddress.Port},
		Status:        config.Status{IP: "127.0.0.1", Port: statusAddress.Port},
	}}
	server := &Server{
		staticConfig:  &config.EffectiveConfig{Config: cfg},
		addrs:         []string{dataAddress},
		server:        newConfiguredHTTPServer(http.NotFoundHandler(), nil),
		statusServer:  newConfiguredHTTPServer(http.NotFoundHandler(), nil),
		controlServer: newConfiguredHTTPServer(http.NotFoundHandler(), nil),
	}
	_, err = server.serveHTTPListenerRuntime(nil, nil)
	if err == nil || !strings.Contains(err.Error(), occupiedControl.Addr().String()) {
		t.Fatalf("serveHTTPListenerRuntime() error = %v, want occupied control address", err)
	}

	for name, address := range map[string]string{
		"data":   dataAddress,
		"status": statusAddress.String(),
	} {
		listener, listenErr := net.Listen("tcp", address)
		if listenErr != nil {
			t.Fatalf("%s listener %s was not released: %v", name, address, listenErr)
		}
		_ = listener.Close()
	}
}

func TestServerShutdownCallsOtelShutdownOnce(t *testing.T) {
	otelCalls := 0
	server := newShutdownLifecycleServer(t, nil)
	server.otelShutdown = func(context.Context) error {
		otelCalls++
		return nil
	}

	if err := server.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if otelCalls != 1 {
		t.Fatalf("otel shutdown calls = %d, want exactly 1", otelCalls)
	}
}

func TestLogStreamResultReportsErrorsAndCompletions(t *testing.T) {
	logStreamResult(streamruntime.Result{
		RouteID:  "route-1",
		Protocol: "tcp",
		Remote:   "192.0.2.1:5000",
		Err:      errors.New("connection reset"),
	})
	logStreamResult(streamruntime.Result{
		RouteID:  "route-2",
		Protocol: "tcp",
		Remote:   "192.0.2.2:5000",
		ClientID: "client-7",
	})
}

type newServerTestEngine struct {
	generation.PublicationEngine
	close func(context.Context) error
}

func (e *newServerTestEngine) Close(ctx context.Context) error { return e.close(ctx) }

func (*newServerTestEngine) acquireHTTP() (httpGenerationLease, bool) {
	return httpGenerationLease{}, false
}

func (*newServerTestEngine) acquireStream() (streamGenerationLease, bool) {
	return streamGenerationLease{}, false
}

func (*newServerTestEngine) refreshStreamMetrics() {}

func TestNewServerFactoryFailureClosesResolverWithoutDoubleClose(t *testing.T) {
	manifest, effective, encryption, resolver := newServerInputs(t)
	factoryErr := errors.New("factory failed")
	var calls []string
	factories := newServerFactories{
		initObservability: func(string) (func(context.Context) error, error) {
			calls = append(calls, "observability")
			return func(context.Context) error {
				calls = append(calls, "observability-close")
				return nil
			}, nil
		},
		newFactory: func(
			*capability.Manifest,
			*config.EffectiveConfig,
			secret.Materializer,
			compiler.WorkerRuntimeObservers,
		) (*compiler.WorkerCompilerFactory, error) {
			calls = append(calls, "factory")
			return nil, factoryErr
		},
		closeResolver: func(resolver *secret.GenerationSecretResolver, ctx context.Context) error {
			calls = append(calls, "resolver-close")
			return resolver.Close(ctx)
		},
	}

	server, err := newServerWithFactories(
		effective, manifest, encryption, resolver, factories,
	)
	if server != nil || !errors.Is(err, factoryErr) {
		t.Fatalf("newServerWithFactories() = (%#v, %v), want factory failure", server, err)
	}
	want := []string{"observability", "factory", "resolver-close", "observability-close"}
	if !slices.Equal(calls, want) {
		t.Fatalf("factory cleanup = %v, want %v", calls, want)
	}
}

func TestNewServerRejectsNilDependencyBeforeTakingOwnership(t *testing.T) {
	manifest, effective, encryption, resolver := newServerInputs(t)
	closeResolver := func(*secret.GenerationSecretResolver, context.Context) error {
		t.Fatal("invalid dependency validation closed resolver")
		return nil
	}
	factories := newServerFactories{
		initObservability: func(string) (func(context.Context) error, error) {
			t.Fatal("invalid dependency validation initialized observability")
			return nil, nil
		},
		closeResolver: closeResolver,
	}
	tests := []struct {
		name       string
		effective  *config.EffectiveConfig
		manifest   *capability.Manifest
		encryption data_encryption.Service
		resolver   *secret.GenerationSecretResolver
	}{
		{name: "effective", manifest: manifest, encryption: encryption, resolver: resolver},
		{name: "manifest", effective: effective, encryption: encryption, resolver: resolver},
		{name: "encryption", effective: effective, manifest: manifest, resolver: resolver},
		{name: "resolver", effective: effective, manifest: manifest, encryption: encryption},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := newServerWithFactories(
				test.effective,
				test.manifest,
				test.encryption,
				test.resolver,
				factories,
			)
			if server != nil || err == nil {
				t.Fatalf("newServerWithFactories() = (%#v, %v), want validation error", server, err)
			}
		})
	}
	if err := resolver.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newServerInputs(
	t *testing.T,
) (*capability.Manifest, *config.EffectiveConfig, data_encryption.Service, *secret.GenerationSecretResolver) {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	encryption := data_encryption.NewService(false, nil, catalog)
	resolver, err := secret.NewGenerationSecretResolver(encryption)
	if err != nil {
		t.Fatal(err)
	}
	effective := &config.EffectiveConfig{}
	return manifest, effective, encryption, resolver
}

type lifecycleTestProducer struct {
	start func(context.Context) error
	stop  func() error
}

func (p *lifecycleTestProducer) Start(ctx context.Context) error { return p.start(ctx) }
func (p *lifecycleTestProducer) Stop() error                     { return p.stop() }

type lifecycleDesiredApplierFunc func(
	context.Context,
	generation.DesiredBatch,
) (generation.Acknowledgement, error)

func (f lifecycleDesiredApplierFunc) Apply(
	ctx context.Context,
	batch generation.DesiredBatch,
) (generation.Acknowledgement, error) {
	return f(ctx, batch)
}

type lifecycleTestStream struct {
	close func(context.Context) error
}

func (r *lifecycleTestStream) Close(ctx context.Context) error { return r.close(ctx) }

type lifecycleTestListener struct {
	closeOnce sync.Once
	closed    chan struct{}
	onClose   func()
}

func (*lifecycleTestListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (*lifecycleTestListener) Addr() net.Addr            { return lifecycleTestAddr("test") }
func (l *lifecycleTestListener) Close() error {
	l.closeOnce.Do(func() {
		if l.onClose != nil {
			l.onClose()
		}
		close(l.closed)
	})
	return nil
}

type lifecycleTestAddr string

func (a lifecycleTestAddr) Network() string { return string(a) }
func (a lifecycleTestAddr) String() string  { return string(a) }

func TestServerStartsProviderBeforeStreamAndHTTP(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	record := func(call string) { mu.Lock(); calls = append(calls, call); mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	engine := &newServerTestEngine{
		close: func(context.Context) error { record("engine-close"); return nil },
	}
	stream := &lifecycleTestStream{close: func(context.Context) error {
		record("stream-close")
		return nil
	}}
	producer := &lifecycleTestProducer{
		start: func(context.Context) error {
			record("provider")
			return nil
		},
		stop: func() error { record("provider-stop"); return nil },
	}
	server := &Server{
		staticConfig: &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
			ProxyMode:   "stream",
			StreamProxy: config.StreamProxy{Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}}},
		}}},
		server:        &http.Server{},
		routes:        newGenerationRouteHandler(engine.acquireHTTP),
		engine:        engine,
		closeResolver: func(*secret.GenerationSecretResolver, context.Context) error { return nil },
		runtimeFactories: serverRuntimeFactories{
			newStream: func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
				record("stream")
				return stream, nil
			},
			startHTTP: func(*Server, context.Context) (<-chan error, error) {
				record("http")
				cancel()
				return make(chan error), nil
			},
			newProducer: func(*Server, context.Context) (configProducer, error) { return producer, nil },
		},
	}
	if err := server.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v", err)
	}
	mu.Lock()
	got := slices.Clone(calls)
	mu.Unlock()
	if len(got) < 3 || !slices.Equal(got[:3], []string{"provider", "stream", "http"}) {
		t.Fatalf("startup order = %v, want provider, stream, HTTP prefix", got)
	}
}

func TestServerStartCancellationJoinsBlockedStandaloneInitialApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apisix.yaml")
	if err := os.WriteFile(path, []byte("routes:\n  - id: r1\n    uri: /one\n#END\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	applyEntered := make(chan struct{})
	applyExited := make(chan struct{})
	applier := lifecycleDesiredApplierFunc(func(
		ctx context.Context,
		_ generation.DesiredBatch,
	) (generation.Acknowledgement, error) {
		close(applyEntered)
		<-ctx.Done()
		close(applyExited)
		return generation.Acknowledgement{}, ctx.Err()
	})
	watcher := config.NewStandaloneFileWatcher(
		path,
		"yaml",
		applier,
		testutil.DataEncryptionService(false, nil),
	)
	producer := &standaloneConfigProducer{watcher: watcher}
	server := newShutdownLifecycleServer(t, nil)
	server.staticConfig = &config.EffectiveConfig{}
	server.runtimeFactories = serverRuntimeFactories{
		startHTTP: func(*Server, context.Context) (<-chan error, error) {
			return make(chan error), nil
		},
		newProducer: func(*Server, context.Context) (configProducer, error) {
			return producer, nil
		},
	}
	startCtx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- server.Start(startCtx) }()
	<-applyEntered

	cancel()
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want %v", err, context.Canceled)
	}
	select {
	case <-applyExited:
	default:
		t.Fatal("Start() returned before the standalone Apply goroutine exited")
	}
}

func TestServerCancellationStopsBlockedProducerBeforeListeners(t *testing.T) {
	producerStartEntered := make(chan struct{})
	producerStartExited := make(chan struct{})
	producerStopEntered := make(chan struct{})
	producer := &lifecycleTestProducer{
		start: func(ctx context.Context) error {
			close(producerStartEntered)
			<-ctx.Done()
			close(producerStartExited)
			return ctx.Err()
		},
		stop: func() error {
			close(producerStopEntered)
			return nil
		},
	}
	listenerStarted := false
	server := newShutdownLifecycleServer(t, nil)
	server.staticConfig = &config.EffectiveConfig{}
	server.runtimeFactories = serverRuntimeFactories{
		startHTTP: func(*Server, context.Context) (<-chan error, error) {
			listenerStarted = true
			return make(chan error), nil
		},
		newProducer: func(*Server, context.Context) (configProducer, error) {
			return producer, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- server.Start(ctx) }()
	<-producerStartEntered
	cancel()
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context cancellation", err)
	}
	select {
	case <-producerStopEntered:
	default:
		t.Fatal("Start() returned before stopping the producer")
	}
	select {
	case <-producerStartExited:
	default:
		t.Fatal("Start() returned before producer Start exited")
	}
	if listenerStarted {
		t.Fatal("HTTP listener started while initial provider synchronization was blocked")
	}
}

func TestServerEtcdInitialFetchFailureStopsBeforeWatchAndListeners(t *testing.T) {
	client := &failingInitialEtcdClient{
		watchStarted: make(chan struct{}),
		closeCalled:  make(chan struct{}),
		fetchErr:     errors.New("etcd offline"),
	}
	producer := newEtcdConfigProducer(client)
	server := newShutdownLifecycleServer(t, nil)
	server.staticConfig = &config.EffectiveConfig{}
	listenerStarted := false
	server.runtimeFactories = serverRuntimeFactories{
		startHTTP: func(*Server, context.Context) (<-chan error, error) {
			listenerStarted = true
			return make(chan error), nil
		},
		newProducer: func(*Server, context.Context) (configProducer, error) {
			return producer, nil
		},
	}
	if err := server.Start(context.Background()); !errors.Is(err, client.fetchErr) {
		t.Fatalf("Start() error = %v, want initial fetch failure", err)
	}
	if listenerStarted {
		t.Fatal("HTTP listeners started before successful initial etcd synchronization")
	}
	select {
	case <-client.watchStarted:
		t.Fatal("etcd watch started after failed initial synchronization")
	default:
	}
	select {
	case <-client.closeCalled:
	default:
		t.Fatal("Start() returned before the etcd client was closed")
	}
}

func TestServerProviderStartFailureDoesNotOpenListeners(t *testing.T) {
	var calls []string
	listenerStarted := false
	engine := &newServerTestEngine{
		close: func(context.Context) error { calls = append(calls, "engine-close"); return nil },
	}
	server := &Server{
		staticConfig: &config.EffectiveConfig{}, server: &http.Server{},
		routes: newGenerationRouteHandler(engine.acquireHTTP), engine: engine,
		closeResolver: func(*secret.GenerationSecretResolver, context.Context) error { return nil },
		runtimeFactories: serverRuntimeFactories{
			startHTTP: func(*Server, context.Context) (<-chan error, error) {
				listenerStarted = true
				return make(chan error), nil
			},
			newProducer: func(*Server, context.Context) (configProducer, error) {
				return &lifecycleTestProducer{
					start: func(context.Context) error { return errors.New("static provider failure") },
					stop:  func() error { calls = append(calls, "provider-stop"); return nil },
				}, nil
			},
		},
	}
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil for static provider failure")
	}
	if listenerStarted {
		t.Fatal("HTTP listeners started before provider initialization succeeded")
	}
	if !slices.Equal(calls, []string{"provider-stop", "engine-close"}) {
		t.Fatalf("startup failure cleanup = %v", calls)
	}
}

func TestServerShutdownStopsProviderBeforeRejectingNewLeases(t *testing.T) {
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	listener := &lifecycleTestListener{closed: make(chan struct{})}
	server := newShutdownLifecycleServer(t, nil)
	server.producer = &lifecycleTestProducer{
		start: func(context.Context) error { return nil },
		stop: func() error {
			close(stopEntered)
			<-releaseStop
			return nil
		},
	}
	server.retainListener(listener)
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(context.Background()) }()
	<-stopEntered
	if _, ok := server.beginStart(context.Background()); ok {
		t.Fatal("new startup admitted after shutdown began")
	}
	select {
	case <-listener.closed:
		t.Fatal("listener closed before provider stop joined")
	default:
	}
	close(releaseStop)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("Shutdown() did not reject new listeners after provider stop")
	}
}

func TestServerShutdownDoesNotCancelLifecycleBeforeProviderStopJoins(t *testing.T) {
	stopEntered := make(chan struct{})
	stopExited := make(chan struct{})
	releaseStop := make(chan struct{})
	listener := &lifecycleTestListener{closed: make(chan struct{})}
	server := newShutdownLifecycleServer(t, nil)
	lifecycleCtx, ok := server.beginStart(context.Background())
	if !ok {
		t.Fatal("beginStart() rejected initial startup")
	}
	server.producer = &lifecycleTestProducer{
		start: func(context.Context) error { return nil },
		stop: func() error {
			close(stopEntered)
			<-releaseStop
			close(stopExited)
			return nil
		},
	}
	server.retainListener(listener)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstShutdownDone := make(chan error, 1)
	go func() { firstShutdownDone <- server.Shutdown(firstCtx) }()
	<-stopEntered

	lifecycleCanceledBeforeJoin := false
	select {
	case <-lifecycleCtx.Done():
		lifecycleCanceledBeforeJoin = true
	default:
	}
	listenerClosedBeforeJoin := false
	select {
	case <-listener.closed:
		listenerClosedBeforeJoin = true
	default:
	}

	cancelFirst()
	if err := <-firstShutdownDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Shutdown() error = %v, want %v", err, context.Canceled)
	}
	lifecycleCanceledAfterTimeout := false
	select {
	case <-lifecycleCtx.Done():
		lifecycleCanceledAfterTimeout = true
	default:
	}
	listenerClosedAfterTimeout := false
	select {
	case <-listener.closed:
		listenerClosedAfterTimeout = true
	default:
	}

	close(releaseStop)
	secondShutdownDone := make(chan error, 1)
	go func() { secondShutdownDone <- server.Shutdown(context.Background()) }()
	<-stopExited
	<-lifecycleCtx.Done()
	server.finishStart()
	if err := <-secondShutdownDone; err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if lifecycleCanceledBeforeJoin {
		t.Fatal("Shutdown() canceled the runtime lifecycle before provider Stop joined")
	}
	if listenerClosedBeforeJoin {
		t.Fatal("Shutdown() closed a listener before provider Stop joined")
	}
	if lifecycleCanceledAfterTimeout {
		t.Fatal("timed-out Shutdown() canceled the runtime lifecycle before provider Stop joined")
	}
	if listenerClosedAfterTimeout {
		t.Fatal("timed-out Shutdown() closed a listener before provider Stop joined")
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("Shutdown() did not close the listener after provider Stop joined")
	}
}

func TestServerShutdownRejectsStreamBeforeHTTPDrain(t *testing.T) {
	sourceCalled := make(chan struct{}, 1)
	runtime, err := streamruntime.NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		func() (streamruntime.RouterLease, bool) {
			sourceCalled <- struct{}{}
			return streamruntime.RouterLease{}, false
		},
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	httpDrainEntered := make(chan struct{})
	releaseHTTPDrain := make(chan struct{})
	server := newShutdownLifecycleServer(t, nil)
	server.streamRuntime = runtime
	server.shutdownHTTP = func(context.Context) error {
		close(httpDrainEntered)
		<-releaseHTTPDrain
		return nil
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown(context.Background()) }()
	<-httpDrainEntered

	connection, dialErr := net.Dial("tcp", runtime.Addresses()[0])
	sourceAcquired := false
	if dialErr == nil {
		<-sourceCalled
		sourceAcquired = true
		_ = connection.Close()
	} else {
		select {
		case <-sourceCalled:
			sourceAcquired = true
		default:
		}
	}
	close(releaseHTTPDrain)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if dialErr == nil {
		t.Fatal("stream listener accepted a connection after shutdown entered HTTP drain")
	}
	if sourceAcquired {
		t.Fatal("stream RouterSource was acquired after stream rejection phase")
	}
}

func TestServerShutdownDrainsHTTPHijackAndStreamBeforeEngineClose(t *testing.T) {
	requestRelease := make(chan struct{})
	hijackRelease := make(chan struct{})
	streamRelease := make(chan struct{})
	engineClosed := make(chan struct{})
	server := newShutdownLifecycleServer(t, func(context.Context) error {
		close(engineClosed)
		return nil
	})
	server.shutdownHTTP = func(context.Context) error { <-requestRelease; return nil }
	server.drainRoutes = func(context.Context) error { <-hijackRelease; return nil }
	server.streamRuntime = &lifecycleTestStream{close: func(ctx context.Context) error {
		select {
		case <-streamRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(context.Background()) }()
	close(requestRelease)
	close(hijackRelease)
	select {
	case <-engineClosed:
		t.Fatal("engine closed before stream drain")
	default:
	}
	close(streamRelease)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	<-engineClosed
}

func TestServerShutdownTimeoutDoesNotReleaseEngineOrResolver(t *testing.T) {
	drainRelease := make(chan struct{})
	var engineCloses, resolverCloses int
	server := newShutdownLifecycleServer(t, func(context.Context) error { engineCloses++; return nil })
	server.shutdownHTTP = func(ctx context.Context) error {
		select {
		case <-drainRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	server.closeResolver = func(*secret.GenerationSecretResolver, context.Context) error {
		resolverCloses++
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Shutdown() error = %v, want context.Canceled", err)
	}
	if engineCloses != 0 || resolverCloses != 0 {
		t.Fatalf("timeout released later owners: engine=%d resolver=%d",
			engineCloses, resolverCloses)
	}
	close(drainRelease)
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("resumed Shutdown() error = %v", err)
	}
	if engineCloses != 1 || resolverCloses != 1 {
		t.Fatalf(
			"resumed cleanup closes = engine:%d resolver:%d",
			engineCloses, resolverCloses,
		)
	}
}

func TestServerShutdownEngineResidualRetriesBeforeLaterOwners(t *testing.T) {
	residualErr := newServerShutdownResidualError(t)
	var calls []string
	engineCloses := 0
	server := newShutdownLifecycleServer(t, func(context.Context) error {
		engineCloses++
		calls = append(calls, "engine")
		if engineCloses == 1 {
			return residualErr
		}
		return nil
	})
	server.closeResolver = func(*secret.GenerationSecretResolver, context.Context) error {
		calls = append(calls, "resolver")
		return nil
	}
	server.otelShutdown = func(context.Context) error {
		calls = append(calls, "observability")
		return nil
	}

	first := server.Shutdown(context.Background())
	var residual *runtime.TaskResidualError
	if !errors.Is(first, context.DeadlineExceeded) || !errors.As(first, &residual) {
		t.Fatalf("first Shutdown() error = %v, want structured deadline residual", first)
	}
	if server.shutdownPhase != shutdownPhaseDrained || server.engineClosed {
		t.Fatalf("first Shutdown() phase = %d, engineClosed = %t, want drained and retained engine",
			server.shutdownPhase, server.engineClosed)
	}
	if !slices.Equal(calls, []string{"engine"}) {
		t.Fatalf("first Shutdown() calls = %v, want engine only", calls)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Shutdown() error = %v", err)
	}
	want := []string{"engine", "engine", "resolver", "observability"}
	if !slices.Equal(calls, want) {
		t.Fatalf("retry Shutdown() calls = %v, want %v", calls, want)
	}
	if errors.Is(server.shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("terminal Shutdown() replay retained transient deadline: %v", server.shutdownErr)
	}
}

func TestServerShutdownEngineResidualPreservesEarlierTerminalDrainError(t *testing.T) {
	terminalDrainErr := errors.New("terminal HTTP drain failure")
	residualErr := newServerShutdownResidualError(t)
	routeDrains := 0
	engineCloses := 0
	server := newShutdownLifecycleServer(t, func(context.Context) error {
		engineCloses++
		if engineCloses == 1 {
			return residualErr
		}
		return nil
	})
	server.shutdownHTTP = func(context.Context) error { return terminalDrainErr }
	server.drainRoutes = func(context.Context) error {
		routeDrains++
		if routeDrains == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}

	first := server.Shutdown(context.Background())
	if !errors.Is(first, terminalDrainErr) || !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown() error = %v, want terminal drain and deadline errors", first)
	}
	if engineCloses != 0 {
		t.Fatalf("first Shutdown() engine closes = %d, want 0 before route drain", engineCloses)
	}

	second := server.Shutdown(context.Background())
	var residual *runtime.TaskResidualError
	if !errors.Is(second, terminalDrainErr) || !errors.As(second, &residual) {
		t.Fatalf("second Shutdown() error = %v, want terminal drain and engine residual", second)
	}

	final := server.Shutdown(context.Background())
	if !errors.Is(final, terminalDrainErr) {
		t.Fatalf("final Shutdown() error = %v, want retained %v", final, terminalDrainErr)
	}
	if errors.Is(final, context.DeadlineExceeded) {
		t.Fatalf("final Shutdown() error retained transient deadline: %v", final)
	}
	if engineCloses != 2 {
		t.Fatalf("engine close calls = %d, want residual attempt plus retry", engineCloses)
	}
}

func TestServerShutdownWaiterReplaysIncompleteAttempt(t *testing.T) {
	residualErr := newServerShutdownResidualError(t)
	engineStarted := make(chan struct{})
	releaseEngine := make(chan struct{})
	engineCloses := 0
	server := newShutdownLifecycleServer(t, func(context.Context) error {
		engineCloses++
		if engineCloses == 1 {
			close(engineStarted)
			<-releaseEngine
			return residualErr
		}
		return nil
	})

	firstDone := make(chan error, 1)
	go func() { firstDone <- server.Shutdown(context.Background()) }()
	<-engineStarted

	waiterEntered := make(chan struct{})
	waiterCtx := &shutdownWaiterContext{
		Context: context.Background(),
		entered: waiterEntered,
		done:    make(chan struct{}),
	}
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- server.Shutdown(waiterCtx) }()
	<-waiterEntered

	close(releaseEngine)
	first := <-firstDone
	waiter := <-waiterDone
	if first == nil || !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first Shutdown() error = %v, want residual error", first)
	}
	if waiter != first {
		t.Fatalf("waiter Shutdown() error = %v, want immutable first-attempt result %v", waiter, first)
	}
	if engineCloses != 1 {
		t.Fatalf("engine close calls after joined attempt = %d, want 1", engineCloses)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("post-attempt Shutdown() retry error = %v", err)
	}
	if engineCloses != 2 {
		t.Fatalf("engine close calls after public retry = %d, want 2", engineCloses)
	}
}

func TestWaitShutdownAttemptPrefersPublishedResult(t *testing.T) {
	want := errors.New("published shutdown result")
	attempt := &shutdownAttemptResult{done: make(chan struct{}), err: want}
	close(attempt.done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := waitShutdownAttempt(ctx, attempt); got != want {
		t.Fatalf("waitShutdownAttempt() error = %v, want published result %v", got, want)
	}
}

type shutdownWaiterContext struct {
	context.Context
	entered chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (c *shutdownWaiterContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.done
}

func newServerShutdownResidualError(t *testing.T) error {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	release := make(chan struct{})
	started := make(chan struct{})
	if err := tasks.Go(runtime.TaskSpec{
		Owner: "plugin/test/server-shutdown", Criticality: runtime.TaskPlugin,
	}, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, residualErr := tasks.Stop(short)
	var residual *runtime.TaskResidualError
	if !errors.As(residualErr, &residual) {
		t.Fatalf("fixture Stop error = %v, want structured residual", residualErr)
	}
	close(release)
	if _, err := tasks.Stop(context.Background()); err != nil {
		t.Fatalf("fixture Stop retry error = %v", err)
	}
	return residualErr
}

func TestServerShutdownClosesEngineResolverObservabilityInOrder(t *testing.T) {
	var calls []string
	server := newShutdownLifecycleServer(t, func(context.Context) error {
		calls = append(calls, "engine")
		return nil
	})
	server.closeResolver = func(*secret.GenerationSecretResolver, context.Context) error {
		calls = append(calls, "resolver")
		return nil
	}
	server.otelShutdown = func(context.Context) error {
		calls = append(calls, "observability")
		return nil
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	want := []string{"engine", "resolver", "observability"}
	if !slices.Equal(calls, want) {
		t.Fatalf("terminal cleanup order = %v, want %v", calls, want)
	}
}

func TestServerRepeatedShutdownReplaysFirstTerminalCleanupError(t *testing.T) {
	t.Run("terminal engine error", func(t *testing.T) {
		terminalErr := errors.New("engine cleanup failed")
		engineCloses := 0
		server := newShutdownLifecycleServer(t, func(context.Context) error {
			engineCloses++
			return terminalErr
		})
		first := server.Shutdown(context.Background())
		second := server.Shutdown(context.Background())
		if !errors.Is(first, terminalErr) || !errors.Is(second, terminalErr) {
			t.Fatalf("Shutdown() errors = %v / %v, want replayed %v", first, second, terminalErr)
		}
		if engineCloses != 1 {
			t.Fatalf("engine close calls = %d, want 1", engineCloses)
		}
	})

	t.Run("terminal drain error before timeout", func(t *testing.T) {
		terminalErr := errors.New("HTTP drain failed")
		drainRelease := make(chan struct{})
		server := newShutdownLifecycleServer(t, nil)
		server.shutdownHTTP = func(context.Context) error { return terminalErr }
		ctx, cancel := context.WithCancel(context.Background())
		server.drainRoutes = func(ctx context.Context) error {
			cancel()
			select {
			case <-drainRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		first := server.Shutdown(ctx)
		if !errors.Is(first, terminalErr) || !errors.Is(first, context.Canceled) {
			t.Fatalf("first Shutdown() error = %v, want terminal and context errors", first)
		}
		close(drainRelease)
		second := server.Shutdown(context.Background())
		third := server.Shutdown(context.Background())
		if !errors.Is(second, terminalErr) || !errors.Is(third, terminalErr) {
			t.Fatalf("resumed Shutdown() errors = %v / %v, want replayed %v", second, third, terminalErr)
		}
	})
}

func newShutdownLifecycleServer(t *testing.T, closeEngine func(context.Context) error) *Server {
	t.Helper()
	if closeEngine == nil {
		closeEngine = func(context.Context) error { return nil }
	}
	engine := &newServerTestEngine{
		close: closeEngine,
	}
	return &Server{
		server:        &http.Server{},
		routes:        newGenerationRouteHandler(engine.acquireHTTP),
		engine:        engine,
		closeResolver: func(*secret.GenerationSecretResolver, context.Context) error { return nil },
	}
}

func TestConfiguredListenAddressesUsesNodeListen(t *testing.T) {
	if got := configuredListenAddresses(nil); !reflect.DeepEqual(got, []string{":8080"}) {
		t.Fatalf("configuredListenAddresses() = %#v, want default :8080", got)
	}

	cfg := &config.Config{Apisix: config.Apisix{
		NodeListen: []config.NodeListen{{Ip: "127.0.0.1", Port: 9080}},
	}}
	if got := configuredListenAddresses(cfg); !reflect.DeepEqual(got, []string{"127.0.0.1:9080"}) {
		t.Fatalf("configuredListenAddresses() = %#v, want node listen address", got)
	}
}

func TestPluginConfiguredConsultsEnabledPlugins(t *testing.T) {
	if pluginConfigured(nil, "node-status") {
		t.Fatal("pluginConfigured() = true without config")
	}

	cfg := &config.Config{Plugins: []string{"node-status"}}
	if !pluginConfigured(cfg, "node-status") {
		t.Fatal("pluginConfigured() = false for an enabled plugin")
	}
	if pluginConfigured(cfg, "prometheus") {
		t.Fatal("pluginConfigured() = true for a disabled plugin")
	}
}

func TestFrontendTLSConfigDefaultsWithoutHTTP2(t *testing.T) {
	tlsConfig := mustGenerationFrontendTLSConfig(t, nil)
	if !reflect.DeepEqual(tlsConfig.NextProtos, []string{"http/1.1"}) {
		t.Fatalf("NextProtos = %v, want only http/1.1", tlsConfig.NextProtos)
	}
	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %v, want TLS 1.2", tlsConfig.MinVersion)
	}
	if tlsConfig.MaxVersion != 0 {
		t.Fatalf("MaxVersion = %v, want unset default for TLS 1.3 support", tlsConfig.MaxVersion)
	}
}

func TestNormalizeRequestPathShortCircuitsAndPreservesTrailingSlash(t *testing.T) {
	sameRequest := httptest.NewRequest(http.MethodGet, "/plain", nil)
	var served *http.Request
	handler := normalizeRequestPath(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), sameRequest)
	if served != sameRequest {
		t.Fatal("normalizeRequestPath() cloned an already-clean request")
	}

	var gotPath string
	handler = normalizeRequestPath(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/trailing/", nil))
	if gotPath != "/trailing/" {
		t.Fatalf("path = %q, want preserved trailing slash", gotPath)
	}
}

func TestStartPrometheusExportServerWithDuplicatePluginNames(t *testing.T) {
	effective := &config.EffectiveConfig{Config: config.Config{
		Plugins: []string{"prometheus", "prometheus"},
		PluginAttr: map[string]map[string]any{
			"prometheus": {"enable_export_server": false},
		},
	}}

	s := &Server{staticConfig: effective}
	if err := s.startPrometheusExportServer(); err != nil {
		t.Fatalf("startPrometheusExportServer() error = %v", err)
	}
	if s.prometheusServer != nil {
		t.Fatal("export server started while disabled")
	}
}

func TestStartPrometheusExportServerWithoutPrometheus(t *testing.T) {
	effective := &config.EffectiveConfig{Config: config.Config{Plugins: []string{"limit-req"}}}

	s := &Server{staticConfig: effective}
	if err := s.startPrometheusExportServer(); err != nil {
		t.Fatalf("startPrometheusExportServer() error = %v", err)
	}
	if s.prometheusServer != nil {
		t.Fatal("export server started without prometheus plugin")
	}
}
