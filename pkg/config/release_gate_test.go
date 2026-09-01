package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const validRuntimeConfig = `
apisix:
  enable_http2: false
  node_listen:
    - ip: 127.0.0.1
      port: 9080
plugins: [request-id, gzip]
stream_plugins: [mqtt-proxy]
nginx_config:
  http:
    client_max_body_size: 1024
    client_body_timeout: 60s
deployment:
  role: traditional
  role_traditional:
    config_provider: etcd
  etcd:
    host: [https://127.0.0.1:2379]
    prefix: /apisix
`

func TestAPISIX317SSLListenOmitsEnableQuicAlias(t *testing.T) {
	if _, ok := reflect.TypeFor[Listen]().FieldByName("EnableQuic"); ok {
		t.Fatal("SSL listener exposes non-APISIX enable_quic alias")
	}
}

func TestAPISIX317ListenerTypesOmitPortLevelHTTP2(t *testing.T) {
	for name, listenerType := range map[string]reflect.Type{
		"HTTP":  reflect.TypeFor[NodeListen](),
		"HTTPS": reflect.TypeFor[Listen](),
	} {
		if _, ok := listenerType.FieldByName("EnableHttp2"); ok {
			t.Fatalf("%s listener exposes deprecated port-level enable_http2", name)
		}
	}
}

func TestAPISIX317ListenerTypesOmitPerListenerProxyProtocol(t *testing.T) {
	for name, listenerType := range map[string]reflect.Type{
		"HTTP": reflect.TypeFor[NodeListen](),
		"TCP":  reflect.TypeFor[TcpListen](),
	} {
		for _, field := range []string{"ProxyProtocol", "ProxyProtocolToUpstream"} {
			if _, ok := listenerType.FieldByName(field); ok {
				t.Fatalf("%s listener exposes non-APISIX %s field", name, field)
			}
		}
	}
}

func TestDataPlaneDeploymentTypeOmitsControlPlaneConfig(t *testing.T) {
	if _, ok := reflect.TypeFor[Deployment]().FieldByName("RoleControlPlane"); ok {
		t.Fatal("data-plane deployment exposes control-plane configuration")
	}
}

func TestLoadEffectiveMergesNestedOverrideAndReplacesLists(t *testing.T) {
	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", `
apisix:
  enable_http2: true
  node_listen:
    - ip: 127.0.0.2
      port: 9081
plugins: [gzip]
nginx_config:
  http:
    client_body_timeout: 30s
deployment:
  etcd:
    prefix: /custom
`)

	cfg, err := loadEffectiveTestFiles(t, base, override)
	if err != nil {
		t.Fatalf("LoadEffective() error = %v", err)
	}
	if got, want := cfg.Plugins, []string{"gzip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plugins = %#v, want replacement %#v", got, want)
	}
	if got, want := cfg.Apisix.ListenAddresses(), []string{"127.0.0.2:9081"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listeners = %#v, want replacement %#v", got, want)
	}
	if !cfg.Apisix.EnableHttp2 {
		t.Fatal("nested override did not enable HTTP/2")
	}
	if cfg.NginxConfig.HTTP.ClientMaxBodySize != 1024 || cfg.NginxConfig.HTTP.ClientBodyTimeout != 30*time.Second {
		t.Fatalf("nginx http merge = body-size:%d timeout:%s, want 1024/30s",
			cfg.NginxConfig.HTTP.ClientMaxBodySize, cfg.NginxConfig.HTTP.ClientBodyTimeout)
	}
	if cfg.Deployment.Etcd.Prefix != "/custom" || len(cfg.Deployment.Etcd.Host) != 1 {
		t.Fatalf("etcd merge = %#v, want retained host and overridden prefix", cfg.Deployment.Etcd)
	}
}

func TestLoadEffectiveSelectsProviderForEffectiveRole(t *testing.T) {
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
	cfg, err := loadEffectiveTestFiles(t, base, override)
	if err != nil {
		t.Fatalf("LoadEffective() error = %v, want standalone data-plane config without etcd", err)
	}
	got, err := EffectiveConfigProvider(cfg)
	if err != nil {
		t.Fatalf("EffectiveConfigProvider() error = %v", err)
	}
	if got != "yaml" {
		t.Fatalf("EffectiveConfigProvider() = %q, want yaml for data_plane role", got)
	}
}

func TestLoadEffectiveAllowsEmptyHTTPPluginList(t *testing.T) {
	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", "plugins: []\n")

	cfg, err := loadEffectiveTestFiles(t, base, override)
	if err != nil {
		t.Fatalf("LoadEffective() error = %v", err)
	}
	if cfg.Plugins == nil || len(cfg.Plugins) != 0 {
		t.Fatalf("plugins = %#v, want explicit empty list", cfg.Plugins)
	}
}

func TestLoadEffectiveIgnoresHarmlessOfficialStaticFields(t *testing.T) {
	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", `
nginx_config:
  envs: [TEST_ENV]
  worker_processes: 1
  main_configuration_snippet: "ignored"
  http:
    access_log_format: "$request"
    real_ip_header: X-Forwarded-For
    client_header_timeout: 17s
apisix:
  enable_dev_mode: true
  router: {http: radixtree_uri}
  proxy_protocol: {listen_http_port: 8080}
  proxy_cache: {cache_ttl: 5s}
deployment:
  admin: {admin_key: [{key: ignored}]}
`)

	cfg, err := loadEffectiveTestFiles(t, base, override)
	if err != nil {
		t.Fatalf("LoadEffective() error = %v", err)
	}
	if got := cfg.NginxConfig.HTTP.ClientHeaderTimeout; got != 17*time.Second {
		t.Fatalf("client_header_timeout = %s, want 17s", got)
	}
}

func TestLoadEffectiveStillRejectsUnsupportedSubsystems(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
	}{
		{name: "admin API", override: "apisix: {enable_admin: true}\n", want: "apisix.enable_admin"},
		{name: "discovery", override: "discovery: {dns: {servers: [127.0.0.1]}}\n", want: "discovery"},
		{name: "external plugin", override: "ext-plugin: {cmd: [/usr/local/bin/plugin]}\n", want: "ext-plugin.cmd"},
		{
			name: "WASM",
			override: `
wasm:
  plugins:
    - name: logger
      file: logger.wasm
      priority: 1
      http_request_phase: access
`,
			want: "wasm.plugins",
		},
		{name: "XRPC", override: "xrpc: {protocols: [{name: pingpong}]}\n", want: "xrpc.protocols"},
		{
			name:     "HTTP3",
			override: "apisix: {ssl: {listen: [{ip: 127.0.0.1, port: 9443, enable_http3: true}]}}\n",
			want:     "enable_http3",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
			override := writeConfigFile(t, "override.yaml", test.override)
			_, err := loadEffectiveTestFiles(t, base, override)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadEffective() error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "static configuration contains") {
				t.Fatalf("LoadEffective() error = %q, want explicit unsupported-subsystem rejection", err)
			}
		})
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
		{name: "control plane", role: "control_plane", provider: "etcd"},
		{name: "unknown role", role: "sidecar", provider: "etcd"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Deployment: Deployment{Role: test.role}}
			switch test.role {
			case "data_plane":
				cfg.Deployment.RoleDataPlane.ConfigProvider = test.provider
			case "traditional":
				cfg.Deployment.RoleTraditional.ConfigProvider = test.provider
			}
			if _, err := EffectiveConfigProvider(cfg); err == nil {
				t.Fatalf("EffectiveConfigProvider(%q, %q) error = nil", test.role, test.provider)
			}
		})
	}
}

func TestLoadEffectiveRejectsControlPlaneRole(t *testing.T) {
	base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
	override := writeConfigFile(t, "override.yaml", `
deployment:
  role: control_plane
  role_control_plane:
    config_provider: etcd
`)

	_, err := loadEffectiveTestFiles(t, base, override)
	if err == nil ||
		!strings.Contains(err.Error(), "deployment.role=control_plane is unsupported by the Go data plane") {
		t.Fatalf("LoadEffective() error = %v, want explicit control-plane rejection", err)
	}
}

func TestLoadEffectiveRejectsIncompleteRuntimeBeforePublication(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
	}{
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := writeConfigFile(t, "base.yaml", validRuntimeConfig)
			override := writeConfigFile(t, "override.yaml", test.override)
			_, err := loadEffectiveTestFiles(t, base, override)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadEffective() error = %v, want field %q", err, test.want)
			}
		})
	}
}

func TestCapabilitySummaryContainsOnlyBoundedSafeFacts(t *testing.T) {
	cfg := &Config{
		Debug: true,
		Apisix: Apisix{
			EnableHttp2:    true,
			NodeListen:     []NodeListen{{Ip: "secret.internal", Port: 9080}},
			Ssl:            Ssl{Enable: true, Listen: []Listen{{Ip: "tls.internal", Port: 9443}}},
			StreamProxy:    StreamProxy{Tcp: []TcpListen{{Addr: "stream.internal:9100"}}, Udp: []string{"9200"}},
			ProxyMode:      "http&stream",
			DataEncryption: DataEncryption{Keyring: []string{"0123456789abcdef"}},
		},
		Plugins:       []string{"request-id", "gzip"},
		StreamPlugins: []string{"mqtt-proxy"},
		Deployment: Deployment{
			Role:            "traditional",
			RoleTraditional: RoleTraditionalConfig{ConfigProvider: "etcd"},
			Etcd: Etcd{
				Host:     []string{"https://admin:password@etcd.internal:2379"},
				Prefix:   "/secret",
				Password: "etcd-password",
				TLS:      EtcdTLS{Cert: "certificate", Key: "private-key"},
			},
		},
	}

	summary := CapabilitySummary(cfg)
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal capability summary: %v", err)
	}
	for _, secret := range []string{
		"secret.internal", "tls.internal", "stream.internal", "admin", "password",
		"/secret", "0123456789abcdef", "certificate", "private-key", "etcd-password",
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

func TestCapabilitySummaryDoesNotAdvertiseControlPlane(t *testing.T) {
	summary := CapabilitySummary(&Config{Deployment: Deployment{Role: "control_plane"}})
	if got := summary["role"]; got != "unknown" {
		t.Fatalf("summary role = %#v, want unknown", got)
	}
	if got := summary["config_provider"]; got != "unknown" {
		t.Fatalf("summary config provider = %#v, want unknown", got)
	}
}

func TestDefaultConfigUsesStartupSafeAPISIX317Settings(t *testing.T) {
	defaultPath := repositoryPath(t, "conf", "config-default.yaml")
	cfg, err := loadEffectiveTestFiles(t, defaultPath, "")
	if err != nil {
		t.Fatalf("LoadEffective() error = %v", err)
	}
	if cfg.Apisix.EnableAdmin {
		t.Fatal("default config admin API is enabled, want startup-safe disabled default")
	}
	if !cfg.Apisix.EnableHttp2 {
		t.Fatal("default config does not use global APISIX 3.17 HTTP/2 setting")
	}
}

func TestUnsupportedRuntimeConfigFailsClosed(t *testing.T) {
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
				cfg.Wasm.Plugins = []WasmPlugin{{}}
			},
		},
		{
			name:  "XRPC protocol",
			field: "xrpc.protocols",
			mutate: func(cfg *Config) {
				cfg.XRPC.Protocols = []XRPCProtocol{{}}
			},
		},
		{
			name:  "HTTP3",
			field: "apisix.ssl.listen[0].enable_http3",
			mutate: func(cfg *Config) {
				cfg.Apisix.Ssl.Listen = []Listen{{}}
				cfg.Apisix.Ssl.Listen[0].EnableHttp3 = true
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validRuntimeConfigForTrustedAddresses()
			test.mutate(cfg)

			err := validateRuntimeConfig(cfg)
			if err == nil {
				t.Fatalf("validateRuntimeConfig() error = nil, want %s rejection", test.field)
			}
			if !strings.Contains(err.Error(), test.field) || !strings.Contains(err.Error(), "Go data plane") {
				t.Fatalf("validateRuntimeConfig() error = %q, want global field rejection for %q", err, test.field)
			}
			if strings.Contains(err.Error(), "logger.wasm") ||
				strings.Contains(err.Error(), "/usr/local/bin/plugin") {
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
		"COPY --chown=apisix:apisix conf/config.yaml conf/config-default.yaml /usr/local/apisix/conf/",
		"apk add --no-cache ca-certificates",
		"USER 10001:10001",
		"CMD [\"-c\", \"/usr/local/apisix/conf/config.yaml\"]",
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

func repositoryPath(t *testing.T, parts ...string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repository path")
	}
	root := filepath.Join(filepath.Dir(filename), "..", "..")
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

func loadEffectiveTestFiles(
	t *testing.T,
	defaultPath string,
	overridePath string,
	environments ...map[string]string,
) (*Config, error) {
	t.Helper()
	environment := map[string]string{}
	if len(environments) != 0 && environments[0] != nil {
		environment = environments[0]
	}
	effective, err := LoadEffective(LoadRequest{
		DefaultPath: defaultPath, OverridePath: overridePath,
		Environment: environment,
	})
	if err != nil {
		return nil, err
	}
	return &effective.Config, nil
}
