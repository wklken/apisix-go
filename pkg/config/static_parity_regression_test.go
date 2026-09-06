package config

import (
	"testing"
	"time"
)

func TestStaticConfigTraditionalYAMLProvider(t *testing.T) {
	effective, err := LoadEffective(
		loadRequestFixture(t, `deployment: {role: traditional, role_traditional: {config_provider: yaml}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := EffectiveConfigProvider(&effective.Config)
	if err != nil || provider != "yaml" {
		t.Fatalf("provider=%q err=%v", provider, err)
	}
}

func TestStaticConfigNumericTimeoutsUseSeconds(t *testing.T) {
	for _, value := range []string{"60", `"60"`, `"60s"`} {
		t.Run(value, func(t *testing.T) {
			effective, err := LoadEffective(
				loadRequestFixture(
					t,
					"nginx_config: {http: {client_body_timeout: "+value+", client_header_timeout: "+value+", keepalive_timeout: "+value+"}}",
				),
			)
			if err != nil {
				t.Fatal(err)
			}
			cfg := effective.Config.NginxConfig.HTTP
			if cfg.ClientBodyTimeout != 60*time.Second || cfg.ClientHeaderTimeout != 60*time.Second ||
				cfg.KeepaliveTimeout != 60*time.Second {
				t.Fatalf("timeouts=%+v", cfg)
			}
		})
	}
}

func TestStaticConfigAcceptsNGINXSendTimeout(t *testing.T) {
	effective, err := LoadEffective(loadRequestFixture(t, `nginx_config: {http: {send_timeout: 10s}}`))
	if err != nil {
		t.Fatal(err)
	}
	if effective.Config.NginxConfig.HTTP.SendTimeout != 10*time.Second {
		t.Fatal("send timeout not retained")
	}
}

func TestStaticConfigRejectsStringBooleansAndPluginLists(t *testing.T) {
	for _, override := range []string{`apisix: {enable_http2: "true"}`, `apisix: {enable_control: "false"}`, `plugins: "request-id,gzip"`, `stream_plugins: "mqtt-proxy"`} {
		t.Run(override, func(t *testing.T) {
			if _, err := LoadEffective(loadRequestFixture(t, override)); err == nil {
				t.Fatal("invalid type was accepted")
			}
		})
	}
}

func TestStaticConfigNullDeletesBeforeSchemaDefaults(t *testing.T) {
	req := loadRequestFixture(t, `apisix: {enable_http2: null}
plugins: null`)
	if err := writeTestConfig(req.DefaultPath, `apisix: {node_listen: [9080], enable_http2: false}
plugins: [request-id]
deployment: {role: data_plane, role_data_plane: {config_provider: yaml}}`); err != nil {
		t.Fatal(err)
	}
	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	if !effective.Config.Apisix.EnableHttp2 || len(effective.Config.Plugins) != 0 {
		t.Fatalf("http2=%v plugins=%v", effective.Config.Apisix.EnableHttp2, effective.Config.Plugins)
	}
}

func TestStaticConfigEnforcesOfficialSchemaConstraints(t *testing.T) {
	for _, override := range []string{
		`deployment: {role: traditional, role_traditional: {config_provider: etcd}, etcd: {host: [localhost:2379], prefix: /apisix}}`,
		`apisix: {data_encryption: {keyring: [123456789012345]}}`,
		`apisix: {data_encryption: {keyring: ["123456789012345"]}}`,
		`apisix: {data_encryption: {keyring: []}}`,
		`apisix: {trusted_addresses: []}`,
	} {
		t.Run(override, func(t *testing.T) {
			if _, err := LoadEffective(loadRequestFixture(t, override)); err == nil {
				t.Fatal("invalid schema value accepted")
			}
		})
	}
	for _, keyring := range []string{`"1234567890123456"`, `["1234567890123456"]`} {
		if _, err := LoadEffective(
			loadRequestFixture(t, `apisix: {data_encryption: {keyring: `+keyring+`}}`),
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStaticConfigYAMLMergeAndAliases(t *testing.T) {
	for _, override := range []string{
		`apisix: {<<: {enable_http2: false}}`,
		`defaults: &settings {enable_http2: false}
apisix: {<<: *settings}`,
		`defaults: &settings {enable_http2: true}
apisix: {enable_http2: false, <<: *settings}`,
		`apisix: {<<: [{enable_http2: false}, {enable_http2: true}]}`,
	} {
		t.Run(override, func(t *testing.T) {
			effective, err := LoadEffective(loadRequestFixture(t, override))
			if err != nil {
				t.Fatal(err)
			}
			if effective.Config.Apisix.EnableHttp2 {
				t.Fatal("YAML merge precedence lost")
			}
		})
	}
}
