package server

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

func TestResolveStreamRoutesResolvesReferencedUpstream(t *testing.T) {
	routes, err := resolveStreamRoutes(
		[]resource.StreamRoute{{ID: "route", UpstreamID: "upstream"}},
		func(id string) (resource.Upstream, error) {
			if id != "upstream" {
				t.Fatalf("upstream lookup id = %q, want upstream", id)
			}
			return resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1883, Weight: 1}},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveStreamRoutes() error = %v", err)
	}
	if len(routes) != 1 || len(routes[0].Upstream.Nodes) != 1 || routes[0].Upstream.Nodes[0].Port != 1883 {
		t.Fatalf("routes = %#v", routes)
	}
}

func TestResolveStreamRoutesMergesInlineServiceWithRouteOverrides(t *testing.T) {
	serviceUpstream := resource.Upstream{
		Scheme: "tcp",
		Nodes:  []resource.Node{{Host: "service.example", Port: 1883, Weight: 1}},
	}
	service := resource.Service{
		ID: "service",
		Plugins: map[string]resource.PluginConfig{
			"mqtt-proxy":   map[string]any{"protocol_level": 3},
			"service-only": map[string]any{},
		},
		Upstream: serviceUpstream,
	}
	routes, err := resolveStreamRoutesWithServices(
		[]resource.StreamRoute{{
			ID:        "route",
			ServiceID: "service",
			Plugins:   map[string]resource.PluginConfig{"mqtt-proxy": map[string]any{"protocol_level": 4}},
		}},
		func(string) (resource.Upstream, error) { return resource.Upstream{}, ErrMissingStreamUpstream },
		func(id string) (resource.Service, error) {
			if id != service.ID {
				t.Fatalf("service lookup id = %q, want %q", id, service.ID)
			}
			return service, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveStreamRoutesWithServices() error = %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("resolved service routes = %#v, want one", routes)
	}
	resolved := routes[0]
	if resolved.ServiceID != "service" ||
		len(resolved.Upstream.Nodes) != 1 ||
		resolved.Upstream.Nodes[0].Host != "service.example" {
		t.Fatalf("resolved service upstream = %#v", routes)
	}
	if got := routes[0].Plugins["mqtt-proxy"].(map[string]any)["protocol_level"]; got != 4 {
		t.Fatalf("route plugin override = %#v, want protocol_level=4", routes[0].Plugins["mqtt-proxy"])
	}
	if _, ok := routes[0].Plugins["service-only"]; !ok {
		t.Fatal("service-only stream plugin was not inherited")
	}
}

func TestResolveStreamRoutesResolvesServiceUpstreamID(t *testing.T) {
	routes, err := resolveStreamRoutesWithServices(
		[]resource.StreamRoute{{ID: "route", ServiceID: "service"}},
		func(id string) (resource.Upstream, error) {
			if id != "service-upstream" {
				t.Fatalf("upstream lookup id = %q, want service-upstream", id)
			}
			return resource.Upstream{
				Scheme: "tcp",
				Nodes:  []resource.Node{{Host: "service-upstream.example", Port: 1883, Weight: 1}},
			}, nil
		},
		func(string) (resource.Service, error) {
			return resource.Service{ID: "service", UpstreamID: "service-upstream"}, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveStreamRoutesWithServices() error = %v", err)
	}
	if len(routes) != 1 || routes[0].UpstreamID != "service-upstream" || len(routes[0].Upstream.Nodes) != 1 ||
		routes[0].Upstream.Nodes[0].Host != "service-upstream.example" {
		t.Fatalf("resolved service upstream_id = %#v", routes)
	}
}

func TestResolveStreamRoutesPrefersRouteUpstreamOverService(t *testing.T) {
	serviceLookupCalls := 0
	routes, err := resolveStreamRoutesWithServices(
		[]resource.StreamRoute{{ID: "route", ServiceID: "service", UpstreamID: "route-upstream"}},
		func(id string) (resource.Upstream, error) {
			if id != "route-upstream" {
				t.Fatalf("upstream lookup id = %q, want route-upstream", id)
			}
			return resource.Upstream{
				Scheme: "tcp",
				Nodes: []resource.Node{{
					Host: "route.example", Port: 1883, Weight: 1,
				}},
			}, nil
		},
		func(string) (resource.Service, error) {
			serviceLookupCalls++
			return resource.Service{
				ID: "service",
				Plugins: map[string]resource.PluginConfig{
					"service-only": map[string]any{},
				},
				Upstream: resource.Upstream{
					Scheme: "tcp",
					Nodes: []resource.Node{{
						Host: "service.example", Port: 1883, Weight: 1,
					}},
				},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveStreamRoutesWithServices() error = %v", err)
	}
	if serviceLookupCalls != 0 {
		t.Fatalf("service lookup calls = %d, want 0 when route upstream_id wins", serviceLookupCalls)
	}
	if len(routes[0].Plugins) != 0 {
		t.Fatalf("route plugins = %#v, want no service merge", routes[0].Plugins)
	}
	if len(routes) != 1 || len(routes[0].Upstream.Nodes) != 1 || routes[0].Upstream.Nodes[0].Host != "route.example" {
		t.Fatalf("route upstream override = %#v", routes)
	}
}

func TestResolveStreamRoutesRejectsMissingReferencedUpstream(t *testing.T) {
	_, err := resolveStreamRoutes(
		[]resource.StreamRoute{{ID: "route", UpstreamID: "missing"}},
		func(string) (resource.Upstream, error) {
			return resource.Upstream{}, ErrMissingStreamUpstream
		},
	)
	if err == nil {
		t.Fatal("resolveStreamRoutes() error = nil for missing upstream")
	}
}

func TestStreamProxyModeEnabled(t *testing.T) {
	for _, test := range []struct {
		mode    string
		enabled bool
	}{
		{mode: "http", enabled: false},
		{mode: "stream", enabled: true},
		{mode: "http&stream", enabled: true},
		{mode: "stream&http", enabled: true},
	} {
		if got := streamProxyModeEnabled(
			&config.Config{Apisix: config.Apisix{ProxyMode: test.mode}},
		); got != test.enabled {
			t.Fatalf("streamProxyModeEnabled(%q) = %v, want %v", test.mode, got, test.enabled)
		}
	}
}

func TestIsStreamRouteEvent(t *testing.T) {
	for _, test := range []struct {
		key        string
		httpReload bool
		stream     bool
	}{
		{key: "/apisix/stream_routes/mqtt", stream: true},
		{key: "/apisix/upstreams/mqtt", httpReload: true, stream: true},
		{key: "/apisix/services/mqtt", httpReload: true, stream: true},
		{key: "/apisix/routes/http", httpReload: true},
		{key: "/apisix/global_rules/1", httpReload: true},
		{key: "/apisix/plugin_configs/1", httpReload: true},
		{key: "/apisix/stream_routes"},
	} {
		event := &store.Event{Key: []byte(test.key)}
		if got := isHTTPRouteEvent(event); got != test.httpReload {
			t.Errorf("isHTTPRouteEvent(%q) = %v, want %v", test.key, got, test.httpReload)
		}
		if got := isStreamRouteEvent(event); got != test.stream {
			t.Errorf("isStreamRouteEvent(%q) = %v, want %v", test.key, got, test.stream)
		}
	}
}

func TestServerShutdownClosesStreamRuntime(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	s := &Server{
		server:        &http.Server{},
		routes:        newRouteHandler(http.NotFoundHandler(), nil),
		streamRuntime: runtime,
	}
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if !runtime.closed {
		t.Fatal("shutdown() did not close stream runtime")
	}
}

type fakeStreamRuntime struct {
	closed      bool
	reloadErr   error
	reloadCalls int
}

type blockingStreamRuntime struct {
	reloadStarted     chan struct{}
	releaseReload     chan struct{}
	closeCalled       chan struct{}
	reloading         atomic.Bool
	closeDuringReload atomic.Bool
}

func (r *blockingStreamRuntime) Reload([]resource.StreamRoute) error {
	r.reloading.Store(true)
	close(r.reloadStarted)
	<-r.releaseReload
	r.reloading.Store(false)
	return nil
}

func (r *blockingStreamRuntime) Close(context.Context) error {
	if r.reloading.Load() {
		r.closeDuringReload.Store(true)
	}
	close(r.closeCalled)
	return nil
}

func (r *blockingStreamRuntime) Addresses() []string { return nil }

func (r *fakeStreamRuntime) Reload([]resource.StreamRoute) error {
	r.reloadCalls++
	return r.reloadErr
}

func (r *fakeStreamRuntime) Close(context.Context) error {
	r.closed = true
	return nil
}

func TestStreamProxyModeEnabledWithoutConfig(t *testing.T) {
	if streamProxyModeEnabled(nil) {
		t.Fatal("streamProxyModeEnabled(nil) = true, want false")
	}
}

func TestStartStreamProxyRejectsUnsupportedStreamConfiguration(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

	tests := []struct {
		name string
		cfg  config.Config
	}{
		{
			name: "empty listeners",
			cfg: config.Config{Apisix: config.Apisix{
				ProxyMode: "stream",
			}},
		},
		{
			name: "udp listener",
			cfg: config.Config{Apisix: config.Apisix{
				ProxyMode: "stream",
				StreamProxy: config.StreamProxy{
					Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
					Udp: []string{"127.0.0.1:0"},
				},
			}},
		},
		{
			name: "top-level proxy protocol",
			cfg: config.Config{Apisix: config.Apisix{
				ProxyMode:     "stream",
				ProxyProtocol: config.ProxyProtocol{EnableTCPPP: true},
				StreamProxy: config.StreamProxy{
					Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
				},
			}},
		},
		{
			name: "top-level upstream proxy protocol",
			cfg: config.Config{Apisix: config.Apisix{
				ProxyMode:     "stream",
				ProxyProtocol: config.ProxyProtocol{EnableTCPPPToUpstream: true},
				StreamProxy: config.StreamProxy{
					Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
				},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config.GlobalConfig = &test.cfg
			if err := (&Server{}).startStreamProxy(context.Background()); err == nil {
				t.Fatalf("startStreamProxy() accepted %s", test.name)
			}
		})
	}
}

func TestStartStreamProxyIgnoresStreamConfigurationInHTTPOnlyMode(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{
		ProxyMode:     "http",
		ProxyProtocol: config.ProxyProtocol{EnableTCPPP: true, EnableTCPPPToUpstream: true},
		StreamProxy: config.StreamProxy{
			Udp: []string{"127.0.0.1:0"},
		},
	}}

	server := &Server{}
	if err := server.startStreamProxy(context.Background()); err != nil {
		t.Fatalf("startStreamProxy() error = %v, want HTTP-only mode to ignore stream settings", err)
	}
	if server.streamRuntime != nil {
		t.Fatal("HTTP-only start unexpectedly published a stream runtime")
	}
}

func TestStartStreamProxyPropagatesRouteLoadError(t *testing.T) {
	previousConfig := config.GlobalConfig
	previousStore := store.ReplaceGlobalStoreForTest(nil)
	t.Cleanup(func() {
		config.GlobalConfig = previousConfig
		store.ReplaceGlobalStoreForTest(previousStore)
	})
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{
		ProxyMode: "stream",
		StreamProxy: config.StreamProxy{
			Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
		},
	}}

	if err := (&Server{}).startStreamProxy(context.Background()); err == nil {
		t.Fatal("startStreamProxy() returned nil for an unavailable route store")
	}
}

func TestStartStreamProxyPublishesOnlyAfterCompleteRuntimeSuccess(t *testing.T) {
	previousConfig := config.GlobalConfig
	previousStore := store.ReplaceGlobalStoreForTest(nil)
	events := make(chan *store.Event)
	storage, err := store.GetStore(t.TempDir()+"/stream-startup.db", events)
	if err != nil {
		t.Fatalf("get store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() {
		config.GlobalConfig = previousConfig
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})
	events <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/stream_routes/raw"),
		Value: []byte(`{"id":"raw","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1":1}}}`),
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{
		ProxyMode: "stream",
		StreamProxy: config.StreamProxy{
			Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
		},
	}}

	server := &Server{}
	if err := server.startStreamProxy(context.Background()); err != nil {
		t.Fatalf("startStreamProxy() error = %v", err)
	}
	runtime, ok := server.streamRuntime.(*streamruntime.Runtime)
	if !ok || runtime == nil || len(runtime.Addresses()) != 1 {
		t.Fatalf("stream runtime = %#v, want one published listener", server.streamRuntime)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
}

func TestCloseStartedStreamRuntimeClearsAfterCleanupError(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	runtime := &streamRuntimeCloseError{err: cleanupErr}
	server := &Server{streamRuntime: runtime}

	err := server.closeStartedStreamRuntime(runtime)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("closeStartedStreamRuntime() error = %v, want cleanup error", err)
	}
	if server.streamRuntime != nil {
		t.Fatal("closeStartedStreamRuntime() retained a failed runtime")
	}
}

func TestStartServingPrometheusFailureClosesNewStreamRuntime(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	startupErr := errors.New("prometheus startup failed")
	server := &Server{streamRuntime: runtime}

	err := server.startServing(
		context.Background(),
		nil,
		func() error { return startupErr },
		func(context.Context) error { t.Fatal("HTTP startup should not run"); return nil },
	)
	if !errors.Is(err, startupErr) {
		t.Fatalf("startServing() error = %v, want prometheus startup error", err)
	}
	if !runtime.closed {
		t.Fatal("startServing() did not close the newly created stream runtime")
	}
	if server.streamRuntime != nil {
		t.Fatal("startServing() retained the newly created stream runtime")
	}
}

func TestStartServingHTTPFailureClosesNewStreamRuntime(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	startupErr := errors.New("HTTP startup failed")
	server := &Server{streamRuntime: runtime}

	err := server.startServing(
		context.Background(),
		nil,
		func() error { return nil },
		func(context.Context) error { return startupErr },
	)
	if !errors.Is(err, startupErr) {
		t.Fatalf("startServing() error = %v, want HTTP startup error", err)
	}
	if !runtime.closed {
		t.Fatal("startServing() did not close the newly created stream runtime")
	}
	if server.streamRuntime != nil {
		t.Fatal("startServing() retained the newly created stream runtime")
	}
}

func TestStartServingPreservesPreexistingStreamRuntime(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	startupErr := errors.New("HTTP startup failed")
	server := &Server{streamRuntime: runtime}

	err := server.startServing(
		context.Background(),
		runtime,
		func() error { return nil },
		func(context.Context) error { return startupErr },
	)
	if !errors.Is(err, startupErr) {
		t.Fatalf("startServing() error = %v, want HTTP startup error", err)
	}
	if runtime.closed {
		t.Fatal("startServing() closed a pre-existing stream runtime")
	}
	if server.streamRuntime != runtime {
		t.Fatal("startServing() replaced or cleared the pre-existing stream runtime")
	}
}

type streamRuntimeCloseError struct {
	err error
}

func (r *streamRuntimeCloseError) Reload([]resource.StreamRoute) error {
	return nil
}

func (r *streamRuntimeCloseError) Close(context.Context) error {
	return r.err
}

func TestAcknowledgedStoreEventPropagatesStreamFailureAndRecovery(t *testing.T) {
	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_server_stream_ack_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_server_stream_ack_ready",
	})
	metrics.SetConfigApplyStreamRequired(true)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	t.Cleanup(func() {
		metrics.ConfigApplyFailures = oldFailures
		metrics.ConfigApplyReady = oldReady
		metrics.SetConfigApplyStreamRequired(false)
	})

	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/stream-ack.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})
	runtime := &fakeStreamRuntime{reloadErr: errors.New("stream reload failed")}
	server := &Server{storage: storage, streamRuntime: runtime}
	server.registerAcknowledgedStoreUpdateHook(context.Background())

	put := func() error {
		event := store.NewAcknowledgedBatch([]store.Mutation{{
			Type:  store.EventTypePut,
			Key:   []byte("/apisix/stream_routes/route"),
			Value: []byte(`{"id":"route","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1":1}}}`),
		}}, store.BatchOptions{})
		events <- event
		return event.Wait(context.Background())
	}
	if err := put(); !errors.Is(err, runtime.reloadErr) {
		t.Fatalf("acknowledged stream update error = %v, want %v", err, runtime.reloadErr)
	}
	if runtime.reloadCalls != 1 {
		t.Fatalf("stream reload calls after failure = %d, want 1", runtime.reloadCalls)
	}
	if metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = true after stream publication failure")
	}

	runtime.reloadErr = nil
	if err := put(); err != nil {
		t.Fatalf("acknowledged stream recovery error = %v", err)
	}
	if runtime.reloadCalls != 2 {
		t.Fatalf("stream reload calls after recovery = %d, want 2", runtime.reloadCalls)
	}
	if !metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = false after stream publication recovery")
	}
}

func TestAcknowledgedStoreEventReloadsStreamOncePerBatchGeneration(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/stream-batch.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})
	runtime := &fakeStreamRuntime{}
	server := &Server{
		addr:          "127.0.0.1:9080",
		storage:       storage,
		routes:        newRouteHandler(http.NotFoundHandler(), nil),
		streamRuntime: runtime,
	}
	server.registerAcknowledgedStoreUpdateHook(context.Background())
	event := store.NewAcknowledgedBatch([]store.Mutation{
		{
			Type:  store.EventTypePut,
			Key:   []byte("/apisix/upstreams/upstream"),
			Value: []byte(`{"id":"upstream","scheme":"tcp","nodes":{"127.0.0.1:1":1}}`),
		},
		{
			Type:  store.EventTypePut,
			Key:   []byte("/apisix/stream_routes/route"),
			Value: []byte(`{"id":"route","upstream_id":"upstream"}`),
		},
	}, store.BatchOptions{})
	events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("acknowledged stream batch error = %v", err)
	}
	if runtime.reloadCalls != 1 {
		t.Fatalf("stream reload calls for one multi-bucket batch = %d, want 1", runtime.reloadCalls)
	}
}

func TestAcknowledgedStoreEventWaitsForInitialStreamPublication(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/stream-startup-race.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})
	events <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/stream_routes/route"),
		Value: []byte(`{"id":"route","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1":1}}}`),
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("seed stream route: %v", err)
	}

	runtime := &fakeStreamRuntime{}
	server := &Server{storage: storage}
	server.streamReloadMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- server.handleAcknowledgedStoreEvent(&store.Event{
			Type: store.EventTypePut,
			Key:  []byte("/apisix/stream_routes/route"),
		}, nil)
	}()
	select {
	case err := <-done:
		server.streamReloadMu.Unlock()
		t.Fatalf("stream update returned before initial publication completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	server.streamRuntime = runtime
	server.streamReloadMu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("stream update after initial publication: %v", err)
	}
	if runtime.reloadCalls != 1 {
		t.Fatalf("stream reload calls after initial publication = %d, want 1", runtime.reloadCalls)
	}
}

func TestStreamReloadFailureDoesNotCommitUnpublishedLastGood(t *testing.T) {
	events := make(chan *store.Event, 4)
	storage, err := store.Open(t.TempDir()+"/stream-unpublished-last-good.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})

	applyAcknowledgedStreamRoute(
		t,
		events,
		store.EventTypePut,
		"/apisix/stream_routes/mqtt",
		`{"id":"mqtt","server_addr":"127.0.0.1","server_port":1883,"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2883":1}}}`,
	)
	runtime := &fakeStreamRuntime{}
	server := &Server{storage: storage, streamRuntime: runtime}
	if err := server.reloadStreamRoutes(); err != nil {
		t.Fatalf("publish mqtt: %v", err)
	}

	applyAcknowledgedStreamRoute(t, events, store.EventTypePut, "/apisix/stream_routes/mqtt",
		`{"id":"mqtt","server_addr":"127.0.0.1","server_port":1883,"upstream_id":"missing"}`)
	if err := server.reloadStreamRoutes(); err == nil {
		t.Fatal("reloadStreamRoutes() accepted missing upstream, want failure without last-good commit")
	}

	if err := store.WriteBucketValueForTest(storage, "stream_routes", "mqtt", []byte(`{`)); err != nil {
		t.Fatalf("corrupt unpublished mqtt: %v", err)
	}
	if err := server.reloadStreamRoutes(); err != nil {
		t.Fatalf("reload after unpublished corruption = %v, want originally published mqtt", err)
	}
	if len(server.streamRoutes) != 1 || server.streamRoutes[0].ID != "mqtt" ||
		server.streamRoutes[0].UpstreamID != "" || len(server.streamRoutes[0].Upstream.Nodes) != 1 ||
		server.streamRoutes[0].ServerPort != 1883 {
		t.Fatalf("published stream routes = %#v, want originally published mqtt", server.streamRoutes)
	}
}

func TestStreamReloadListenConflictDoesNotReplaceLastGood(t *testing.T) {
	events := make(chan *store.Event, 4)
	storage, err := store.Open(t.TempDir()+"/stream-conflict-last-good.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})

	applyAcknowledgedStreamRoute(
		t,
		events,
		store.EventTypePut,
		"/apisix/stream_routes/a",
		`{"id":"a","server_addr":"127.0.0.1","server_port":1883,"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2883":1}}}`,
	)
	applyAcknowledgedStreamRoute(
		t,
		events,
		store.EventTypePut,
		"/apisix/stream_routes/b",
		`{"id":"b","server_addr":"127.0.0.1","server_port":1884,"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2884":1}}}`,
	)
	runtime := &fakeStreamRuntime{}
	server := &Server{storage: storage, streamRuntime: runtime}
	if err := server.reloadStreamRoutes(); err != nil {
		t.Fatalf("publish A/B: %v", err)
	}

	applyAcknowledgedStreamRoute(
		t,
		events,
		store.EventTypePut,
		"/apisix/stream_routes/b",
		`{"id":"b","server_addr":"127.0.0.1","server_port":1883,"upstream":{"type":"roundrobin","nodes":{"127.0.0.1:2884":1}}}`,
	)
	runtime.reloadErr = errors.New("conflicting listen address")
	if err := server.reloadStreamRoutes(); err == nil {
		t.Fatal("reloadStreamRoutes() accepted conflicting B")
	}

	if err := store.WriteBucketValueForTest(storage, "stream_routes", "b", []byte(`{`)); err != nil {
		t.Fatalf("corrupt unpublished B: %v", err)
	}
	runtime.reloadErr = nil
	if err := server.reloadStreamRoutes(); err != nil {
		t.Fatalf("reload after unpublished B corruption = %v, want original B", err)
	}
	var recoveredB bool
	for _, route := range server.streamRoutes {
		if route.ID == "b" {
			recoveredB = true
			if route.ServerPort != 1884 {
				t.Fatalf("recovered B = %#v, want originally published port 1884", route)
			}
		}
	}
	if !recoveredB {
		t.Fatalf("published stream routes = %#v, want recovered B", server.streamRoutes)
	}
}

func applyAcknowledgedStreamRoute(
	t *testing.T,
	events chan *store.Event,
	eventType store.EventType,
	key, value string,
) {
	t.Helper()
	event := store.NewAcknowledgedEvent()
	event.Type = eventType
	event.Key = []byte(key)
	event.Value = []byte(value)
	events <- event
	if err := event.Wait(context.Background()); err != nil {
		t.Fatalf("apply %s: %v", key, err)
	}
}

func TestShutdownDoesNotCloseStreamRuntimeDuringAcknowledgedReload(t *testing.T) {
	events := make(chan *store.Event)
	storage, err := store.Open(t.TempDir()+"/stream-shutdown-race.db", events)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	storage.Start()
	t.Cleanup(func() {
		store.ReplaceGlobalStoreForTest(previousStore)
		_ = storage.Stop()
	})
	events <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/stream_routes/route"),
		Value: []byte(`{"id":"route","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1":1}}}`),
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("seed stream route: %v", err)
	}
	runtime := &blockingStreamRuntime{
		reloadStarted: make(chan struct{}),
		releaseReload: make(chan struct{}),
		closeCalled:   make(chan struct{}),
	}
	server := &Server{storage: storage, streamRuntime: runtime}
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- server.handleAcknowledgedStoreEvent(&store.Event{
			Type: store.EventTypePut,
			Key:  []byte("/apisix/stream_routes/route"),
		}, nil)
	}()
	<-runtime.reloadStarted
	shutdownDone := make(chan error, 1)
	go func() {
		err, _ := server.shutdownAttempt(context.Background())
		shutdownDone <- err
	}()
	select {
	case <-runtime.closeCalled:
		close(runtime.releaseReload)
		<-reloadDone
		<-shutdownDone
		t.Fatal("stream runtime closed while acknowledged reload was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(runtime.releaseReload)
	if err := <-reloadDone; err != nil {
		t.Fatalf("acknowledged stream reload: %v", err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdownAttempt() error = %v", err)
	}
	if runtime.closeDuringReload.Load() {
		t.Fatal("stream runtime Close overlapped Reload")
	}
}

func TestStartStreamProxyRecordsInitialConfigApplyStage(t *testing.T) {
	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_server_stream_initial_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_server_stream_initial_ready",
	})
	metrics.SetConfigApplyStreamRequired(true)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	t.Cleanup(func() {
		metrics.ConfigApplyFailures = oldFailures
		metrics.ConfigApplyReady = oldReady
		metrics.SetConfigApplyStreamRequired(false)
	})
	previousConfig := config.GlobalConfig
	previousStore := store.ReplaceGlobalStoreForTest(nil)
	t.Cleanup(func() {
		config.GlobalConfig = previousConfig
		store.ReplaceGlobalStoreForTest(previousStore)
	})
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{
		ProxyMode: "stream",
		StreamProxy: config.StreamProxy{
			Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
		},
	}}
	events := make(chan *store.Event)
	storage, err := store.GetStore(t.TempDir()+"/stream-initial-stage.db", events)
	if err != nil {
		t.Fatalf("get store: %v", err)
	}
	storage.Start()
	t.Cleanup(func() { _ = storage.Stop() })
	events <- &store.Event{
		Type:  store.EventTypePut,
		Key:   []byte("/apisix/stream_routes/route"),
		Value: []byte(`{"id":"route","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1":1}}}`),
	}
	if err := storage.Sync(); err != nil {
		t.Fatalf("seed stream route: %v", err)
	}
	server := &Server{}
	if err := server.startStreamProxy(context.Background()); err != nil {
		t.Fatalf("startStreamProxy() error = %v", err)
	}
	t.Cleanup(func() { _ = server.streamRuntime.Close(context.Background()) })
	if !metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = false after successful initial stream publication")
	}
}

func TestStartStreamProxyRecordsInitialConfigApplyFailure(t *testing.T) {
	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_server_stream_initial_failure_record_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_server_stream_initial_failure_record_ready",
	})
	metrics.SetConfigApplyStreamRequired(true)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	t.Cleanup(func() {
		metrics.ConfigApplyFailures = oldFailures
		metrics.ConfigApplyReady = oldReady
		metrics.SetConfigApplyStreamRequired(false)
	})
	previousConfig := config.GlobalConfig
	previousStore := store.ReplaceGlobalStoreForTest(nil)
	t.Cleanup(func() {
		config.GlobalConfig = previousConfig
		store.ReplaceGlobalStoreForTest(previousStore)
	})
	config.GlobalConfig = &config.Config{Apisix: config.Apisix{
		ProxyMode: "stream",
		StreamProxy: config.StreamProxy{
			Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
		},
	}}
	if err := (&Server{}).startStreamProxy(context.Background()); err == nil {
		t.Fatal("startStreamProxy() error = nil, want route-load failure")
	}
	if metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = true after initial stream publication failure")
	}
}
