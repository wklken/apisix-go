# Governance, Capability Manifest, and Profiles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Establish one machine-readable source of truth for APISIX 3.17 capabilities, orthogonal profiles, behavior status, evidence maturity, generated status documentation, and owner-approved divergences.

**Architecture:** A strict embedded manifest in `pkg/capability` owns compatibility facts without importing runtime or configuration packages. `pkg/config` owns the three profile axes and validates qualification selections through stable manifest queries; generated plugin registration and documentation eliminate the current manual registries and Markdown-as-database behavior. A required governance workflow rejects manifest drift, unaccounted upstream corpus blocks, undocumented gaps, and active divergences without an accepted owner-approved ADR.

**Tech Stack:** Go 1.26, `go:embed`, `go.yaml.in/yaml/v3`, Cobra-independent generation command, Markdown generation, shell workflow contract tests, GitHub Actions, CODEOWNERS.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- Preserve the APISIX namespace; version Go-native extensions separately.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test` for this plan.
- Run `source .envrc && make build` after Go production code changes.
- Do not add a dependency; use the repository's existing `go.yaml.in/yaml/v3` module.
- Do not retain temporary legacy adapters or proxy-only facades; each registry/profile cutover deletes the replaced path in the same task.
- Keep the four existing untracked files under `docs/reviews/` outside every implementation commit.
- Keep current historical findings and evidence. Mark superseded claims and archive generated-input documents; never erase their prior text.
- Behavior status and evidence maturity are independent. `Full` behavior never implies qualification.
- New non-compatibility extensions remain frozen until the APISIX 3.17 convergence gates pass.

---

## File and Responsibility Map

- `pkg/capability/manifest.yaml`: sole editable capability, target, behavior, evidence, qualification, platform, gap, and divergence data.
- `pkg/capability/types.go`: stable manifest vocabulary and enums; it imports neither `pkg/config` nor `pkg/plugin`.
- `pkg/capability/load.go`: strict embedded loader, validation, cloning, lookup, and qualification queries.
- `pkg/capability/load_test.go`: strict-schema, duplicate, query, and evidence fail-closed tests.
- `cmd/capability-gen/main.go`: deterministic generator/checker for Go registration and Markdown status artifacts.
- `cmd/capability-gen/main_test.go`: golden generation, path containment, and drift-check tests.
- `pkg/plugin/registry_gen.go`: generated constructor imports and `pluginRegistry`; never hand-edited.
- `pkg/plugin/init.go`: hand-written plugin interfaces and execution helpers after registry data is removed.
- `pkg/plugin/capability_registry.go`: consumes generated declarative capability entries while retaining runtime resolution logic.
- `pkg/plugin/manifest_contract_test.go`: proves generated factories, runtime phase registries, priorities, scopes, and central facts agree.
- `pkg/config/profiles.go`: owns `CompatibilityTarget`, `SecurityProfile`, `QualificationProfile`, and `ProfileSelection`.
- `pkg/config/profiles_test.go`: validates the three axes and evidence-derived qualification selection.
- `pkg/config/types.go`, `pkg/config/init.go`: replace `deployment.profile` with the three root profile fields and route existing policy validation through `ProfileSelection`.
- `t/plugin/coverage_test.go`, `t/plugin/corpus_test.go`: consume the central manifest instead of parsing `docs/plugins.md` or duplicating an upstream commit constant.
- `docs/plugins.md`: fully generated current status; no longer an editable truth source.
- `docs/history/plugins-2026-08-23.md`: byte-for-byte archive of the pre-cutover status document.
- `docs/architecture/compatibility-contract.md`: human-readable governance and profile contract.
- `docs/architecture/legacy-conflicts.md`: exact supersession map for earlier design and review claims.
- `docs/architecture/adr/0000-template.md`: mandatory divergence ADR schema.
- `docs/architecture/adr/0001-compatibility-governance.md`: records the owner-approved APISIX/Go namespace and parity policy from decisions 1–42.
- `docs/architecture/adr/0002-strict-security-profile.md`: records the owner-approved strict-profile divergences from decisions 23–68.
- `docs/architecture/adr/0003-platform-support.md`: records the owner-approved platform publication boundary from decisions 107–140.
- `docs/design.md`, `docs/reviews/convergence-decisions.md`: retain their evidence and gain precise supersession annotations.
- `docs/configuration.md`, `docs/production-profile.md`, `conf/config-production.yaml`: use the three profile axes and describe blocked evidence honestly.
- `README.md`, `README.zh-CN.md`: contain only localized generated capability summaries between fixed markers.
- `scripts/capability_status_gate_test.sh`: workflow shape and read-only generation contract test.
- `.github/workflows/capability-status.yml`: required governance job replacing `plugin-status.yml` atomically.
- `.github/CODEOWNERS`: requires project-owner review for manifest, profile, generated status, and ADR changes.
- `Makefile`: exposes `generate-capabilities`, `check-capability-drift`, and `test-capability-status`.

## Stable Cross-Plan Interfaces

The following signatures are fixed for plans 02–09. Changing one requires updating the program plan and every consuming child plan in the same documentation commit.

```go
// package capability
type Manifest struct {
	SchemaVersion         int
	Target                Target
	Plugins               []PluginCapability
	QualificationProfiles []QualificationProfile
	Divergences           []Divergence
}
func Load() (*Manifest, error)
func (m *Manifest) Plugin(name string) (PluginCapability, bool)
func (m *Manifest) Qualification(name string) (QualificationProfile, bool)
func (m *Manifest) QualifiedPlugins(profile string) []string

// package config
type CompatibilityTarget string
type SecurityProfile string
type QualificationProfile string
type ProfileSelection struct {
	Compatibility CompatibilityTarget
	Security      SecurityProfile
	Qualification QualificationProfile
}
func (p ProfileSelection) Validate(manifest *capability.Manifest) error
```

`Load` parses a new independent manifest on every call; there is no global manifest singleton. `Plugin` accepts either a canonical capability name or a public factory/config key. `Qualification` and `QualifiedPlugins` return copied slices; callers cannot mutate the loaded manifest. `RequiredPlugins` and `QualifiedPlugins` are sorted public factory/config keys, not implementation package names.

---

### Task 1: Define and Strictly Load the Capability Manifest

**Files:**
- Create: `pkg/capability/manifest.yaml`
- Create: `pkg/capability/types.go`
- Create: `pkg/capability/load.go`
- Create: `pkg/capability/load_test.go`

**Interfaces:**
- Consumes: APISIX target `3.17.0` / `9ef2ecab67f652d38365049613610ef649bb4ad0`; no Go package from another program boundary
- Produces: `capability.Manifest`, `capability.Load()`, `(*Manifest).Plugin(string)`, `(*Manifest).Qualification(string)`, `(*Manifest).QualifiedPlugins(string)`

- [ ] **Step 1: Write a failing strict-loader test**

Add the following fixture-oriented tests to `pkg/capability/load_test.go`:

```go
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
```

- [ ] **Step 2: Run the loader test and confirm the package is absent**

Run: `bash -lc 'source .envrc && go test ./pkg/capability -run "^(TestParseRejectsUnknownFields|TestManifestQueriesReturnCopies)$" -count=1'`

Expected: FAIL with `stat .../pkg/capability: directory not found`.

- [ ] **Step 3: Add the exact manifest vocabulary**

Create `pkg/capability/types.go` with these exported types and constants:

```go
package capability

type Namespace string
type Domain string
type BehaviorStatus string
type EvidenceKind string
type EvidenceState string
type DivergenceStatus string

const (
	NamespaceAPISIX Namespace = "apisix"
	NamespaceGoV1  Namespace = "apisix-go/v1"
	DomainHTTP     Domain = "http"
	DomainStream   Domain = "stream"

	BehaviorFull          BehaviorStatus = "full"
	BehaviorPartial       BehaviorStatus = "partial"
	BehaviorNotApplicable BehaviorStatus = "not_applicable"
	BehaviorDeferred      BehaviorStatus = "deferred"

	EvidenceSchema         EvidenceKind = "schema"
	EvidenceUnit           EvidenceKind = "unit"
	EvidenceUpstream       EvidenceKind = "converted_upstream"
	EvidenceDifferential   EvidenceKind = "differential"
	EvidenceRealDependency EvidenceKind = "real_dependency"
	EvidenceFailure        EvidenceKind = "failure"
	EvidenceRecovery       EvidenceKind = "recovery"

	EvidenceVerified      EvidenceState = "verified"
	EvidenceMissing       EvidenceState = "missing"
	EvidenceDeferred      EvidenceState = "deferred"
	EvidenceFlaky         EvidenceState = "flaky"
	EvidenceStale         EvidenceState = "stale"
	EvidenceNotApplicable EvidenceState = "not_applicable"

	DivergenceProposed DivergenceStatus = "proposed"
	DivergenceAccepted DivergenceStatus = "accepted"
	DivergenceRetired  DivergenceStatus = "retired"
)

type Target struct {
	Name         string `yaml:"name"`
	Version      string `yaml:"version"`
	SourceCommit string `yaml:"source_commit"`
	Image        string `yaml:"image"`
}

type Factory struct {
	Key         string `yaml:"key"`
	ImportPath  string `yaml:"import_path"`
	ImportAlias string `yaml:"import_alias"`
	Constructor string `yaml:"constructor"`
}

type EvidenceClaim struct {
	State  EvidenceState `yaml:"state"`
	Refs   []string      `yaml:"refs"`
	Owner  string        `yaml:"owner"`
	Reason string        `yaml:"reason"`
}

type Evidence struct {
	Schema         EvidenceClaim `yaml:"schema"`
	Unit           EvidenceClaim `yaml:"unit"`
	Upstream       EvidenceClaim `yaml:"converted_upstream"`
	Differential   EvidenceClaim `yaml:"differential"`
	RealDependency EvidenceClaim `yaml:"real_dependency"`
	Failure        EvidenceClaim `yaml:"failure"`
	Recovery       EvidenceClaim `yaml:"recovery"`
}

type PluginCapability struct {
	Name               string         `yaml:"name"`
	Implementation     string         `yaml:"implementation"`
	Namespace          Namespace      `yaml:"namespace"`
	Domains            []Domain       `yaml:"domains"`
	APISIXDefault      bool           `yaml:"apisix_default"`
	Factories          []Factory      `yaml:"factories"`
	Phases             []string       `yaml:"phases"`
	Priority           int            `yaml:"priority"`
	Scopes             []string       `yaml:"scopes"`
	InstanceScope      string         `yaml:"instance_scope"`
	Behavior           BehaviorStatus `yaml:"behavior"`
	BehaviorSummary    string         `yaml:"behavior_summary"`
	KnownGaps          []string       `yaml:"known_gaps"`
	Evidence           Evidence       `yaml:"evidence"`
	DivergenceIDs      []string       `yaml:"divergence_ids"`
	SupportedPlatforms []string       `yaml:"supported_platforms"`
}

type QualificationProfile struct {
	Name             string         `yaml:"name"`
	Domains          []string       `yaml:"domains"`
	RequiredPlugins  []string       `yaml:"required_plugins"`
	RequiredEvidence []EvidenceKind `yaml:"required_evidence"`
}

type Divergence struct {
	ID              string           `yaml:"id"`
	Status          DivergenceStatus `yaml:"status"`
	Compatibility   string           `yaml:"compatibility_target"`
	ADR             string           `yaml:"adr"`
	OwnerApprovalRef string           `yaml:"owner_approval_ref"`
}

type Manifest struct {
	SchemaVersion         int                    `yaml:"schema_version"`
	Target                Target                 `yaml:"target"`
	Plugins               []PluginCapability     `yaml:"plugins"`
	QualificationProfiles []QualificationProfile `yaml:"qualification_profiles"`
	Divergences           []Divergence            `yaml:"divergences"`
	pluginsByName         map[string]int
	profilesByName        map[string]int
}
```

- [ ] **Step 4: Add the embedded strict loader**

Create `pkg/capability/load.go` with `go:embed`, `yaml.Decoder.KnownFields(true)`, a second-document EOF check, validation, deep clones, and sorted output. Its public surface must be exactly:

```go
//go:embed manifest.yaml
var manifestYAML []byte

func Load() (*Manifest, error) { return Parse(manifestYAML) }
func Parse(data []byte) (*Manifest, error)
func (m *Manifest) Plugin(name string) (PluginCapability, bool)
func (m *Manifest) Qualification(name string) (QualificationProfile, bool)
func (m *Manifest) QualifiedPlugins(profile string) []string
```

`validate()` must reject a schema version other than `1`, a target other than the exact spec target, duplicate plugin/profile/factory/divergence IDs, unknown enum values, blank owner/reason for non-verified evidence, verified evidence without refs, `not_applicable` evidence without a concrete applicability reason, gaps on `full`, missing gaps on `partial`/`deferred`, APISIX entries without a domain, registered factories without import/alias/constructor, unsorted or duplicate qualification domains/plugins/evidence kinds, a qualification `required_plugins` key that has no factory, active divergence references absent from the top-level ledger, and an accepted divergence without both ADR and owner approval reference. `Qualification` returns a deep copy. `QualifiedPlugins` walks only the selected profile's `RequiredPlugins` and includes a key only when the plugin is APISIX namespace, `full`, supports one of the profile domains, and every required evidence claim is either `verified` or explicitly `not_applicable`; `missing`, `deferred`, `flaky`, and `stale` always fail qualification.

- [ ] **Step 5: Add a minimal valid embedded manifest**

Start `pkg/capability/manifest.yaml` with the exact target, qualification rule, and `request-id` entry below; Task 2 replaces this seed with the complete inventory before any generated registry cutover:

```yaml
schema_version: 1
target:
  name: apisix-3.17
  version: 3.17.0
  source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
  image: apache/apisix:3.17.0
qualification_profiles:
  - name: http-data-plane-v1
    domains: [http]
    required_plugins: [request-id]
    required_evidence: [converted_upstream, differential, failure, real_dependency, recovery, schema, unit]
plugins:
  - name: request-id
    implementation: request_id
    namespace: apisix
    domains: [http]
    apisix_default: true
    factories:
      - key: request-id
        import_path: github.com/wklken/apisix-go/pkg/plugin/request_id
        import_alias: request_id
        constructor: Plugin
    phases: [rewrite]
    priority: 12015
    scopes: [global_rule, route, service]
    instance_scope: effective-config
    behavior: full
    behavior_summary: Adds or propagates the configured request identifier.
    known_gaps: []
    evidence:
      schema: {state: verified, refs: [pkg/plugin/request_id/plugin.go], owner: request-id, reason: ""}
      unit: {state: verified, refs: [pkg/plugin/request_id/plugin_test.go], owner: request-id, reason: ""}
      converted_upstream: {state: stale, refs: [t/plugin/request-id.yaml], owner: request-id, reason: corpus pin differs from the compatibility oracle}
      differential: {state: missing, refs: [], owner: request-id, reason: pinned APISIX differential evidence is not recorded}
      real_dependency: {state: not_applicable, refs: [], owner: request-id, reason: plugin has no external dependency}
      failure: {state: missing, refs: [], owner: request-id, reason: failure evidence is not classified}
      recovery: {state: not_applicable, refs: [], owner: request-id, reason: plugin has no recovery lifecycle}
    divergence_ids: []
    supported_platforms: [linux-amd64, linux-arm64, darwin-amd64, darwin-arm64, windows-amd64]
divergences: []
```

- [ ] **Step 6: Run the loader tests and inspect the public API**

Run: `bash -lc 'source .envrc && go test ./pkg/capability -run "^(TestParse|TestManifest)" -count=1 && go doc ./pkg/capability.Manifest ./pkg/capability.Load'`

Expected: PASS; documentation shows no dependency on `pkg/config` or `pkg/plugin`.

- [ ] **Step 7: Commit the manifest foundation**

```bash
git add pkg/capability
git commit -m "feat(governance): define capability manifest contract"
```

### Task 2: Migrate Every Existing Capability Fact Without Promoting Evidence

**Files:**
- Modify: `pkg/capability/manifest.yaml`
- Create: `pkg/plugin/manifest_contract_test.go`
- Modify: `pkg/plugin/capability_registry_test.go`
- Test: `pkg/capability/load_test.go`

**Interfaces:**
- Consumes: `capability.Load()`, all 115 keys in `pluginRegistry`, 114 runtime identities in `capabilityRegistry`, all 118 rows in `docs/plugins.md`, and `t/plugin/corpus_scope.yaml`
- Produces: a complete `capability.Manifest`; every former source row has one central entry and no claim is stronger than current evidence

- [ ] **Step 1: Add a failing inventory-completeness test**

Create `pkg/plugin/manifest_contract_test.go` in package `plugin`:

```go
package plugin

import (
	"sort"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestCapabilityManifestCoversEveryFactory(t *testing.T) {
	m, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, entry := range m.Plugins {
		for _, factory := range entry.Factories {
			seen[factory.Key] = entry.Name
		}
	}
	var missing, extra []string
	for key := range pluginRegistry {
		if _, ok := seen[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range seen {
		if _, ok := pluginRegistry[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("factory inventory mismatch: missing=%v extra=%v", missing, extra)
	}
}
```

- [ ] **Step 2: Run the completeness test and capture the exact missing list**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin -run "^TestCapabilityManifestCoversEveryFactory$" -count=1'`

Expected: FAIL listing every current factory except `request-id`; save this output in the task notes used to review the migration.

- [ ] **Step 3: Add manifest invariants for the documented APISIX set**

Extend `pkg/capability/load_test.go`:

```go
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
	for _, name := range []string{"ext-plugin-pre-req", "ext-plugin-post-req", "ext-plugin-post-resp", "inspect", "ai"} {
		plugin, ok := m.Plugin(name)
		if !ok || plugin.Behavior != BehaviorNotApplicable {
			t.Fatalf("%s = %#v/%v, want not_applicable", name, plugin, ok)
		}
	}
}
```

- [ ] **Step 4: Replace the seed with the complete inventory using fail-closed mapping rules**

Populate one `PluginCapability` per current `docs/plugins.md` row and per missing APISIX default. Apply these rules mechanically and review the resulting diff row by row:

```text
docs Full    -> behavior: full, known_gaps: []
docs Partial -> behavior: partial, copy the exact Known behavior gap text
docs N/A     -> behavior: not_applicable, copy the exact native/runtime reason
registered factory -> one factories entry using the current pkg/plugin/init.go import and constructor
factory alias -> multiple factories under one canonical entry; preserve otel/opentelemetry and serverless aliases explicitly
current capability registry -> copy phases and ownership as declarations, never infer an unlisted phase
plugin Init priority -> copy the exact integer; manifest_contract_test compares it to GetPriority()
existing focused test path -> evidence state verified with the exact path
converted corpus at c3d7... while target is 9ef2... -> evidence state stale, never verified
no recorded differential or real-dependency result -> missing with owner and reason
explicit native/runtime absence -> not_applicable only for that evidence dimension
```

Do not convert a documentation statement, fixture, or successful constructor call into differential, real-dependency, failure, or recovery evidence.

Replace the seed profile's `required_plugins` with the current candidate set as sorted manifest data, not a Go constant:

```yaml
required_plugins: [basic-auth, cors, jwt-auth, key-auth, prometheus, request-id]
```

These six entries remain unqualified while required evidence is stale or missing. A later evidence PR changes the claims, not the selection algorithm.

- [ ] **Step 5: Add runtime-fact comparison tests**

In `pkg/plugin/manifest_contract_test.go`, add checks that every factory constructor is non-nil, initializes successfully, matches the manifest implementation name exception, and has the declared priority. Compare the central phase declarations to `CapabilitySpecForFactory` and `RequestStageFor`; the test must fail on a phase present in runtime but absent from the manifest or vice versa.

```go
func TestCapabilityManifestMatchesRuntimeFacts(t *testing.T) {
	m, err := capability.Load()
	if err != nil { t.Fatal(err) }
	for _, entry := range m.Plugins {
		for _, declared := range entry.Factories {
			factory := pluginRegistry[declared.Key]
			if factory == nil { t.Fatalf("factory %s missing", declared.Key) }
			instance := factory()
			if err := instance.Init(); err != nil { t.Fatalf("%s Init: %v", declared.Key, err) }
			if instance.GetPriority() != entry.Priority {
				t.Fatalf("%s priority = %d, manifest = %d", declared.Key, instance.GetPriority(), entry.Priority)
			}
		}
	}
}
```

- [ ] **Step 6: Run exact manifest and runtime fact gates**

Run: `bash -lc 'source .envrc && go test ./pkg/capability ./pkg/plugin -run "^(TestLoadedManifest|TestCapabilityManifest)" -count=1'`

Expected: PASS with 104 APISIX defaults, every factory accounted for, and no implicit evidence promotion.

- [ ] **Step 7: Commit the complete central inventory**

```bash
git add pkg/capability/manifest.yaml pkg/capability/load_test.go pkg/plugin/manifest_contract_test.go pkg/plugin/capability_registry_test.go
git commit -m "feat(governance): inventory plugin behavior and evidence"
```

### Task 3: Generate Plugin Registration and Remove Duplicate Declarative Registries

**Files:**
- Create: `cmd/capability-gen/main.go`
- Create: `cmd/capability-gen/main_test.go`
- Create: `pkg/plugin/registry_gen.go`
- Modify: `pkg/plugin/init.go` (`pluginRegistry` and constructor import block)
- Modify: `pkg/plugin/capability_registry.go` (`capabilityManifestEntries`)
- Modify: `pkg/plugin/init_test.go`
- Modify: `pkg/plugin/capability_registry_test.go`

**Interfaces:**
- Consumes: `capability.Load()` and `PluginCapability.Factories`
- Produces: generated `pluginRegistry`, generated declarative capability entries, stable `plugin.New(name)` behavior

- [ ] **Step 1: Write a failing deterministic-generator test**

Create `cmd/capability-gen/main_test.go`:

```go
package main

import (
	"bytes"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestRenderRegistryIsDeterministic(t *testing.T) {
	m, err := capability.Load()
	if err != nil { t.Fatal(err) }
	first, err := renderRegistry(m)
	if err != nil { t.Fatal(err) }
	second, err := renderRegistry(m)
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(first, second) {
		t.Fatal("registry generation is nondeterministic")
	}
	if !bytes.Contains(first, []byte(`"request-id": func() Plugin { return &request_id.Plugin{} }`)) {
		t.Fatal("request-id constructor missing")
	}
}
```

- [ ] **Step 2: Run the generator test and verify the renderer is absent**

Run: `bash -lc 'source .envrc && go test ./cmd/capability-gen -run "^TestRenderRegistryIsDeterministic$" -count=1'`

Expected: FAIL with `undefined: renderRegistry`.

- [ ] **Step 3: Implement deterministic generation and check mode**

Implement `cmd/capability-gen/main.go` with `-repo-root` defaulting to `.`, `-check`, and `-write`. Sort imports by path and factories by key; use `go/format` before comparison. Reject an output path that escapes the cleaned repository root. Generate the header below into `pkg/plugin/registry_gen.go`:

```go
// Code generated by go run ./cmd/capability-gen -write; DO NOT EDIT.

package plugin

// imports are rendered from manifest Factory facts.

var pluginRegistry = map[string]func() Plugin{
	// one sorted constructor per Factory.Key
}
```

The same run must generate a private `generatedCapabilityManifestEntries()` function containing central phase/ownership facts. Runtime-only resolution remains in `capability_registry.go`; delete the handwritten `capabilityManifestEntries()` table and call the generated function instead.

- [ ] **Step 4: Generate `registry_gen.go` and verify compile-time equivalence**

Run: `bash -lc 'source .envrc && go run ./cmd/capability-gen -repo-root . -write && go test ./pkg/plugin -run "^(TestNew|TestCapabilityRegistry|TestCapabilityManifest)" -count=1'`

Expected: FAIL with a duplicate `pluginRegistry` or generated manifest-entry symbol. This red result proves the generated replacement exists before the handwritten copy is removed.

- [ ] **Step 5: Delete the replaced constructor imports and handwritten maps atomically**

Remove all plugin constructor imports and `pluginRegistry` from `pkg/plugin/init.go`. Remove `capabilityManifestEntries()` and its private entry table from `pkg/plugin/capability_registry.go`; retain runtime composition, `CapabilitySpecForFactory`, request/response ownership resolution, and executor behavior.

- [ ] **Step 6: Prove generation drift fails closed**

Add a `TestCheckDetectsDrift` test that writes generated output into `t.TempDir()`, changes one byte, and expects `checkOutputs` to report the exact relative filename. Then run:

Run: `bash -lc 'source .envrc && go test ./cmd/capability-gen ./pkg/plugin -run "^(TestRender|TestCheck|TestNew|TestCapabilityRegistry|TestCapabilityManifest)" -count=1 && go run ./cmd/capability-gen -repo-root . -check && make build'`

Expected: PASS; `rg -n 'var pluginRegistry|func capabilityManifestEntries' pkg/plugin --glob '*.go'` reports only the generated registry symbol and no handwritten manifest function.

- [ ] **Step 7: Commit the generated registry cutover**

```bash
git add cmd/capability-gen pkg/plugin
git commit -m "refactor(plugin): generate registration from capability manifest"
```

### Task 4: Replace the Monolithic Deployment Profile With Three Axes

**Files:**
- Create: `pkg/config/profiles.go`
- Create: `pkg/config/profiles_test.go`
- Modify: `pkg/config/types.go` (`Config`, `Deployment`, `HTTPDataPlaneV1Profile`)
- Modify: `pkg/config/init.go` (`validateRuntimeConfig`, `profileAwareRuntimeError`, `validateHTTPDataPlaneV1Profile`)
- Modify: `pkg/config/release_gate_test.go`
- Modify: `pkg/route/production_policy.go`
- Modify: `pkg/route/production_policy_test.go`
- Modify: `pkg/route/builder.go` (Kafka profile decision at `GlobalConfig.Deployment.Profile`)
- Modify: `pkg/route/unsupported_semantics_test.go`
- Modify: `conf/config-default.yaml`
- Modify: `conf/config-production.yaml`

**Interfaces:**
- Consumes: `capability.Manifest`, `(*Manifest).QualifiedPlugins(string)`
- Produces: `config.CompatibilityTarget`, `config.SecurityProfile`, `config.QualificationProfile`, `config.ProfileSelection`, `ProfileSelection.Validate(*capability.Manifest) error`

- [ ] **Step 1: Write failing table tests for orthogonal axes**

Create `pkg/config/profiles_test.go`:

```go
package config

import (
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestProfileSelectionValidate(t *testing.T) {
	m, err := capability.Load()
	if err != nil { t.Fatal(err) }
	tests := []struct {
		name string
		in ProfileSelection
		wantErr string
	}{
		{name: "compat without qualification", in: ProfileSelection{Compatibility: CompatibilityAPISIX317, Security: SecurityCompat}},
		{name: "strict is independent", in: ProfileSelection{Compatibility: CompatibilityAPISIX317, Security: SecurityStrict}},
		{name: "known but not yet qualified", in: ProfileSelection{Compatibility: CompatibilityAPISIX317, Security: SecurityStrict, Qualification: QualificationHTTPDataPlaneV1}, wantErr: "unqualified required plugins"},
		{name: "unknown target", in: ProfileSelection{Compatibility: "apisix-master", Security: SecurityCompat}, wantErr: "compatibility_target"},
		{name: "unknown security", in: ProfileSelection{Compatibility: CompatibilityAPISIX317, Security: "unsafe"}, wantErr: "security_profile"},
		{name: "unknown qualification", in: ProfileSelection{Compatibility: CompatibilityAPISIX317, Security: SecurityCompat, Qualification: "future"}, wantErr: "qualification_profile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.in.Validate(m)
			if test.wantErr == "" && err != nil { t.Fatal(err) }
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the profile test and confirm the types are absent**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^TestProfileSelectionValidate$" -count=1'`

Expected: FAIL with undefined profile types.

- [ ] **Step 3: Implement the stable profile interface**

Create `pkg/config/profiles.go`:

```go
package config

import (
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
)

type CompatibilityTarget string
type SecurityProfile string
type QualificationProfile string

const (
	CompatibilityAPISIX317 CompatibilityTarget = "apisix-3.17"
	SecurityCompat SecurityProfile = "compat"
	SecurityStrict SecurityProfile = "strict"
	QualificationNone QualificationProfile = ""
	QualificationHTTPDataPlaneV1 QualificationProfile = "http-data-plane-v1"
)

type ProfileSelection struct {
	Compatibility CompatibilityTarget
	Security SecurityProfile
	Qualification QualificationProfile
}

func (p ProfileSelection) Validate(manifest *capability.Manifest) error {
	if manifest == nil { return fmt.Errorf("capability manifest must not be nil") }
	if p.Compatibility != CompatibilityTarget(manifest.Target.Name) {
		return fmt.Errorf("compatibility_target %q is unsupported", p.Compatibility)
	}
	if !slices.Contains([]SecurityProfile{SecurityCompat, SecurityStrict}, p.Security) {
		return fmt.Errorf("security_profile %q is unsupported", p.Security)
	}
	if p.Qualification == QualificationNone { return nil }
	qualification, ok := manifest.Qualification(string(p.Qualification))
	if !ok {
		return fmt.Errorf("qualification_profile %q is unsupported", p.Qualification)
	}
	qualified := manifest.QualifiedPlugins(string(p.Qualification))
	if !slices.Equal(qualification.RequiredPlugins, qualified) {
		return fmt.Errorf("qualification_profile %q has unqualified required plugins", p.Qualification)
	}
	return nil
}
```

`Qualification` must return a copy whose `Domains`, `RequiredPlugins`, and `RequiredEvidence` cannot mutate manifest storage. `QualifiedPlugins` must distinguish an unknown profile (`nil`) from a known profile with no passing plugins (non-nil empty slice). `ProfileSelection.Validate` fails closed until all `RequiredPlugins` meet all required evidence.

- [ ] **Step 4: Add the three root configuration fields and remove the legacy field**

Add these fields to `config.Config` with `mapstructure` tags and remove `Deployment.Profile` and `HTTPDataPlaneV1Profile`:

```go
CompatibilityTarget CompatibilityTarget `mapstructure:"compatibility_target"`
SecurityProfile SecurityProfile `mapstructure:"security_profile"`
QualificationProfile QualificationProfile `mapstructure:"qualification_profile"`

func (c *Config) Profiles() ProfileSelection {
	return ProfileSelection{
		Compatibility: c.CompatibilityTarget,
		Security: c.SecurityProfile,
		Qualification: c.QualificationProfile,
	}
}
```

In the current Viper loader, default only `compatibility_target=apisix-3.17` and `security_profile=compat`; keep qualification empty. Task 02 replaces this temporary Viper defaulting with presence-aware defaults.

Until Task 02 introduces `config.LoadRequest`, `loadConfigFiles` must call `capability.Load()` once and pass the returned pointer to `validateRuntimeConfig(cfg, manifest)`. Do not cache it in a package variable. Task 02's bootstrap then becomes the sole caller of `capability.Load()` and passes that same explicit pointer through `LoadRequest`.

- [ ] **Step 5: Split runtime policy validation by axis**

Replace `validateHTTPDataPlaneV1Profile` with `validateSecurityProfile(cfg, selection)` and `validateQualificationProfile(cfg, manifest, selection)`. Move `debug`, trusted CIDR, verified etcd/upstream TLS, credential hiding, and secret protections under `strict`; move HTTP-only role/provider/protocol and evidence-derived plugin membership under `http-data-plane-v1`. Use:

```go
qualification, ok := manifest.Qualification(string(selection.Qualification))
if !ok {
	return profileFieldError(string(selection.Qualification), "qualification_profile", "is not defined by the capability manifest")
}
if !slices.Equal(cfg.Plugins, qualification.RequiredPlugins) {
	return profileFieldError(string(selection.Qualification), "plugins", "must equal the manifest qualification required_plugins set")
}
if !slices.Equal(qualification.RequiredPlugins, manifest.QualifiedPlugins(string(selection.Qualification))) {
	return profileFieldError(string(selection.Qualification), "evidence", "does not satisfy every required evidence claim")
}
```

If required evidence is incomplete, reject the qualification selection and report the missing claims in `docs/production-profile.md`; do not restore the former six-name allowlist outside the manifest. The manifest's `RequiredPlugins` list is the auditable profile contract, and changing it requires evidence and owner review.

- [ ] **Step 6: Cut every production policy call site to `ProfileSelection`**

Update route policy helpers to accept a `ProfileSelection` argument explicitly instead of reading `config.GlobalConfig.Deployment.Profile`. Kafka remains outside the HTTP qualification profile but remains available under compatibility target `apisix-3.17`; strict security alone must not disable it. Update focused tests so each rejected behavior identifies the responsible axis.

- [ ] **Step 7: Update checked-in configuration**

Use these keys in `conf/config-default.yaml`:

```yaml
compatibility_target: apisix-3.17
security_profile: compat
qualification_profile: ""
```

Use these keys in `conf/config-production.yaml`:

```yaml
compatibility_target: apisix-3.17
security_profile: strict
qualification_profile: http-data-plane-v1
```

Delete `deployment.profile` from both files.

- [ ] **Step 8: Run focused profile and route-policy tests**

Run: `bash -lc 'source .envrc && go test ./pkg/config ./pkg/route -run "^(TestProfileSelection|TestHTTPDataPlane|TestProductionPolicy|TestUnsupportedUpstreamScheme)" -count=1 && make build'`

Expected: PASS; `rg -n 'HTTPDataPlaneV1Profile|Deployment\.Profile|deployment\.profile' pkg conf --glob '*.go' --glob '*.yaml'` prints no result.

- [ ] **Step 9: Commit the profile cutover**

```bash
git add pkg/config pkg/route conf/config-default.yaml conf/config-production.yaml
git commit -m "refactor(config): split compatibility security and qualification profiles"
```

### Task 5: Make Corpus and Qualification Evidence Consume the Manifest

**Files:**
- Modify: `t/plugin/coverage_test.go`
- Modify: `t/plugin/corpus_test.go`
- Modify: `t/plugin/corpus_scope.yaml`
- Modify: `t/plugin/README.md`
- Test: `pkg/capability/load_test.go`

**Interfaces:**
- Consumes: `capability.Load()`, `PluginCapability.Evidence`, target source commit
- Produces: manifest-driven integration selection and corpus accounting; stale `c3d7...` evidence cannot qualify `9ef2...`

- [ ] **Step 1: Write a failing test that forbids Markdown selection**

Replace `TestSupportedPluginManifestSelection` with a manifest-driven test named `TestCapabilityManifestSelection`. It must call `capability.Load()`, collect factory keys whose converted-upstream claim references a `t/plugin/*.yaml` manifest, and compare that set to checked-in manifest files. It must not open `docs/plugins.md`.

```go
func TestCapabilityManifestSelection(t *testing.T) {
	m, err := capability.Load()
	if err != nil { t.Fatal(err) }
	want := map[string]bool{}
	for _, plugin := range m.Plugins {
		claim := plugin.Evidence.Upstream
		for _, ref := range claim.Refs {
			if strings.HasPrefix(ref, "t/plugin/") && strings.HasSuffix(ref, ".yaml") {
				want[strings.TrimSuffix(filepath.Base(ref), ".yaml")] = true
			}
		}
	}
	// Compare want to manifestYAMLFiles(), preserving redirect2's explicit alias.
}
```

- [ ] **Step 2: Run the selector and verify it fails on current docs parsing**

Run: `bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./t/plugin -run "^TestCapabilityManifestSelection$" -count=1'`

Expected: FAIL until imports, exact alias handling, and manifest evidence references are complete.

- [ ] **Step 3: Remove duplicated target constants**

Delete `pinnedAPISIXSourceCommit` from `coverage_test.go`. Read `m.Target.SourceCommit` for the compatibility oracle. Preserve `corpus_scope.yaml`'s current `c3d7d5ec69774121f53d2e20d29d09c816795dd7` as its explicit historical corpus commit until it is re-accounted against `9ef2...`; do not rewrite only the hash.

- [ ] **Step 4: Encode stale-corpus behavior explicitly**

Add `TestCorpusEvidenceMatchesCompatibilityTarget`: if `corpus_scope.yaml` commit differs from `Manifest.Target.SourceCommit`, every `converted_upstream` claim sourced only from that corpus must be `stale`, and `QualifiedPlugins("http-data-plane-v1")` must exclude it. When a later PR re-accounts every source label at `9ef2...`, that PR may change the claim to `verified` atomically with the ledger.

- [ ] **Step 5: Replace status-derived coverage helpers**

Delete `supportedPluginNames`, Markdown regex parsing, hard-coded supported counts, and `upstreamSourceAbsences` from `coverage_test.go`. Model source absences on the corresponding plugin's upstream evidence claim with a reason and no manifest reference. Keep manifest syntax/semantic tests and exact corpus label accounting.

- [ ] **Step 6: Run the focused offline evidence gates**

Run: `bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./t/plugin -run "^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestCorpusScope|TestUpstreamCorpusAccountingWithoutSourceCheckout|TestCorpusEvidenceMatchesCompatibilityTarget)$" -count=1'`

Expected: PASS without an APISIX checkout; output does not claim stale converted cases are qualified.

- [ ] **Step 7: Commit the evidence-source cutover**

```bash
git add pkg/capability t/plugin
git commit -m "refactor(test): derive corpus selection from capability evidence"
```

### Task 6: Generate Status Documentation and Preserve the Previous Record

**Files:**
- Modify: `cmd/capability-gen/main.go`
- Modify: `cmd/capability-gen/main_test.go`
- Create: `docs/history/plugins-2026-08-23.md`
- Replace generated content: `docs/plugins.md`
- Modify generated block: `README.md`
- Modify generated block: `README.zh-CN.md`
- Modify: `t/plugin/README.md`

**Interfaces:**
- Consumes: complete `capability.Manifest`
- Produces: deterministic `docs/plugins.md`, localized English/Chinese README summaries, and `-check` drift result; historical status remains readable

- [ ] **Step 1: Archive the exact pre-cutover status document**

Copy the frozen pre-cutover blob `7e719051:docs/plugins.md` (blob `1498111e499f2aaa8d9ffd23b696d63f8512c737`, SHA-256 `54c6ef75d33b8e88d8473941b5219846c6fc4596e2d00e1904bfec819092f825`) to `docs/history/plugins-2026-08-23.md` and prepend only this archival banner:

```markdown
> Historical snapshot archived during the 2026-08-23 capability-manifest cutover.
> It is evidence of prior project claims, not the current source of truth.
> Current generated status: [`docs/plugins.md`](../plugins.md).
```

Verify the remainder matches the frozen pre-cutover blob after removing the three-line banner. The banner is exactly three lines with no extra blank separator.

Run: `diff -u <(git show 7e719051:docs/plugins.md) <(tail -n +4 docs/history/plugins-2026-08-23.md)` and verify the tail SHA-256 is `54c6ef75d33b8e88d8473941b5219846c6fc4596e2d00e1904bfec819092f825`.

Expected: no output.

- [ ] **Step 2: Write failing Markdown golden tests**

Add tests for `renderPluginsMarkdown` and localized `renderReadmeSummary` output. The plugin document must show behavior and seven evidence columns separately, include target/version/commit, label stale/flaky/deferred evidence visibly, list every known gap verbatim, and state that generated rows are projections rather than proof. Both README summaries must derive the same counts, qualification state, and gaps from the manifest; neither may retain hand-maintained inventory numbers.

```go
func TestRenderPluginsMarkdownSeparatesBehaviorAndEvidence(t *testing.T) {
	m, err := capability.Load()
	if err != nil { t.Fatal(err) }
	got, err := renderPluginsMarkdown(m)
	if err != nil { t.Fatal(err) }
	for _, text := range []string{"Behavior status", "Schema", "Differential", "Real dependency", "Recovery", "stale"} {
		if !bytes.Contains(got, []byte(text)) { t.Fatalf("generated plugins.md missing %q", text) }
	}
}
```

- [ ] **Step 3: Implement deterministic documentation rendering**

Sort APISIX defaults first by name, then Go extension entries by namespace/name. Compute every count from the manifest. Never print `100%` unless all Go-applicable APISIX defaults have `behavior: full` and every required evidence claim for the named qualification profile is `verified` or explicitly `not_applicable` with a concrete reason.

- [ ] **Step 4: Generate the README block with stable markers**

Replace the manually maintained status paragraphs in `README.md` and `README.zh-CN.md` with localized content inside the same stable markers:

```markdown
<!-- BEGIN GENERATED CAPABILITY SUMMARY -->
<!-- Code generated by go run ./cmd/capability-gen -write; DO NOT EDIT. -->
[computed target, behavior counts, qualified count, and explicit gaps]
<!-- END GENERATED CAPABILITY SUMMARY -->
```

The renderer must replace only the bounded block in each file and fail if either marker is missing or duplicated. English and Chinese prose may differ, but every numeric fact and gap set must be computed from the same manifest snapshot.

- [ ] **Step 5: Generate and check current artifacts**

Run: `bash -lc 'source .envrc && go run ./cmd/capability-gen -repo-root . -write && go run ./cmd/capability-gen -repo-root . -check'`

Expected: PASS; a second `-write` produces no diff.

- [ ] **Step 6: Run generated-document tests and historical integrity check**

Run: `bash -lc 'source .envrc && go test ./cmd/capability-gen -run "^(TestRenderPluginsMarkdown|TestRenderReadmeSummary|TestCheck)" -count=1 && git diff --check'`

Expected: PASS; `docs/plugins.md` has a generated header, both README marker blocks contain manifest-derived summaries, and `docs/history/plugins-2026-08-23.md` retains all prior table rows.

- [ ] **Step 7: Commit generated status and archive**

```bash
git add cmd/capability-gen docs/plugins.md docs/history/plugins-2026-08-23.md README.md README.zh-CN.md t/plugin/README.md
git commit -m "docs(governance): generate capability status from manifest"
```

### Task 7: Add the Owner-Approved Divergence ADR Gate

**Files:**
- Create: `docs/architecture/adr/0000-template.md`
- Create: `docs/architecture/adr/0001-compatibility-governance.md`
- Create: `docs/architecture/adr/0002-strict-security-profile.md`
- Create: `docs/architecture/adr/0003-platform-support.md`
- Create: `docs/architecture/compatibility-contract.md`
- Modify: `pkg/capability/manifest.yaml`
- Modify: `cmd/capability-gen/main.go`
- Modify: `cmd/capability-gen/main_test.go`
- Create or Modify: `.github/CODEOWNERS`

**Interfaces:**
- Consumes: `Manifest.Divergences`, the 200 owner-confirmed decisions, compatibility target
- Produces: `capability-gen -check` rejection for unapproved active divergence and CODEOWNERS review boundary

- [ ] **Step 1: Write failing ADR-validation tests**

Add table tests that reject a missing ADR, a proposed ADR referenced by an active divergence, mismatched divergence ID, target mismatch, missing decision reference, and owner other than `wklken`.

```go
func TestValidateDivergenceADRRequiresAcceptedOwnerDecision(t *testing.T) {
	root := t.TempDir()
	d := capability.Divergence{
		ID: "DIV-001", Status: capability.DivergenceAccepted,
		Compatibility: "apisix-3.17", ADR: "docs/architecture/adr/0001-example.md",
		OwnerApprovalRef: "2026-08-22 decisions 1-3",
	}
	err := validateDivergenceADR(root, d)
	if err == nil || !strings.Contains(err.Error(), "missing ADR") {
		t.Fatalf("error = %v, want missing ADR", err)
	}
}
```

- [ ] **Step 2: Run the ADR test and confirm validation is absent**

Run: `bash -lc 'source .envrc && go test ./cmd/capability-gen -run "^TestValidateDivergenceADR" -count=1'`

Expected: FAIL with `undefined: validateDivergenceADR`.

- [ ] **Step 3: Define the exact ADR front matter**

Create `docs/architecture/adr/0000-template.md` with strict YAML front matter:

```markdown
---
id: ADR-0000
title: Replace with the decided divergence
status: proposed
compatibility_target: apisix-3.17
divergence_ids: [DIV-000]
owner: wklken
owner_approval_ref: "project-owner review URL or dated architecture decision range"
date: 2026-08-23
---

# Context

State the pinned APISIX behavior and evidence.

# Decision

State the exact observable divergence and namespace/profile where it applies.

# Consequences

State compatibility, security, migration, testing, and rollback consequences.

# Evidence required to retire

State the exact differential test or capability change that retires this ADR.
```

The gate accepts `status: accepted` only. The template remains `proposed` and is never referenced by the manifest.

- [ ] **Step 4: Record only decisions already approved in this review**

Create three accepted ADRs with `owner: wklken` and these exact decision references:

```text
ADR-0001 / DIV-001-go-native-extension-identity / `owner_approval_ref: "decisions 1-42"`: APISIX namespace stays compatibility-pure, Go-native implementation identity is allowed, extensions use apisix-go/v1.
ADR-0002 / DIV-002-strict-security-profile / `owner_approval_ref: "decisions 23, 35, 63-68"`: compat preserves pinned observable defaults/bugs, strict adds versioned security controls.
ADR-0003 / DIV-003-platform-artifact-policy / `owner_approval_ref: "decisions 107, 118-130"`: Linux production, native-smoked macOS artifacts, Windows source-buildable experimental and no Windows artifact.
```

Add those exact three IDs as top-level accepted manifest divergences with `compatibility_target: apisix-3.17`, their matching ADR paths, and the exact decision-reference text above. They govern global namespace/profile/artifact policy and are not plugin-specific, so do not add them to any plugin's `divergence_ids`. Do not mark a feature gap as a divergence. A gap remains `partial` or `deferred`; a divergence is only an intentional observable departure that is active in a namespace/profile.

- [ ] **Step 5: Implement ADR validation**

Parse front matter with `yaml.Decoder.KnownFields(true)`. For every accepted manifest divergence, verify the file is under `docs/architecture/adr/`, its ID and target match, its `divergence_ids` contains the manifest ID, status is accepted, owner is exactly `wklken`, and `owner_approval_ref` equals the manifest reference. Reject an accepted ADR not referenced by any active divergence to prevent stale governance claims.

- [ ] **Step 6: Add CODEOWNERS governance paths**

Add these exact lines to `.github/CODEOWNERS`, preserving unrelated existing owners:

```text
/pkg/capability/manifest.yaml @wklken
/pkg/config/profiles.go @wklken
/docs/architecture/adr/ @wklken
/docs/plugins.md @wklken
```

- [ ] **Step 7: Write the compatibility contract**

In `docs/architecture/compatibility-contract.md`, define APISIX vs `apisix-go/v1` namespaces, the three profile axes, behavior/evidence vocabulary, qualification fail-closed rule, corpus pin rule, ADR lifecycle, owner approval, extension freeze, and the production-readiness prohibition. Link the spec and generated manifest status.

- [ ] **Step 8: Run the ADR and manifest gates**

Run: `bash -lc 'source .envrc && go test ./cmd/capability-gen ./pkg/capability -run "^(TestValidateDivergenceADR|TestLoadedManifest)" -count=1 && go run ./cmd/capability-gen -repo-root . -check'`

Expected: PASS; changing an accepted ADR owner or removing its file makes `-check` fail with the exact divergence ID.

- [ ] **Step 9: Commit the governance boundary**

```bash
git add .github/CODEOWNERS docs/architecture pkg/capability/manifest.yaml cmd/capability-gen
git commit -m "feat(governance): require owner-approved divergence ADRs"
```

### Task 8: Supersede Conflicting Documents Without Deleting Historical Evidence

**Files:**
- Create: `docs/architecture/legacy-conflicts.md`
- Modify: `docs/design.md` (profile section, SIGHUP/WebSocket statement, route-schema statement)
- Modify: `docs/reviews/convergence-decisions.md` (document header and `ARCH-03`)
- Modify: `docs/configuration.md` (`deployment.profile` table row and status workflow)
- Modify: `docs/production-profile.md` (profile fields, plugin qualification, evidence warning)
- Test: `cmd/capability-gen/main_test.go`

**Interfaces:**
- Consumes: compatibility contract, `config.ProfileSelection`, generated status
- Produces: an explicit supersession graph; old evidence remains readable but cannot govern new implementation

- [ ] **Step 1: Write a failing conflict-scan test**

Add `TestGovernedDocsContainNoActiveLegacyClaims` using exact forbidden active claims and required supersession markers:

```go
func TestGovernedDocsContainNoActiveLegacyClaims(t *testing.T) {
	tests := []struct{ path, forbidden string }{
		{"docs/configuration.md", "`deployment.profile` | Empty selects compatibility mode"},
		{"docs/production-profile.md", "deployment.profile: http-data-plane-v1"},
	}
	for _, test := range tests {
		data, err := os.ReadFile(filepath.Join(repoRoot(t), test.path))
		if err != nil { t.Fatal(err) }
		if bytes.Contains(data, []byte(test.forbidden)) {
			t.Errorf("%s retains active legacy claim %q", test.path, test.forbidden)
		}
	}
}
```

For `docs/design.md` and `convergence-decisions.md`, require a supersession annotation instead of forbidding the historical sentence.

- [ ] **Step 2: Run the conflict scan and capture every current conflict**

Run: `bash -lc 'source .envrc && go test ./cmd/capability-gen -run "^TestGovernedDocsContainNoActiveLegacyClaims$" -count=1'`

Expected: FAIL on `deployment.profile` and missing supersession markers.

- [ ] **Step 3: Create the exact legacy conflict ledger**

Create `docs/architecture/legacy-conflicts.md` with one row for each conflict:

```markdown
| Historical source | Preserved claim | Governing replacement | Disposition |
| --- | --- | --- | --- |
| `docs/design.md`, candidate profile section | one `deployment.profile` combines compatibility and strict qualification | `compatibility_target`, `security_profile`, `qualification_profile` | superseded 2026-08-23 |
| `docs/design.md`, lifecycle section | route retirement closes WebSockets and SIGHUP only drains/exits | supervisor generation handoff preserves ordinary hijacked connections | superseded 2026-08-23 |
| `docs/design.md`, route schema section | retain only the current compatibility subset | pinned APISIX observable contract and explicit gap accounting | superseded 2026-08-23 |
| `docs/plugins.md` before this plan | hand-edited table is the single source of truth | `pkg/capability/manifest.yaml` plus generated `docs/plugins.md` | archived in `docs/history/plugins-2026-08-23.md` |
| `docs/reviews/convergence-decisions.md`, ARCH-03 | do not import/cover the full pinned route contract | program specification HTTP compatibility contract | historical remediation evidence; decision superseded |
| `docs/configuration.md`, lifecycle/SIGHUP section | retired generations close WebSockets and SIGHUP only drains/exits | supervisor generation handoff target, with the current pre-convergence behavior labeled explicitly until implemented | superseded as governing design 2026-08-23 |
| `docs/production-profile.md`, lifecycle/SIGHUP section | retirement closes hijacked connections and SIGHUP exits after drain | supervisor generation handoff target, with the current pre-convergence behavior labeled explicitly until implemented | superseded as governing design 2026-08-23 |
```

- [ ] **Step 4: Annotate historical documents in place**

Add a top banner to `docs/reviews/convergence-decisions.md` stating that the ledger remains evidence for completed remediations and its prospective architecture decisions are superseded where listed in `legacy-conflicts.md`. Under `ARCH-03`, add a dated `Superseded:` paragraph but retain the original decision text unchanged.

In `docs/design.md`, retain the old lifecycle and route-schema text under a `Historical behavior before convergence` label, then immediately link the governing spec and compatibility contract. Do not silently rewrite history into the new behavior.

- [ ] **Step 5: Rewrite current operational documents to the new axes**

Update `docs/configuration.md` and `docs/production-profile.md` to show the three exact YAML keys from Task 4. Explain that `security_profile: strict` does not imply qualification, and `qualification_profile: http-data-plane-v1` derives its plugin set from verified evidence. If the initial set is empty or blocked, link the generated blocked reason; do not preserve a hand-maintained six-plugin or evidence table. Their lifecycle/SIGHUP sections must distinguish the current pre-convergence implementation from the governing supervisor-generation target instead of presenting either as already delivered.

- [ ] **Step 6: Run documentation conflict and link checks**

Run: `bash -lc 'source .envrc && go test ./cmd/capability-gen -run "^TestGovernedDocsContainNoActiveLegacyClaims$" -count=1 && go run ./cmd/capability-gen -repo-root . -check && git diff --check'`

Expected: PASS; historical sentences remain only inside clearly marked historical context.

- [ ] **Step 7: Commit the supersession ledger**

```bash
git add docs/architecture/legacy-conflicts.md docs/design.md docs/reviews/convergence-decisions.md docs/configuration.md docs/production-profile.md
git commit -m "docs(governance): supersede conflicting architecture claims"
```

### Task 9: Replace the Plugin-Status Workflow With a Manifest Drift Gate

**Files:**
- Create: `scripts/capability_status_gate_test.sh`
- Delete: `scripts/plugin_status_gate_test.sh`
- Create: `.github/workflows/capability-status.yml`
- Delete: `.github/workflows/plugin-status.yml`
- Modify: `.github/workflows/unit-test.yml`
- Modify: `scripts/release_gate_test.sh`
- Modify: `docs/runbooks/production-release.md`
- Modify: `Makefile`
- Test: `scripts/capability_status_gate_test.sh`

**Interfaces:**
- Consumes: generator `-check`, capability/unit/plugin/corpus focused tests, ADR gate
- Produces: required read-only `Capability Status Contract` check and local Make targets

- [ ] **Step 1: Write the new workflow contract test first**

Create `scripts/capability_status_gate_test.sh` by retaining the current parser helpers but changing the exact contract to:

```text
workflow: .github/workflows/capability-status.yml
name: Capability Status Contract
pull_request: no path filter
push master paths: Makefile, pkg/capability/**, pkg/config/profiles.go,
  pkg/plugin/registry_gen.go, cmd/capability-gen/**, docs/plugins.md,
  docs/architecture/**, t/plugin/*.yaml, t/plugin/corpus_scope.yaml,
  t/plugin/coverage_test.go, t/plugin/corpus_test.go,
  scripts/capability_status_gate_test.sh, .github/CODEOWNERS,
  .github/workflows/capability-status.yml, .github/workflows/unit-test.yml
permissions: contents read
steps: checkout, setup Go from go.mod, source .envrc and make check-capability-drift,
  source .envrc and make test-capability-status
forbidden: TestPluginIntegration, make test-integration, write-mode generation
```

- [ ] **Step 2: Run the script and verify the new workflow is absent**

Run: `bash scripts/capability_status_gate_test.sh`

Expected: FAIL with `missing capability status workflow`.

- [ ] **Step 3: Add exact Make targets**

Add:

```make
.PHONY: generate-capabilities
generate-capabilities:
	go run ./cmd/capability-gen -repo-root . -write

.PHONY: check-capability-drift
check-capability-drift:
	go run ./cmd/capability-gen -repo-root . -check

.PHONY: test-capability-status
test-capability-status:
	go test ./pkg/capability ./pkg/config ./pkg/plugin -run '^(TestLoadedManifest|TestManifest|TestProfileSelection|TestCapabilityManifest|TestCapabilityRegistry)' -count=1
	APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./t/plugin -run '^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestUpstreamCorpusAccountingWithoutSourceCheckout|TestCorpusEvidenceMatchesCompatibilityTarget)$$' -count=1
```

Remove `test-plugin-status`; do not leave it as an alias.

- [ ] **Step 4: Create the required read-only workflow**

Create `.github/workflows/capability-status.yml` matching the tested contract. Every Go/Make line must execute through `bash -lc 'source .envrc && ...'`. The workflow must run `-check`, never `-write`.

- [ ] **Step 5: Delete the replaced workflow and script**

Delete `.github/workflows/plugin-status.yml` and `scripts/plugin_status_gate_test.sh` in this task. Update `.github/workflows/unit-test.yml` contract references from plugin status to capability status without adding broad tests. Update `scripts/release_gate_test.sh` in the same atomic cutover: it must require `.github/workflows/capability-status.yml`, the three new Make targets, and the capability workflow's contract-test/drift/status steps, and must not retain any path, target, job, or diagnostic that names the deleted plugin-status workflow. Update `docs/runbooks/production-release.md` to name `Capability Status Contract` and describe the manifest/ADR drift gate; do not leave an active release instruction requiring the deleted check.

- [ ] **Step 6: Run the local workflow and drift gates**

Run: `bash -lc 'source .envrc && bash scripts/capability_status_gate_test.sh && bash scripts/release_gate_test.sh && make check-capability-drift && make test-capability-status'`

Expected: all commands PASS; no real-process `t/plugin` case starts.

- [ ] **Step 7: Run build smoke and inspect the atomic deletion**

Run: `bash -lc 'source .envrc && make build && git diff --check && test ! -e .github/workflows/plugin-status.yml && test ! -e scripts/plugin_status_gate_test.sh'`

Expected: PASS.

- [ ] **Step 8: Commit the governance workflow cutover**

```bash
git add Makefile .github/workflows scripts docs/runbooks/production-release.md
git commit -m "ci(governance): enforce capability manifest drift gate"
```

### Task 10: Run the Governance Milestone Acceptance and Dead-Truth Scan

**Files:**
- Verify: `pkg/capability/**`
- Verify: `pkg/config/profiles.go`
- Verify: `pkg/plugin/registry_gen.go`
- Verify: `docs/plugins.md`, `docs/architecture/**`, `README.md`, `README.zh-CN.md`
- Verify: `t/plugin/corpus_scope.yaml`, `.github/workflows/capability-status.yml`

**Interfaces:**
- Consumes: all Task 1–9 outputs
- Produces: accepted `capability.Manifest` and `config.ProfileSelection` for plan 02

- [ ] **Step 1: Prove no editable duplicate plugin-status database remains**

Run:

```bash
rg -n 'single source of truth|factory count =|want 115|want 114|supported plugins =|want 100|HTTPDataPlaneV1Profile|deployment\.profile' \
  pkg t/plugin docs README*.md --glob '*.go' --glob '*.md' --glob '*.yaml'
```

Expected: numeric inventory/status claims appear only in historical archives or generated output; legacy profile symbols have no production or test call site.

- [ ] **Step 2: Prove the generated registry has no handwritten proxy**

Run: `rg -n 'var pluginRegistry|func capabilityManifestEntries|generatedCapabilityManifestEntries' pkg/plugin --glob '*.go'`

Expected: one generated `pluginRegistry`, one generated declarative entry function, and runtime consumption of that function; no second handwritten table.

- [ ] **Step 3: Run the complete impact-scoped governance gate**

Run: `bash -lc 'source .envrc && go test ./pkg/capability ./pkg/config ./pkg/plugin -run "^(TestLoadedManifest|TestManifest|TestParse|TestProfileSelection|TestCapabilityManifest|TestCapabilityRegistry|TestNew)" -count=1 && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./t/plugin -run "^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestCorpusScope|TestUpstreamCorpusAccountingWithoutSourceCheckout|TestCorpusEvidenceMatchesCompatibilityTarget)$" -count=1 && make check-capability-drift && make build'`

Expected: PASS. A stale, flaky, missing, or deferred required claim excludes its plugin from qualification; an explicitly inapplicable dimension remains accounted for without blocking it.

- [ ] **Step 4: Check formatting and unowned changes**

Run: `git diff --check && git status --short`

Expected: only governance-plan paths are changed; the four pre-existing untracked review documents remain unmodified and uncommitted.

- [ ] **Step 5: Record milestone completion**

```bash
git add pkg/capability pkg/config pkg/plugin cmd/capability-gen t/plugin \
  docs/plugins.md docs/history docs/architecture docs/design.md \
  docs/reviews/convergence-decisions.md docs/configuration.md \
  docs/production-profile.md README.md README.zh-CN.md conf Makefile scripts .github
git commit -m "feat(governance): establish generated compatibility truth"
```

Skip this commit if Tasks 1–9 already left no uncommitted governance diff; never create an empty commit.

## Self-Review Results

- **Spec coverage:** Tasks 1–3 establish the central manifest and eliminate duplicate registration facts; Task 4 creates the three profile axes; Task 5 separates behavior from evidence and accounts for the pinned-corpus mismatch; Task 6 generates status; Task 7 gates divergences through owner-approved ADRs; Task 8 supersedes conflicting documents without deleting evidence; Task 9 makes drift a required CI failure; Task 10 verifies the milestone interface for downstream plans. No governance requirement from the program specification is unassigned.
- **Placeholder scan:** The plan contains no deferred implementation marker, generic error-handling instruction, unspecified test request, or reference to an undefined public function. Every code-producing task names concrete functions, files, commands, and pass/fail outcomes.
- **Type consistency:** `pkg/capability` owns `Manifest`, `Load`, `Plugin`, `Qualification`, and `QualifiedPlugins` and never imports `pkg/config`; `pkg/config` owns all three profile types and `ProfileSelection.Validate(*capability.Manifest)`. `Qualification` returns copied domains/plugins/evidence slices. `QualifiedPlugins` consistently returns sorted factory/config keys, uses `nil` only for an unknown qualification profile, and returns a non-nil empty slice for a known profile with no passing plugins.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-23-governance-manifest-profiles.md`. Execute it task-by-task with `superpowers:subagent-driven-development` (recommended, fresh implementation worker and review between tasks) or `superpowers:executing-plans` (inline batches with checkpoints). This child plan must complete before `2026-08-23-static-effective-config.md` begins.
