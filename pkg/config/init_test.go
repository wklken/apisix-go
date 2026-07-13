package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestLoadSupportsOfficialConfigShapes(t *testing.T) {
	previous := GlobalConfig
	t.Cleanup(func() { GlobalConfig = previous })

	v := viper.New()
	v.SetConfigType("yaml")
	err := v.ReadConfig(strings.NewReader(`
apisix:
  node_listen:
    - 9080
    - ip: 127.0.0.2
      port: 9081
      enable_http2: true
  enable_http2: true
  proxy_cache:
    cache_ttl: 10s
  stream_proxy:
    tcp:
      - "9100"
    udp:
      - 9200
      - "127.0.0.1:9201"
  ssl:
    listen:
      - port: 9443
        enable_http3: true
    ssl_protocols: TLSv1.2 TLSv1.3
  lru:
    secret:
      ttl: 300
      count: 512
      neg_ttl: 60
      neg_count: 512
nginx_config:
  worker_shutdown_timeout: 240s
  http:
    client_header_timeout: 60s
    client_body_timeout: 60s
    keepalive_timeout: 60s
    send_timeout: 10s
    client_max_body_size: 1024
    upstream:
      keepalive: 320
      keepalive_requests: 1000
      keepalive_timeout: 60s
graphql:
  max_size: 1048576
ext-plugin:
  cmd: ["example-plugin"]
wasm:
  plugins:
    - name: wasm_log
      priority: 7999
      file: log.wasm
xrpc:
  protocols:
    - name: pingpong
events:
  module: lua-resty-events
plugins: [request-id, gzip]
stream_plugins: [mqtt-proxy]
plugin_attr:
  prometheus:
    export_addr:
      ip: 127.0.0.1
      port: 9091
deployment:
  role: traditional
  role_data_plane:
    config_provider: yaml
  admin:
    admin_key_required: true
    enable_admin_ui: true
    admin_listen:
      ip: 127.0.0.1
      port: 9180
  etcd:
    host: ["https://127.0.0.1:2379"]
    prefix: /apisix
    timeout: 30
    watch_timeout: 50
    startup_retry: 2
    tls:
      verify: true
      sni: etcd.example.com
`))
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}

	cfg, err := load(v)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if got, want := cfg.Apisix.ListenAddresses(), []string{
		"0.0.0.0:9080",
		"127.0.0.2:9081",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListenAddresses() = %#v, want %#v", got, want)
	}
	if got, want := cfg.Apisix.StreamProxy.Tcp[0].Addr, ":9100"; got != want {
		t.Fatalf("stream tcp address = %q, want %q", got, want)
	}
	if !cfg.Apisix.Ssl.Listen[0].EnableHttp3 {
		t.Fatal("ssl.listen.enable_http3 = false, want true")
	}
	if got, want := cfg.Apisix.Ssl.SslProtocols, "TLSv1.2 TLSv1.3"; got != want {
		t.Fatalf("ssl_protocols = %q, want %q", got, want)
	}
	if got, want := cfg.NginxConfig.HTTP.ClientBodyTimeout, 60*time.Second; got != want {
		t.Fatalf("client_body_timeout = %s, want %s", got, want)
	}
	if got, want := cfg.NginxConfig.HTTP.Upstream.Keepalive, 320; got != want {
		t.Fatalf("upstream.keepalive = %d, want %d", got, want)
	}
	if got, want := cfg.Deployment.Etcd.TLS.SNI, "etcd.example.com"; got != want {
		t.Fatalf("etcd.tls.sni = %q, want %q", got, want)
	}
	exportAddr, ok := cfg.PluginAttr["prometheus"]["export_addr"].(map[string]any)
	if !ok {
		t.Fatalf("plugin_attr.prometheus.export_addr = %#v, want map", cfg.PluginAttr["prometheus"]["export_addr"])
	}
	if got, want := exportAddr["port"], 9091; got != want {
		t.Fatalf("plugin_attr.prometheus.export_addr.port = %#v, want %v", got, want)
	}
}

func TestApisixListenAddressesDefaultsToLegacyAddress(t *testing.T) {
	if got, want := (Apisix{}).ListenAddresses(), []string{":8080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListenAddresses() = %#v, want %#v", got, want)
	}
}

func TestLoadSupportsScalarNodeListen(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader("apisix:\n  node_listen: 9080\n")); err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}

	cfg, err := load(v)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if got, want := cfg.Apisix.ListenAddresses(), []string{"0.0.0.0:9080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListenAddresses() = %#v, want %#v", got, want)
	}
}
