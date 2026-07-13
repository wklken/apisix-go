package server

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
)

func TestServerInfoReportingEnabledOnlyForEtcdBackedNonDataPlane(t *testing.T) {
	previous := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = previous })

	tests := []struct {
		name    string
		config  *config.Config
		enabled bool
	}{
		{
			name: "traditional etcd",
			config: &config.Config{
				Plugins: []string{"server-info"},
				Deployment: config.Deployment{
					Role:            "traditional",
					RoleTraditional: config.RoleTraditionalConfig{ConfigProvider: "etcd"},
				},
			},
			enabled: true,
		},
		{
			name: "data plane",
			config: &config.Config{
				Plugins: []string{"server-info"},
				Deployment: config.Deployment{
					Role:            "data_plane",
					RoleTraditional: config.RoleTraditionalConfig{ConfigProvider: "etcd"},
				},
			},
		},
		{
			name: "server-info disabled",
			config: &config.Config{
				Deployment: config.Deployment{
					Role:            "traditional",
					RoleTraditional: config.RoleTraditionalConfig{ConfigProvider: "etcd"},
				},
			},
		},
		{
			name: "non-etcd provider",
			config: &config.Config{
				Plugins: []string{"server-info"},
				Deployment: config.Deployment{
					Role:            "traditional",
					RoleTraditional: config.RoleTraditionalConfig{ConfigProvider: "yaml"},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config.GlobalConfig = test.config
			if got := serverInfoReportingEnabled(); got != test.enabled {
				t.Fatalf("serverInfoReportingEnabled() = %t, want %t", got, test.enabled)
			}
		})
	}
}
