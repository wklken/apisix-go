package config

import "time"

const (
	DefaultConfigFile        = "conf/config-default.yaml"
	defaultClientMaxBodySize = int64(10 * 1024 * 1024)
	defaultClientBodyTimeout = 60 * time.Second
)

func builtinDefaults(paths RuntimePaths) *valueNode {
	return mustNodeFromAny(map[string]any{
		"nginx_config": map[string]any{"http": map[string]any{
			"client_max_body_size": defaultClientMaxBodySize,
			"client_body_timeout":  defaultClientBodyTimeout.String(),
		}},
		"apisix": map[string]any{
			"status":  map[string]any{"ip": "127.0.0.1", "port": 7085},
			"control": map[string]any{"ip": "127.0.0.1", "port": 9090},
		},
		"apisix_go": map[string]any{"runtime_paths": map[string]any{
			"data_dir": paths.DataDir, "runtime_dir": paths.RuntimeDir,
			"log_dir": paths.LogDir, "temp_dir": paths.TempDir,
		}},
	}, FieldSource{Kind: SourceBuiltin, Origin: "apisix-go-runtime-defaults", Explicit: false})
}
