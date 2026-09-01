package config

import (
	"fmt"
	"net"
	"slices"
	"strings"
)

func validateEffective(effective *EffectiveConfig) error {
	if effective == nil {
		return fmt.Errorf("effective config must not be nil")
	}
	return validateRuntimeConfig(&effective.Config)
}

func validateDeprecatedPortLevelHTTP2(root *valueNode) error {
	if root == nil || root.kind != nodeMapping {
		return nil
	}
	apisix := root.mapping["apisix"]
	if apisix == nil || apisix.kind != nodeMapping {
		return nil
	}
	if err := validateDeprecatedListenerHTTP2(apisix.mapping["node_listen"], "apisix.node_listen"); err != nil {
		return err
	}
	ssl := apisix.mapping["ssl"]
	if ssl == nil || ssl.kind != nodeMapping {
		return nil
	}
	return validateDeprecatedListenerHTTP2(ssl.mapping["listen"], "apisix.ssl.listen")
}

func validateDeprecatedListenerHTTP2(listeners *valueNode, field string) error {
	if listeners == nil {
		return nil
	}
	items := listeners.sequence
	if listeners.kind == nodeMapping {
		items = []*valueNode{listeners}
	} else if listeners.kind != nodeSequence {
		return nil
	}
	for index, listener := range items {
		if listener == nil || listener.kind != nodeMapping {
			continue
		}
		if value, exists := listener.mapping["enable_http2"]; exists && value.kind != nodeNull {
			return fmt.Errorf(
				"%s[%d].enable_http2 is deprecated; use apisix.enable_http2",
				field,
				index,
			)
		}
	}
	return nil
}

func validateRuntimeConfig(cfg *Config) error {
	if err := validateHTTPPluginAllowlist(cfg.Plugins); err != nil {
		return err
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
	for index, address := range cfg.Apisix.TrustedAddresses {
		address = strings.TrimSpace(address)
		if address == "" || net.ParseIP(address) != nil {
			continue
		}
		if _, _, parseErr := net.ParseCIDR(address); parseErr != nil {
			return fmt.Errorf("apisix.trusted_addresses[%d] must be a valid CIDR or IP address", index)
		}
	}
	if cfg.NginxConfig.HTTP.ClientMaxBodySize < 0 {
		return fmt.Errorf("nginx_config.http.client_max_body_size must be non-negative")
	}
	if cfg.NginxConfig.HTTP.ClientBodyTimeout <= 0 {
		return fmt.Errorf("nginx_config.http.client_body_timeout must be positive")
	}
	if sendTimeout := cfg.NginxConfig.HTTP.SendTimeout; sendTimeout != 0 {
		return fmt.Errorf(
			"nginx_config.http.send_timeout must be zero because Go cannot implement NGINX write-idle semantics",
		)
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
		if listener.EnableHttp3 {
			return fmt.Errorf("apisix.ssl.listen[%d].enable_http3 is unsupported by the Go data plane", index)
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
		return "", fmt.Errorf("deployment.role=control_plane is unsupported by the Go data plane")
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
