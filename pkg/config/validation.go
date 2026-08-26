package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
)

func validateEffective(effective *EffectiveConfig, unused []string, manifest *capability.Manifest) error {
	if effective == nil {
		return fmt.Errorf("effective config must not be nil")
	}
	if err := effective.Profiles.Validate(manifest); err != nil {
		return err
	}
	if effective.Profiles.Security == SecurityStrict && len(unused) != 0 {
		return fmt.Errorf("security_profile %s: unknown static configuration field", SecurityStrict)
	}
	if err := validateRuntimePaths(effective.Paths, effective.Profiles); err != nil {
		return err
	}
	return validateRuntimeConfig(&effective.Config, manifest)
}

func validateRuntimePaths(paths RuntimePaths, selection ProfileSelection) error {
	if paths.DataDir == "" || !filepath.IsAbs(paths.DataDir) {
		return runtimePathError(selection, "data_dir")
	}
	if selection.Qualification == QualificationNone {
		return nil
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "data_dir", value: paths.DataDir},
		{name: "runtime_dir", value: paths.RuntimeDir},
		{name: "log_dir", value: paths.LogDir},
		{name: "temp_dir", value: paths.TempDir},
	} {
		if field.value == "" || !filepath.IsAbs(field.value) {
			return runtimePathError(selection, field.name)
		}
	}
	return nil
}

func runtimePathError(selection ProfileSelection, name string) error {
	if selection.Qualification != QualificationNone {
		return fmt.Errorf(
			"qualification_profile %s: runtime path %s must be a non-empty absolute path",
			selection.Qualification, name,
		)
	}
	return fmt.Errorf("runtime path %s must be a non-empty absolute path", name)
}

func validateRuntimeConfig(cfg *Config, manifest *capability.Manifest) error {
	selection := cfg.Profiles()
	if err := selection.Validate(manifest); err != nil {
		return err
	}
	if err := validateHTTPPluginAllowlist(cfg.Plugins); err != nil {
		return profileAwareRuntimeError(selection, err)
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
				fmt.Errorf("apisix.node_listen[%d].port must be between 1 and 65535", index),
			)
		}
		if listener.Ip != "" && net.ParseIP(listener.Ip) == nil {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf("apisix.node_listen[%d].ip must be a valid IP address", index),
			)
		}
	}
	if (cfg.Apisix.Status.IP != "" || cfg.Apisix.Status.Port != 0) &&
		(cfg.Apisix.Status.Port < 1 || cfg.Apisix.Status.Port > 65535) {
		return profileAwareRuntimeError(
			selection,
			fmt.Errorf("apisix.status.port must be between 1 and 65535"),
		)
	}
	if (cfg.Apisix.Status.IP != "" || cfg.Apisix.Status.Port != 0) && net.ParseIP(cfg.Apisix.Status.IP) == nil {
		return profileAwareRuntimeError(
			selection,
			fmt.Errorf("apisix.status.ip must be a valid IP address"),
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
		if limit.value <= 0 {
			return profileAwareRuntimeError(
				selection,
				fmt.Errorf("%s must be positive", limit.field),
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
				fmt.Errorf("apisix.trusted_addresses[%d] must be a valid CIDR or IP address", index),
			)
		}
	}
	if cfg.NginxConfig.HTTP.ClientMaxBodySize <= 0 {
		return profileAwareRuntimeError(selection, fmt.Errorf(
			"nginx_config.http.client_max_body_size must be positive",
		))
	}
	if cfg.NginxConfig.HTTP.ClientBodyTimeout <= 0 {
		return profileAwareRuntimeError(selection, fmt.Errorf(
			"nginx_config.http.client_body_timeout must be positive",
		))
	}
	if sendTimeout := cfg.NginxConfig.HTTP.SendTimeout; sendTimeout != 0 {
		return profileAwareRuntimeError(selection, fmt.Errorf(
			"nginx_config.http.send_timeout must be zero because Go cannot implement NGINX write-idle semantics",
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
			return fmt.Errorf("apisix.ssl.listen[%d].enable_quic is unsupported by the Go data plane", index)
		}
		if listener.EnableHttp3 {
			return fmt.Errorf("apisix.ssl.listen[%d].enable_http3 is unsupported by the Go data plane", index)
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

func validateQualificationProfile(cfg *Config, manifest *capability.Manifest, selection ProfileSelection) error {
	if selection.Qualification == QualificationNone {
		return nil
	}
	profile := string(selection.Qualification)
	_, ok := manifest.Qualification(profile)
	if !ok {
		return profileFieldError(profile, "qualification_profile", "is not defined by the capability manifest")
	}
	if err := ValidateQualificationPlugins(cfg.Plugins, selection, manifest); err != nil {
		return err
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
		return "", fmt.Errorf("deployment.role is unsupported")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if role == "data_plane" {
		if slices.Contains([]string{"etcd", "yaml", "json"}, provider) {
			return provider, nil
		}
		return "", fmt.Errorf("deployment.role_data_plane.config_provider is unsupported")
	}
	if provider != "etcd" {
		return "", fmt.Errorf("deployment.%s config_provider must be etcd", role)
	}
	return provider, nil
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
			return fmt.Errorf("plugins[%d] must not be empty", index)
		}
		if name != strings.TrimSpace(name) {
			return fmt.Errorf("plugins[%d] must not have leading or trailing whitespace", index)
		}
		if previous, ok := seen[name]; ok {
			return fmt.Errorf("plugins[%d] duplicates plugins[%d]", index, previous)
		}
		seen[name] = index
	}
	return nil
}
