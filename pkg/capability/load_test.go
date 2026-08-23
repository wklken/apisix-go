package capability

import (
	"slices"
	"strings"
	"testing"
)

func TestParseRejectsUnknownFields(t *testing.T) {
	data := []byte(`schema_version: 1
target:
  name: apisix-3.17
  version: 3.17.0
  source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
plugins: []
qualification_profiles: []
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

func TestQualifiedPluginsDistinguishesUnknownProfile(t *testing.T) {
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	qualified := m.QualifiedPlugins("http-data-plane-v1")
	if qualified == nil || len(qualified) != 0 {
		t.Fatalf("QualifiedPlugins() = %#v, want non-nil empty slice", qualified)
	}
	if unknown := m.QualifiedPlugins("missing"); unknown != nil {
		t.Fatalf("QualifiedPlugins(missing) = %#v, want nil", unknown)
	}
}
