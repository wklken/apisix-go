package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadEffectiveUsesFileLayersAndIgnoresAPISIXGOOverlays(t *testing.T) {
	req := loadRequestFixture(t, "proxy: {max_in_flight: 20}\n")
	req.Environment = map[string]string{"APISIXGO_PROXY_MAX_IN_FLIGHT": "30"}

	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Config.Proxy.MaxInFlight != 20 {
		t.Fatalf("max_in_flight = %d, want file value 20", effective.Config.Proxy.MaxInFlight)
	}
}

func TestLoadEffectiveAppliesOfficialEtcdHostEnvironmentOverride(t *testing.T) {
	req := loadRequestFixture(t, "deployment: {etcd: {host: ['http://from-file:2379']}}\n")
	req.Environment["APISIX_DEPLOYMENT_ETCD_HOST"] = `["https://from-env:2379"]`

	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://from-env:2379"}
	if !reflect.DeepEqual(effective.Config.Deployment.Etcd.Host, want) {
		t.Fatalf("etcd hosts = %#v, want %#v", effective.Config.Deployment.Etcd.Host, want)
	}
}

func TestLoadEffectiveIgnoresInvalidOfficialEtcdHostEnvironmentOverride(t *testing.T) {
	req := loadRequestFixture(t, "deployment: {etcd: {host: ['http://from-file:2379']}}\n")
	req.Environment["APISIX_DEPLOYMENT_ETCD_HOST"] = "not-json"

	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://from-file:2379"}
	if !reflect.DeepEqual(effective.Config.Deployment.Etcd.Host, want) {
		t.Fatalf("etcd hosts = %#v, want unchanged file value %#v", effective.Config.Deployment.Etcd.Host, want)
	}
}

func TestLoadEffectiveIgnoresNullOfficialEtcdHostEnvironmentOverride(t *testing.T) {
	req := loadRequestFixture(t, "deployment: {etcd: {host: ['http://from-file:2379']}}\n")
	req.Environment["APISIX_DEPLOYMENT_ETCD_HOST"] = "null"

	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://from-file:2379"}
	if !reflect.DeepEqual(effective.Config.Deployment.Etcd.Host, want) {
		t.Fatalf("etcd hosts = %#v, want unchanged file value %#v", effective.Config.Deployment.Etcd.Host, want)
	}
}

func TestLoadEffectiveAppliesEmptyOfficialEtcdHostEnvironmentOverride(t *testing.T) {
	req := loadRequestFixture(t, "deployment: {etcd: {host: ['http://from-file:2379']}}\n")
	req.Environment["APISIX_DEPLOYMENT_ETCD_HOST"] = "[]"

	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Config.Deployment.Etcd.Host == nil || len(effective.Config.Deployment.Etcd.Host) != 0 {
		t.Fatalf("etcd hosts = %#v, want applied empty array", effective.Config.Deployment.Etcd.Host)
	}
}

func TestLoadEffectiveUsesOfficialHTTPAccessLogDefault(t *testing.T) {
	effective := loadEffectiveFixture(t, "")
	if !effective.Config.NginxConfig.HTTP.EnableAccessLog {
		t.Fatal("nginx_config.http.enable_access_log = false, want APISIX 3.17 default true")
	}
}

func TestLoadEffectivePreservesExactUntypedNumber(t *testing.T) {
	effective := loadEffectiveFixture(t, `plugin_attr: {prometheus: {large: 9007199254740993}}`)
	got := effective.Config.PluginAttr["prometheus"]["large"]
	if got != json.Number("9007199254740993") {
		t.Fatalf("large = %#v (%T)", got, got)
	}
}

func TestLoadEffectiveResolvesRelativeRuntimePathAgainstOwningFile(t *testing.T) {
	req := loadRequestFixture(t, `apisix_go: {runtime_paths: {data_dir: relative-data}}`)
	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(req.OverridePath), "relative-data")
	if effective.Paths.DataDir != want {
		t.Fatalf("data_dir = %q, want %q", effective.Paths.DataDir, want)
	}
}

func TestLoadEffectiveRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name     string
		override string
		want     string
	}{
		{
			name:     "unknown root field",
			override: "unknown_section: {token: must-not-appear}\n",
			want:     "static configuration contains 1 unsupported field",
		},
		{
			name:     "unknown nested fields",
			override: "apisix: {node_listen: [{port: 9080, first_unknown: a, second_unknown: b}]}\n",
			want:     "static configuration contains 2 unsupported fields",
		},
		{
			name:     "unknown runtime extension field",
			override: "apisix_go: {runtime_paths: {unknown_path: must-not-appear}}\n",
			want:     "static configuration contains 1 unsupported field",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadEffective(loadRequestFixture(t, test.override))
			if err == nil || err.Error() != test.want {
				t.Fatalf("LoadEffective() error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "must-not-appear") || strings.Contains(err.Error(), "unknown") {
				t.Fatalf("LoadEffective() error exposed an unsupported path or value: %q", err)
			}
		})
	}
}

func TestLoadEffectiveValidationErrorsDoNotExposeExpandedOrOverrideValues(t *testing.T) {
	const secret = "must-not-appear-runtime-secret"
	tests := []struct {
		name     string
		sentinel string
		apply    func(*LoadRequest)
	}{
		{
			name:     "APISIX environment expansion",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				if err := writeTestConfig(req.OverridePath, "deployment: {role: '${{ROLE}}'}\n"); err != nil {
					t.Fatal(err)
				}
				req.Environment["ROLE"] = secret
			},
		},
		{
			name:     "APISIX structural decode",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				if err := writeTestConfig(
					req.OverridePath,
					"deployment: {etcd: {password: {nested: '${{PASSWORD}}'}}}\n",
				); err != nil {
					t.Fatal(err)
				}
				req.Environment["PASSWORD"] = secret
			},
		},
		{
			name:     "APISIX numeric overflow",
			sentinel: "999999999999999999999999999",
			apply: func(req *LoadRequest) {
				if err := writeTestConfig(req.OverridePath, "proxy: {max_in_flight: '${{LIMIT}}'}\n"); err != nil {
					t.Fatal(err)
				}
				req.Environment["LIMIT"] = "999999999999999999999999999"
			},
		},
		{
			name:     "APISIX duration decode",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				if err := writeTestConfig(
					req.OverridePath,
					"nginx_config: {http: {client_body_timeout: '${{TIMEOUT}}'}}\n",
				); err != nil {
					t.Fatal(err)
				}
				req.Environment["TIMEOUT"] = secret
			},
		},
		{
			name:     "APISIX listener port decode",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				if err := writeTestConfig(req.OverridePath, "apisix: {node_listen: ':${{PORT}}'}\n"); err != nil {
					t.Fatal(err)
				}
				req.Environment["PORT"] = secret
			},
		},
		{
			name:     "APISIX listener numeric overflow",
			sentinel: "999999999999999999999999999",
			apply: func(req *LoadRequest) {
				if err := writeTestConfig(req.OverridePath, "apisix: {node_listen: '${{PORT}}'}\n"); err != nil {
					t.Fatal(err)
				}
				req.Environment["PORT"] = "999999999999999999999999999"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := loadRequestFixture(t, "")
			test.apply(&req)
			_, err := LoadEffective(req)
			if err == nil {
				t.Fatal("LoadEffective() error = nil")
			}
			if strings.Contains(err.Error(), test.sentinel) {
				t.Fatalf("LoadEffective() error leaked an expanded or override value: %q", err)
			}
		})
	}
}

func TestLoadEffectiveRequiresAbsoluteInputPaths(t *testing.T) {
	req := loadRequestFixture(t, "")
	req.DefaultPath = "relative-default.yaml"
	if _, err := LoadEffective(req); err == nil || err.Error() !=
		"load effective config: default path must be a non-empty absolute path" {
		t.Fatalf("relative default error = %v", err)
	}
	req = loadRequestFixture(t, "")
	req.OverridePath = "relative-override.yaml"
	if _, err := LoadEffective(req); err == nil || err.Error() !=
		"load effective config: override path must be absolute" {
		t.Fatalf("relative override error = %v", err)
	}
}

func TestStaticSecretTagsCoverStaticCredentials(t *testing.T) {
	assertSecretTag := func(owner reflect.Type, fieldName, want string) {
		t.Helper()
		field, ok := owner.FieldByName(fieldName)
		if !ok {
			t.Fatalf("%s.%s is missing", owner, fieldName)
		}
		if got := field.Tag.Get("secret"); got != want {
			t.Fatalf("%s.%s secret tag = %q, want %q", owner, fieldName, got, want)
		}
	}
	assertSecretTag(reflect.TypeFor[DataEncryption](), "Keyring", "true")
	assertSecretTag(reflect.TypeFor[AdminKey](), "Key", "true")
	assertSecretTag(reflect.TypeFor[Etcd](), "Password", "true")
	assertSecretTag(reflect.TypeFor[Config](), "PluginAttr", "container")
}

func TestLoadEffectiveTreatsEmptyDocumentAsNoopAndNullAsReplacement(t *testing.T) {
	req := loadRequestFixture(t, "")
	if err := writeTestConfig(req.OverridePath, "# comment only\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEffective(req); err != nil {
		t.Fatalf("comment-only override error = %v", err)
	}
	if err := writeTestConfig(req.OverridePath, "null\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEffective(req); err == nil || !strings.Contains(err.Error(), "root must be a mapping") {
		t.Fatalf("explicit null error = %v", err)
	}
}

func loadEffectiveFixture(t *testing.T, override string) *EffectiveConfig {
	t.Helper()
	effective, err := LoadEffective(loadRequestFixture(t, override))
	if err != nil {
		t.Fatal(err)
	}
	return effective
}

func loadRequestFixture(t *testing.T, override string) LoadRequest {
	t.Helper()
	root := t.TempDir()
	defaults := filepath.Join(root, "default.yaml")
	if err := writeTestConfig(defaults, `
apisix: {node_listen: [{port: 9080}]}
proxy: {max_idle_conns: 10, max_idle_conns_per_host: 10, max_conns_per_host: 10, max_in_flight: 10}
nginx_config: {http: {client_max_body_size: 1024, client_body_timeout: 60s}}
plugins: [request-id]
deployment: {role: data_plane, role_data_plane: {config_provider: yaml}}
`); err != nil {
		t.Fatal(err)
	}
	overridePath := filepath.Join(root, "override.yaml")
	if err := writeTestConfig(overridePath, override); err != nil {
		t.Fatal(err)
	}
	return LoadRequest{
		DefaultPath: defaults, OverridePath: overridePath,
		DefaultPaths: RuntimePaths{
			DataDir: filepath.Join(root, "data"), RuntimeDir: filepath.Join(root, "run"),
			LogDir: filepath.Join(root, "log"), TempDir: filepath.Join(root, "tmp"),
		},
		Environment: map[string]string{},
	}
}

func writeTestConfig(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
