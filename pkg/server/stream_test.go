package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

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
	if streamProxyModeEnabled(nil) {
		t.Fatal("streamProxyModeEnabled(nil) = true, want false")
	}
}

type immutableStreamTestEngine struct {
	generation.PublicationEngine
	acquire func() (streamGenerationLease, bool)
}

func (*immutableStreamTestEngine) InstallRecovery(context.Context, generation.RecoveryState) error {
	return nil
}

func (*immutableStreamTestEngine) Close(context.Context) error { return nil }

func (*immutableStreamTestEngine) acquireHTTP() (httpGenerationLease, bool) {
	return httpGenerationLease{}, false
}

func (e *immutableStreamTestEngine) acquireStream() (streamGenerationLease, bool) {
	if e.acquire == nil {
		return streamGenerationLease{}, false
	}
	return e.acquire()
}

func (*immutableStreamTestEngine) refreshStreamMetrics() {}

type fakeStreamRuntime struct {
	closed    atomic.Int32
	closeErr  error
	addresses []string
	onClose   func()
}

func (r *fakeStreamRuntime) Close(context.Context) error {
	r.closed.Add(1)
	if r.onClose != nil {
		r.onClose()
	}
	return r.closeErr
}

func (r *fakeStreamRuntime) Addresses() []string { return r.addresses }

func TestServerShutdownClosesStreamRuntime(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	s := &Server{streamRuntime: runtime}
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if got := runtime.closed.Load(); got != 1 {
		t.Fatalf("stream runtime close calls = %d, want 1", got)
	}
	if s.currentStreamRuntime() != nil {
		t.Fatal("shutdown() retained the stream runtime")
	}
}

func TestStartImmutableStreamRuntimeRejectsUnsupportedConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{name: "empty listeners", cfg: config.Config{Apisix: config.Apisix{ProxyMode: "stream"}}},
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
			server := &Server{
				staticConfig: &config.EffectiveConfig{Config: test.cfg},
				engine:       &immutableStreamTestEngine{},
			}
			if err := server.startImmutableStreamRuntime(
				context.Background(),
				func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
					t.Fatal("runtime factory called for invalid stream configuration")
					return nil, nil
				},
			); err == nil {
				t.Fatalf("startImmutableStreamRuntime() accepted %s", test.name)
			}
		})
	}
}

func TestStartImmutableStreamRuntimeIgnoresStreamConfigurationInHTTPOnlyMode(t *testing.T) {
	effective := &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
		ProxyMode:     "http",
		ProxyProtocol: config.ProxyProtocol{EnableTCPPP: true, EnableTCPPPToUpstream: true},
		StreamProxy:   config.StreamProxy{Udp: []string{"127.0.0.1:0"}},
	}}}

	server := &Server{staticConfig: effective}
	err := server.startImmutableStreamRuntime(
		context.Background(),
		func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
			t.Fatal("runtime factory called in HTTP-only mode")
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("startImmutableStreamRuntime() error = %v, want HTTP-only mode to ignore stream settings", err)
	}
	if server.currentStreamRuntime() != nil {
		t.Fatal("HTTP-only start unexpectedly published a stream runtime")
	}
}

func TestStartImmutableStreamRuntimeRequiresGenerationEngine(t *testing.T) {
	server := &Server{staticConfig: immutableStreamConfig()}
	err := server.startImmutableStreamRuntime(
		context.Background(),
		func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
			t.Fatal("runtime factory called without a generation engine")
			return nil, nil
		},
	)
	if err == nil || err.Error() != "generation engine is required for stream runtime" {
		t.Fatalf("startImmutableStreamRuntime() error = %v, want missing generation engine", err)
	}
}

func TestStartImmutableStreamRuntimePublishesGenerationSource(t *testing.T) {
	var releases atomic.Int32
	engine := &immutableStreamTestEngine{acquire: func() (streamGenerationLease, bool) {
		return streamGenerationLease{Release: func() { releases.Add(1) }}, true
	}}
	runtime := &fakeStreamRuntime{addresses: []string{"127.0.0.1:12345"}}
	server := &Server{staticConfig: immutableStreamConfig(), engine: engine}
	var factoryCalls atomic.Int32
	err := server.startImmutableStreamRuntime(
		context.Background(),
		func(_ context.Context, listeners []config.TcpListen, source streamruntime.RouterSource) (streamRuntimeOwner, error) {
			factoryCalls.Add(1)
			if len(listeners) != 1 || listeners[0].Addr != "127.0.0.1:0" {
				t.Fatalf("listeners = %#v, want immutable stream listener", listeners)
			}
			lease, ok := source()
			if !ok {
				t.Fatal("generation router source unavailable")
			}
			lease.Release()
			return runtime, nil
		},
	)
	if err != nil {
		t.Fatalf("startImmutableStreamRuntime() error = %v", err)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("runtime factory calls = %d, want 1", got)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("generation lease releases = %d, want 1", got)
	}
	if got := server.currentStreamRuntime(); got != runtime {
		t.Fatalf("published stream runtime = %#v, want %#v", got, runtime)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
}

func TestStartImmutableStreamRuntimeDoesNotPublishFactoryFailure(t *testing.T) {
	wantErr := errors.New("listener startup failed")
	server := &Server{staticConfig: immutableStreamConfig(), engine: &immutableStreamTestEngine{}}
	err := server.startImmutableStreamRuntime(
		context.Background(),
		func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
			return nil, wantErr
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("startImmutableStreamRuntime() error = %v, want %v", err, wantErr)
	}
	if server.currentStreamRuntime() != nil {
		t.Fatal("failed runtime factory published a stream runtime")
	}
}

func TestCloseStartedStreamRuntimeClearsBeforeClosing(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	server := &Server{}
	runtime := &fakeStreamRuntime{closeErr: cleanupErr}
	runtime.onClose = func() {
		if server.currentStreamRuntime() != nil {
			t.Error("stream runtime remained published during Close")
		}
	}
	server.streamRuntime = runtime

	err := server.closeStartedStreamRuntime(runtime)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("closeStartedStreamRuntime() error = %v, want cleanup error", err)
	}
	if server.currentStreamRuntime() != nil {
		t.Fatal("closeStartedStreamRuntime() retained a failed runtime")
	}
}

func TestStartServingClosesOnlyNewStreamRuntimeOnFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		prometheus error
		http       error
	}{
		{name: "prometheus", prometheus: errors.New("prometheus startup failed")},
		{name: "http", http: errors.New("HTTP startup failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeStreamRuntime{}
			server := &Server{streamRuntime: runtime}
			err := server.startServing(
				context.Background(), nil,
				func() error { return test.prometheus },
				func(context.Context) error { return test.http },
			)
			wantErr := test.prometheus
			if wantErr == nil {
				wantErr = test.http
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("startServing() error = %v, want %v", err, wantErr)
			}
			if got := runtime.closed.Load(); got != 1 {
				t.Fatalf("runtime close calls = %d, want 1", got)
			}
			if server.currentStreamRuntime() != nil {
				t.Fatal("startServing() retained the newly created stream runtime")
			}
		})
	}
}

func TestStartServingPreservesPreexistingStreamRuntime(t *testing.T) {
	runtime := &fakeStreamRuntime{}
	startupErr := errors.New("HTTP startup failed")
	server := &Server{streamRuntime: runtime}

	err := server.startServing(
		context.Background(), runtime,
		func() error { return nil },
		func(context.Context) error { return startupErr },
	)
	if !errors.Is(err, startupErr) {
		t.Fatalf("startServing() error = %v, want HTTP startup error", err)
	}
	if got := runtime.closed.Load(); got != 0 {
		t.Fatalf("pre-existing runtime close calls = %d, want 0", got)
	}
	if server.currentStreamRuntime() != runtime {
		t.Fatal("startServing() replaced or cleared the pre-existing stream runtime")
	}
}

func TestStartImmutableStreamRuntimeRecordsConfigApplyStage(t *testing.T) {
	oldFailures, oldReady := metrics.ConfigApplyFailures, metrics.ConfigApplyReady
	metrics.ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_server_immutable_stream_initial_failures_total",
	})
	metrics.ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_server_immutable_stream_initial_ready",
	})
	metrics.SetConfigApplyStreamRequired(true)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageHTTPRoutes)
	t.Cleanup(func() {
		metrics.ConfigApplyFailures = oldFailures
		metrics.ConfigApplyReady = oldReady
		metrics.SetConfigApplyStreamRequired(false)
	})

	server := &Server{staticConfig: immutableStreamConfig(), engine: &immutableStreamTestEngine{}}
	runtime := &fakeStreamRuntime{}
	if err := server.startImmutableStreamRuntime(
		context.Background(),
		func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
			return runtime, nil
		},
	); err != nil {
		t.Fatalf("startImmutableStreamRuntime() error = %v", err)
	}
	if !metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = false after successful immutable stream startup")
	}
	_ = server.closeStartedStreamRuntime(runtime)

	failing := &Server{staticConfig: immutableStreamConfig()}
	if err := failing.startImmutableStreamRuntime(
		context.Background(),
		func(context.Context, []config.TcpListen, streamruntime.RouterSource) (streamRuntimeOwner, error) {
			t.Fatal("runtime factory called without generation engine")
			return nil, nil
		},
	); err == nil {
		t.Fatal("startImmutableStreamRuntime() error = nil, want missing generation engine")
	}
	if metrics.GetReadiness().ConfigApplyReady {
		t.Fatal("config readiness = true after immutable stream startup failure")
	}
}

func immutableStreamConfig() *config.EffectiveConfig {
	return &config.EffectiveConfig{Config: config.Config{Apisix: config.Apisix{
		ProxyMode: "stream",
		StreamProxy: config.StreamProxy{
			Tcp: []config.TcpListen{{Addr: "127.0.0.1:0"}},
		},
	}}}
}
