package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

func LoadEffective(req LoadRequest) (*EffectiveConfig, error) {
	if req.DefaultPath == "" || !filepath.IsAbs(req.DefaultPath) {
		return nil, fmt.Errorf("load effective config: default path must be a non-empty absolute path")
	}
	if req.OverridePath != "" && !filepath.IsAbs(req.OverridePath) {
		return nil, fmt.Errorf("load effective config: override path must be absolute")
	}

	defaultPath := filepath.Clean(req.DefaultPath)
	root := builtinDefaults()
	defaultDocument, err := readConfigDocument(defaultPath, req.Environment)
	if err != nil {
		return nil, err
	}
	root = mergeNodes(root, defaultDocument)

	overridePath := ""
	if req.OverridePath != "" {
		overridePath = filepath.Clean(req.OverridePath)
	}
	if overridePath != "" && !sameConfigPath(defaultPath, overridePath) {
		override, readErr := readConfigDocument(overridePath, req.Environment)
		if readErr != nil {
			return nil, readErr
		}
		root = mergeNodes(root, override)
	}
	if err := applyReservedEnvironment(root, req.Environment); err != nil {
		return nil, err
	}

	cfg, unused, err := decodeConfig(root)
	if err != nil {
		return nil, err
	}
	effective := &EffectiveConfig{Config: *cfg}
	if err := validateEffective(effective, unused); err != nil {
		return nil, err
	}
	return effective, nil
}

func sameConfigPath(first, second string) bool {
	return filepath.Clean(first) == filepath.Clean(second)
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
