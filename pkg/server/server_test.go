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
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/store"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"

	"github.com/go-chi/chi/v5"
)

func TestPrometheusInitErrorsPropagateToServerCallers(t *testing.T) {
	const childEnv = "APISIX_GO_SERVER_PROMETHEUS_INIT_CHILD"
	if os.Getenv(childEnv) == "1" {
		config.GlobalConfig = &config.Config{
			Plugins: []string{"prometheus"},
			PluginAttr: map[string]map[string]any{
				"prometheus": {"max_http_series": "not-an-integer"},
			},
		}
		if err := (&Server{}).startPrometheusExportServer(); err == nil ||
			!strings.HasPrefix(err.Error(), "initialize prometheus metrics: ") {
			t.Fatalf("startPrometheusExportServer() error = %v, want metrics init prefix", err)
		}

		// A nil store makes the ordering contract observable: invalid metrics
		// configuration must return before storage hooks or Start are touched.
		server := &Server{server: &http.Server{}}
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

func TestServerShutdownRetriesPrometheusExpirationWait(t *testing.T) {
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
	if stopCalls != 2 {
		t.Fatalf("expiration stop calls = %d, want 2", stopCalls)
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
	storage, err := store.Open(filepath.Join(t.TempDir(), "shutdown.db"), events)
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
		server:        &http.Server{},
		routes:        newRouteHandler(http.NotFoundHandler(), nil),
		producer:      producer,
		streamRuntime: stream,
		storage:       storage,
		otelShutdown:  func(context.Context) error { return traceErr },
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
	storage, err := store.Open(filepath.Join(t.TempDir(), "scheduler.db"), events)
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

func TestShutdownDuringStandaloneInitialReloadDoesNotLeaveStartBlocked(t *testing.T) {
	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "conf"), 0o700); err != nil {
		t.Fatalf("make config directory: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	configPath := filepath.Join(root, "conf", "apisix.yaml")
	configData := []byte("routes:\n  - id: route-1\n    uri: /one\n#END\n")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}
	config.GlobalConfig = &config.Config{
		Deployment: config.Deployment{
			Role:          "data_plane",
			RoleDataPlane: config.RoleConfig{ConfigProvider: "yaml"},
		},
		Apisix: config.Apisix{ProxyMode: "stream"},
	}
	events := make(chan *store.Event, 8)
	storage, err := store.Open(filepath.Join(root, "startup.db"), events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	t.Cleanup(func() { _ = storage.Stop() })
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	storage.AddEventUpdateHook(func(event *store.Event) {
		if string(event.Key) != "/apisix/routes/route-1" {
			return
		}
		enteredOnce.Do(func() { close(entered) })
		<-release
	})
	server := &Server{
		addr:     "127.0.0.1:0",
		addrs:    []string{"127.0.0.1:0"},
		server:   &http.Server{},
		routes:   newRouteHandler(http.NotFoundHandler(), nil),
		clusters: proxy.NewClusterRegistry(proxy.NopClusterObserver{}),
		events:   events,
		storage:  storage,
	}
	startCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan error, 1)
	go func() { startDone <- server.Start(startCtx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial standalone Reload() did not reach the paused Store hook")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		if err == nil {
			t.Fatal("Shutdown() returned before initial Reload() was released")
		}
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() remained blocked after initial Reload() was released")
	}
	cancel()
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Start() remained blocked after concurrent Shutdown()")
	}
}

func TestStartFailureCleanupIsBoundedWithActiveHTTPRequest(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(filepath.Join(t.TempDir(), "startup-cleanup.db"), events)
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

func TestEtcdWatchExitIsJoinedBeforeClientClose(t *testing.T) {
	client := &fakeEtcdClient{
		watchStarted: make(chan struct{}),
		releaseWatch: make(chan struct{}),
		closeCalled:  make(chan struct{}),
	}
	producer := newEtcdConfigProducer(context.Background(), client)
	producer.Start()
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

func TestStandaloneStartupDeletesPersistedResourceRemovedFromFile(t *testing.T) {
	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "conf"), 0o700); err != nil {
		t.Fatalf("make config directory: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	configPath := filepath.Join(root, "conf", "apisix.yaml")
	if err := os.WriteFile(configPath, []byte("routes: []\n#END\n"), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}

	config.GlobalConfig = &config.Config{Deployment: config.Deployment{
		Role:          "data_plane",
		RoleDataPlane: config.RoleConfig{ConfigProvider: "yaml"},
	}}
	events := make(chan *store.Event, 8)
	storage, err := store.Open(filepath.Join(root, "store.db"), events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	server := &Server{events: events, storage: storage}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	events <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/routes/stale"),
		Value: []byte(`{"id":"stale","uri":"/old"}`),
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	if err := server.startConfigProvider(context.Background()); err != nil {
		t.Fatalf("startConfigProvider() error = %v", err)
	}
	value, err := storage.GetFromBucket("routes", []byte("stale"))
	if err != nil {
		t.Fatalf("read stale resource: %v", err)
	}
	if value != nil {
		t.Fatalf("stale resource = %s, want deleted during initial reconciliation", value)
	}
}

func TestStartFailureStopsStandaloneProducerAndStore(t *testing.T) {
	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDir) })
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "conf"), 0o700); err != nil {
		t.Fatalf("make config directory: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	configPath := filepath.Join(root, "conf", "apisix.yaml")
	if err := os.WriteFile(configPath, []byte("routes: []\n#END\n"), 0o600); err != nil {
		t.Fatalf("write standalone config: %v", err)
	}
	config.GlobalConfig = &config.Config{
		Deployment: config.Deployment{
			Role:          "data_plane",
			RoleDataPlane: config.RoleConfig{ConfigProvider: "yaml"},
		},
		Apisix: config.Apisix{ProxyMode: "stream"},
	}
	events := make(chan *store.Event, 8)
	storage, err := store.Open(filepath.Join(root, "store.db"), events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	server := &Server{
		addr:     "127.0.0.1:0",
		addrs:    []string{"127.0.0.1:0"},
		server:   &http.Server{},
		routes:   newRouteHandler(http.NotFoundHandler(), nil),
		clusters: proxy.NewClusterRegistry(proxy.NopClusterObserver{}),
		events:   events,
		storage:  storage,
	}

	err = server.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stream mode requires") {
		t.Fatalf("Start() error = %v, want stream startup error", err)
	}
	if server.producer == nil {
		t.Fatal("Start() did not retain standalone producer before startup failure")
	}
	if err := server.producer.Stop(); err != nil {
		t.Fatalf("producer Stop() after Start() failure = %v", err)
	}
	if _, err := storage.SnapshotBuckets(config.StandaloneBuckets()); err == nil {
		t.Fatal("Store remained open after Start() failure cleanup")
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
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{}
	server := newConfiguredHTTPServer(http.NotFoundHandler())
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
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{
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

	if got, want := configuredListenAddresses(), []string{
		"0.0.0.0:9080",
		"127.0.0.2:9081",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredListenAddresses() = %#v, want %#v", got, want)
	}

	server := newConfiguredHTTPServer(http.NotFoundHandler())
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
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable: true,
		Listen: []config.Listen{
			{Port: 9443},
			{Ip: "127.0.0.2", Port: 9444},
		},
	}}}

	if got, want := configuredTLSListenAddresses(), []string{
		"0.0.0.0:9443",
		"127.0.0.2:9444",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredTLSListenAddresses() = %#v, want %#v", got, want)
	}

	config.GlobalConfig.Apisix.Ssl.Enable = false
	if got := configuredTLSListenAddresses(); len(got) != 0 {
		t.Fatalf("configuredTLSListenAddresses() = %#v, want no disabled listeners", got)
	}
}

func TestConfiguredHTTPServerAndFrontendTLSAdvertiseHTTP2(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{Ssl: config.Ssl{
		Enable:       true,
		Listen:       []config.Listen{{Port: 9443, EnableHttp2: true}},
		SslProtocols: "TLSv1.2 TLSv1.3",
		SslCiphers:   "ECDHE-RSA-AES128-GCM-SHA256",
	}}}

	server := newConfiguredHTTPServer(http.NotFoundHandler())
	if _, ok := server.TLSNextProto["h2"]; !ok {
		t.Fatal("configured HTTP server does not install an HTTP/2 handler")
	}
	if server.Protocols.UnencryptedHTTP2() {
		t.Fatal("TLS-only HTTP/2 configuration enabled plaintext h2c")
	}

	tlsConfig := mustFrontendTLSConfig(t)
	if !slices.Contains(tlsConfig.NextProtos, "h2") {
		t.Fatalf("frontend TLS protocols = %v, want h2", tlsConfig.NextProtos)
	}

	config.GlobalConfig.Apisix.Ssl.Listen[0].EnableHttp2 = false
	if protocols := mustFrontendTLSConfig(t).NextProtos; slices.Contains(protocols, "h2") {
		t.Fatalf("disabled frontend TLS protocols = %v, must not advertise h2", protocols)
	}
}

func TestConfiguredHTTPServerEnablesH2COnlyForPlaintextListener(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{NodeListen: []config.NodeListen{{
		Port: 9080, EnableHttp2: true,
	}}}}

	server := newConfiguredHTTPServer(http.NotFoundHandler())
	if !server.Protocols.UnencryptedHTTP2() {
		t.Fatal("explicit plaintext HTTP/2 listener did not enable h2c")
	}
}

type failingStrictRouteBuilder struct {
	stopped bool
}

func (*failingStrictRouteBuilder) BuildStrict() (*chi.Mux, error) {
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

func TestApplyStandaloneSnapshotPublishesOnlySuccessfulRouteChanges(t *testing.T) {
	tests := []struct {
		name   string
		result config.StandaloneReloadResult
		err    error
		want   []string
	}{
		{
			name:   "route snapshot syncs before route publication",
			result: config.StandaloneReloadResult{ChangedHTTPRouteBuckets: []string{"routes"}},
			want:   []string{"sync", "routes"},
		},
		{
			name: "stream snapshot syncs before both publications",
			result: config.StandaloneReloadResult{
				ChangedHTTPRouteBuckets: []string{"upstreams"},
				ChangedStreamBuckets:    []string{"upstreams"},
			},
			want: []string{"sync", "routes", "streams"},
		},
		{
			name:   "stream-route-only snapshot preserves HTTP handler",
			result: config.StandaloneReloadResult{ChangedStreamBuckets: []string{"stream_routes"}},
			want:   []string{"sync", "streams"},
		},
		{
			name: "metadata-only snapshot publishes HTTP handler",
			result: config.StandaloneReloadResult{
				ChangedHTTPRouteBuckets: []string{"plugin_metadata"},
			},
			want: []string{"sync", "routes"},
		},
		{
			name: "global-rule snapshot publishes HTTP handler",
			result: config.StandaloneReloadResult{
				ChangedHTTPRouteBuckets: []string{"global_rules"},
			},
			want: []string{"sync", "routes"},
		},
		{
			name: "plugin-config snapshot publishes HTTP handler",
			result: config.StandaloneReloadResult{
				ChangedHTTPRouteBuckets: []string{"plugin_configs"},
			},
			want: []string{"sync", "routes"},
		},
		{
			name: "failed snapshot does not publish",
			err:  context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			_ = applyStandaloneSnapshot(
				test.result,
				test.err,
				func() error { calls = append(calls, "sync"); return nil },
				func() error { calls = append(calls, "routes"); return nil },
				func() { calls = append(calls, "streams") },
			)
			if !slices.Equal(calls, test.want) {
				t.Fatalf("calls = %v, want %v", calls, test.want)
			}
		})
	}
}

func TestApplyStandaloneSnapshotPropagatesRouteBuildFailure(t *testing.T) {
	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_standalone_route_apply_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_standalone_route_apply_ready",
	})
	metrics.RecordConfigApplySuccess()
	t.Cleanup(func() { metrics.ConfigApplyFailures, metrics.ConfigApplyReady = oldFailures, oldReady })

	wantErr := errors.New("disabled plugin route")
	var streams int
	err := applyStandaloneSnapshot(
		config.StandaloneReloadResult{ChangedHTTPRouteBuckets: []string{"routes"}},
		nil,
		func() error { return nil },
		func() error { return wantErr },
		func() { streams++ },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("applyStandaloneSnapshot() error = %v, want %v", err, wantErr)
	}
	if !errors.Is(err, errStandaloneHTTPRoutePublication) {
		t.Fatalf("applyStandaloneSnapshot() error = %v, want standalone route-publication classification", err)
	}
	if streams != 0 {
		t.Fatalf("stream reload calls = %d, want 0 after route-build failure", streams)
	}
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageHTTPRoutes)
	if got := configApplyCounterValue(t, metrics.ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count after route-build failure = %v, want 1", got)
	}
	if got := configApplyGaugeValue(t, metrics.ConfigApplyReady); got != 0 {
		t.Fatalf("ready after route-build failure = %v, want 0", got)
	}
}

func TestPrometheusExportServerConfigDefaults(t *testing.T) {
	cfg := newPrometheusExportServerConfig(nil)

	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if cfg.ExportURI != "/apisix/prometheus/metrics" {
		t.Fatalf("ExportURI = %q, want /apisix/prometheus/metrics", cfg.ExportURI)
	}
	if cfg.ExportIP != "127.0.0.1" {
		t.Fatalf("ExportIP = %q, want 127.0.0.1", cfg.ExportIP)
	}
	if cfg.ExportPort != 9091 {
		t.Fatalf("ExportPort = %d, want 9091", cfg.ExportPort)
	}
	if cfg.Address() != "127.0.0.1:9091" {
		t.Fatalf("Address() = %q, want 127.0.0.1:9091", cfg.Address())
	}
}

func TestPrometheusExportServerConfigUsesOfficialPluginAttr(t *testing.T) {
	cfg := newPrometheusExportServerConfig(map[string]any{
		"enable_export_server": false,
		"export_uri":           "/metrics",
		"export_addr": map[string]any{
			"ip":   "0.0.0.0",
			"port": 19091,
		},
	})

	if cfg.Enabled {
		t.Fatal("Enabled = true, want false")
	}
	if cfg.ExportURI != "/metrics" {
		t.Fatalf("ExportURI = %q, want /metrics", cfg.ExportURI)
	}
	if cfg.ExportIP != "0.0.0.0" {
		t.Fatalf("ExportIP = %q, want 0.0.0.0", cfg.ExportIP)
	}
	if cfg.ExportPort != 19091 {
		t.Fatalf("ExportPort = %d, want 19091", cfg.ExportPort)
	}
	if cfg.Address() != "0.0.0.0:19091" {
		t.Fatalf("Address() = %q, want 0.0.0.0:19091", cfg.Address())
	}
}

func TestFrontendTLSGetCertificateSelectsFromPublishedIndex(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.GetStore(t.TempDir()+"/frontend-tls.db", events)
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

	getCertificate := mustFrontendTLSConfig(t).GetCertificate
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

	previousConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previousConfig })
	config.GlobalConfig = &config.Config{
		Apisix: config.Apisix{
			Ssl: config.Ssl{FallbackSNI: "api.example.test"},
		},
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
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

	config.GlobalConfig = nil
	if frontendHTTP2Enabled() {
		t.Fatal("frontendHTTP2Enabled() = true without config")
	}
	if frontendPlainHTTP2Enabled() {
		t.Fatal("frontendPlainHTTP2Enabled() = true without config")
	}
	if got := configuredTLSListenAddresses(); got != nil {
		t.Fatalf("configuredTLSListenAddresses() = %#v, want nil without config", got)
	}

	config.GlobalConfig = &config.Config{}
	if frontendHTTP2Enabled() {
		t.Fatal("frontendHTTP2Enabled() = true with default config")
	}
	if frontendPlainHTTP2Enabled() {
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
		addr:   address,
		addrs:  []string{address},
		server: newConfiguredHTTPServer(http.NotFoundHandler()),
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

func TestServerShutdownClosesEtcdClient(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	server := &Server{
		server:        &http.Server{},
		routes:        newRouteHandler(http.NotFoundHandler(), nil),
		streamRuntime: runtime,
		etcdClient:    &etcd.ConfigClient{},
	}

	if err := server.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if !runtime.closed {
		t.Fatal("stream runtime was not closed on shutdown")
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
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = nil
	if got := configuredListenAddresses(); !reflect.DeepEqual(got, []string{":8080"}) {
		t.Fatalf("configuredListenAddresses() = %#v, want default :8080", got)
	}

	config.GlobalConfig = &config.Config{Apisix: config.Apisix{
		NodeListen: []config.NodeListen{{Ip: "127.0.0.1", Port: 9080}},
	}}
	if got := configuredListenAddresses(); !reflect.DeepEqual(got, []string{"127.0.0.1:9080"}) {
		t.Fatalf("configuredListenAddresses() = %#v, want node listen address", got)
	}
}

func TestPluginConfiguredConsultsEnabledPlugins(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = nil
	if pluginConfigured("node-status") {
		t.Fatal("pluginConfigured() = true without config")
	}

	config.GlobalConfig = &config.Config{Plugins: []string{"node-status"}}
	if !pluginConfigured("node-status") {
		t.Fatal("pluginConfigured() = false for an enabled plugin")
	}
	if pluginConfigured("prometheus") {
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

	handleStoreEventUpdate(httpEvent, func() { httpCalls++ }, func() { streamCalls++ })
	handleStoreEventUpdate(streamEvent, func() { httpCalls++ }, func() { streamCalls++ })
	handleStoreEventUpdate(nil, func() { httpCalls++ }, func() { streamCalls++ })

	if httpCalls != 1 || streamCalls != 1 {
		t.Fatalf("http/stream calls = %d/%d, want 1/1", httpCalls, streamCalls)
	}
}

func TestFrontendTLSConfigDefaultsWithoutHTTP2(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = nil

	tlsConfig := mustFrontendTLSConfig(t)
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
	oldConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = oldConfig })
	config.GlobalConfig = &config.Config{
		Plugins: []string{"prometheus", "prometheus"},
		PluginAttr: map[string]map[string]any{
			"prometheus": {"enable_export_server": false},
		},
	}

	s := &Server{}
	if err := s.startPrometheusExportServer(); err != nil {
		t.Fatalf("startPrometheusExportServer() error = %v", err)
	}
	if s.prometheusServer != nil {
		t.Fatal("export server started while disabled")
	}
}

func TestStartPrometheusExportServerWithoutPrometheus(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = oldConfig })
	config.GlobalConfig = &config.Config{Plugins: []string{"limit-req"}}

	s := &Server{}
	if err := s.startPrometheusExportServer(); err != nil {
		t.Fatalf("startPrometheusExportServer() error = %v", err)
	}
	if s.prometheusServer != nil {
		t.Fatal("export server started without prometheus plugin")
	}
}
