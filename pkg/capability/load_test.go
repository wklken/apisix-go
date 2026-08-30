package capability

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParseRejectsRemovedGovernanceFields(t *testing.T) {
	base := minimalManifestYAML()
	for _, field := range []string{
		"behavior: full",
		"behavior_summary: complete",
		"known_gaps: []",
		"evidence: {}",
		"divergence_ids: []",
		"supported_platforms: [linux-amd64]",
	} {
		t.Run(strings.SplitN(field, ":", 2)[0], func(t *testing.T) {
			candidate := bytes.Replace(
				base,
				[]byte("    priority: 1\n"),
				[]byte("    priority: 1\n    "+field+"\n"),
				1,
			)
			if _, err := Parse(candidate); err == nil || !strings.Contains(err.Error(), "field") {
				t.Fatalf("Parse() error = %v, want unknown-field rejection", err)
			}
		})
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	data := append(minimalManifestYAML(), []byte("surprise: true\n")...)
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "field surprise") {
		t.Fatalf("Parse() error = %v, want unknown-field error", err)
	}
}

func TestManifestQueriesReturnCopies(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	plugin, ok := manifest.Plugin("request-id")
	if !ok {
		t.Fatal("request-id missing")
	}
	plugin.Scopes[0] = "mutated"
	again, _ := manifest.Plugin("request-id")
	if slices.Contains(again.Scopes, "mutated") {
		t.Fatal("Plugin returned mutable manifest storage")
	}
}

func TestLoadedManifestPinsRuntimeAPISIX317Inventory(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Plugins) == 0 {
		t.Fatal("manifest has no runtime plugin entries")
	}
	if _, ok := manifest.Plugin("request-id"); !ok {
		t.Fatal("request-id factory is absent")
	}
	defaults := 0
	for _, plugin := range manifest.Plugins {
		if plugin.APISIXDefault {
			defaults++
		}
	}
	if defaults != 104 {
		t.Fatalf("APISIX default count = %d, want 104", defaults)
	}
	for _, name := range []string{
		"ext-plugin-pre-req",
		"ext-plugin-post-req",
		"ext-plugin-post-resp",
		"inspect",
		"ocsp-stapling",
	} {
		plugin, ok := manifest.Plugin(name)
		if !ok {
			t.Fatalf("%s is absent from the APISIX 3.17 inventory", name)
		}
		if len(plugin.Factories) != 0 {
			t.Fatalf("%s factories = %#v, want none for native/runtime-only capability", name, plugin.Factories)
		}
	}
}

func TestParseRejectsInvalidRuntimeManifestFixtures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		raw    func([]byte) []byte
		want   string
	}{
		{
			name: "second YAML document",
			raw: func(data []byte) []byte {
				return append(data, []byte("\n---\nschema_version: 1\n")...)
			},
			want: "multiple YAML documents",
		},
		{name: "nested unknown field", raw: addNestedTargetField, want: "field surprise"},
		{
			name: "schema version mismatch",
			mutate: func(manifest *Manifest) {
				manifest.SchemaVersion = 2
			},
			want: "schema_version",
		},
		{
			name: "target mismatch",
			mutate: func(manifest *Manifest) {
				manifest.Target.Version = "3.18.0"
			},
			want: "target must be",
		},
		{
			name: "duplicate plugin id",
			mutate: func(manifest *Manifest) {
				duplicate := manifest.Plugins[0]
				duplicate.Factories = nil
				manifest.Plugins = append(manifest.Plugins, duplicate)
			},
			want: "duplicate plugin",
		},
		{
			name: "duplicate factory id",
			mutate: func(manifest *Manifest) {
				duplicate := manifest.Plugins[0]
				duplicate.Name = "other"
				manifest.Plugins = append(manifest.Plugins, duplicate)
			},
			want: "duplicate factory",
		},
		{
			name: "canonical and factory collision",
			mutate: func(manifest *Manifest) {
				collision := manifest.Plugins[0]
				collision.Factories = append([]Factory(nil), collision.Factories...)
				collision.Name = "collision"
				collision.Factories[0].Key = "request-id-capability"
				manifest.Plugins = append(manifest.Plugins, collision)
			},
			want: "factory id \"request-id-capability\" collides with plugin id",
		},
		{
			name: "unknown namespace",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].Namespace = Namespace("unknown")
			},
			want: "unknown namespace",
		},
		{
			name: "unknown plugin domain",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].Domains = []Domain{"tcp"}
			},
			want: "unknown domain",
		},
		{
			name: "apisix plugin without domain",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].Domains = nil
			},
			want: "must declare a domain",
		},
		{
			name: "factory without import path",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].Factories[0].ImportPath = ""
			},
			want: "import_path",
		},
		{
			name: "factory without alias",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].Factories[0].ImportAlias = ""
			},
			want: "import_alias",
		},
		{
			name: "factory without constructor",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].Factories[0].Constructor = ""
			},
			want: "constructor",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			if test.mutate != nil {
				test.mutate(&manifest)
			}
			data := marshalManifest(t, manifest)
			if test.raw != nil {
				data = test.raw(data)
			}
			_, err := Parse(data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestManifestQueriesReturnDeepCopies(t *testing.T) {
	manifest := testManifest()
	copyPlugin := manifest.Plugins[0]
	copyPlugin.Factories = append([]Factory(nil), copyPlugin.Factories...)
	copyPlugin.Name = "copy-capability"
	copyPlugin.Factories[0].Key = "copy"
	copyPlugin.Phases = []string{"rewrite", "access"}
	copyPlugin.Scopes = []string{"route", "service"}
	copyPlugin.SecretDeclarations = []SecretDeclaration{{
		Factory: "copy",
		Source:  SecretPluginConfig,
		Field:   "token",
	}}
	manifest.Plugins = append(manifest.Plugins, copyPlugin)
	loaded := parseManifest(t, manifest)

	byName, ok := loaded.Plugin("copy-capability")
	if !ok {
		t.Fatal("canonical plugin name missing")
	}
	byFactory, ok := loaded.Plugin("copy")
	if !ok || byFactory.Name != byName.Name {
		t.Fatal("factory key did not resolve to canonical plugin")
	}
	original := yamlPluginSnapshot(t, byName)
	byName.Domains[0] = DomainStream
	byName.Factories[0].Key = "mutated"
	byName.Phases[0] = "mutated"
	byName.Scopes[0] = "mutated"
	byName.SecretDeclarations[0].Field = "mutated"
	again, _ := loaded.Plugin("copy-capability")
	if !reflect.DeepEqual(original, again) {
		t.Fatal("Plugin returned mutable manifest storage")
	}
}

func testManifest() Manifest {
	return Manifest{
		SchemaVersion: 1,
		Target: Target{
			Name:         expectedTargetName,
			Version:      expectedTargetVersion,
			SourceCommit: expectedSourceCommit,
			Image:        expectedTargetImage,
		},
		Plugins: []PluginCapability{{
			Name:           "request-id-capability",
			Implementation: "request_id",
			Namespace:      NamespaceAPISIX,
			Domains:        []Domain{DomainHTTP},
			APISIXDefault:  true,
			Factories: []Factory{{
				Key:         "request-id",
				ImportPath:  "example/plugin/request_id",
				ImportAlias: "request_id",
				Constructor: "Plugin",
			}},
			Phases:        []string{"rewrite"},
			Priority:      12015,
			Scopes:        []string{"route"},
			InstanceScope: "effective-config",
		}},
	}
}

func minimalManifestYAML() []byte {
	return []byte(`schema_version: 1
target:
  name: apisix-3.17
  version: 3.17.0
  source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
  image: apache/apisix:3.17.0
plugins:
  - name: example
    implementation: example
    namespace: apisix
    domains: [http]
    apisix_default: true
    factories:
      - key: example
        import_path: example/plugin
        import_alias: example
        constructor: Plugin
    phases: [rewrite]
    priority: 1
    scopes: [route]
    instance_scope: effective-config
`)
}

func parseManifest(t *testing.T, manifest Manifest) *Manifest {
	t.Helper()
	data := marshalManifest(t, manifest)
	loaded, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return loaded
}

func marshalManifest(t *testing.T, manifest Manifest) []byte {
	t.Helper()
	data, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	return data
}

func yamlPluginSnapshot(t *testing.T, plugin PluginCapability) PluginCapability {
	t.Helper()
	data, err := yaml.Marshal(plugin)
	if err != nil {
		t.Fatalf("yaml.Marshal(plugin) error = %v", err)
	}
	var snapshot PluginCapability
	if err := yaml.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("yaml.Unmarshal(plugin) error = %v", err)
	}
	return snapshot
}

func addNestedTargetField(data []byte) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) != "target:" {
			continue
		}
		indent := "  "
		for next := index + 1; next < len(lines); next++ {
			if strings.TrimSpace(lines[next]) == "" {
				continue
			}
			indent = lines[next][:len(lines[next])-len(strings.TrimLeft(lines[next], " \t"))]
			break
		}
		return []byte(
			strings.Join(lines[:index+1], "") +
				indent + "surprise: true\n" +
				strings.Join(lines[index+1:], ""),
		)
	}
	return data
}
