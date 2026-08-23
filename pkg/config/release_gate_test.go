package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"go.yaml.in/yaml/v3"
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
	if got, want := cfg.Profiles(), (ProfileSelection{
		Compatibility: CompatibilityAPISIX317,
		Security:      SecurityCompat,
	}); got != want {
		t.Fatalf("default profiles = %#v, want %#v", got, want)
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

func TestLoadConfigFilesRejectsRemovedDeploymentProfileBeforePublication(t *testing.T) {
	previous := &Config{Debug: true}
	GlobalConfig = previous
	t.Cleanup(func() { GlobalConfig = previous })

	for _, test := range []struct {
		name  string
		setup func(*testing.T) string
	}{
		{
			name: "file",
			setup: func(t *testing.T) string {
				return writeConfigFile(t, "override.yaml", "deployment:\n  profile: http-data-plane-v1\n")
			},
		},
		{
			name: "environment",
			setup: func(t *testing.T) string {
				t.Setenv("APISIXGO_DEPLOYMENT_PROFILE", "http-data-plane-v1")
				return ""
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			GlobalConfig = previous
			base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
			_, err := loadConfigFiles(base, test.setup(t))
			if err == nil {
				t.Fatal("loadConfigFiles() error = nil, want removed deployment.profile rejection")
			}
			for _, field := range []string{
				"deployment.profile", "removed", "compatibility_target", "security_profile", "qualification_profile",
			} {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("loadConfigFiles() error = %q, want %q", err, field)
				}
			}
			if GlobalConfig != previous {
				t.Fatal("GlobalConfig changed after removed deployment.profile rejection")
			}
		})
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

func TestProductionConfigRequiresExplicitEtcdEndpoint(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })
	t.Setenv("APISIXGO_DEPLOYMENT_ETCD_HOST", "")
	t.Setenv("APISIXGO_QUALIFICATION_PROFILE", "")

	defaultPath, productionPath := repositoryConfigPaths(t)
	_, err := loadConfigFiles(defaultPath, productionPath)
	if err == nil || !strings.Contains(err.Error(), "deployment.etcd.host") {
		t.Fatalf("loadConfigFiles() error = %v, want missing production etcd endpoint rejection", err)
	}
}

func TestHTTPDataPlaneProductionConfigFailsClosedUntilQualified(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })
	t.Setenv("APISIXGO_DEPLOYMENT_ETCD_HOST", "https://etcd.example:2379")

	defaultPath, productionPath := repositoryConfigPaths(t)
	_, err := loadConfigFiles(defaultPath, productionPath)
	if err == nil || !strings.Contains(err.Error(), "http-data-plane-v1") ||
		!strings.Contains(err.Error(), "unqualified required plugins") {
		t.Fatalf("loadConfigFiles() error = %v, want incomplete qualification evidence rejection", err)
	}
	if GlobalConfig != previous {
		t.Fatal("GlobalConfig changed before qualification validation completed")
	}
}

func TestHTTPDataPlanePluginOrderMatchesManifestConfigAndDocumentation(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	qualification, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
	if !ok {
		t.Fatal("HTTP data-plane qualification is missing")
	}

	production := readPluginListYAML(t, repositoryPath(t, "conf", "config-production.yaml"))
	documented := readDocumentedHTTPProfilePlugins(t)
	for source, got := range map[string][]string{
		"conf/config-production.yaml": production,
		"docs/production-profile.md":  documented,
	} {
		if !reflect.DeepEqual(got, qualification.RequiredPlugins) {
			t.Errorf("%s plugins = %#v, want manifest order %#v", source, got, qualification.RequiredPlugins)
		}
	}
}

func readPluginListYAML(t *testing.T, path string) []string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document struct {
		Plugins []string `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return document.Plugins
}

func readDocumentedHTTPProfilePlugins(t *testing.T) []string {
	t.Helper()
	path := repositoryPath(t, "docs", "production-profile.md")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const marker = "- The ordered HTTP plugin list is exactly:\n\n  ```yaml\n"
	_, after, ok := strings.Cut(string(contents), marker)
	if !ok {
		t.Fatalf("%s is missing the ordered HTTP plugin YAML example", path)
	}
	example, _, ok := strings.Cut(after, "\n  ```")
	if !ok {
		t.Fatalf("%s has an unterminated ordered HTTP plugin YAML example", path)
	}
	var document struct {
		Plugins []string `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(example), &document); err != nil {
		t.Fatalf("parse ordered HTTP plugin example in %s: %v", path, err)
	}
	return document.Plugins
}

func TestDefaultConfigDisablesAdmin(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })

	defaultPath := repositoryPath(t, "conf", "config-default.yaml")
	cfg, err := loadConfigFiles(defaultPath, "")
	if err != nil {
		t.Fatalf("loadConfigFiles() error = %v", err)
	}
	if cfg.Apisix.EnableAdmin {
		t.Fatal("default config admin API is enabled, want startup-safe disabled default")
	}
}

func TestHTTPDataPlaneQualificationAcceptsValidConfig(t *testing.T) {
	if err := validateRuntimeConfig(validProfileSelectionConfig(), qualifiedProfileTestManifest(t)); err != nil {
		t.Fatalf("validateRuntimeConfig() error = %v, want valid HTTP qualification", err)
	}
}

func TestUnsupportedRuntimeConfigRejectsEveryMode(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		mutate func(*Config)
	}{
		{
			name:  "admin API",
			field: "apisix.enable_admin",
			mutate: func(cfg *Config) {
				cfg.Apisix.EnableAdmin = true
			},
		},
		{
			name:  "service discovery",
			field: "discovery",
			mutate: func(cfg *Config) {
				cfg.Discovery = Discovery{"dns": map[string]any{"servers": []string{"127.0.0.1:53"}}}
			},
		},
		{
			name:  "external plugin command",
			field: "ext-plugin.cmd",
			mutate: func(cfg *Config) {
				cfg.ExtPlugin.Cmd = []string{"/usr/local/bin/plugin"}
			},
		},
		{
			name:  "WASM plugin",
			field: "wasm.plugins",
			mutate: func(cfg *Config) {
				cfg.Wasm.Plugins = []WasmPlugin{{Name: "logger", File: "logger.wasm"}}
			},
		},
		{
			name:  "XRPC protocol",
			field: "xrpc.protocols",
			mutate: func(cfg *Config) {
				cfg.XRPC.Protocols = []XRPCProtocol{{Name: "pingpong"}}
			},
		},
		{
			name:  "QUIC",
			field: "apisix.ssl.listen[0].enable_quic",
			mutate: func(cfg *Config) {
				cfg.Apisix.Ssl.Listen[0].EnableQuic = true
			},
		},
		{
			name:  "HTTP3",
			field: "apisix.ssl.listen[0].enable_http3",
			mutate: func(cfg *Config) {
				cfg.Apisix.Ssl.Listen[0].EnableHttp3 = true
			},
		},
	}

	for _, selection := range []struct {
		name  string
		value ProfileSelection
	}{
		{
			name: "compatibility",
			value: ProfileSelection{
				Compatibility: CompatibilityAPISIX317,
				Security:      SecurityCompat,
			},
		},
		{
			name: "strict HTTP qualification",
			value: ProfileSelection{
				Compatibility: CompatibilityAPISIX317,
				Security:      SecurityStrict,
				Qualification: QualificationHTTPDataPlaneV1,
			},
		},
	} {
		for _, test := range tests {
			t.Run(selection.name+"/"+test.name, func(t *testing.T) {
				cfg := validProfileSelectionConfig()
				cfg.CompatibilityTarget = selection.value.Compatibility
				cfg.SecurityProfile = selection.value.Security
				cfg.QualificationProfile = selection.value.Qualification
				test.mutate(cfg)

				err := validateRuntimeConfig(cfg, qualifiedProfileTestManifest(t))
				if err == nil {
					t.Fatalf("validateRuntimeConfig() error = nil, want %s rejection", test.field)
				}
				if !strings.Contains(err.Error(), test.field) {
					t.Fatalf("validateRuntimeConfig() error = %q, want field %q", err, test.field)
				}
				if !strings.Contains(err.Error(), "Go data plane") {
					t.Fatalf("validateRuntimeConfig() error = %q, want global unsupported-runtime policy", err)
				}
				if strings.Contains(err.Error(), "logger.wasm") ||
					strings.Contains(err.Error(), "/usr/local/bin/plugin") {
					t.Fatalf("validateRuntimeConfig() error leaked config value: %q", err)
				}
			})
		}
	}
}

func TestProductionPolicyRejectsOneMutatedFieldPerRow(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		mutate func(*Config)
	}{
		{
			name:  "debug",
			field: "debug",
			mutate: func(cfg *Config) {
				cfg.Debug = true
			},
		},
		{
			name:  "role",
			field: "deployment.role",
			mutate: func(cfg *Config) {
				cfg.Deployment.Role = "control_plane"
				cfg.Deployment.RoleControlPlane.ConfigProvider = "etcd"
			},
		},
		{
			name:  "provider",
			field: "deployment.role_data_plane.config_provider",
			mutate: func(cfg *Config) {
				cfg.Deployment.RoleDataPlane.ConfigProvider = "yaml"
			},
		},
		{
			name:  "insecure etcd endpoint",
			field: "deployment.etcd.host[0]",
			mutate: func(cfg *Config) {
				cfg.Deployment.Etcd.Host[0] = "http://etcd.example:2379"
			},
		},
		{
			name:  "missing etcd TLS verification",
			field: "deployment.etcd.tls.verify",
			mutate: func(cfg *Config) {
				cfg.Deployment.Etcd.TLS.Verify = nil
			},
		},
		{
			name:  "disabled etcd TLS verification",
			field: "deployment.etcd.tls.verify",
			mutate: func(cfg *Config) {
				verify := false
				cfg.Deployment.Etcd.TLS.Verify = &verify
			},
		},
		{
			name:  "client body size",
			field: "nginx_config.http.client_max_body_size",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.HTTP.ClientMaxBodySize = 0
			},
		},
		{
			name:  "client body timeout",
			field: "nginx_config.http.client_body_timeout",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.HTTP.ClientBodyTimeout = 0
			},
		},
		{
			name:  "proxy mode",
			field: "apisix.proxy_mode",
			mutate: func(cfg *Config) {
				cfg.Apisix.ProxyMode = "http&stream"
			},
		},
		{
			name:  "TCP stream listener",
			field: "apisix.stream_proxy.tcp",
			mutate: func(cfg *Config) {
				cfg.Apisix.StreamProxy.Tcp = []TcpListen{{Addr: ":9100"}}
			},
		},
		{
			name:  "UDP stream listener",
			field: "apisix.stream_proxy.udp",
			mutate: func(cfg *Config) {
				cfg.Apisix.StreamProxy.Udp = []string{"9200"}
			},
		},
		{
			name:  "stream plugins",
			field: "stream_plugins",
			mutate: func(cfg *Config) {
				cfg.StreamPlugins = []string{"mqtt-proxy"}
			},
		},
		{
			name:  "trusted addresses empty",
			field: "apisix.trusted_addresses",
			mutate: func(cfg *Config) {
				cfg.Apisix.TrustedAddresses = nil
			},
		},
		{
			name:  "trusted address invalid",
			field: "apisix.trusted_addresses[0]",
			mutate: func(cfg *Config) {
				cfg.Apisix.TrustedAddresses = []string{"10.0.0.0"}
			},
		},
		{
			name:  "plugin order",
			field: "plugins",
			mutate: func(cfg *Config) {
				cfg.Plugins[0], cfg.Plugins[1] = cfg.Plugins[1], cfg.Plugins[0]
			},
		},
		{
			name:  "plugin local state",
			field: "plugins",
			mutate: func(cfg *Config) {
				cfg.Plugins = append(cfg.Plugins, "file-logger")
			},
		},
		{
			name:  "missing plugin",
			field: "plugins",
			mutate: func(cfg *Config) {
				cfg.Plugins = cfg.Plugins[:len(cfg.Plugins)-1]
			},
		},
		{
			name:  "HTTP access log enabled",
			field: "nginx_config.http.enable_access_log",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.HTTP.EnableAccessLog = true
			},
		},
		{
			name:  "HTTP access log path",
			field: "nginx_config.http.access_log",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.HTTP.AccessLog = "/var/log/apisix/access.log"
			},
		},
		{
			name:  "HTTP access log buffer",
			field: "nginx_config.http.access_log_buffer",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.HTTP.AccessLogBuffer = 1
			},
		},
		{
			name:  "HTTP access log format",
			field: "nginx_config.http.access_log_format",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.HTTP.AccessLogFormat = "$request"
			},
		},
		{
			name:  "HTTP access log format escape",
			field: "nginx_config.http.access_log_format_escape",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.HTTP.AccessLogFormatEscape = "json"
			},
		},
		{
			name:  "stream access log enabled",
			field: "nginx_config.stream.enable_access_log",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.Stream.EnableAccessLog = true
			},
		},
		{
			name:  "stream access log path",
			field: "nginx_config.stream.access_log",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.Stream.AccessLog = "/var/log/apisix/stream.log"
			},
		},
		{
			name:  "stream access log format",
			field: "nginx_config.stream.access_log_format",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.Stream.AccessLogFormat = "$protocol"
			},
		},
		{
			name:  "stream access log format escape",
			field: "nginx_config.stream.access_log_format_escape",
			mutate: func(cfg *Config) {
				cfg.NginxConfig.Stream.AccessLogFormatEscape = "json"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validProfileSelectionConfig()
			test.mutate(cfg)

			err := validateRuntimeConfig(cfg, qualifiedProfileTestManifest(t))
			if err == nil {
				t.Fatalf("validateRuntimeConfig() error = nil, want %s rejection", test.field)
			}
			if !strings.Contains(err.Error(), test.field) {
				t.Fatalf("validateRuntimeConfig() error = %q, want field %q", err, test.field)
			}
			if !strings.Contains(err.Error(), string(SecurityStrict)) &&
				!strings.Contains(err.Error(), string(QualificationHTTPDataPlaneV1)) {
				t.Fatalf("validateRuntimeConfig() error = %q, want responsible security or qualification axis", err)
			}
			if strings.Contains(err.Error(), "/var/log/apisix") || strings.Contains(err.Error(), "$request") {
				t.Fatalf("validateRuntimeConfig() error leaked config value: %q", err)
			}
		})
	}
}

func TestProductionDockerfileContract(t *testing.T) {
	contents, err := os.ReadFile(repositoryPath(t, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(contents)
	for _, want := range []string{
		"COPY --chown=apisix:apisix conf/config.yaml conf/config-default.yaml " +
			"conf/config-production.yaml /usr/local/apisix/conf/",
		"apk add --no-cache ca-certificates",
		"USER 10001:10001",
		"CMD [\"-c\", \"/usr/local/apisix/conf/config-production.yaml\"]",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
	for _, reject := range []string{"curl", "HEALTHCHECK"} {
		if strings.Contains(dockerfile, reject) {
			t.Errorf("Dockerfile unexpectedly contains %q", reject)
		}
	}
}

func repositoryConfigPaths(t *testing.T) (string, string) {
	t.Helper()
	return repositoryPath(t, "conf", "config-default.yaml"), repositoryPath(t, "conf", "config-production.yaml")
}

func repositoryPath(t *testing.T, parts ...string) string {
	t.Helper()
	root := filepath.Join("..", "..")
	return filepath.Join(append([]string{root}, parts...)...)
}

func writeConfigFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
