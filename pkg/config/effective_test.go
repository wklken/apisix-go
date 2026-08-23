package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestLoadEffectiveAppliesAllLayersAndProvenance(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, "proxy: {max_in_flight: 20}\n")
	req.Environment = map[string]string{"APISIXGO_PROXY_MAX_IN_FLIGHT": "30"}
	req.CLIOverrides = map[string]any{"proxy.max_in_flight": 40}

	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Config.Proxy.MaxInFlight != 40 {
		t.Fatalf("max_in_flight = %d, want 40", effective.Config.Proxy.MaxInFlight)
	}
	if got := effective.Provenance["proxy.max_in_flight"]; got != (FieldSource{
		Kind: SourceCLI, Origin: "proxy.max_in_flight", Explicit: true,
	}) {
		t.Fatalf("source = %+v", got)
	}
	if effective.Profiles != (ProfileSelection{
		Compatibility: CompatibilityAPISIX317,
		Security:      SecurityCompat,
	}) {
		t.Fatalf("profiles = %+v", effective.Profiles)
	}
}

func TestLoadEffectiveDistinguishesNullFalseZeroEmptyAndAbsent(t *testing.T) {
	effective := loadEffectiveFixture(t, `
debug: false
apisix: {id: ""}
graphql: null
`)
	for _, path := range []string{"debug", "apisix.id", "graphql"} {
		if _, ok := effective.Provenance[path]; !ok {
			t.Fatalf("%s has no provenance", path)
		}
	}
	if _, ok := effective.Provenance["apisix.enable_dev_mode"]; ok {
		t.Fatal("absent field became explicit")
	}
}

func TestLoadEffectivePreservesExactUntypedNumber(t *testing.T) {
	effective := loadEffectiveFixture(t, `plugin_attr: {prometheus: {large: 9007199254740993}}`)
	got := effective.Config.PluginAttr["prometheus"]["large"]
	if got != json.Number("9007199254740993") {
		t.Fatalf("large = %#v (%T)", got, got)
	}
}

func TestLoadEffectiveResolvesCompatRelativeRuntimePathAgainstOwningFile(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, `apisix_go: {runtime_paths: {data_dir: relative-data}}`)
	effective, err := LoadEffective(req)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(req.OverridePath), "relative-data")
	if effective.Paths.DataDir != want {
		t.Fatalf("data_dir = %q, want %q", effective.Paths.DataDir, want)
	}
}

func TestLoadEffectiveResolvesEnvironmentAndCLIPathsAgainstOverrideFile(t *testing.T) {
	for _, test := range []struct {
		name  string
		apply func(*LoadRequest)
		want  string
	}{
		{
			name: "APISIXGO",
			apply: func(req *LoadRequest) {
				req.Environment["APISIXGO_RUNTIME_PATHS_DATA_DIR"] = "environment-data"
			},
			want: "environment-data",
		},
		{
			name: "CLI",
			apply: func(req *LoadRequest) {
				req.CLIOverrides["apisix_go.runtime_paths.data_dir"] = "cli-data"
			},
			want: "cli-data",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := loadRequestFixture(t, SecurityCompat, "")
			test.apply(&req)
			effective, err := LoadEffective(req)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(filepath.Dir(req.OverridePath), test.want)
			if effective.Paths.DataDir != want {
				t.Fatalf("data_dir = %q, want %q", effective.Paths.DataDir, want)
			}
		})
	}
}

func TestLoadEffectiveUnknownFieldsFollowSecurityProfile(t *testing.T) {
	compat := loadRequestFixture(t, SecurityCompat, "unknown_section: {token: must-not-appear}\n")
	effective, err := LoadEffective(compat)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := effective.Provenance["unknown_section.token"]; !ok {
		t.Fatal("ignored field missing provenance")
	}

	const secretKey = "must-not-appear-secret-key"
	const secretValue = "must-not-appear-secret-value"
	strict := loadRequestFixture(t, SecurityStrict, "${{UNKNOWN_KEY}}: {token: "+secretValue+"}\n")
	strict.Environment["UNKNOWN_KEY"] = secretKey
	_, err = LoadEffective(strict)
	want := "security_profile strict: unknown static configuration field"
	if err == nil || err.Error() != want {
		t.Fatalf("strict LoadEffective() error = %v, want %q", err, want)
	}
	if strings.Contains(err.Error(), secretKey) || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("strict LoadEffective() error leaked an expanded key or value: %q", err)
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
			name:     "APISIXGO profile override",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				req.Environment["APISIXGO_SECURITY_PROFILE"] = secret
			},
		},
		{
			name:     "APISIXGO typed decode",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				req.Environment["APISIXGO_PROXY_MAX_IN_FLIGHT"] = secret
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
		{
			name:     "CLI plugin validation",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				req.CLIOverrides["plugins"] = []any{" " + secret}
			},
		},
		{
			name:     "qualified plugin membership",
			sentinel: secret,
			apply: func(req *LoadRequest) {
				manifest := qualifiedProfileTestManifest(t)
				qualification, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
				if !ok {
					t.Fatal("qualification profile is missing")
				}
				req.Manifest = manifest
				override := `
qualification_profile: http-data-plane-v1
apisix: {proxy_mode: http}
plugins: [` + strings.Join(qualification.RequiredPlugins, ",") + `]
deployment:
  role: data_plane
  role_data_plane: {config_provider: etcd}
  etcd: {host: [https://etcd.example:2379], prefix: /apisix}
`
				if err := writeTestConfig(req.OverridePath, override); err != nil {
					t.Fatal(err)
				}
				plugins := append([]string(nil), qualification.RequiredPlugins...)
				plugins[0] = secret
				req.CLIOverrides["plugins"] = plugins
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := loadRequestFixture(t, SecurityCompat, "")
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

func TestLoadEffectiveRejectsRemovedDeploymentProfile(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, "deployment: {profile: http-data-plane-v1}\n")
	_, err := LoadEffective(req)
	if err == nil || !strings.Contains(err.Error(), removedDeploymentProfileError) {
		t.Fatalf("LoadEffective() error = %v", err)
	}
}

func TestLoadEffectiveRequiresExplicitManifest(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, "")
	req.Manifest = nil
	_, err := LoadEffective(req)
	if err == nil || err.Error() != "load effective config: capability manifest is required" {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadEffectiveRequiresAbsoluteInputPaths(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, "")
	req.DefaultPath = "relative-default.yaml"
	if _, err := LoadEffective(req); err == nil || err.Error() !=
		"load effective config: default path must be a non-empty absolute path" {
		t.Fatalf("relative default error = %v", err)
	}
	req = loadRequestFixture(t, SecurityCompat, "")
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
	req := loadRequestFixture(t, SecurityCompat, "")
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

func TestLoadEffectiveQualificationRequiresFourAbsoluteRuntimePaths(t *testing.T) {
	manifest := qualifiedProfileTestManifest(t)
	qualification, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
	if !ok {
		t.Fatal("qualification profile is missing")
	}
	override := `
qualification_profile: http-data-plane-v1
apisix: {proxy_mode: http}
plugins: [` + strings.Join(qualification.RequiredPlugins, ",") + `]
deployment:
  role: data_plane
  role_data_plane: {config_provider: etcd}
  etcd: {host: [https://etcd.example:2379], prefix: /apisix}
apisix_go: {runtime_paths: {log_dir: ""}}
`
	req := loadRequestFixture(t, SecurityCompat, override)
	req.Manifest = manifest
	_, err := LoadEffective(req)
	want := "qualification_profile http-data-plane-v1: runtime path log_dir must be a non-empty absolute path"
	if err == nil || err.Error() != want {
		t.Fatalf("LoadEffective() error = %v, want %q", err, want)
	}
}

func loadEffectiveFixture(t *testing.T, override string) *EffectiveConfig {
	t.Helper()
	effective, err := LoadEffective(loadRequestFixture(t, SecurityCompat, override))
	if err != nil {
		t.Fatal(err)
	}
	return effective
}

func loadRequestFixture(t *testing.T, security SecurityProfile, override string) LoadRequest {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
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
	profileOverlay := "compatibility_target: apisix-3.17\nsecurity_profile: " + string(security) + "\n"
	if err := writeTestConfig(overridePath, profileOverlay+override); err != nil {
		t.Fatal(err)
	}
	return LoadRequest{
		DefaultPath: defaults, OverridePath: overridePath,
		DefaultPaths: RuntimePaths{
			DataDir: filepath.Join(root, "data"), RuntimeDir: filepath.Join(root, "run"),
			LogDir: filepath.Join(root, "log"), TempDir: filepath.Join(root, "tmp"),
		},
		Environment: map[string]string{}, CLIOverrides: map[string]any{}, Manifest: manifest,
	}
}

func writeTestConfig(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
