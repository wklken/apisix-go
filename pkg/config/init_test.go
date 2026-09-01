package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDecodeConfigSupportsOfficialConfigShapes(t *testing.T) {
	effective := loadEffectiveFixture(t, `
apisix:
  node_listen: [9080, {ip: 127.0.0.2, port: 9081}]
  enable_http2: true
  proxy_cache: {cache_ttl: 10s}
  stream_proxy: {tcp: ["9100"], udp: [9200, "127.0.0.1:9201"]}
  ssl: {ssl_protocols: TLSv1.2 TLSv1.3}
  lru: {secret: {ttl: 300, count: 512, neg_ttl: 60, neg_count: 512}}
  events: {module: lua-resty-events}
nginx_config:
  worker_shutdown_timeout: 240s
  http:
    client_header_timeout: 60s
    client_body_timeout: 60s
    keepalive_timeout: 60s
    send_timeout: 0s
    client_max_body_size: 1024
    upstream: {keepalive: 320, keepalive_requests: 1000, keepalive_timeout: 60s}
graphql: {max_size: 1048576}
plugins: [request-id, gzip]
stream_plugins: [mqtt-proxy]
plugin_attr: {prometheus: {export_addr: {ip: 127.0.0.1, port: 9091}}}
`)
	cfg := &effective.Config
	wantAddresses := []string{"0.0.0.0:9080", "127.0.0.2:9081"}
	if got := cfg.Apisix.ListenAddresses(); !reflect.DeepEqual(got, wantAddresses) {
		t.Fatalf("ListenAddresses() = %#v, want %#v", got, wantAddresses)
	}
	if got := cfg.Apisix.StreamProxy.Tcp[0].Addr; got != ":9100" {
		t.Fatalf("stream tcp address = %q", got)
	}
	if got := cfg.NginxConfig.HTTP.ClientBodyTimeout; got != 60*time.Second {
		t.Fatalf("client_body_timeout = %s", got)
	}
	if got := cfg.PluginAttr["prometheus"]["export_addr"].(map[string]any)["port"]; got != json.Number("9091") {
		t.Fatalf("plugin_attr port = %#v", got)
	}
}

func TestLoadEffectiveAppliesAPISIXRequestBodyDefaultsWhenOmitted(t *testing.T) {
	req := loadRequestFixture(t, "")
	if err := writeTestConfig(req.DefaultPath, `
apisix: {node_listen: [{port: 9080}]}
plugins: [request-id]
deployment: {role: data_plane, role_data_plane: {config_provider: yaml}}
`); err != nil {
		t.Fatal(err)
	}
	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := effective.Config.NginxConfig.HTTP.ClientMaxBodySize; got != 0 {
		t.Fatalf("client_max_body_size = %d", got)
	}
	if got := effective.Config.NginxConfig.HTTP.ClientBodyTimeout; got != 60*time.Second {
		t.Fatalf("client_body_timeout = %s", got)
	}
	if got := effective.Config.Apisix.Status; got != (Status{IP: "127.0.0.1", Port: 7085}) {
		t.Fatalf("apisix.status = %#v, want loopback status listener", got)
	}
	if got := effective.Config.Apisix.Control; got != (Control{Ip: "127.0.0.1", Port: 9090}) {
		t.Fatalf("apisix.control = %#v, want APISIX 3.17 control listener default", got)
	}
}

func TestLoadEffectiveAllowsUnlimitedClientBodySize(t *testing.T) {
	effective := loadEffectiveFixture(t, `
nginx_config:
  http:
    client_max_body_size: 0
`)
	if got := effective.Config.NginxConfig.HTTP.ClientMaxBodySize; got != 0 {
		t.Fatalf("client_max_body_size = %d, want unlimited 0", got)
	}
}

func TestLoadEffectivePreservesExplicitControlListenerOverride(t *testing.T) {
	effective := loadEffectiveFixture(t, `
apisix:
  enable_control: true
  control: {ip: 127.0.0.2, port: 19090}
`)
	if got := effective.Config.Apisix.Control; got != (Control{Ip: "127.0.0.2", Port: 19090}) {
		t.Fatalf("apisix.control = %#v, want explicit override", got)
	}
}

func TestLoadEffectiveRejectsInvalidRuntimeValues(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
	}{
		{name: "send timeout", override: "nginx_config: {http: {send_timeout: 1s}}", want: "send_timeout"},
		{
			name:     "negative body size",
			override: "nginx_config: {http: {client_max_body_size: -1}}",
			want:     "client_max_body_size",
		},
		{
			name:     "body timeout",
			override: "nginx_config: {http: {client_body_timeout: 0s}}",
			want:     "client_body_timeout",
		},
		{name: "status port", override: "apisix: {status: {ip: 127.0.0.1, port: 0}}", want: "apisix.status.port"},
		{
			name:     "status IP",
			override: "apisix: {status: {ip: public.example.com, port: 7085}}",
			want:     "apisix.status.ip",
		},
		{
			name:     "control port",
			override: "apisix: {enable_control: true, control: {port: 0}}",
			want:     "apisix.control.port",
		},
		{
			name:     "control IP",
			override: "apisix: {enable_control: true, control: {ip: public.example.com}}",
			want:     "apisix.control.ip",
		},
		{name: "empty plugin", override: "plugins: ['']", want: "plugins[0] must not be empty"},
		{name: "duplicate plugin", override: "plugins: [request-id, request-id]", want: "duplicates"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadEffective(loadRequestFixture(t, test.override))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadEffective() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadEffectiveRejectsDeprecatedPortLevelHTTP2(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
	}{
		{
			name:     "HTTP listener",
			override: "apisix: {node_listen: [{port: 9080, enable_http2: false}]}",
			want:     "apisix.node_listen[0].enable_http2 is deprecated; use apisix.enable_http2",
		},
		{
			name:     "HTTPS listener",
			override: "apisix: {ssl: {listen: [{port: 9443, enable_http2: false}]}}",
			want:     "apisix.ssl.listen[0].enable_http2 is deprecated; use apisix.enable_http2",
		},
		{
			name:     "singleton HTTP listener mapping",
			override: "apisix: {node_listen: {port: 9080, enable_http2: false}}",
			want:     "apisix.node_listen[0].enable_http2 is deprecated; use apisix.enable_http2",
		},
		{
			name:     "singleton HTTPS listener mapping",
			override: "apisix: {ssl: {listen: {port: 9443, enable_http2: false}}}",
			want:     "apisix.ssl.listen[0].enable_http2 is deprecated; use apisix.enable_http2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadEffective(loadRequestFixture(t, test.override))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadEffective() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadEffectiveAllowsNullPortLevelHTTP2(t *testing.T) {
	for name, override := range map[string]string{
		"HTTP listener":  "apisix: {node_listen: [{port: 9080, enable_http2: null}]}",
		"HTTPS listener": "apisix: {ssl: {listen: [{port: 9443, enable_http2: null}]}}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadEffective(loadRequestFixture(t, override)); err != nil {
				t.Fatalf("LoadEffective() error = %v, want null to match an omitted field", err)
			}
		})
	}
}

func TestLoadEffectiveSupportsScalarNodeListen(t *testing.T) {
	effective := loadEffectiveFixture(t, "apisix: {node_listen: 9080}")
	if got, want := effective.Config.Apisix.ListenAddresses(), []string{"0.0.0.0:9080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListenAddresses() = %#v, want %#v", got, want)
	}
}

func TestApisixListenAddressesDefaultsToLegacyAddress(t *testing.T) {
	if got, want := (Apisix{}).ListenAddresses(), []string{":8080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListenAddresses() = %#v, want %#v", got, want)
	}
}
