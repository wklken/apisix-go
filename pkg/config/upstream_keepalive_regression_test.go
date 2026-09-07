package config

import (
	"testing"
	"time"
)

func TestUpstreamKeepaliveDefaultsAndOverride(t *testing.T) {
	for _, test := range []struct {
		override string
		size     int
		idle     time.Duration
		requests int
	}{
		{"", 320, 60 * time.Second, 1000},
		{"nginx_config: {http: {upstream: {keepalive: 7}}}", 7, 60 * time.Second, 1000},
		{"nginx_config: {http: {upstream: {keepalive_timeout: 0}}}", 320, 0, 1000},
		{"nginx_config: {http: {upstream: {keepalive_requests: 2}}}", 320, 60 * time.Second, 2},
	} {
		cfg, err := LoadEffective(loadRequestFixture(t, test.override))
		if err != nil {
			t.Fatal(err)
		}
		pool := cfg.Config.NginxConfig.HTTP.Upstream
		if pool == nil || pool.Keepalive != test.size || pool.KeepaliveTimeout != test.idle ||
			pool.KeepaliveRequests != test.requests {
			t.Fatalf("pool=%+v want %d/%s", pool, test.size, test.idle)
		}
	}
}

func TestUpstreamKeepaliveRejectsNegativeRequestsAtLoad(t *testing.T) {
	_, err := LoadEffective(loadRequestFixture(t, "nginx_config: {http: {upstream: {keepalive_requests: -1}}}"))
	if err == nil {
		t.Fatal("negative keepalive_requests admitted at startup")
	}
}
