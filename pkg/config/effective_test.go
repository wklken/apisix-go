package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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

func TestRenderEffectiveRedactedNil(t *testing.T) {
	_, err := RenderEffectiveRedacted(nil)
	if err == nil || err.Error() != "render effective config: config is required" {
		t.Fatalf("RenderEffectiveRedacted() error = %v", err)
	}
}

func TestRenderEffectiveRedactedDeterministicSchemaAndSecrets(t *testing.T) {
	effective := &EffectiveConfig{
		Config: Config{
			GraphQL: GraphQL{MaxSize: 7},
			Apisix:  Apisix{DataEncryption: DataEncryption{Keyring: []string{"encryption-secret"}}},
			Deployment: Deployment{
				Admin: Admin{AdminKey: []AdminKey{{Name: "admin", Key: "admin-secret", Role: "admin"}}},
				Etcd: Etcd{
					Host:     []string{"https://etcd-user:etcd-password@etcd.example:2379/path?token=url-secret"},
					Password: "etcd-secret",
				},
			},
			PluginAttr: map[string]map[string]any{
				"prometheus": {"token": "plugin-attr-secret"},
			},
			Discovery: Discovery{
				"etcd": map[string]any{"password": "discovery-provider-secret"},
			},
			NginxConfig: NginxConfig{HTTP: NginxHTTP{
				ClientMaxBodySize: 9007199254740993,
				ClientBodyTimeout: 3 * time.Second,
			}},
		},
		Paths: RuntimePaths{
			DataDir:    "/var/lib/apisix-go",
			RuntimeDir: "/run/apisix-go",
			LogDir:     "/var/log/apisix-go",
			TempDir:    "/var/tmp/apisix-go",
		},
		Profiles: ProfileSelection{
			Compatibility: CompatibilityAPISIX317,
			Security:      SecurityStrict,
			Qualification: QualificationHTTPDataPlaneV1,
		},
		Provenance: Provenance{
			"graphql.max_size": {
				Kind: SourceCLI, Origin: "graphql.max_size", Explicit: true,
			},
			"plugin_attr.prometheus.token": {
				Kind: SourceDefaultFile, Origin: "/etc/apisix/default.yaml", Explicit: true,
			},
			"deployment.etcd.password": {
				Kind: SourceOverrideFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
			"deployment.etcd.host[0]": {
				Kind: SourceOverrideFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
			"deployment.admin.admin_key[0].key": {
				Kind: SourceOverrideFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
		},
	}

	data, err := RenderEffectiveRedacted(effective)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "{\n  \"config\": ") {
		t.Fatalf("top-level output does not start with config: %s", text)
	}
	for _, secret := range []string{
		"encryption-secret", "admin-secret", "etcd-password", "etcd-secret",
		"url-secret", "plugin-attr-secret", "discovery-provider-secret",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("redacted output leaked %q: %s", secret, text)
		}
	}
	for _, want := range []string{
		`"config"`, `"paths"`, `"profiles"`, `"provenance"`, `"ignored_fields"`,
		`"client_max_body_size": 9007199254740993`, `"client_body_timeout": 3000000000`,
		`"key": "[REDACTED]"`, `"password": "[REDACTED]"`,
		`"prometheus": "[REDACTED]"`, `"etcd": "[REDACTED]"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("redacted output missing %q: %s", want, text)
		}
	}
	var decoded struct {
		Config        map[string]any   `json:"config"`
		Provenance    []map[string]any `json:"provenance"`
		IgnoredFields []string         `json:"ignored_fields"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Config == nil || decoded.Provenance == nil || decoded.IgnoredFields == nil {
		t.Fatalf("empty output collections must be JSON arrays/maps: %#v", decoded)
	}
}

func TestRenderEffectiveRedactedMasksDynamicAndUnknownPaths(t *testing.T) {
	const dynamicKey = "must-not-appear-dynamic-key"
	const unknownKey = "must-not-appear-unknown-key"
	effective := &EffectiveConfig{
		Config: Config{
			PluginAttr: map[string]map[string]any{
				dynamicKey: {"token": "dynamic-secret"},
				"tenant.a": {"token": "unsafe-plugin-secret"},
			},
		},
		Provenance: Provenance{
			"plugin_attr." + dynamicKey: {
				Kind: SourceAPISIXEnv, Origin: "PLUGIN_NAME", Explicit: true,
			},
			`plugin_attr["tenant.a"].token`: {
				Kind: SourceDefaultFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
			"unknown_section." + unknownKey: {
				Kind: SourceOverrideFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
		},
	}
	data, err := RenderEffectiveRedacted(effective)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, secret := range []string{dynamicKey, unknownKey, "dynamic-secret", "unsafe-plugin-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("redacted output leaked %q: %s", secret, text)
		}
	}
	for _, marker := range []string{"apisix_env:opaque:", "plugin:opaque:", "unknown:opaque:", "redacted:opaque:"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("redacted output missing opaque marker %q: %s", marker, text)
		}
	}
	if strings.Index(text, `"config"`) > strings.Index(text, `"paths"`) {
		t.Fatal("config must precede paths")
	}
}

func TestRenderEffectiveRedactedFailsClosedForInvalidDynamicKey(t *testing.T) {
	invalidKey := string([]byte{0xff, 0xfe})
	effective := &EffectiveConfig{Config: Config{
		PluginAttr: map[string]map[string]any{invalidKey: {"token": "invalid-key-secret"}},
	}}
	data, err := RenderEffectiveRedacted(effective)
	if err != nil {
		t.Fatalf("RenderEffectiveRedacted() error = %v", err)
	}
	text := string(data)
	if strings.Contains(text, "invalid-key-secret") || strings.Contains(text, invalidKey) {
		t.Fatalf("redacted output leaked an invalid dynamic key or value: %q", text)
	}
	if !strings.Contains(text, `opaque:`) {
		t.Fatalf("redacted output missing opaque invalid-key marker: %s", text)
	}
}

func TestRenderEffectiveRedactedSecretRegistry(t *testing.T) {
	if err := validateSecretInventory(reflect.TypeFor[Config]()); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalProvenancePathTokenizer(t *testing.T) {
	valid := []string{
		"apisix",
		"apisix.node_listen",
		"apisix.node_listen[0].port",
		`plugin_attr["tenant.\\\"prod\\\\blue"].token`,
		`["unsafe.root"].token`,
	}
	for _, path := range valid {
		if _, err := parseCanonicalPath(path); err != nil {
			t.Errorf("parseCanonicalPath(%q) error = %v", path, err)
		}
	}
	invalid := []string{
		`plugin_attr["safe"].token`,
		"apisix.node_listen[00].port",
		"apisix.node_listen[-1].port",
		"apisix.node_listen[18446744073709551616].port",
		`plugin_attr["bad\q"].token`,
		`plugin_attr["tenant"]trailing`,
		"apisix.",
	}
	for _, path := range invalid {
		if _, err := parseCanonicalPath(path); err == nil {
			t.Errorf("parseCanonicalPath(%q) unexpectedly succeeded", path)
		}
	}
}

func TestCanonicalProvenanceSchemaMatcher(t *testing.T) {
	known := []string{
		"apisix_go",
		"apisix_go.runtime_paths",
		"apisix_go.runtime_paths.data_dir",
		"apisix.node_listen[0]",
		"apisix.node_listen[0].port",
		`plugin_attr["tenant.a"].token`,
		`discovery["provider.name"].token`,
	}
	for _, path := range known {
		tokens, err := parseCanonicalPath(path)
		if err != nil || !knownConfigTokens(tokens) {
			t.Errorf("knownConfigTokens(%q) = false, parse error = %v", path, err)
		}
	}
	unknown := []string{
		"apisix_go.runtime_paths.unknown",
		"apisix.node_listen[0].unknown",
		"plugin_attr[0]",
	}
	for _, path := range unknown {
		tokens, err := parseCanonicalPath(path)
		if err != nil {
			t.Errorf("parseCanonicalPath(%q) error = %v", path, err)
			continue
		}
		if knownConfigTokens(tokens) {
			t.Errorf("knownConfigTokens(%q) = true, want false", path)
		}
	}
}

func TestRenderEffectiveRedactedOpaqueCorrelationAndOrdering(t *testing.T) {
	const dynamicKey = "dynamic-plugin"
	const unsafeKey = "tenant.a"
	const unknownPath = `unknown_section["unknown.key"]`
	effective := &EffectiveConfig{
		Config: Config{
			GraphQL: GraphQL{MaxSize: 1},
			PluginAttr: map[string]map[string]any{
				dynamicKey: {"token": "dynamic-secret"},
				unsafeKey:  {"token": "unsafe-secret"},
			},
		},
		Provenance: Provenance{
			"graphql.max_size": {
				Kind: SourceAPISIXEnv, Origin: "GRAPHQL_MAX_SIZE", Explicit: true,
			},
			"plugin_attr." + dynamicKey: {
				Kind: SourceAPISIXEnv, Origin: "PLUGIN_NAME", Explicit: true,
			},
			`plugin_attr["tenant.a"]`: {
				Kind: SourceDefaultFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
			`plugin_attr["tenant.a"].token`: {
				Kind: SourceDefaultFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
			unknownPath: {
				Kind: SourceAPISIXEnv, Origin: "UNKNOWN_NAME", Explicit: true,
			},
			"apisix_go.runtime_paths.data_dir": {
				Kind: SourceDefaultFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
			"apisix.node_listen[0].port": {
				Kind: SourceDefaultFile, Origin: "/etc/apisix/config.yaml", Explicit: true,
			},
		},
	}

	first, err := RenderEffectiveRedacted(effective)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderEffectiveRedacted(effective)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("repeated rendering is not deterministic")
	}
	text := string(first)
	for _, secret := range []string{dynamicKey, unsafeKey, "dynamic-secret", "unsafe-secret", `"token"`, unknownPath} {
		if strings.Contains(text, secret) {
			t.Fatalf("redacted output leaked %q: %s", secret, text)
		}
	}
	var dump struct {
		Config struct {
			PluginAttr map[string]any `json:"plugin_attr"`
		} `json:"config"`
		Provenance    []provenanceEntry `json:"provenance"`
		IgnoredFields []string          `json:"ignored_fields"`
	}
	if err := json.Unmarshal(first, &dump); err != nil {
		t.Fatal(err)
	}
	dynamicDisplay := ""
	unsafeDisplay := ""
	for key := range dump.Config.PluginAttr {
		if strings.HasPrefix(key, "apisix_env:opaque:") {
			dynamicDisplay = key
		}
		if strings.HasPrefix(key, "plugin:opaque:") {
			unsafeDisplay = key
		}
	}
	if dynamicDisplay == "" || unsafeDisplay == "" {
		t.Fatalf("config dynamic keys were not safely correlated: %#v", dump.Config.PluginAttr)
	}
	findPath := func(pathPrefix string) (provenanceEntry, bool) {
		for _, entry := range dump.Provenance {
			if strings.HasPrefix(entry.Path, pathPrefix) {
				return entry, true
			}
		}
		return provenanceEntry{}, false
	}
	findExactPath := func(path string) (provenanceEntry, bool) {
		for _, entry := range dump.Provenance {
			if entry.Path == path {
				return entry, true
			}
		}
		return provenanceEntry{}, false
	}
	if entry, ok := findExactPath(dynamicDisplay); !ok {
		t.Fatalf("dynamic config/provenance opaque IDs differ: config=%q provenance=%+v", dynamicDisplay, entry)
	}
	if entry, ok := findPath("plugin_attr." + unsafeDisplay + ".redacted:"); !ok {
		t.Fatalf("secret descendant was not redacted: %#v", dump.Provenance)
	} else if !strings.HasPrefix(entry.Path, "plugin_attr."+unsafeDisplay+".redacted:opaque:") {
		t.Fatalf("unexpected secret descendant display path: %q", entry.Path)
	}
	unknownDisplay := "unknown:opaque:"
	if len(dump.IgnoredFields) != 1 || !strings.HasPrefix(dump.IgnoredFields[0], unknownDisplay) {
		t.Fatalf("unknown path was not opaque/deduplicated: %#v", dump.IgnoredFields)
	}
	if entry, ok := findPath(unknownDisplay); !ok || entry.Path != dump.IgnoredFields[0] {
		t.Fatalf("unknown provenance/ignored IDs differ: %#v / %#v", dump.Provenance, dump.IgnoredFields)
	}
	for _, path := range []string{"apisix_env:opaque:", "plugin_attr.", "apisix_go.runtime_paths.data_dir", "apisix.node_listen[0].port"} {
		if _, ok := findPath(path); !ok {
			t.Fatalf("provenance entry %q is missing: %#v", path, dump.Provenance)
		}
	}
	if !sort.SliceIsSorted(dump.Provenance, func(left, right int) bool {
		return dump.Provenance[left].Path < dump.Provenance[right].Path
	}) {
		t.Fatalf("provenance is not sorted: %#v", dump.Provenance)
	}
	if !sort.StringsAreSorted(dump.IgnoredFields) {
		t.Fatalf("ignored fields are not sorted: %#v", dump.IgnoredFields)
	}
}

func TestRenderEffectiveRedactedPreservesMultipleAPISIXEnvironmentOrigins(t *testing.T) {
	effective := &EffectiveConfig{
		Config: Config{GraphQL: GraphQL{MaxSize: 1}},
		Provenance: Provenance{
			"graphql.max_size": {
				Kind: SourceAPISIXEnv, Origin: "KEY_A,KEY_B", Explicit: true,
			},
		},
	}

	data, err := RenderEffectiveRedacted(effective)
	if err != nil {
		t.Fatal(err)
	}
	var dump struct {
		Provenance []provenanceEntry `json:"provenance"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		t.Fatal(err)
	}
	if len(dump.Provenance) != 1 || dump.Provenance[0].Origin != "KEY_A,KEY_B" {
		t.Fatalf("APISIX environment origin = %#v, want KEY_A,KEY_B", dump.Provenance)
	}
}

func TestSanitizeEtcdEndpoint(t *testing.T) {
	for _, test := range []struct {
		name, endpoint, want string
	}{
		{name: "plain", endpoint: "https://etcd.example:2379", want: "https://etcd.example:2379"},
		{name: "userinfo", endpoint: "https://user:password@etcd.example:2379", want: "https://[REDACTED]@etcd.example:2379"},
		{name: "userinfo-root", endpoint: "https://user:password@etcd.example:2379/", want: "https://[REDACTED]@etcd.example:2379"},
		{name: "root", endpoint: "https://etcd.example:2379/", want: "https://etcd.example:2379/"},
		{name: "empty-query", endpoint: "https://etcd.example:2379?", want: "https://etcd.example:2379?"},
		{name: "path", endpoint: "https://etcd.example:2379/v3?token=secret#fragment", want: "https://etcd.example:2379/<redacted>"},
		{name: "invalid", endpoint: "not a URL must-not-appear", want: "[REDACTED]"},
		{name: "empty", endpoint: "", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeEtcdEndpoint(test.endpoint); got != test.want {
				t.Fatalf("sanitizeEtcdEndpoint() = %q, want %q", got, test.want)
			}
		})
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
