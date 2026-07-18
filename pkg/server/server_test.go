package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
)

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
	handler := stripUntrustedForwardedFor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := stripUntrustedForwardedFor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusNoContent)
	}), []string{"127.0.0.0/24"})
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotForwardedFor != "1.1.1.1" {
		t.Fatalf("X-Forwarded-For = %q, want trusted header preserved", gotForwardedFor)
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
			SendTimeout:         3 * time.Second,
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
	if server.WriteTimeout != 3*time.Second {
		t.Fatalf("WriteTimeout = %s, want 3s", server.WriteTimeout)
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
		Enable: true,
		Listen: []config.Listen{{Port: 9443, EnableHttp2: true}},
	}}}

	server := newConfiguredHTTPServer(http.NotFoundHandler())
	if _, ok := server.TLSNextProto["h2"]; !ok {
		t.Fatal("configured HTTP server does not install an HTTP/2 handler")
	}
	if server.Protocols.UnencryptedHTTP2() {
		t.Fatal("TLS-only HTTP/2 configuration enabled plaintext h2c")
	}

	tlsConfig := frontendTLSConfig()
	if !slices.Contains(tlsConfig.NextProtos, "h2") {
		t.Fatalf("frontend TLS protocols = %v, want h2", tlsConfig.NextProtos)
	}

	config.GlobalConfig.Apisix.Ssl.Listen[0].EnableHttp2 = false
	if protocols := frontendTLSConfig().NextProtos; slices.Contains(protocols, "h2") {
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

func TestInitialRouteHandlerUsesNotFoundForFailedBuild(t *testing.T) {
	handler := initialRouteHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
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
			name: "metadata-only snapshot preserves handlers",
			want: []string{"sync"},
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
			applyStandaloneSnapshot(
				test.result,
				test.err,
				func() { calls = append(calls, "sync") },
				func() { calls = append(calls, "routes") },
				func() { calls = append(calls, "streams") },
			)
			if !slices.Equal(calls, test.want) {
				t.Fatalf("calls = %v, want %v", calls, test.want)
			}
		})
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
