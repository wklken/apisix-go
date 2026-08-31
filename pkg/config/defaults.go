package config

import "time"

const (
	DefaultConfigFile        = "conf/config-default.yaml"
	defaultClientBodyTimeout = 60 * time.Second
)

func builtinDefaults() *valueNode {
	return mustNodeFromAny(map[string]any{
		"nginx_config": map[string]any{"http": map[string]any{
			"enable_access_log":    true,
			"client_max_body_size": 0,
			"client_body_timeout":  defaultClientBodyTimeout.String(),
		}},
		"apisix": map[string]any{
			"status":  map[string]any{"ip": "127.0.0.1", "port": 7085},
			"control": map[string]any{"ip": "127.0.0.1", "port": 9090},
		},
	}, "")
}
