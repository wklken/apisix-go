package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
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
	closed bool
}

func (r *fakeStreamRuntime) Reload([]resource.StreamRoute) error {
	return nil
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
