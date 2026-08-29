package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
)

func TestConfiguredControlAddressHonorsEnableAndOverride(t *testing.T) {
	tests := []struct {
		name    string
		config  *config.Config
		address string
		enabled bool
	}{
		{name: "disabled", config: &config.Config{}},
		{
			name:    "default",
			config:  &config.Config{Apisix: config.Apisix{EnableControl: true}},
			address: "127.0.0.1:9090",
			enabled: true,
		},
		{
			name: "override",
			config: &config.Config{Apisix: config.Apisix{
				EnableControl: true,
				Control:       config.Control{Ip: "127.0.0.2", Port: 19090},
			}},
			address: "127.0.0.2:19090",
			enabled: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, enabled := configuredControlAddress(test.config)
			if address != test.address || enabled != test.enabled {
				t.Fatalf("configuredControlAddress() = %q/%t, want %q/%t", address, enabled, test.address, test.enabled)
			}
		})
	}
}

func TestControlHandlerUsesSharedServerInfoView(t *testing.T) {
	view := server_info.NewView("control-node")
	view.SetEtcdVersion("3.6.13")
	cfg := &config.Config{Apisix: config.Apisix{EnableControl: true}, Plugins: []string{"server-info"}}
	response := httptest.NewRecorder()
	newControlHandler(cfg, view).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/server_info", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	var body server_info.Response
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ID != "control-node" || body.EtcdVersion != "3.6.13" {
		t.Fatalf("server-info response = %+v, want shared view", body)
	}
}

func TestServerInfoReportingEnabledOnlyForEtcdBackedNonDataPlane(t *testing.T) {
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
			if got := serverInfoReportingEnabled(test.config); got != test.enabled {
				t.Fatalf("serverInfoReportingEnabled() = %t, want %t", got, test.enabled)
			}
		})
	}
}
