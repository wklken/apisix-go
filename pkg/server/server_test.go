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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
	"github.com/wklken/apisix-go/pkg/testutil"

	"github.com/go-chi/chi/v5"
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
		nil,
		generation.RecoveryState{},
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

func TestServerShutdownClosesClusterRegistry(t *testing.T) {
	clusters := proxy.NewClusterRegistry(proxy.NopClusterObserver{})
	lease, err := clusters.Acquire(proxy.ClusterConfig{
		Name:    "shutdown",
		Targets: map[string]int{"http://127.0.0.1:18090": 1},
		Transport: (&proxy.TransportOptionBuilder{}).
			WithDialTimeout(time.Second).
			Build(),
		MaxInFlight: 4,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if got := clusters.Len(); got != 1 {
		t.Fatalf("registry.Len() = %d, want 1", got)
	}

	s := &Server{server: &http.Server{}, routes: newRouteHandler(http.NotFoundHandler(), nil), clusters: clusters}
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if !lease.Cluster().Closed() {
		t.Fatal("shutdown() did not close the cluster registry")
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

func TestServerShutdownStopsProducerBeforeStoreAndJoinsErrors(t *testing.T) {
	producerErr := errors.New("producer stop failed")
	streamErr := errors.New("stream stop failed")
	traceErr := errors.New("trace stop failed")
	events := make(chan *store.Event)
	storage, err := store.Open(
		filepath.Join(t.TempDir(), "shutdown.db"),
		events,
		testutil.DataEncryptionService(false, nil),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })
	producer := &fakeConfigProducer{stopErr: producerErr, stopSeen: make(chan struct{})}
	producer.onStop = func() {
		if _, err := storage.SnapshotBuckets([]string{"routes"}); err != nil {
			t.Errorf("Store was closed before producer Stop(): %v", err)
		}
	}
	stream := &streamRuntimeCloseError{err: streamErr}
	server := &Server{
		server:         &http.Server{},
		routes:         newRouteHandler(http.NotFoundHandler(), nil),
		producer:       producer,
		streamRuntime:  stream,
		storage:        storage,
		dataEncryption: testutil.DataEncryptionService(false, nil),
		otelShutdown:   func(context.Context) error { return traceErr },
	}

	err = server.Shutdown(context.Background())
	for _, want := range []error{producerErr, streamErr, traceErr} {
		if !errors.Is(err, want) {
			t.Fatalf("Shutdown() error = %v, want joined %v", err, want)
		}
	}
	if producer.stopped != 1 {
		t.Fatalf("producer Stop calls = %d, want 1", producer.stopped)
	}
	if err := server.Shutdown(context.Background()); !errors.Is(err, producerErr) {
		t.Fatalf("repeated Shutdown() error = %v, want original joined error", err)
	}
	if producer.stopped != 1 {
		t.Fatalf("repeated Shutdown() called producer Stop %d times, want 1", producer.stopped)
	}
}

func TestServerShutdownCancelsAndJoinsQueuedReloadScheduler(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(
		filepath.Join(t.TempDir(), "scheduler.db"),
		events,
		testutil.DataEncryptionService(false, nil),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	t.Cleanup(func() { _ = storage.Stop() })
	server := &Server{
		server:          &http.Server{},
		routes:          newRouteHandler(http.NotFoundHandler(), nil),
		storage:         storage,
		dataEncryption:  testutil.DataEncryptionService(false, nil),
		reloadEventChan: make(chan struct{}, 1),
	}
	server.startReloadScheduler(context.Background())
	server.reloadEventChan <- struct{}{}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case <-server.schedulerDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown() returned before reload scheduler exited")
	}
	if _, err := storage.SnapshotBuckets([]string{"routes"}); err == nil {
		t.Fatal("Store remained open after scheduler shutdown")
	}
}

func TestStartFailureCleanupIsBoundedWithActiveHTTPRequest(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(
		filepath.Join(t.TempDir(), "startup-cleanup.db"),
		events,
		testutil.DataEncryptionService(false, nil),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	routes := newRouteHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}), nil)
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

	server := &Server{server: httpServer, routes: routes, storage: storage}
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
	if _, err := storage.SnapshotBuckets([]string{"routes"}); err != nil {
		t.Fatalf("startup cleanup closed the Store while the active handler still owned it: %v", err)
	}
	release()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("active request did not exit after release")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := storage.SnapshotBuckets([]string{"routes"}); err != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("startup cleanup did not close the Store after the active handler exited")
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
	handler := normalizeForwardedHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusNoContent)
	}), []string{"192.128.0.0/16"})
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotForwardedFor != "" {
		t.Fatalf("X-Forwarded-For = %q, want forged header removed", gotForwardedFor)
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

func TestHealthEndpointsKeepLivenessIndependentAndReserveExactPaths(t *testing.T) {
	restoreMetrics := installHealthMetrics(t)
	defer restoreMetrics()

	dynamicCalls := 0
	handler := newHealthHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dynamicCalls++
		w.WriteHeader(http.StatusNoContent)
	}), false)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("/livez status = %d, want %d", response.Code, http.StatusOK)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before config apply status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	wantBody := `{"config_apply_ready":false,"etcd_reachable":false}`
	if got, want := strings.TrimSpace(response.Body.String()), wantBody; got != want {
		t.Fatalf("/readyz failure body = %q, want %q", got, want)
	}

	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("/readyz after standalone config apply status = %d, want %d", response.Code, http.StatusOK)
	}

	for _, path := range []string{"/livez/anything", "/readyz/anything"} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want dynamic %d", path, response.Code, http.StatusNoContent)
		}
	}
	if dynamicCalls != 2 {
		t.Fatalf("dynamic fallback calls = %d, want 2", dynamicCalls)
	}
}

func TestConfiguredHTTPHandlerLimitsHealthEndpoints(t *testing.T) {
	cfg := &config.Config{NginxConfig: config.NginxConfig{
		HTTP: config.NginxHTTP{ClientMaxBodySize: 3},
	}}
	handler := newConfiguredHTTPHandler(http.NotFoundHandler(), cfg)

	for _, path := range []string{"/livez", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("abcd"))
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			assertRequestBodyLimitResponse(t, response)
		})
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

func TestReadyzRequiresEtcdReachabilityInEtcdMode(t *testing.T) {
	restoreMetrics := installHealthMetrics(t)
	defer restoreMetrics()

	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	handler := newHealthHandler(http.NotFoundHandler(), true)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before etcd reachability status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	wantBody := `{"config_apply_ready":true,"etcd_reachable":false}`
	if got, want := strings.TrimSpace(response.Body.String()), wantBody; got != want {
		t.Fatalf("/readyz etcd failure body = %q, want %q", got, want)
	}

	metrics.RecordEtcdReachable(true)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("/readyz after etcd reachability status = %d, want %d", response.Code, http.StatusOK)
	}

	metrics.RecordEtcdReachable(false)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after etcd loss status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
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

	tlsConfig := mustFrontendTLSConfig(t, cfg)
	if !slices.Contains(tlsConfig.NextProtos, "h2") {
		t.Fatalf("frontend TLS protocols = %v, want h2", tlsConfig.NextProtos)
	}

	cfg.Apisix.Ssl.Listen[0].EnableHttp2 = false
	if protocols := mustFrontendTLSConfig(t, cfg).NextProtos; slices.Contains(protocols, "h2") {
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

type failingStrictRouteBuilder struct {
	stopped bool
}

func (*failingStrictRouteBuilder) BuildWithRouteQuarantine() (*chi.Mux, error) {
	return nil, errors.New("invalid initial snapshot")
}

func (builder *failingStrictRouteBuilder) Stop() { builder.stopped = true }

func TestInitialRouteBuildFailureIsRejected(t *testing.T) {
	builder := &failingStrictRouteBuilder{}
	routes := newRouteHandler(http.NotFoundHandler(), nil)
	err := buildAndInstallInitialRoutes(routes, builder)
	if err == nil || !strings.Contains(err.Error(), "build initial routes") {
		t.Fatalf("buildAndInstallInitialRoutes() error = %v", err)
	}
	if !builder.stopped {
		t.Fatal("failed initial builder was not stopped")
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

func TestFetchAndSyncInitialEtcdConfigContextIsPropagated(t *testing.T) {
	ctx := t.Context()
	seen := make(chan context.Context, 1)
	err := fetchAndSyncInitialEtcdConfigContext(
		ctx,
		func(fetchCtx context.Context) error {
			seen <- fetchCtx
			return nil
		},
		func() error { return nil },
	)
	if err != nil {
		t.Fatalf("fetchAndSyncInitialEtcdConfigContext() error = %v", err)
	}
	select {
	case fetchCtx := <-seen:
		if fetchCtx != ctx {
			t.Fatalf("fetch context = %p, want startup context %p", fetchCtx, ctx)
		}
	default:
		t.Fatal("fetchAndSyncInitialEtcdConfigContext() did not invoke fetch")
	}
}

func TestFrontendTLSGetCertificateSelectsFromPublishedIndex(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.GetStore(t.TempDir()+"/frontend-tls.db", events, testutil.DataEncryptionService(false, nil))
	if err != nil {
		t.Fatalf("get store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })

	cert, key := frontendTestCertificatePEM(t, "api.example.test")
	put := func(bucket, id string, value []byte) {
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/" + bucket + "/" + id)
		event.Value = value
		events <- event
	}
	ssl := func(id string, snis []string, status int) []byte {
		snisJSON := "[]"
		if len(snis) > 0 {
			snisJSON = `["` + strings.Join(snis, `","`) + `"]`
		}
		return []byte(`{"id":"` + id + `","snis":` + snisJSON +
			`,"cert":"` + cert + `","key":"` + key + `","status":` + strconv.Itoa(status) + `}`)
	}
	put("ssls", "ssl-wild", ssl("ssl-wild", []string{"*.example.test"}, 1))
	put("ssls", "ssl-exact", ssl("ssl-exact", []string{"api.example.test"}, 1))
	put("ssls", "ssl-disabled", ssl("ssl-disabled", []string{"disabled.example.org"}, 0))
	if err := storage.Sync(); err != nil {
		t.Fatalf("SSL storage sync: %v", err)
	}

	cfg := &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{FallbackSNI: "api.example.test"}}}
	getCertificate := mustFrontendTLSConfig(t, cfg).GetCertificate
	selected, err := getCertificate(&tls.ClientHelloInfo{ServerName: "api.example.test"})
	if err != nil {
		t.Fatalf("GetCertificate(exact) error = %v", err)
	}
	if selected == nil || len(selected.Certificate) == 0 {
		t.Fatal("GetCertificate(exact) returned an empty certificate")
	}

	wildcard, err := getCertificate(&tls.ClientHelloInfo{ServerName: "a.example.test"})
	if err != nil {
		t.Fatalf("GetCertificate(wildcard) error = %v", err)
	}
	if wildcard == nil || len(wildcard.Certificate) == 0 {
		t.Fatal("GetCertificate(wildcard) returned an empty certificate")
	}

	if _, err := getCertificate(&tls.ClientHelloInfo{ServerName: "disabled.example.org"}); err == nil {
		t.Fatal("GetCertificate(disabled) error = nil")
	}
	if _, err := getCertificate(&tls.ClientHelloInfo{ServerName: "unknown.example.org"}); err == nil {
		t.Fatal("GetCertificate(unknown) error = nil")
	}

	fallback, err := getCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate(empty SNI with fallback) error = %v", err)
	}
	if fallback == nil || len(fallback.Certificate) == 0 {
		t.Fatal("GetCertificate(empty SNI with fallback) returned an empty certificate")
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

func TestStartReturnsListenError(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listener: %v", err)
	}
	defer func() { _ = occupied.Close() }()
	address := occupied.Addr().String()

	server := &Server{
		staticConfig: &config.EffectiveConfig{},
		addr:         address,
		addrs:        []string{address},
		server:       newConfiguredHTTPServer(http.NotFoundHandler(), nil),
	}
	done := make(chan error, 1)
	go func() { done <- server.startHTTPListeners(t.Context()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start() error = nil for an occupied listener address")
		}
		if !strings.Contains(err.Error(), occupied.Addr().String()) {
			t.Fatalf("Start() error = %v, want the occupied address in the error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start() did not return a listener bind error")
	}
}

func TestServerShutdownCallsOtelShutdownOnce(t *testing.T) {
	otelCalls := 0
	server := &Server{
		server: &http.Server{},
		routes: newRouteHandler(http.NotFoundHandler(), nil),
		otelShutdown: func(ctx context.Context) error {
			otelCalls++
			return nil
		},
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
	install func(context.Context, generation.RecoveryState) error
	close   func(context.Context) error
}

func (e *newServerTestEngine) InstallRecovery(ctx context.Context, recovery generation.RecoveryState) error {
	return e.install(ctx, recovery)
}

func (e *newServerTestEngine) Close(ctx context.Context) error { return e.close(ctx) }

func (*newServerTestEngine) acquireHTTP() (httpGenerationLease, bool) {
	return httpGenerationLease{}, false
}

func (*newServerTestEngine) acquireStream() (streamGenerationLease, bool) {
	return streamGenerationLease{}, false
}

type newServerTestJournal struct {
	generation.Journal
	close func() error
}

func (j *newServerTestJournal) Close() error { return j.close() }

func TestNewServerRecoveryInstallFailureClosesEngineResolverJournalAndObservabilityInReverse(t *testing.T) {
	manifest, effective, encryption, resolver := newServerInputs(t)
	recoveryErr := errors.New("install recovery failed")
	var calls []string
	journal := &newServerTestJournal{close: func() error {
		calls = append(calls, "journal-close")
		return nil
	}}
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
			return &compiler.WorkerCompilerFactory{}, nil
		},
		newEngine: func(*Server, *compiler.WorkerCompilerFactory) (generationEngineOwner, error) {
			calls = append(calls, "engine")
			return &newServerTestEngine{
				install: func(context.Context, generation.RecoveryState) error {
					calls = append(calls, "recovery")
					return recoveryErr
				},
				close: func(context.Context) error {
					calls = append(calls, "engine-close")
					return nil
				},
			}, nil
		},
		newCoordinator: generation.NewCoordinator,
		closeResolver: func(resolver *secret.GenerationSecretResolver, ctx context.Context) error {
			calls = append(calls, "resolver-close")
			return resolver.Close(ctx)
		},
		closeJournal: func(journal generation.Journal) error {
			return journal.Close()
		},
	}

	server, err := newServerWithFactories(
		effective, manifest, encryption, resolver, journal, generation.RecoveryState{}, factories,
	)
	if server != nil || !errors.Is(err, recoveryErr) {
		t.Fatalf("newServerWithFactories() = (%#v, %v), want recovery failure", server, err)
	}
	want := []string{
		"observability", "factory", "engine", "recovery", "engine-close",
		"resolver-close", "journal-close", "observability-close",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("constructor cleanup = %v, want %v", calls, want)
	}
}

func TestNewServerFactoryFailureClosesResolverAndJournalWithoutDoubleClose(t *testing.T) {
	manifest, effective, encryption, resolver := newServerInputs(t)
	factoryErr := errors.New("factory failed")
	var calls []string
	journal := &newServerTestJournal{close: func() error {
		calls = append(calls, "journal-close")
		return nil
	}}
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
		closeJournal: func(journal generation.Journal) error { return journal.Close() },
	}

	server, err := newServerWithFactories(
		effective, manifest, encryption, resolver, journal, generation.RecoveryState{}, factories,
	)
	if server != nil || !errors.Is(err, factoryErr) {
		t.Fatalf("newServerWithFactories() = (%#v, %v), want factory failure", server, err)
	}
	want := []string{"observability", "factory", "resolver-close", "journal-close", "observability-close"}
	if !slices.Equal(calls, want) {
		t.Fatalf("factory cleanup = %v, want %v", calls, want)
	}
}

func TestNewServerRejectsNilDependencyBeforeTakingOwnership(t *testing.T) {
	manifest, effective, encryption, resolver := newServerInputs(t)
	journal := &newServerTestJournal{close: func() error {
		t.Fatal("invalid dependency validation closed journal")
		return nil
	}}
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
		closeJournal:  func(generation.Journal) error { t.Fatal("closed journal"); return nil },
	}
	tests := []struct {
		name       string
		effective  *config.EffectiveConfig
		manifest   *capability.Manifest
		encryption data_encryption.Service
		resolver   *secret.GenerationSecretResolver
		journal    generation.Journal
	}{
		{name: "effective", manifest: manifest, encryption: encryption, resolver: resolver, journal: journal},
		{name: "manifest", effective: effective, encryption: encryption, resolver: resolver, journal: journal},
		{name: "encryption", effective: effective, manifest: manifest, resolver: resolver, journal: journal},
		{name: "resolver", effective: effective, manifest: manifest, encryption: encryption, journal: journal},
		{name: "journal", effective: effective, manifest: manifest, encryption: encryption, resolver: resolver},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := newServerWithFactories(
				test.effective,
				test.manifest,
				test.encryption,
				test.resolver,
				test.journal,
				generation.RecoveryState{},
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
	profiles := config.ProfileSelection{
		Compatibility: config.CompatibilityTarget(manifest.Target.Name),
		Security:      config.SecurityCompat,
	}
	effective := &config.EffectiveConfig{
		Config: config.Config{
			CompatibilityTarget: profiles.Compatibility,
			SecurityProfile:     profiles.Security,
		},
		Profiles: profiles,
		Paths:    config.RuntimePaths{DataDir: t.TempDir()},
	}
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

func (*lifecycleTestStream) Reload([]resource.StreamRoute) error { return nil }
func (r *lifecycleTestStream) Close(ctx context.Context) error   { return r.close(ctx) }

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

func TestServerStartsRecoveredEngineThenStreamHTTPAndProvider(t *testing.T) {
	var calls []string
	var mu sync.Mutex
	record := func(call string) { mu.Lock(); calls = append(calls, call); mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	engine := &newServerTestEngine{
		install: func(context.Context, generation.RecoveryState) error { return nil },
		close:   func(context.Context) error { record("engine-close"); return nil },
	}
	stream := &lifecycleTestStream{close: func(context.Context) error {
		record("stream-close")
		return nil
	}}
	producer := &lifecycleTestProducer{
		start: func(context.Context) error {
			record("provider")
			cancel()
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
		journal:       &newServerTestJournal{close: func() error { return nil }},
		closeResolver: func(*secret.GenerationSecretResolver, context.Context) error { return nil },
		closeJournal:  func(journal generation.Journal) error { return journal.Close() },
		runtimeFactories: serverRuntimeFactories{
			newStream: func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
				record("stream")
				return stream, nil
			},
			startHTTP: func(*Server, context.Context) (<-chan error, error) {
				record("http")
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
	if len(got) < 3 || !slices.Equal(got[:3], []string{"stream", "http", "provider"}) {
		t.Fatalf("startup order = %v, want stream, HTTP, provider prefix", got)
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

func TestServerListenerFailureStopsAndJoinsBlockedProducerStart(t *testing.T) {
	terminalErr := errors.New("terminal HTTP listener failure")
	producerStartEntered := make(chan struct{})
	producerStartExited := make(chan struct{})
	releaseProducerStart := make(chan struct{})
	producerStopEntered := make(chan struct{})
	producer := &lifecycleTestProducer{
		start: func(context.Context) error {
			close(producerStartEntered)
			<-releaseProducerStart
			close(producerStartExited)
			return nil
		},
		stop: func() error {
			close(producerStopEntered)
			close(releaseProducerStart)
			<-producerStartExited
			return nil
		},
	}
	serveErrors := make(chan error, 1)
	server := newShutdownLifecycleServer(t, nil)
	server.staticConfig = &config.EffectiveConfig{}
	server.runtimeFactories = serverRuntimeFactories{
		startHTTP: func(*Server, context.Context) (<-chan error, error) {
			return serveErrors, nil
		},
		newProducer: func(*Server, context.Context) (configProducer, error) {
			return producer, nil
		},
	}
	startDone := make(chan error, 1)
	go func() { startDone <- server.Start(context.Background()) }()
	<-producerStartEntered

	serveErrors <- terminalErr
	if err := <-startDone; !errors.Is(err, terminalErr) {
		t.Fatalf("Start() error = %v, want %v", err, terminalErr)
	}
	select {
	case <-producerStopEntered:
	default:
		t.Fatal("Start() returned before stopping the producer")
	}
	select {
	case <-producerStartExited:
	default:
		t.Fatal("Start() returned before the producer Start goroutine exited")
	}
}

func TestServerOfflineRecoveryServesWhileProviderReadinessIsDegraded(t *testing.T) {
	providerStarted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := &newServerTestEngine{
		install: func(context.Context, generation.RecoveryState) error { return nil },
		close:   func(context.Context) error { return nil },
	}
	server := &Server{
		staticConfig:  &config.EffectiveConfig{},
		server:        &http.Server{},
		routes:        newGenerationRouteHandler(engine.acquireHTTP),
		engine:        engine,
		journal:       &newServerTestJournal{close: func() error { return nil }},
		closeResolver: func(*secret.GenerationSecretResolver, context.Context) error { return nil },
		closeJournal:  func(journal generation.Journal) error { return journal.Close() },
		runtimeFactories: serverRuntimeFactories{
			startHTTP: func(*Server, context.Context) (<-chan error, error) {
				return make(chan error), nil
			},
			newProducer: func(*Server, context.Context) (configProducer, error) {
				return &lifecycleTestProducer{
					start: func(context.Context) error { close(providerStarted); return nil },
					stop:  func() error { return nil },
				}, nil
			},
		},
	}
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	<-providerStarted
	select {
	case err := <-done:
		t.Fatalf("Start() returned on transient degraded readiness: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() after cancellation error = %v", err)
	}
}

func TestServerEtcdInitialFetchFailureStartsWatchAndServesRecoveredHandler(t *testing.T) {
	client := &failingInitialEtcdClient{
		watchStarted: make(chan struct{}),
		closeCalled:  make(chan struct{}),
		fetchErr:     errors.New("etcd offline"),
	}
	producer := newEtcdConfigProducer(client)
	fixture := newCountedHTTPLeaseFixture(t, 316)
	server := newShutdownLifecycleServer(t, nil)
	server.staticConfig = &config.EffectiveConfig{}
	server.routes = newGenerationRouteHandler(fixture.Acquire)
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
	<-client.watchStarted

	response := httptest.NewRecorder()
	server.routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/recovered", nil))
	if response.Code == http.StatusServiceUnavailable {
		t.Fatalf("recovered handler status = %d after initial etcd fetch failure", response.Code)
	}
	if got := fixture.releases.Load(); got != 1 {
		t.Fatalf("recovered handler release count = %d, want 1", got)
	}
	select {
	case err := <-startDone:
		t.Fatalf("Start() returned after transient initial etcd failure: %v", err)
	default:
	}

	cancel()
	if err := <-startDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want %v", err, context.Canceled)
	}
	select {
	case <-client.closeCalled:
	default:
		t.Fatal("Start() returned before the etcd client was closed")
	}
}

func TestServerProviderStartFailureClosesStartedListenersBeforeEngine(t *testing.T) {
	var calls []string
	listener := &lifecycleTestListener{closed: make(chan struct{}), onClose: func() {
		calls = append(calls, "listener-close")
	}}
	engine := &newServerTestEngine{
		install: func(context.Context, generation.RecoveryState) error { return nil },
		close:   func(context.Context) error { calls = append(calls, "engine-close"); return nil },
	}
	server := &Server{
		staticConfig: &config.EffectiveConfig{}, server: &http.Server{},
		routes: newGenerationRouteHandler(engine.acquireHTTP), engine: engine,
		journal:       &newServerTestJournal{close: func() error { return nil }},
		closeResolver: func(*secret.GenerationSecretResolver, context.Context) error { return nil },
		closeJournal:  func(journal generation.Journal) error { return journal.Close() },
		runtimeFactories: serverRuntimeFactories{
			startHTTP: func(server *Server, _ context.Context) (<-chan error, error) {
				server.retainListener(listener)
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
	listenerIndex, engineIndex := slices.Index(calls, "listener-close"), slices.Index(calls, "engine-close")
	if listenerIndex < 0 || engineIndex < 0 || listenerIndex > engineIndex {
		t.Fatalf("startup failure cleanup order = %v", calls)
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

func TestServerShutdownJoinsLateStartupOwnersBeforeListenerRejection(t *testing.T) {
	var callsMu sync.Mutex
	var calls []string
	producerStopCalls := 0
	lateProducerStopErr := errors.New("late producer stop failed")
	record := func(call string) {
		callsMu.Lock()
		calls = append(calls, call)
		callsMu.Unlock()
	}
	httpConstructionEntered := make(chan struct{})
	listenerRetained := make(chan struct{})
	providerConstructionEntered := make(chan struct{})
	releaseProviderConstruction := make(chan struct{})
	producerStopEntered := make(chan struct{})
	producerStopExited := make(chan struct{})
	releaseProducerStop := make(chan struct{})
	listener := &lifecycleTestListener{
		closed:  make(chan struct{}),
		onClose: func() { record("listener-close") },
	}
	producer := &lifecycleTestProducer{
		start: func(context.Context) error { return nil },
		stop: func() error {
			close(producerStopEntered)
			<-releaseProducerStop
			callsMu.Lock()
			producerStopCalls++
			callsMu.Unlock()
			record("provider-stop")
			close(producerStopExited)
			return lateProducerStopErr
		},
	}
	server := newShutdownLifecycleServer(t, nil)
	server.staticConfig = &config.EffectiveConfig{}
	server.shutdownHTTP = func(context.Context) error {
		<-producerStopEntered
		return nil
	}
	server.runtimeFactories = serverRuntimeFactories{
		startHTTP: func(server *Server, ctx context.Context) (<-chan error, error) {
			close(httpConstructionEntered)
			<-ctx.Done()
			server.retainListener(listener)
			close(listenerRetained)
			return make(chan error), nil
		},
		newProducer: func(*Server, context.Context) (configProducer, error) {
			close(providerConstructionEntered)
			<-releaseProviderConstruction
			return producer, nil
		},
	}
	startDone := make(chan error, 1)
	go func() { startDone <- server.Start(context.Background()) }()
	<-httpConstructionEntered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown(context.Background()) }()
	<-listenerRetained
	<-providerConstructionEntered
	close(releaseProviderConstruction)
	<-producerStopEntered
	close(releaseProducerStop)
	if err := <-shutdownDone; !errors.Is(err, lateProducerStopErr) {
		t.Fatalf("Shutdown() error = %v, want %v", err, lateProducerStopErr)
	}
	if err := <-startDone; !errors.Is(err, context.Canceled) || !errors.Is(err, lateProducerStopErr) {
		t.Fatalf("Start() error = %v, want %v and %v", err, context.Canceled, lateProducerStopErr)
	}
	if err := server.Shutdown(context.Background()); !errors.Is(err, lateProducerStopErr) {
		t.Fatalf("repeated Shutdown() error = %v, want %v", err, lateProducerStopErr)
	}
	select {
	case <-producerStopExited:
	default:
		t.Fatal("Shutdown() returned before joining the late provider Stop")
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("Shutdown() left a listener retained by the admitted startup")
	}
	callsMu.Lock()
	got := slices.Clone(calls)
	gotStopCalls := producerStopCalls
	callsMu.Unlock()
	if !slices.Equal(got, []string{"provider-stop", "listener-close"}) {
		t.Fatalf("late owner shutdown order = %v", got)
	}
	if gotStopCalls != 1 {
		t.Fatalf("late producer Stop calls = %d, want 1", gotStopCalls)
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

func TestServerShutdownTimeoutDoesNotReleaseEngineResolverOrJournal(t *testing.T) {
	drainRelease := make(chan struct{})
	var engineCloses, resolverCloses, journalCloses int
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
	server.journal = &newServerTestJournal{close: func() error { journalCloses++; return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Shutdown() error = %v, want context.Canceled", err)
	}
	if engineCloses != 0 || resolverCloses != 0 || journalCloses != 0 {
		t.Fatalf("timeout released later owners: engine=%d resolver=%d journal=%d",
			engineCloses, resolverCloses, journalCloses)
	}
	close(drainRelease)
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("resumed Shutdown() error = %v", err)
	}
	if engineCloses != 1 || resolverCloses != 1 || journalCloses != 1 {
		t.Fatalf(
			"resumed cleanup closes = engine:%d resolver:%d journal:%d",
			engineCloses, resolverCloses, journalCloses,
		)
	}
}

func TestServerShutdownClosesEngineResolverJournalObservabilityInOrder(t *testing.T) {
	var calls []string
	server := newShutdownLifecycleServer(t, func(context.Context) error {
		calls = append(calls, "engine")
		return nil
	})
	server.closeResolver = func(*secret.GenerationSecretResolver, context.Context) error {
		calls = append(calls, "resolver")
		return nil
	}
	server.journal = &newServerTestJournal{close: func() error {
		calls = append(calls, "journal")
		return nil
	}}
	server.otelShutdown = func(context.Context) error {
		calls = append(calls, "observability")
		return nil
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	want := []string{"engine", "resolver", "journal", "observability"}
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
		install: func(context.Context, generation.RecoveryState) error { return nil },
		close:   closeEngine,
	}
	return &Server{
		server:        &http.Server{},
		routes:        newGenerationRouteHandler(engine.acquireHTTP),
		engine:        engine,
		journal:       &newServerTestJournal{close: func() error { return nil }},
		closeResolver: func(*secret.GenerationSecretResolver, context.Context) error { return nil },
		closeJournal:  func(journal generation.Journal) error { return journal.Close() },
	}
}

func TestSendReloadEventBuffersAndCoalesces(t *testing.T) {
	server := &Server{reloadEventChan: make(chan struct{}, 1)}

	server.SendReloadEvent()
	server.SendReloadEvent()
	select {
	case <-server.reloadEventChan:
	default:
		t.Fatal("SendReloadEvent() did not buffer a reload event")
	}
	select {
	case <-server.reloadEventChan:
		t.Fatal("SendReloadEvent() buffered more than one coalesced event")
	default:
	}
}

func TestListenReloadEventReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := &Server{reloadEventChan: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		server.listenReloadEvent(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listenReloadEvent() did not return after context cancellation")
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

func TestRouteEventBucket(t *testing.T) {
	if _, ok := routeEventBucket(nil); ok {
		t.Fatal("routeEventBucket(nil) = ok, want missing bucket")
	}
	if bucket, ok := routeEventBucket(&store.Event{Key: []byte("/apisix/routes/route-1")}); !ok || bucket != "routes" {
		t.Fatalf("routeEventBucket() = %q/%t, want routes/true", bucket, ok)
	}
	if bucket, ok := routeEventBucket(&store.Event{Key: []byte("/apisix/secrets/vault/test")}); !ok ||
		bucket != "secrets" {
		t.Fatalf("routeEventBucket(secret) = %q/%t, want secrets/true", bucket, ok)
	}
	if _, ok := routeEventBucket(&store.Event{Key: []byte("short")}); ok {
		t.Fatal("routeEventBucket(short key) = ok, want missing bucket")
	}
}

func TestHandleStoreEventUpdateDispatchesByBucket(t *testing.T) {
	var httpCalls, streamCalls int
	httpEvent := &store.Event{Key: []byte("/apisix/routes/route-1")}
	streamEvent := &store.Event{Key: []byte("/apisix/stream_routes/stream-1")}
	serviceEvent := &store.Event{Key: []byte("/apisix/services/service-1")}

	handleStoreEventUpdate(httpEvent, func() { httpCalls++ }, func() { streamCalls++ })
	handleStoreEventUpdate(streamEvent, func() { httpCalls++ }, func() { streamCalls++ })
	handleStoreEventUpdate(serviceEvent, func() { httpCalls++ }, func() { streamCalls++ })
	handleStoreEventUpdate(nil, func() { httpCalls++ }, func() { streamCalls++ })

	if httpCalls != 2 || streamCalls != 2 {
		t.Fatalf("http/stream calls = %d/%d, want 2/2", httpCalls, streamCalls)
	}
}

func TestFrontendTLSConfigDefaultsWithoutHTTP2(t *testing.T) {
	tlsConfig := mustFrontendTLSConfig(t, nil)
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
