package limit_count

import (
	"encoding/json"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestAPISIX317SchemaOmitsConfigurableCost(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if _, ok := document.Properties["cost"]; ok {
		t.Fatal("schema exposes non-APISIX configurable cost")
	}
}

func TestAPISIX317SchemaOmitsSyncInterval(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if _, ok := document.Properties["sync_interval"]; ok {
		t.Fatal("schema exposes non-APISIX sync_interval")
	}
}

func TestAPISIX317SchemaOmitsWindowType(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if _, ok := document.Properties["window_type"]; ok {
		t.Fatal("schema exposes non-APISIX window_type")
	}
}

func TestAPISIX317SchemaAcceptsEmptyStringLimitsAndRules(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
	}{
		{
			name: "root string limits",
			config: map[string]any{
				"count": "", "time_window": "",
			},
		},
		{
			name: "rule string limits",
			config: map[string]any{
				"rules": []any{map[string]any{
					"count": "", "time_window": "", "key": "$http_x_user",
				}},
			},
		},
		{
			name:   "empty rules",
			config: map[string]any{"rules": []any{}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err != nil {
				t.Fatalf("schema rejected APISIX-valid config %#v: %v", test.config, err)
			}
		})
	}
}

func TestAPISIX317SchemaRejectionMatrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	validBase := func() map[string]any {
		return map[string]any{
			"count":       2,
			"time_window": 60,
			"key":         "remote_addr",
		}
	}
	tests := []struct {
		name   string
		config map[string]any
	}{
		{
			name: "rejected_msg must be a string",
			config: map[string]any{
				"count": 2, "time_window": 60, "rejected_msg": 42,
			},
		},
		{
			name: "rejected_msg must not be empty",
			config: map[string]any{
				"count": 2, "time_window": 60, "rejected_msg": "",
			},
		},
		{
			name:   "count is required without rules",
			config: map[string]any{"time_window": 60},
		},
		{
			name:   "count must be positive",
			config: map[string]any{"count": 0, "time_window": 60},
		},
		{
			name:   "time_window must be positive",
			config: map[string]any{"count": 2, "time_window": 0},
		},
		{
			name: "redis policy requires redis_host",
			config: map[string]any{
				"count": 2, "time_window": 60, "policy": "redis",
			},
		},
		{
			name: "redis cluster policy requires nodes",
			config: map[string]any{
				"count": 2, "time_window": 60, "policy": "redis-cluster",
				"redis_cluster_name": "fixture-cluster",
			},
		},
		{
			name: "redis cluster ssl must be boolean",
			config: map[string]any{
				"count": 2, "time_window": 60, "policy": "redis-cluster",
				"redis_cluster_name":  "fixture-cluster",
				"redis_cluster_nodes": []any{"127.0.0.1:6379"},
				"redis_cluster_ssl":   "true",
			},
		},
		{
			name: "redis cluster ssl verify must be boolean",
			config: map[string]any{
				"count": 2, "time_window": 60, "policy": "redis-cluster",
				"redis_cluster_name":       "fixture-cluster",
				"redis_cluster_nodes":      []any{"127.0.0.1:6379"},
				"redis_cluster_ssl_verify": "false",
			},
		},
		{
			name: "redis sentinel policy is not supported",
			config: map[string]any{
				"count": 2, "time_window": 60, "policy": "redis-sentinel",
				"redis_sentinels":   []any{map[string]any{"host": "127.0.0.1", "port": 26379}},
				"redis_master_name": "mymaster",
			},
		},
		{
			name: "rules and root quota are mutually exclusive",
			config: func() map[string]any {
				config := validBase()
				config["rules"] = []any{map[string]any{
					"count": 1, "time_window": 60, "key": "$http_x_user",
				}}
				return config
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatalf("schema accepted invalid config %#v", test.config)
			}
		})
	}
}

func TestAPISIX317SchemaRejectsNestedRedisCompatibilityContainers(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
	}{
		{
			name: "redis config",
			config: map[string]any{
				"count": 2, "time_window": 60, "policy": "redis",
				"redis_config": map[string]any{"redis_host": "127.0.0.1"},
			},
		},
		{
			name: "redis cluster config",
			config: map[string]any{
				"count": 2, "time_window": 60, "policy": "redis-cluster",
				"redis_cluster_config": map[string]any{
					"redis_cluster_nodes": []any{"127.0.0.1:6379"},
					"redis_cluster_name":  "fixture-cluster",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatalf("schema accepted non-APISIX nested Redis config %#v", test.config)
			}
		})
	}
}
