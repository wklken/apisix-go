package capability

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParseRejectsUnknownFields(t *testing.T) {
	data := []byte(`schema_version: 1
target:
  name: apisix-3.17
  version: 3.17.0
  source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
plugins: []
divergences: []
surprise: true
`)
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "field surprise") {
		t.Fatalf("Parse() error = %v, want unknown-field error", err)
	}
}

func TestManifestQueriesReturnCopies(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	plugin, ok := m.Plugin("request-id")
	if !ok {
		t.Fatal("request-id missing")
	}
	plugin.Scopes[0] = "mutated"
	again, _ := m.Plugin("request-id")
	if slices.Contains(again.Scopes, "mutated") {
		t.Fatal("Plugin returned mutable manifest storage")
	}
}

func TestLoadedManifestPinsCompleteAPISIX317Inventory(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, plugin := range m.Plugins {
		if plugin.APISIXDefault {
			defaults++
		}
		if plugin.Behavior == BehaviorFull && len(plugin.KnownGaps) != 0 {
			t.Fatalf("full plugin %s has gaps %v", plugin.Name, plugin.KnownGaps)
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
		plugin, ok := m.Plugin(name)
		if !ok || plugin.Behavior != BehaviorNotApplicable {
			t.Fatalf("%s = %#v/%v, want not_applicable", name, plugin, ok)
		}
		if len(plugin.Factories) != 0 {
			t.Fatalf("%s factories = %#v, want none for native/runtime-only capability", name, plugin.Factories)
		}
	}
	ai, ok := m.Plugin("ai")
	if !ok || ai.Behavior != BehaviorNotApplicable {
		t.Fatalf("ai = %#v/%v, want not_applicable", ai, ok)
	}
}

func TestParseRejectsInvalidManifestFixtures(t *testing.T) {
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
		{
			name: "nested unknown field",
			raw:  addNestedTargetField,
			want: "field surprise",
		},
		{
			name: "schema version mismatch",
			mutate: func(m *Manifest) {
				m.SchemaVersion = 2
			},
			want: "schema_version",
		},
		{
			name: "target mismatch",
			mutate: func(m *Manifest) {
				m.Target.Version = "3.18.0"
			},
			want: "target must be",
		},
		{
			name: "duplicate plugin id",
			mutate: func(m *Manifest) {
				duplicate := m.Plugins[0]
				duplicate.Factories = nil
				m.Plugins = append(m.Plugins, duplicate)
			},
			want: "duplicate plugin",
		},
		{
			name: "duplicate factory id",
			mutate: func(m *Manifest) {
				duplicate := m.Plugins[0]
				duplicate.Name = "other"
				m.Plugins = append(m.Plugins, duplicate)
			},
			want: "duplicate factory",
		},
		{
			name: "canonical and factory collision",
			mutate: func(m *Manifest) {
				collision := m.Plugins[0]
				collision.Factories = append([]Factory(nil), collision.Factories...)
				collision.Name = "collision"
				collision.Factories[0].Key = "request-id-capability"
				m.Plugins = append(m.Plugins, collision)
			},
			want: "factory id \"request-id-capability\" collides with plugin id",
		},
		{
			name: "duplicate divergence id",
			mutate: func(m *Manifest) {
				m.Divergences = append(m.Divergences, m.Divergences[0])
			},
			want: "duplicate divergence",
		},
		{
			name: "unknown namespace",
			mutate: func(m *Manifest) {
				m.Plugins[0].Namespace = Namespace("unknown")
			},
			want: "unknown namespace",
		},
		{
			name: "unknown plugin domain",
			mutate: func(m *Manifest) {
				m.Plugins[0].Domains = []Domain{Domain("tcp")}
			},
			want: "unknown domain",
		},
		{
			name: "unknown behavior",
			mutate: func(m *Manifest) {
				m.Plugins[0].Behavior = BehaviorStatus("unknown")
			},
			want: "unknown behavior",
		},
		{
			name: "unknown evidence state",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema.State = EvidenceState("unknown")
			},
			want: "unknown state",
		},
		{
			name: "unknown divergence status",
			mutate: func(m *Manifest) {
				m.Divergences[0].Status = DivergenceStatus("unknown")
			},
			want: "unknown divergence status",
		},
		{
			name: "non-verified evidence without owner",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = EvidenceClaim{State: EvidenceMissing, Reason: "evidence is pending"}
			},
			want: "owner and reason",
		},
		{
			name: "non-verified evidence without reason",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = EvidenceClaim{State: EvidenceMissing, Owner: "fixture-owner"}
			},
			want: "owner and reason",
		},
		{
			name: "verified evidence without refs",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = EvidenceClaim{State: EvidenceVerified, Owner: "fixture-owner"}
			},
			want: "verified claim requires refs",
		},
		{
			name: "not applicable TBD reason",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("TBD")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable TODO reason",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("TODO")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable pending reason",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("pending")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable unknown reason",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("unknown")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable unknown status phrase",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("status unknown")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable TBD status phrase",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("this is TBD")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable N/A status phrase",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("N/A for now")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable digits only",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("123 456")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable status code",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("status 200")
			},
			want: "concrete applicability reason",
		},
		{
			name: "not applicable punctuation-only reason",
			mutate: func(m *Manifest) {
				m.Plugins[0].Evidence.Schema = notApplicableClaim("!!! --- ...")
			},
			want: "concrete applicability reason",
		},
		{
			name: "full behavior with gaps",
			mutate: func(m *Manifest) {
				m.Plugins[0].KnownGaps = []string{"fixture gap"}
			},
			want: "full behavior must not declare known gaps",
		},
		{
			name: "partial behavior without gaps",
			mutate: func(m *Manifest) {
				m.Plugins[0].Behavior = BehaviorPartial
			},
			want: "partial behavior requires known gaps",
		},
		{
			name: "deferred behavior without gaps",
			mutate: func(m *Manifest) {
				m.Plugins[0].Behavior = BehaviorDeferred
			},
			want: "deferred behavior requires known gaps",
		},
		{
			name: "apisix plugin without domain",
			mutate: func(m *Manifest) {
				m.Plugins[0].Domains = nil
			},
			want: "must declare a domain",
		},
		{
			name: "factory without import path",
			mutate: func(m *Manifest) {
				m.Plugins[0].Factories[0].ImportPath = ""
			},
			want: "import_path",
		},
		{
			name: "factory without alias",
			mutate: func(m *Manifest) {
				m.Plugins[0].Factories[0].ImportAlias = ""
			},
			want: "import_alias",
		},
		{
			name: "factory without constructor",
			mutate: func(m *Manifest) {
				m.Plugins[0].Factories[0].Constructor = ""
			},
			want: "constructor",
		},
		{
			name: "missing divergence reference",
			mutate: func(m *Manifest) {
				m.Plugins[0].DivergenceIDs = []string{"missing-divergence"}
			},
			want: "absent from the top-level ledger",
		},
		{
			name: "accepted divergence without ADR",
			mutate: func(m *Manifest) {
				m.Divergences[0] = Divergence{
					ID:               "divergence-1",
					Status:           DivergenceAccepted,
					OwnerApprovalRef: "owner/approval",
				}
			},
			want: "requires adr and owner_approval_ref",
		},
		{
			name: "accepted divergence without owner approval",
			mutate: func(m *Manifest) {
				m.Divergences[0] = Divergence{
					ID:     "divergence-1",
					Status: DivergenceAccepted,
					ADR:    "adr/0001",
				}
			},
			want: "requires adr and owner_approval_ref",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := testManifest()
			if tt.mutate != nil {
				tt.mutate(&manifest)
			}
			data := marshalManifest(t, manifest)
			if tt.raw != nil {
				data = tt.raw(data)
			}
			_, err := Parse(data)
			if err == nil {
				t.Fatal("Parse() unexpectedly succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want substring %q", err, tt.want)
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
	copyPlugin.Behavior = BehaviorPartial
	copyPlugin.KnownGaps = []string{"copy capability has a known gap"}
	copyPlugin.Phases = []string{"rewrite", "access"}
	copyPlugin.Scopes = []string{"route", "service"}
	copyPlugin.DivergenceIDs = []string{"divergence-1"}
	copyPlugin.SupportedPlatforms = []string{"linux-amd64", "darwin-arm64"}
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
	byName.KnownGaps[0] = "mutated"
	byName.Evidence.Schema.Refs[0] = "mutated"
	byName.Evidence.Unit.Refs[0] = "mutated"
	byName.Evidence.Upstream.Refs[0] = "mutated"
	byName.Evidence.Differential.Refs[0] = "mutated"
	byName.Evidence.RealDependency.Refs[0] = "mutated"
	byName.Evidence.Failure.Refs[0] = "mutated"
	byName.DivergenceIDs[0] = "mutated"
	byName.SupportedPlatforms[0] = "mutated"
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
			Phases:          []string{"rewrite"},
			Priority:        12015,
			Scopes:          []string{"route"},
			InstanceScope:   "effective-config",
			Behavior:        BehaviorFull,
			BehaviorSummary: "Adds a request identifier.",
			Evidence:        testEvidence(),
			DivergenceIDs:   []string{},
			SupportedPlatforms: []string{
				"linux-amd64",
				"darwin-arm64",
			},
		}},
		Divergences: []Divergence{{
			ID:     "divergence-1",
			Status: DivergenceProposed,
		}},
	}
}

func testEvidence() Evidence {
	claim := func(ref string) EvidenceClaim {
		return EvidenceClaim{State: EvidenceVerified, Refs: []string{ref}, Owner: "fixture-owner"}
	}
	return Evidence{
		Schema:         claim("fixture/schema"),
		Unit:           claim("fixture/unit"),
		Upstream:       claim("fixture/upstream"),
		Differential:   claim("fixture/differential"),
		RealDependency: claim("fixture/real-dependency"),
		Failure:        claim("fixture/failure"),
	}
}

func notApplicableClaim(reason string) EvidenceClaim {
	return EvidenceClaim{State: EvidenceNotApplicable, Owner: "fixture-owner", Reason: reason}
}

func parseManifest(t *testing.T, manifest Manifest) *Manifest {
	t.Helper()
	return parseManifestYAML(t, marshalManifest(t, manifest))
}

func marshalManifest(t *testing.T, manifest Manifest) []byte {
	t.Helper()
	data, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	return data
}

func parseManifestYAML(t *testing.T, data []byte) *Manifest {
	t.Helper()
	manifest, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return manifest
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
