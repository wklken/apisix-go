package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validRuntimeConfig = `
apisix:
  node_listen:
    - ip: 127.0.0.1
      port: 9080
      enable_http2: false
plugins: [request-id, gzip]
stream_plugins: [mqtt-proxy]
proxy:
  max_idle_conns: 100
  max_idle_conns_per_host: 20
  max_conns_per_host: 50
  max_in_flight: 40
deployment:
  role: traditional
  role_traditional:
    config_provider: etcd
  etcd:
    host: [https://127.0.0.1:2379]
    prefix: /apisix
`

func TestLoadConfigFilesMergesNestedOverrideAndReplacesLists(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })

	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", `
apisix:
  node_listen:
    - ip: 127.0.0.2
      port: 9081
      enable_http2: true
plugins: [gzip]
proxy:
  max_in_flight: 60
deployment:
  etcd:
    prefix: /custom
`)

	cfg, err := loadConfigFiles(base, override)
	if err != nil {
		t.Fatalf("loadConfigFiles() error = %v", err)
	}
	if got, want := cfg.Plugins, []string{"gzip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plugins = %#v, want replacement %#v", got, want)
	}
	if got, want := cfg.Apisix.ListenAddresses(), []string{"127.0.0.2:9081"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listeners = %#v, want replacement %#v", got, want)
	}
	if !cfg.Apisix.NodeListen[0].EnableHttp2 {
		t.Fatal("nested override did not enable HTTP/2")
	}
	if cfg.Proxy.MaxIdleConns != 100 || cfg.Proxy.MaxInFlight != 60 {
		t.Fatalf("proxy merge = idle:%d in-flight:%d, want 100/60", cfg.Proxy.MaxIdleConns, cfg.Proxy.MaxInFlight)
	}
	if cfg.Deployment.Etcd.Prefix != "/custom" || len(cfg.Deployment.Etcd.Host) != 1 {
		t.Fatalf("etcd merge = %#v, want retained host and overridden prefix", cfg.Deployment.Etcd)
	}
}

func TestLoadConfigFilesEnvironmentOverridesMergedFiles(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })
	t.Setenv("APISIXGO_PROXY_MAX_IN_FLIGHT", "77")

	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", "proxy:\n  max_in_flight: 60\n")
	cfg, err := loadConfigFiles(base, override)
	if err != nil {
		t.Fatalf("loadConfigFiles() error = %v", err)
	}
	if cfg.Proxy.MaxInFlight != 77 {
		t.Fatalf("proxy.max_in_flight = %d, want environment override 77", cfg.Proxy.MaxInFlight)
	}
}

func TestLoadConfigFilesEnvironmentOverridesFieldsAbsentFromFiles(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })
	t.Setenv("APISIXGO_DEPLOYMENT_ROLE", "data_plane")
	t.Setenv("APISIXGO_DEPLOYMENT_ROLE_DATA_PLANE_CONFIG_PROVIDER", "yaml")
	t.Setenv("APISIXGO_APISIX_SSL_FALLBACK_SNI", "fallback.example")

	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", "deployment:\n  etcd:\n    host: []\n    prefix: ''\n")
	cfg, err := loadConfigFiles(base, override)
	if err != nil {
		t.Fatalf("loadConfigFiles() error = %v, want absent struct fields bound from environment", err)
	}
	if got := cfg.Deployment.RoleDataPlane.ConfigProvider; got != "yaml" {
		t.Fatalf("role_data_plane.config_provider = %q, want yaml", got)
	}
	if got := cfg.Apisix.Ssl.FallbackSNI; got != "fallback.example" {
		t.Fatalf("apisix.ssl.fallback_sni = %q, want fallback.example", got)
	}
}

func TestLoadConfigFilesEmptyEnvironmentReplacementFailsClosed(t *testing.T) {
	previous := GlobalConfig
	GlobalConfig = previous
	t.Cleanup(func() { GlobalConfig = previous })
	t.Setenv("APISIXGO_PLUGINS", "")

	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	_, err := loadConfigFiles(base, "")
	if err == nil || !strings.Contains(err.Error(), "plugins") {
		t.Fatalf("loadConfigFiles() error = %v, want empty environment plugin replacement rejected", err)
	}
}

func TestLoadConfigFilesSelectsProviderForEffectiveRole(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })

	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", `
deployment:
  role: data_plane
  role_data_plane:
    config_provider: yaml
  etcd:
    host: []
    prefix: ""
`)
	cfg, err := loadConfigFiles(base, override)
	if err != nil {
		t.Fatalf("loadConfigFiles() error = %v, want standalone data-plane config without etcd", err)
	}
	got, err := EffectiveConfigProvider(cfg)
	if err != nil {
		t.Fatalf("EffectiveConfigProvider() error = %v", err)
	}
	if got != "yaml" {
		t.Fatalf("EffectiveConfigProvider() = %q, want yaml for data_plane role", got)
	}
}

func TestEffectiveConfigProviderRejectsUnsupportedRolePairs(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		provider string
	}{
		{name: "data plane missing", role: "data_plane"},
		{name: "data plane xds", role: "data_plane", provider: "xds"},
		{name: "traditional yaml", role: "traditional", provider: "yaml"},
		{name: "control plane json", role: "control_plane", provider: "json"},
		{name: "unknown role", role: "sidecar", provider: "etcd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Deployment: Deployment{Role: test.role}}
			switch test.role {
			case "data_plane":
				cfg.Deployment.RoleDataPlane.ConfigProvider = test.provider
			case "control_plane":
				cfg.Deployment.RoleControlPlane.ConfigProvider = test.provider
			default:
				cfg.Deployment.RoleTraditional.ConfigProvider = test.provider
			}
			if _, err := EffectiveConfigProvider(cfg); err == nil {
				t.Fatalf("EffectiveConfigProvider(%q, %q) error = nil", test.role, test.provider)
			}
		})
	}
}

func TestLoadConfigFilesRejectsIncompleteRuntimeBeforePublication(t *testing.T) {
	previous := &Config{Debug: true}
	GlobalConfig = previous
	t.Cleanup(func() { GlobalConfig = previous })

	tests := []struct {
		name     string
		override string
		want     string
	}{
		{name: "empty plugins", override: "plugins: []\n", want: "plugins"},
		{name: "empty listeners", override: "apisix:\n  node_listen: []\n", want: "node_listen"},
		{name: "invalid listener", override: "apisix:\n  node_listen: [{port: 70000}]\n", want: "node_listen"},
		{
			name:     "invalid listener IP",
			override: "apisix:\n  node_listen: [{ip: not-an-ip, port: 9080}]\n",
			want:     "node_listen",
		},
		{name: "missing etcd hosts", override: "deployment:\n  etcd:\n    host: []\n", want: "deployment.etcd.host"},
		{name: "blank etcd host", override: "deployment:\n  etcd:\n    host: ['   ']\n", want: "deployment.etcd.host"},
		{
			name:     "missing etcd prefix",
			override: "deployment:\n  etcd:\n    prefix: \"\"\n",
			want:     "deployment.etcd.prefix",
		},
		{name: "zero max idle", override: "proxy:\n  max_idle_conns: 0\n", want: "proxy.max_idle_conns"},
		{
			name:     "zero per host",
			override: "proxy:\n  max_idle_conns_per_host: 0\n",
			want:     "proxy.max_idle_conns_per_host",
		},
		{name: "zero connections", override: "proxy:\n  max_conns_per_host: 0\n", want: "proxy.max_conns_per_host"},
		{name: "zero in flight", override: "proxy:\n  max_in_flight: 0\n", want: "proxy.max_in_flight"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			GlobalConfig = previous
			base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
			override := writeConfigFile(t, "override.yaml", test.override)
			_, err := loadConfigFiles(base, override)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadConfigFiles() error = %v, want field %q", err, test.want)
			}
			if GlobalConfig != previous {
				t.Fatal("GlobalConfig changed before runtime validation completed")
			}
		})
	}
}

func TestCapabilitySummaryContainsOnlyBoundedSafeFacts(t *testing.T) {
	cfg := &Config{
		Debug: true,
		Apisix: Apisix{
			NodeListen:     []NodeListen{{Ip: "secret.internal", Port: 9080, EnableHttp2: true}},
			Ssl:            Ssl{Enable: true, Listen: []Listen{{Ip: "tls.internal", Port: 9443}}},
			StreamProxy:    StreamProxy{Tcp: []TcpListen{{Addr: "stream.internal:9100"}}, Udp: []string{"9200"}},
			ProxyMode:      "http&stream",
			DataEncryption: DataEncryption{Keyring: []string{"0123456789abcdef"}},
		},
		Plugins:       []string{"request-id", "gzip"},
		StreamPlugins: []string{"mqtt-proxy"},
		Proxy:         Proxy{MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, MaxInFlight: 1},
		Deployment: Deployment{
			Role:            "traditional",
			RoleTraditional: RoleTraditionalConfig{ConfigProvider: "etcd"},
			Etcd: Etcd{
				Host:     []string{"https://admin:password@etcd.internal:2379"},
				Prefix:   "/secret",
				Password: "etcd-password",
				TLS:      EtcdTLS{Cert: "certificate", Key: "private-key"},
			},
			Admin: Admin{AdminKey: []AdminKey{{Key: "admin-token"}}},
		},
	}

	summary := CapabilitySummary(cfg)
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal capability summary: %v", err)
	}
	for _, secret := range []string{
		"secret.internal", "tls.internal", "stream.internal", "admin", "password",
		"/secret", "0123456789abcdef", "certificate", "private-key", "admin-token", "etcd-password",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("capability summary leaked %q: %s", secret, encoded)
		}
	}
	for key, want := range map[string]any{
		"http_listener_count":   1,
		"https_listener_count":  1,
		"stream_listener_count": 2,
		"plugin_count":          2,
		"stream_plugin_count":   1,
		"etcd_endpoint_count":   1,
		"http2_enabled":         true,
		"tls_enabled":           true,
	} {
		if got := summary[key]; got != want {
			t.Fatalf("summary[%q] = %#v, want %#v", key, got, want)
		}
	}
}

func writeConfigFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
