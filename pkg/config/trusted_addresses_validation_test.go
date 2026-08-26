package config

import (
	"strings"
	"testing"
)

func TestValidateRuntimeConfigAcceptsBareTrustedIPInCompatibilityMode(t *testing.T) {
	cfg := validCompatibilityConfigForTrustedAddresses()
	cfg.Apisix.TrustedAddresses = []string{"10.0.0.1", "2001:db8::1"}
	if err := validateRuntimeConfig(cfg, loadProfileTestManifest(t)); err != nil {
		t.Fatalf("validateRuntimeConfig() error = %v, want bare IP addresses accepted", err)
	}
}

func TestValidateRuntimeConfigRejectsInvalidTrustedAddressInCompatibilityMode(t *testing.T) {
	cfg := validCompatibilityConfigForTrustedAddresses()
	cfg.Apisix.TrustedAddresses = []string{"not-an-ip"}
	if err := validateRuntimeConfig(cfg, loadProfileTestManifest(t)); err == nil ||
		!strings.Contains(err.Error(), "apisix.trusted_addresses[0]") {
		t.Fatalf("validateRuntimeConfig() error = %v, want trusted address rejection", err)
	}
}

func validCompatibilityConfigForTrustedAddresses() *Config {
	return &Config{
		CompatibilityTarget: CompatibilityAPISIX317,
		SecurityProfile:     SecurityCompat,
		Plugins:             []string{"request-id"},
		Apisix:              Apisix{NodeListen: []NodeListen{{Ip: "127.0.0.1", Port: 9080}}},
		Proxy:               Proxy{MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, MaxInFlight: 1},
		NginxConfig: NginxConfig{HTTP: NginxHTTP{
			ClientMaxBodySize: 1,
			ClientBodyTimeout: 1,
		}},
		Deployment: Deployment{
			Role:            "traditional",
			RoleTraditional: RoleTraditionalConfig{ConfigProvider: "etcd"},
			Etcd:            Etcd{Host: []string{"https://127.0.0.1:2379"}, Prefix: "/apisix"},
		},
	}
}
