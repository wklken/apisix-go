package plugin

import (
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestDescriptorForFactoryUsesManifestPhasePriorityScope(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := DescriptorForFactory(manifest, "request-id")
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := manifest.Plugin("request-id")
	if descriptor.Priority != entry.Priority || descriptor.Factory != "request-id" ||
		!slices.Equal(descriptor.Phases, []Phase{PhaseRewrite}) {
		t.Fatalf("descriptor = %#v, manifest = %#v", descriptor, entry)
	}
}

func TestDescriptorForFactoryRejectsFactoryOutsideEntry(t *testing.T) {
	manifest, err := capability.Parse([]byte(`
schema_version: 1
target:
  name: apisix-3.17
  version: 3.17.0
  source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
  image: apache/apisix:3.17.0
plugins:
  - name: request-id
    implementation: request-id
    namespace: apisix
    domains: [http]
    apisix_default: true
    factories:
      - key: alias-only
        import_path: example.invalid/request-id
        import_alias: request_id
        constructor: New
    phases: [rewrite]
    priority: 100
    scopes: [route]
    instance_scope: route
    behavior: full
    behavior_summary: test
    known_gaps: []
    evidence:
      schema: {state: not_applicable, refs: [], owner: test, reason: not exercised by descriptor unit fixture}
      unit: {state: not_applicable, refs: [], owner: test, reason: not exercised by descriptor unit fixture}
      converted_upstream: {state: not_applicable, refs: [], owner: test, reason: not exercised by descriptor unit fixture}
      differential: {state: not_applicable, refs: [], owner: test, reason: not exercised by descriptor unit fixture}
      real_dependency: {state: not_applicable, refs: [], owner: test, reason: not exercised by descriptor unit fixture}
      failure: {state: not_applicable, refs: [], owner: test, reason: not exercised by descriptor unit fixture}
      recovery: {state: not_applicable, refs: [], owner: test, reason: not exercised by descriptor unit fixture}
    divergence_ids: []
    supported_platforms: [linux-amd64]
qualification_profiles: []
divergences: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DescriptorForFactory(manifest, "request-id"); err == nil {
		t.Fatal("DescriptorForFactory() error = nil")
	}
}
