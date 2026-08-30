package config

import (
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
)

func validateEffective(effective *EffectiveConfig, unused []string) error {
	if effective == nil {
		return fmt.Errorf("effective config must not be nil")
	}
	if len(unused) > 0 {
		field := "fields"
		if len(unused) == 1 {
			field = "field"
		}
		return fmt.Errorf("static configuration contains %d unsupported %s", len(unused), field)
	}
	if err := validateRuntimePaths(effective.Paths); err != nil {
		return err
	}
	return validateRuntimeConfig(&effective.Config)
}

func validateRuntimePaths(paths RuntimePaths) error {
	if paths.DataDir == "" || !filepath.IsAbs(paths.DataDir) {
		return fmt.Errorf("runtime path data_dir must be a non-empty absolute path")
	}
	return nil
}

func validateRuntimeConfig(cfg *Config) error {
	if err := validateHTTPPluginAllowlist(cfg.Plugins); err != nil {
		return err
	}
	if len(cfg.Plugins) == 0 {
		return fmt.Errorf("plugins must contain at least one HTTP plugin")
	}
	if len(cfg.Apisix.NodeListen) == 0 {
		return fmt.Errorf("apisix.node_listen must contain at least one listener")
	}
	for index, listener := range cfg.Apisix.NodeListen {
		if listener.Port < 1 || listener.Port > 65535 {
			return fmt.Errorf("apisix.node_listen[%d].port must be between 1 and 65535", index)
		}
		if listener.Ip != "" && net.ParseIP(listener.Ip) == nil {
			return fmt.Errorf("apisix.node_listen[%d].ip must be a valid IP address", index)
		}
	}
	if (cfg.Apisix.Status.IP != "" || cfg.Apisix.Status.Port != 0) &&
		(cfg.Apisix.Status.Port < 1 || cfg.Apisix.Status.Port > 65535) {
		return fmt.Errorf("apisix.status.port must be between 1 and 65535")
	}
	if (cfg.Apisix.Status.IP != "" || cfg.Apisix.Status.Port != 0) && net.ParseIP(cfg.Apisix.Status.IP) == nil {
		return fmt.Errorf("apisix.status.ip must be a valid IP address")
	}
	if cfg.Apisix.EnableControl && (cfg.Apisix.Control.Port < 1 || cfg.Apisix.Control.Port > 65535) {
		return fmt.Errorf("apisix.control.port must be between 1 and 65535")
	}
	if cfg.Apisix.EnableControl && net.ParseIP(cfg.Apisix.Control.Ip) == nil {
		return fmt.Errorf("apisix.control.ip must be a valid IP address")
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
			return fmt.Errorf("%s must be positive", limit.field)
		}
	}
	for index, address := range cfg.Apisix.TrustedAddresses {
		address = strings.TrimSpace(address)
		if address == "" || net.ParseIP(address) != nil {
			continue
		}
		if _, _, parseErr := net.ParseCIDR(address); parseErr != nil {
			return fmt.Errorf("apisix.trusted_addresses[%d] must be a valid CIDR or IP address", index)
		}
	}
	if cfg.NginxConfig.HTTP.ClientMaxBodySize <= 0 {
		return fmt.Errorf("nginx_config.http.client_max_body_size must be positive")
	}
	if cfg.NginxConfig.HTTP.ClientBodyTimeout <= 0 {
		return fmt.Errorf("nginx_config.http.client_body_timeout must be positive")
	}
	if sendTimeout := cfg.NginxConfig.HTTP.SendTimeout; sendTimeout != 0 {
		return fmt.Errorf(
			"nginx_config.http.send_timeout must be zero because Go cannot implement NGINX write-idle semantics",
		)
	}
	if err := validateProcessAccessLogs(cfg); err != nil {
		return err
	}

	provider, err := EffectiveConfigProvider(cfg)
	if err != nil {
		return err
	}
	if provider == "etcd" {
		if len(cfg.Deployment.Etcd.Host) == 0 {
			return fmt.Errorf("deployment.etcd.host must contain at least one endpoint for the etcd provider")
		}
		for index, endpoint := range cfg.Deployment.Etcd.Host {
			if strings.TrimSpace(endpoint) == "" {
				return fmt.Errorf("deployment.etcd.host[%d] must not be empty for the etcd provider", index)
			}
		}
		if strings.TrimSpace(cfg.Deployment.Etcd.Prefix) == "" {
			return fmt.Errorf("deployment.etcd.prefix must not be empty for the etcd provider")
		}
	}
	return validateUnsupportedRuntimeConfig(cfg)
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

func validateProcessAccessLogs(cfg *Config) error {
	http := cfg.NginxConfig.HTTP
	stream := cfg.NginxConfig.Stream
	logRotateOwnsHTTPSelection := slices.Contains(cfg.Plugins, "log-rotate")
	for _, field := range []struct {
		name   string
		active bool
	}{
		{name: "nginx_config.http.enable_access_log", active: http.EnableAccessLog && !logRotateOwnsHTTPSelection},
		{name: "nginx_config.http.access_log", active: http.AccessLog != "" && !logRotateOwnsHTTPSelection},
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
