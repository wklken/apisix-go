package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
)

var GlobalConfig *Config

const (
	DefaultConfigFile        = "conf/config-default.yaml"
	defaultClientMaxBodySize = int64(10 * 1024 * 1024)
	defaultClientBodyTimeout = 60 * time.Second
)

func Load(overridePath string) (*Config, error) {
	return loadConfigFiles(DefaultConfigFile, overridePath)
}

func loadConfigFiles(defaultPath, overridePath string) (*Config, error) {
	v := viper.NewWithOptions(viper.ExperimentalBindStruct())
	v.SetConfigFile(defaultPath)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("load default config %q: %w", defaultPath, err)
	}

	if overridePath != "" && !sameConfigPath(defaultPath, overridePath) {
		v.SetConfigFile(overridePath)
		if err := v.MergeInConfig(); err != nil {
			return nil, fmt.Errorf("merge config override %q: %w", overridePath, err)
		}
	}
	v.SetEnvPrefix("APISIXGO")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AllowEmptyEnv(true)
	v.AutomaticEnv()

	cfg, err := load(v)
	if err != nil {
		return nil, err
	}
	manifest, err := capability.Load()
	if err != nil {
		return nil, fmt.Errorf("load capability manifest: %w", err)
	}
	if err := validateRuntimeConfig(cfg, manifest); err != nil {
		return nil, err
	}

	GlobalConfig = cfg
	data_encryption.Configure(cfg.Apisix.DataEncryption.EnableEncryptFields, cfg.Apisix.DataEncryption.Keyring)
	return cfg, nil
}

func sameConfigPath(first, second string) bool {
	firstPath, firstErr := filepath.Abs(first)
	secondPath, secondErr := filepath.Abs(second)
	if firstErr == nil && secondErr == nil {
		return filepath.Clean(firstPath) == filepath.Clean(secondPath)
	}
	return filepath.Clean(first) == filepath.Clean(second)
}

func load(v *viper.Viper) (*Config, error) {
	v.SetDefault("compatibility_target", CompatibilityAPISIX317)
	v.SetDefault("security_profile", SecurityCompat)
	v.SetDefault("nginx_config.http.client_max_body_size", defaultClientMaxBodySize)
	v.SetDefault("nginx_config.http.client_body_timeout", defaultClientBodyTimeout)
	rawPlugins := v.Get("plugins")

	var cfg Config
	err := v.Unmarshal(&cfg, viper.DecodeHook(configDecodeHook))
	if err != nil {
		return nil, err
	}
	if rawPlugins != nil {
		if plugins, ok := decodeHTTPPluginAllowlist(rawPlugins); ok {
			cfg.Plugins = plugins
		}
	}
	if err := validateHTTPPluginAllowlist(cfg.Plugins); err != nil {
		return nil, err
	}
	if sendTimeout := cfg.NginxConfig.HTTP.SendTimeout; sendTimeout != 0 {
		return nil, fmt.Errorf(
			"nginx_config.http.send_timeout must be zero because Go cannot implement NGINX write-idle semantics, got %s",
			sendTimeout,
		)
	}

	for _, limit := range []struct {
		field string
		value int
	}{
		{field: "proxy.max_idle_conns", value: cfg.Proxy.MaxIdleConns},
		{field: "proxy.max_idle_conns_per_host", value: cfg.Proxy.MaxIdleConnsPerHost},
		{field: "proxy.max_conns_per_host", value: cfg.Proxy.MaxConnsPerHost},
		{field: "proxy.max_in_flight", value: cfg.Proxy.MaxInFlight},
	} {
		if limit.value < 0 {
			return nil, fmt.Errorf("%s must be non-negative, got %d", limit.field, limit.value)
		}
	}

	return &cfg, nil
}

func validateRuntimeConfig(cfg *Config, manifest *capability.Manifest) error {
	selection := cfg.Profiles()
	if err := selection.Validate(manifest); err != nil {
		return err
	}
	if len(cfg.Plugins) == 0 {
		return profileAwareRuntimeError(selection, fmt.Errorf("plugins must contain at least one HTTP plugin"))
	}
	if len(cfg.Apisix.NodeListen) == 0 {
		return profileAwareRuntimeError(selection, fmt.Errorf("apisix.node_listen must contain at least one listener"))
	}
	for index, listener := range cfg.Apisix.NodeListen {
		if listener.Port < 1 || listener.Port > 65535 {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf("apisix.node_listen[%d].port must be between 1 and 65535, got %d", index, listener.Port),
			)
		}
		if listener.Ip != "" && net.ParseIP(listener.Ip) == nil {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf("apisix.node_listen[%d].ip must be a valid IP address, got %q", index, listener.Ip),
			)
		}
	}
	for _, limit := range []struct {
		field string
		value int
	}{
		{field: "proxy.max_idle_conns", value: cfg.Proxy.MaxIdleConns},
		{field: "proxy.max_idle_conns_per_host", value: cfg.Proxy.MaxIdleConnsPerHost},
		{field: "proxy.max_conns_per_host", value: cfg.Proxy.MaxConnsPerHost},
		{field: "proxy.max_in_flight", value: cfg.Proxy.MaxInFlight},
	} {
		if limit.value <= 0 {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf("%s must be positive, got %d", limit.field, limit.value),
			)
		}
	}
	for index, address := range cfg.Apisix.TrustedAddresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if net.ParseIP(address) != nil {
			continue
		}
		if _, _, parseErr := net.ParseCIDR(address); parseErr != nil {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf(
					"apisix.trusted_addresses[%d] must be a valid CIDR or IP address",
					index,
				),
			)
		}
	}
	if cfg.NginxConfig.HTTP.ClientMaxBodySize <= 0 {
		return profileAwareRuntimeError(selection, fmt.Errorf(
			"nginx_config.http.client_max_body_size must be positive, got %d",
			cfg.NginxConfig.HTTP.ClientMaxBodySize,
		))
	}
	if cfg.NginxConfig.HTTP.ClientBodyTimeout <= 0 {
		return profileAwareRuntimeError(selection, fmt.Errorf(
			"nginx_config.http.client_body_timeout must be positive, got %s",
			cfg.NginxConfig.HTTP.ClientBodyTimeout,
		))
	}
	if err := validateProcessAccessLogs(cfg); err != nil {
		return profileAwareRuntimeError(selection, err)
	}

	provider, err := EffectiveConfigProvider(cfg)
	if err != nil {
		return profileAwareRuntimeError(selection, err)
	}
	if provider == "etcd" {
		if len(cfg.Deployment.Etcd.Host) == 0 {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf("deployment.etcd.host must contain at least one endpoint for the etcd provider"),
			)
		}
		for index, endpoint := range cfg.Deployment.Etcd.Host {
			if strings.TrimSpace(endpoint) == "" {
				return profileAwareRuntimeError(
					selection,
					fmt.Errorf("deployment.etcd.host[%d] must not be empty for the etcd provider", index),
				)
			}
		}
		if strings.TrimSpace(cfg.Deployment.Etcd.Prefix) == "" {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf("deployment.etcd.prefix must not be empty for the etcd provider"),
			)
		}
	}
	if err := validateUnsupportedRuntimeConfig(cfg); err != nil {
		return err
	}

	if err := validateSecurityProfile(cfg, selection); err != nil {
		return err
	}
	return validateQualificationProfile(cfg, manifest, selection)
}

func profileAwareRuntimeError(selection ProfileSelection, err error) error {
	if selection.Security == SecurityStrict {
		return fmt.Errorf("%s: %w", SecurityStrict, err)
	}
	if selection.Qualification != QualificationNone {
		return fmt.Errorf("%s: %w", selection.Qualification, err)
	}
	return err
}

func validateUnsupportedRuntimeConfig(cfg *Config) error {
	for _, unsupported := range []struct {
		field    string
		isActive bool
	}{
		{field: "apisix.enable_admin", isActive: cfg.Apisix.EnableAdmin},
		{field: "discovery", isActive: len(cfg.Discovery) > 0},
		{field: "ext-plugin.cmd", isActive: len(cfg.ExtPlugin.Cmd) > 0},
		{field: "wasm.plugins", isActive: len(cfg.Wasm.Plugins) > 0},
		{field: "xrpc.protocols", isActive: len(cfg.XRPC.Protocols) > 0},
	} {
		if unsupported.isActive {
			return fmt.Errorf("%s is unsupported by the Go data plane", unsupported.field)
		}
	}
	for index, listener := range cfg.Apisix.Ssl.Listen {
		if listener.EnableQuic {
			return fmt.Errorf(
				"apisix.ssl.listen[%d].enable_quic is unsupported by the Go data plane",
				index,
			)
		}
		if listener.EnableHttp3 {
			return fmt.Errorf(
				"apisix.ssl.listen[%d].enable_http3 is unsupported by the Go data plane",
				index,
			)
		}
	}
	return nil
}

func validateSecurityProfile(cfg *Config, selection ProfileSelection) error {
	if selection.Security != SecurityStrict {
		return nil
	}
	profile := string(selection.Security)
	if cfg.Debug {
		return profileFieldError(profile, "debug", "must be false")
	}
	provider, err := EffectiveConfigProvider(cfg)
	if err != nil {
		return profileFieldError(profile, "deployment", "must select a supported role and config provider")
	}
	if provider == "etcd" {
		for index, endpoint := range cfg.Deployment.Etcd.Host {
			parsed, parseErr := url.Parse(endpoint)
			if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" {
				return profileFieldError(
					profile,
					fmt.Sprintf("deployment.etcd.host[%d]", index),
					"must use an HTTPS endpoint",
				)
			}
		}
		if cfg.Deployment.Etcd.TLS.Verify == nil || !*cfg.Deployment.Etcd.TLS.Verify {
			return profileFieldError(profile, "deployment.etcd.tls.verify", "must be explicitly true")
		}
	}
	if len(cfg.Apisix.TrustedAddresses) == 0 {
		return profileFieldError(profile, "apisix.trusted_addresses", "must contain at least one CIDR")
	}
	for index, address := range cfg.Apisix.TrustedAddresses {
		if _, _, parseErr := net.ParseCIDR(address); parseErr != nil {
			return profileFieldError(
				profile,
				fmt.Sprintf("apisix.trusted_addresses[%d]", index),
				"must be a valid CIDR",
			)
		}
	}
	return nil
}

func validateQualificationProfile(
	cfg *Config,
	manifest *capability.Manifest,
	selection ProfileSelection,
) error {
	if selection.Qualification == QualificationNone {
		return nil
	}
	profile := string(selection.Qualification)
	qualification, ok := manifest.Qualification(profile)
	if !ok {
		return profileFieldError(profile, "qualification_profile", "is not defined by the capability manifest")
	}
	if !slices.Equal(cfg.Plugins, qualification.RequiredPlugins) {
		return profileFieldError(profile, "plugins", "must equal the manifest qualification required_plugins set")
	}
	if !slices.Equal(qualification.RequiredPlugins, manifest.QualifiedPlugins(profile)) {
		return profileFieldError(profile, "evidence", "does not satisfy every required evidence claim")
	}
	if selection.Qualification != QualificationHTTPDataPlaneV1 {
		return nil
	}
	if cfg.Deployment.Role != "data_plane" {
		return profileFieldError(profile, "deployment.role", "must be data_plane")
	}
	provider, err := EffectiveConfigProvider(cfg)
	if err != nil || provider != "etcd" {
		return profileFieldError(profile, "deployment.role_data_plane.config_provider", "must resolve to etcd")
	}
	if cfg.Apisix.ProxyMode != "http" {
		return profileFieldError(profile, "apisix.proxy_mode", "must be http")
	}
	if len(cfg.Apisix.StreamProxy.Tcp) > 0 {
		return profileFieldError(profile, "apisix.stream_proxy.tcp", "must be empty")
	}
	if len(cfg.Apisix.StreamProxy.Udp) > 0 {
		return profileFieldError(profile, "apisix.stream_proxy.udp", "must be empty")
	}
	if len(cfg.StreamPlugins) > 0 {
		return profileFieldError(profile, "stream_plugins", "must be empty")
	}
	return nil
}

func validateProcessAccessLogs(cfg *Config) error {
	http := cfg.NginxConfig.HTTP
	stream := cfg.NginxConfig.Stream
	for _, field := range []struct {
		name   string
		active bool
	}{
		{name: "nginx_config.http.enable_access_log", active: http.EnableAccessLog},
		{name: "nginx_config.http.access_log", active: http.AccessLog != ""},
		{name: "nginx_config.http.access_log_buffer", active: http.AccessLogBuffer != 0},
		{name: "nginx_config.http.access_log_format", active: http.AccessLogFormat != ""},
		{name: "nginx_config.http.access_log_format_escape", active: http.AccessLogFormatEscape != ""},
		{name: "nginx_config.stream.enable_access_log", active: stream.EnableAccessLog},
		{name: "nginx_config.stream.access_log", active: stream.AccessLog != ""},
		{name: "nginx_config.stream.access_log_format", active: stream.AccessLogFormat != ""},
		{name: "nginx_config.stream.access_log_format_escape", active: stream.AccessLogFormatEscape != ""},
	} {
		if field.active {
			return fmt.Errorf("%s is unsupported by the Go data plane", field.name)
		}
	}
	return nil
}

func profileFieldError(profile, field, requirement string) error {
	return fmt.Errorf("%s: %s %s", profile, field, requirement)
}

func EffectiveConfigProvider(cfg *Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("deployment config must not be nil")
	}
	role := strings.ToLower(strings.TrimSpace(cfg.Deployment.Role))
	var provider string
	switch role {
	case "data_plane":
		provider = cfg.Deployment.RoleDataPlane.ConfigProvider
	case "control_plane":
		provider = cfg.Deployment.RoleControlPlane.ConfigProvider
	case "traditional":
		provider = cfg.Deployment.RoleTraditional.ConfigProvider
	default:
		return "", fmt.Errorf("deployment.role %q is unsupported", cfg.Deployment.Role)
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if role == "data_plane" {
		if slices.Contains([]string{"etcd", "yaml", "json"}, provider) {
			return provider, nil
		}
		return "", fmt.Errorf("deployment.role_data_plane.config_provider %q is unsupported", provider)
	}
	if provider != "etcd" {
		return "", fmt.Errorf("deployment.%s config_provider must be etcd, got %q", role, provider)
	}
	return provider, nil
}

func CapabilitySummary(cfg *Config) map[string]any {
	if cfg == nil {
		return nil
	}
	http2Enabled := cfg.Apisix.EnableHttp2
	for _, listener := range cfg.Apisix.NodeListen {
		http2Enabled = http2Enabled || listener.EnableHttp2
	}
	provider, err := EffectiveConfigProvider(cfg)
	if err != nil {
		provider = "unknown"
	}
	streamListeners := len(cfg.Apisix.StreamProxy.Tcp) + len(cfg.Apisix.StreamProxy.Udp)
	return map[string]any{
		"debug":                 cfg.Debug,
		"role":                  boundedSummaryValue(cfg.Deployment.Role, "traditional", "data_plane", "control_plane"),
		"config_provider":       boundedSummaryValue(provider, "etcd", "yaml", "json"),
		"http_listener_count":   len(cfg.Apisix.NodeListen),
		"https_listener_count":  len(cfg.Apisix.Ssl.Listen),
		"stream_listener_count": streamListeners,
		"http2_enabled":         http2Enabled,
		"tls_enabled":           cfg.Apisix.Ssl.Enable && len(cfg.Apisix.Ssl.Listen) > 0,
		"stream_enabled":        streamListeners > 0,
		"plugin_count":          len(cfg.Plugins),
		"stream_plugin_count":   len(cfg.StreamPlugins),
		"etcd_endpoint_count":   len(cfg.Deployment.Etcd.Host),
		"proxy_limits_configured": cfg.Proxy.MaxIdleConns > 0 &&
			cfg.Proxy.MaxIdleConnsPerHost > 0 && cfg.Proxy.MaxConnsPerHost > 0 && cfg.Proxy.MaxInFlight > 0,
	}
}

func boundedSummaryValue(value string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if slices.Contains(allowed, value) {
		return value
	}
	if value == "" {
		return "unset"
	}
	return "unknown"
}

func decodeHTTPPluginAllowlist(raw any) ([]string, bool) {
	switch value := raw.(type) {
	case string:
		if strings.Contains(value, ",") {
			return strings.Split(value, ","), true
		}
		if value == "" || value != strings.TrimSpace(value) {
			return []string{value}, true
		}
		return strings.Fields(value), true
	case []string:
		return append([]string(nil), value...), true
	case []any:
		plugins := make([]string, len(value))
		for index, item := range value {
			name, ok := item.(string)
			if !ok {
				return nil, false
			}
			plugins[index] = name
		}
		return plugins, true
	default:
		return nil, false
	}
}

func validateHTTPPluginAllowlist(names []string) error {
	seen := make(map[string]int, len(names))
	for index, name := range names {
		if name == "" {
			return fmt.Errorf("plugins[%d] %q must not be empty", index, name)
		}
		if name != strings.TrimSpace(name) {
			return fmt.Errorf("plugins[%d] %q must not have leading or trailing whitespace", index, name)
		}
		if previous, ok := seen[name]; ok {
			return fmt.Errorf("plugins[%d] %q duplicates plugins[%d]", index, name, previous)
		}
		seen[name] = index
	}
	return nil
}

func configDecodeHook(from reflect.Type, to reflect.Type, data any) (any, error) {
	if from.Kind() == reflect.String && to == reflect.TypeFor[time.Duration]() {
		return time.ParseDuration(strings.TrimSpace(data.(string)))
	}

	switch to {
	case reflect.TypeFor[NodeListen]():
		return decodeNodeListen(data)
	case reflect.TypeFor[TcpListen]():
		return decodeTCPListen(data)
	}
	if to.Kind() == reflect.Slice && to.Elem() == reflect.TypeFor[NodeListen]() {
		listen, err := decodeNodeListen(data)
		if err != nil {
			return nil, err
		}
		if value, ok := listen.(NodeListen); ok {
			return []NodeListen{value}, nil
		}
	}
	if to.Kind() == reflect.Slice && to.Elem() == reflect.TypeFor[TcpListen]() {
		listen, err := decodeTCPListen(data)
		if err != nil {
			return nil, err
		}
		if value, ok := listen.(TcpListen); ok {
			return []TcpListen{value}, nil
		}
	}

	if from.Kind() == reflect.String && to.Kind() == reflect.Slice && to.Elem().Kind() == reflect.String {
		value := strings.TrimSpace(data.(string))
		if value == "" {
			return []string{}, nil
		}
		if strings.Contains(value, ",") {
			parts := strings.Split(value, ",")
			for index := range parts {
				parts[index] = strings.TrimSpace(parts[index])
			}
			return parts, nil
		}
		return strings.Fields(value), nil
	}

	return data, nil
}

func decodeNodeListen(data any) (any, error) {
	port, host, ok, err := decodeListenAddress(data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return data, nil
	}
	return NodeListen{Ip: host, Port: port}, nil
}

func decodeTCPListen(data any) (any, error) {
	port, host, ok, err := decodeListenAddress(data)
	if err != nil {
		return nil, err
	}
	if !ok {
		if address, isString := data.(string); isString {
			return TcpListen{Addr: strings.TrimSpace(address)}, nil
		}
		return data, nil
	}
	return TcpListen{Addr: net.JoinHostPort(host, strconv.Itoa(port))}, nil
}

func decodeListenAddress(data any) (port int, host string, ok bool, err error) {
	switch value := data.(type) {
	case int:
		return value, "", true, nil
	case int8:
		return int(value), "", true, nil
	case int16:
		return int(value), "", true, nil
	case int32:
		return int(value), "", true, nil
	case int64:
		return int(value), "", true, nil
	case uint:
		return int(value), "", true, nil
	case uint8:
		return int(value), "", true, nil
	case uint16:
		return int(value), "", true, nil
	case uint32:
		return int(value), "", true, nil
	case uint64:
		return int(value), "", true, nil
	case string:
		address := strings.TrimSpace(value)
		if address == "" {
			return 0, "", true, nil
		}
		if port, err := strconv.Atoi(address); err == nil {
			return port, "", true, nil
		}
		if portString, found := strings.CutPrefix(address, ":"); found {
			port, err := strconv.Atoi(portString)
			return port, "", true, err
		}
		parsedHost, parsedPort, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return 0, "", false, nil
		}
		port, err = strconv.Atoi(parsedPort)
		return port, parsedHost, true, err
	default:
		return 0, "", false, nil
	}
}
