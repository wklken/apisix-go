# Static Effective Configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Viper-defined process-global configuration with a presence-aware, provenance-carrying `config.EffectiveConfig`, explicit profile axes, side-effect-free validation commands, and explicitly injected static configuration and data-encryption dependencies.

**Architecture:** Parse YAML into an internal typed node tree that preserves presence, `null`, exact numbers, and source metadata; merge builtin/default-file/override-file/APISIX-template/APISIXGO/CLI layers explicitly; then decode and validate once into immutable `EffectiveConfig`. Cut the running process over atomically: `cmd` loads one manifest and one effective configuration, passes both configuration and an immutable data-encryption service into `server`, and all downstream server, route, store, metric, and plugin owners receive dependencies explicitly rather than reading package globals.

**Tech Stack:** Go 1.26, Cobra, `go.yaml.in/yaml/v3`, `github.com/go-viper/mapstructure/v2`, existing `pkg/capability`, existing `pkg/data_encryption`, standard-library `encoding/json`, reflection only for schema indexing and redacted rendering.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is Apache APISIX 3.17.0 at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.
- The APISIX environment syntax is `${{NAME}}` and `${{NAME:=fallback}}`, including substitutions in YAML keys; a missing variable without a fallback is an error.
- The Go extension override namespace is `APISIXGO_*`; it is separate from APISIX template substitution and is applied after both files.
- Merge order is builtin defaults, default file, override file, APISIXGO environment overrides, then CLI overrides. APISIX `${{...}}` substitution occurs inside each parsed file layer and records the environment variable as the winning source.
- Nested mappings merge recursively. A sequence replaces the lower sequence. Explicit `null` replaces the lower value and remains present in `Provenance`.
- Compatibility, security, and qualification are independent: `apisix-3.17`, `compat|strict`, and empty or `http-data-plane-v1` respectively.
- `config.LoadEffective` must be deterministic from `LoadRequest`; it must not call `os.Getenv`, read a global manifest, publish a global config, configure encryption, open bbolt, bind a socket, start a provider, or create a goroutine.
- `LoadRequest.Manifest == nil` is a stable configuration error. Bootstrap obtains the manifest once with `capability.Load()` and passes it explicitly.
- Bootstrap also calls `DefaultRuntimePaths()` once and passes the result as `LoadRequest.DefaultPaths`; `LoadEffective` never discovers platform directories itself.
- Runtime-path overrides live only at `apisix_go.runtime_paths.{data_dir,runtime_dir,log_dir,temp_dir}`. Their environment aliases omit the repeated `apisix_go` segment: `APISIXGO_RUNTIME_PATHS_*`; CLI uses the full dotted path.
- Keep exact integers and `json.Number`; never decode an untyped configuration number through `float64`.
- Compatibility mode retains unknown static fields in provenance as ignored paths; strict mode rejects them. Removed `deployment.profile` is rejected in every mode with the three replacement field names.
- No secret value may appear in config errors, effective-config output, provenance origins, logs, or test failure strings.
- Source `.envrc` before every Go or Make command.
- Use impact-scoped tests; do not run `go test ./...`, `go test ./pkg/...`, or `make test`.
- Run `source .envrc && make build` after the atomic runtime cutover and final CLI work.
- Do not retain `config.Load`, `loadConfigFiles`, `config.GlobalConfig`, `data_encryption.Configure`, `data_encryption.Keyring`, an optional dependency fallback, or a proxy-only facade after cutover.
- Do not edit or stage the four user-owned untracked files under `docs/reviews/`.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `pkg/config/effective.go` | Public `LoadRequest`, `EffectiveConfig`, `Provenance`, and source types fixed by the program plan. |
| `pkg/config/profiles.go` | Profile types and manifest-evidence validation produced by plan 01; this plan only consumes them. |
| `pkg/config/qualification.go` | Effective enabled-plugin set validation for a selected qualification profile. |
| `pkg/config/paths.go` | Cross-platform bootstrap defaults, canonical effective runtime paths, and the durable journal path. |
| `pkg/config/node.go` | Internal node representation that distinguishes map/list/scalar/null and carries leaf provenance. |
| `pkg/config/document.go` | YAML-node parsing, exact scalar conversion, duplicate-key rejection, and APISIX `${{...}}` expansion. |
| `pkg/config/merge.go` | Recursive map merge, sequence replacement, explicit-null replacement, and provenance flattening. |
| `pkg/config/extension_env.go` | Reflection-derived `APISIXGO_*` index and deterministic extension overlays. |
| `pkg/config/defaults.go` | The only Go-runtime builtin static defaults not supplied by the pinned APISIX default file. |
| `pkg/config/decode.go` | Presence-aware typed decode, exact-number range checks, and unused-field collection. |
| `pkg/config/validation.go` | Runtime, security-profile, qualification-profile, and removed-field validation. |
| `pkg/config/redact.go` | Deterministic redacted effective-config document; unknown paths are listed without values. |
| `pkg/config/init.go` | Final `LoadEffective` pipeline only; historical Viper loader and globals are deleted. |
| `pkg/data_encryption/service.go` | Immutable encryption/decryption owner constructed from effective static config. |
| `pkg/plugin/base/types.go`, `pkg/plugin/types.go`, `pkg/plugin/init.go` | Explicit immutable static config and secret resolver injection into plugin instances. |
| `pkg/server/server.go` | Owns the effective config and encryption service and passes them down. |
| `cmd/config.go` | `apisix config test` and `apisix config dump --effective --redacted`. |

The runtime cutover table in Task 4 lists every production global reader. Its replacements are part of one commit because decision 196C forbids a committed dual path.

### Task 1: Define Effective Configuration and Qualification-Set Interfaces

**Files:**
- Create: `pkg/config/effective.go`
- Create: `pkg/config/qualification.go`
- Create: `pkg/config/paths.go`
- Test: `pkg/config/effective_contract_test.go`

**Interfaces:**
- Consumes: `config.ProfileSelection`, `config.ProfileSelection.Validate(*capability.Manifest) error`, `capability.Load()`, `capability.Manifest.Qualification(name string) (capability.QualificationProfile, bool)`, and `capability.Manifest.QualifiedPlugins(name string) []string` from plan 01. `ProfileSelection.Validate` already enforces compatibility `apisix-3.17`, security `compat|strict`, qualification existence, and exact equality between required and evidence-qualified plugin sets without reading `Config`.
- Produces: `config.EffectiveConfig`, `config.Provenance`, `config.LoadRequest`, `config.RuntimePaths`, `config.DefaultRuntimePaths() (RuntimePaths, error)`, `config.JournalPath(*EffectiveConfig) string`, and `config.ValidateQualificationPlugins([]string, ProfileSelection, *capability.Manifest) error`.

- [ ] **Step 1: Write the effective-config and enabled-plugin contract tests**

```go
func TestEffectiveConfigContractCarriesProfilesAndProvenance(t *testing.T) {
	effective := EffectiveConfig{
		Provenance: Provenance{"proxy.max_in_flight": {
			Kind: SourceCLI, Origin: "proxy.max_in_flight", Explicit: true,
		}},
		Profiles: ProfileSelection{Compatibility: CompatibilityAPISIX317, Security: SecurityCompat},
		Paths: RuntimePaths{DataDir: "/var/lib/apisix-go"},
	}
	if effective.Provenance["proxy.max_in_flight"].Kind != SourceCLI { t.Fatal("source kind was lost") }
	if effective.Profiles.Compatibility != CompatibilityAPISIX317 { t.Fatal("profile was lost") }
	if got := JournalPath(&effective); got != filepath.Join("/var/lib/apisix-go", "apisix-go-store.db") {
		t.Fatalf("JournalPath() = %q", got)
	}
}

func TestJournalPathRejectsMissingOrNonAbsoluteDataDir(t *testing.T) {
	for _, effective := range []*EffectiveConfig{nil, {}, {Paths: RuntimePaths{DataDir: "relative"}}} {
		if got := JournalPath(effective); got != "" { t.Fatalf("JournalPath() = %q, want empty", got) }
	}
}

func TestDefaultRuntimePathsAreAbsoluteAndNonEmpty(t *testing.T) {
	paths, err := DefaultRuntimePaths()
	if err != nil { t.Fatal(err) }
	for name, path := range map[string]string{
		"data_dir": paths.DataDir, "runtime_dir": paths.RuntimeDir,
		"log_dir": paths.LogDir, "temp_dir": paths.TempDir,
	} {
		if path == "" || !filepath.IsAbs(path) { t.Fatalf("%s = %q", name, path) }
	}
}

func TestValidateQualificationPluginsReportsStableSetDifference(t *testing.T) {
	manifest := qualifiedProfileTestManifest(t)
	profile, ok := manifest.Qualification("http-data-plane-v1")
	if !ok || len(profile.RequiredPlugins) == 0 { t.Fatal("qualification profile has no required plugins") }
	selection := ProfileSelection{
		Compatibility: CompatibilityAPISIX317,
		Security: SecurityStrict,
		Qualification: QualificationHTTPDataPlaneV1,
	}
	enabled := append([]string(nil), profile.RequiredPlugins[1:]...)
	enabled = append(enabled, "zz-extra")
	err := ValidateQualificationPlugins(enabled, selection, manifest)
	want := fmt.Sprintf("qualification_profile http-data-plane-v1: missing plugins [%s]; unexpected plugins [zz-extra]",
		profile.RequiredPlugins[0])
	if err == nil || err.Error() != want {
		t.Fatalf("ValidateQualificationPlugins() error = %v", err)
	}
}
```

- [ ] **Step 2: Run the focused tests and confirm the plan-02 contracts do not exist**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^Test(EffectiveConfigContract|DefaultRuntimePaths|ValidateQualificationPlugins)" -count=1'`

Expected: FAIL with undefined `EffectiveConfig`, `SourceCLI`, and `ValidateQualificationPlugins`; plan-01 profile symbols compile.

- [ ] **Step 3: Add the exact public contracts**

```go
// pkg/config/effective.go
package config

import "github.com/wklken/apisix-go/pkg/capability"

type SourceKind string

const (
	SourceBuiltin     SourceKind = "builtin"
	SourceDefaultFile SourceKind = "default_file"
	SourceOverrideFile SourceKind = "override_file"
	SourceAPISIXEnv   SourceKind = "apisix_env"
	SourceAPISIXGOEnv SourceKind = "apisixgo_env"
	SourceCLI         SourceKind = "cli"
)

type FieldSource struct {
	Kind     SourceKind `json:"kind"`
	Origin   string     `json:"origin"`
	Explicit bool       `json:"explicit"`
}

type Provenance map[string]FieldSource

type EffectiveConfig struct {
	Config     Config
	Provenance Provenance
	Profiles   ProfileSelection
	Paths      RuntimePaths
}

type LoadRequest struct {
	DefaultPath  string
	OverridePath string
	DefaultPaths RuntimePaths
	Environment  map[string]string
	CLIOverrides map[string]any
	Manifest     *capability.Manifest
}
```

```go
// pkg/config/paths.go
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type RuntimePaths struct {
	DataDir    string `mapstructure:"data_dir" json:"data_dir"`
	RuntimeDir string `mapstructure:"runtime_dir" json:"runtime_dir"`
	LogDir     string `mapstructure:"log_dir" json:"log_dir"`
	TempDir    string `mapstructure:"temp_dir" json:"temp_dir"`
}

func DefaultRuntimePaths() (RuntimePaths, error) {
	configDir, err := os.UserConfigDir()
	if err != nil { return RuntimePaths{}, fmt.Errorf("resolve user config directory: %w", err) }
	cacheDir, err := os.UserCacheDir()
	if err != nil { return RuntimePaths{}, fmt.Errorf("resolve user cache directory: %w", err) }
	paths := RuntimePaths{
		DataDir: filepath.Join(configDir, "apisix-go", "data"),
		RuntimeDir: filepath.Join(cacheDir, "apisix-go", "run"),
		LogDir: filepath.Join(cacheDir, "apisix-go", "log"),
		TempDir: filepath.Join(os.TempDir(), "apisix-go"),
	}
	for name, path := range map[string]string{"data_dir": paths.DataDir, "runtime_dir": paths.RuntimeDir,
		"log_dir": paths.LogDir, "temp_dir": paths.TempDir} {
		if path == "" || !filepath.IsAbs(path) { return RuntimePaths{}, fmt.Errorf("default %s is not absolute", name) }
	}
	return paths, nil
}

func JournalPath(e *EffectiveConfig) string {
	if e == nil || e.Paths.DataDir == "" || !filepath.IsAbs(e.Paths.DataDir) { return "" }
	return filepath.Join(e.Paths.DataDir, "apisix-go-store.db")
}
```

`JournalPath` returns empty for nil effective configuration and for empty or non-absolute `DataDir`, so callers fail before filesystem access and cannot silently recreate a cwd-relative journal. Runtime construction later rejects invalid effective configuration. Do not call `os.MkdirAll` in either path function.

```go
// pkg/config/qualification.go
package config

import (
	"fmt"
	"slices"
	"sort"

	"github.com/wklken/apisix-go/pkg/capability"
)

func ValidateQualificationPlugins(enabled []string, selection ProfileSelection, manifest *capability.Manifest) error {
	if err := selection.Validate(manifest); err != nil { return err }
	if selection.Qualification == "" {
		return nil
	}
	profile, ok := manifest.Qualification(string(selection.Qualification))
	if !ok {
		return fmt.Errorf("qualification_profile %q is not declared by the capability manifest", selection.Qualification)
	}
	want := append([]string(nil), profile.RequiredPlugins...)
	got := append([]string(nil), enabled...)
	sort.Strings(want)
	sort.Strings(got)
	missing := make([]string, 0)
	unexpected := make([]string, 0)
	for _, name := range want {
		if !slices.Contains(got, name) { missing = append(missing, name) }
	}
	for _, name := range got {
		if !slices.Contains(want, name) { unexpected = append(unexpected, name) }
	}
	if len(missing) != 0 || len(unexpected) != 0 {
		return fmt.Errorf("qualification_profile %s: missing plugins %v; unexpected plugins %v",
			selection.Qualification, missing, unexpected)
	}
	return nil
}
```

Tests reuse the existing `qualifiedProfileTestManifest(t)` helper, which loads an independent embedded manifest and removes only the test copy's evidence requirements so set-difference behavior is reachable. Also assert that reordered complete input passes, missing/unexpected output is stable, and neither the enabled input nor the manifest required slice is mutated. Do not construct a second manifest representation in `pkg/config`. The new helper intentionally compares plugin membership independent of order; the existing runtime remains order-sensitive until Task 4 atomically switches consumers and updates its old order-rejection test.

- [ ] **Step 4: Re-run the existing dependency-thinning contract**

Run the existing `TestConfigPackageDoesNotDepend*` dependency-thinning contract and rely on Go compilation to reject an import cycle. The existing test bans ingress-controller and Kubernetes dependencies; it does not itself assert the `config -> capability` edge, so do not claim or duplicate a nonexistent import-boundary assertion.

- [ ] **Step 5: Run the focused interface tests**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^(TestEffectiveConfigContract|TestDefaultRuntimePaths|TestProfileSelection|TestValidateQualificationPlugins|TestConfigPackageDoesNotDepend)" -count=1'`

Expected: PASS.

- [ ] **Step 6: Commit the interface contract**

```bash
git add pkg/config/effective.go pkg/config/qualification.go pkg/config/paths.go pkg/config/effective_contract_test.go
git commit -m "feat(config): define effective configuration contract"
```

### Task 2: Parse Presence, Exact Numbers, and APISIX Environment Templates

**Files:**
- Create: `pkg/config/node.go`
- Create: `pkg/config/document.go`
- Test: `pkg/config/document_test.go`

**Interfaces:**
- Consumes: `config.FieldSource` and source constants from Task 1.
- Produces: internal `valueNode`, `parseDocument([]byte, FieldSource, map[string]string) (*valueNode, error)`, `readConfigDocument(string, FieldSource, map[string]string) (*valueNode, error)`, `nodeToAny(*valueNode) any`, and `cloneNode(*valueNode) *valueNode` used by merge and typed decode.

- [ ] **Step 1: Write parser tests for all distinct value states and exact numbers**

```go
func TestParseDocumentPreservesPresenceKindsAndExactNumbers(t *testing.T) {
	doc, err := parseDocument([]byte(`
absent_parent: {present_null: null, disabled: false, zero: 0, empty: ""}
plugin_attr: {prometheus: {large: 9007199254740993}}
`), FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true}, nil)
	if err != nil { t.Fatal(err) }
	if _, ok := doc.mapping["absent"]; ok { t.Fatal("absent path became present") }
	parent := doc.mapping["absent_parent"]
	if parent.mapping["present_null"].kind != nodeNull { t.Fatal("null kind was lost") }
	if got := parent.mapping["disabled"].scalar; got != false { t.Fatalf("disabled = %#v", got) }
	if got := parent.mapping["zero"].scalar; got != json.Number("0") { t.Fatalf("zero = %#v", got) }
	if got := parent.mapping["empty"].scalar; got != "" { t.Fatalf("empty = %#v", got) }
	large := doc.mapping["plugin_attr"].mapping["prometheus"].mapping["large"].scalar
	if large != json.Number("9007199254740993") { t.Fatalf("large = %#v", large) }
}

func TestParseDocumentRejectsDuplicateMappingKeys(t *testing.T) {
	_, err := parseDocument([]byte("apisix:\n  enable_http2: true\n  enable_http2: false\n"),
		FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate key apisix.enable_http2") {
		t.Fatalf("parseDocument() error = %v", err)
	}
}
```

Extend this group beyond the abbreviated examples above. Cover absent versus
explicit `null`, `false`, zero, empty string, sequence and mapping presence;
integers beyond both `float64`'s exact range and `uint64`; YAML hexadecimal,
octal, binary, legacy-octal and underscored integers normalized to exact base-10
`json.Number`; finite high-precision decimals normalized lexically without a
`float64` round trip; `.5`, `-.5`, `1.`, and a leading `+`; and fail-closed
rejection of every infinity/NaN spelling without including the rejected scalar
in the error. `nodeToAny` must retain `json.Number` values.

Also test that a second YAML document is rejected and that `cloneNode` deeply
copies mappings and sequences while preserving every scalar, source, and
`pathBase`. An empty or comment-only document is an empty mapping/no-op layer;
an explicit YAML `null` document remains `nodeNull`. YAML anchors, aliases, and merge keys are not silently expanded in
this milestone: reject an anchored node, an alias node, or `!!merge` with a
stable field-path-only error. This is a fail-closed, explicitly unqualified
YAML-syntax gap, not evidence of full APISIX configuration parity; supporting
it requires a later compatibility decision and differential corpus. A quoted
or explicit-`!!str` key whose literal value is `<<` is an ordinary string key,
not a merge key, and must remain accepted.

- [ ] **Step 2: Write APISIX 3.17 environment-template tests**

The pinned contract comes from `conf/config.yaml` and `apisix/cli/file.lua` at
APISIX `3.17.0` (`9ef2ecab67f652d38365049613610ef649bb4ad0`): substitution is
allowed in values and keys, outer whitespace in `${{ VAR }}` is accepted, a
missing required variable errors, and `:=` supplies a trimmed fallback only
when the name is absent from `LoadRequest.Environment`. A present variable with
an empty value wins over the fallback. Expansion may occur multiple times in a
single string. The implementation receives an explicit environment snapshot
and must never call `os.Getenv`.

```go
func TestParseDocumentExpandsAPISIXEnvironmentInKeysAndValues(t *testing.T) {
	doc, err := parseDocument([]byte(`
deployment: {etcd: {host: ["http://${{ETCD_HOST}}:2379"]}}
plugin_attr: {nodes: {"${{HOST}}:${{PORT:=9080}}": 1}}
empty_fallback: "${{OPTIONAL:=}}"
`), FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true},
		map[string]string{"ETCD_HOST": "etcd.internal", "HOST": "127.0.0.1"})
	if err != nil { t.Fatal(err) }
	if got := doc.mapping["deployment"].mapping["etcd"].mapping["host"].sequence[0].scalar;
		got != "http://etcd.internal:2379" { t.Fatalf("host = %#v", got) }
	nodes := doc.mapping["plugin_attr"].mapping["nodes"]
	if _, ok := nodes.mapping["127.0.0.1:9080"]; !ok { t.Fatalf("expanded keys = %#v", nodes.mapping) }
	if got := doc.mapping["empty_fallback"].scalar; got != "" { t.Fatalf("empty_fallback = %#v", got) }
	if got := doc.mapping["deployment"].mapping["etcd"].mapping["host"].sequence[0].source;
		got.Kind != SourceAPISIXEnv || got.Origin != "ETCD_HOST" || !got.Explicit {
		t.Fatalf("source = %+v", got)
	}
}

func TestParseDocumentRejectsMissingAPISIXEnvironment(t *testing.T) {
	_, err := parseDocument([]byte("value: '${{MISSING}}'\n"),
		FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true}, map[string]string{})
	if err == nil || err.Error() != "expand APISIX environment at value: MISSING is not set" {
		t.Fatalf("parseDocument() error = %v", err)
	}
}
```

Add cases for templates in mapping keys, scalar values, and sequence values;
multiple variables; `${{ VAR }}` outer whitespace; trimmed and empty fallbacks;
present-empty values; and a stable first-missing-variable error in textual
order. When expansion was used and the complete expanded scalar is `true`,
`false`, or a supported finite numeric spelling, retype it exactly as APISIX
does after substitution: booleans remain booleans and numbers become exact
`json.Number`; a result such as `prefix-${{PORT}}` remains a string. This rule
also applies when the YAML scalar containing the template was quoted.

APISIX performs this post-expansion numeric decision with Lua `tonumber`, not
with YAML scalar grammar. Implement a separate exact lexical path: decimal
`077` and `08` become decimal 77 and 8; signed decimal integers, decimal
fractions/exponents, and LuaJIT-supported hexadecimal and binary integers are
numeric; YAML-only `0o17` and underscored `1_000` remain strings. Do not reuse
legacy-octal YAML normalization and do not introduce a `float64` precision
round trip. LuaJIT also accepts hexadecimal floating-point strings; this
milestone leaves those as strings because no exact bounded conversion is yet
defined. Treat that as an explicit unqualified differential gap, not APISIX
parity, until a later corpus and owner decision retire it.

Template provenance is node-observable. Variables used by a template key apply
to the mapping/sequence containers and every descendant leaf below the expanded
key; variables used by a descendant value are unioned with them. Sort and deduplicate the names before storing
`FieldSource{Kind: SourceAPISIXEnv, Origin: strings.Join(names, ","), Explicit: true}`.
Expansion must retain the original file `pathBase`.
Reject an APISIX environment expansion whose resulting key or value is not
valid UTF-8 before it enters the node tree; the error contains only the safe
field location, never the environment value.

Duplicate literal keys may name the literal field path. A collision caused by
one or more expanded keys must report the unexpanded parent path and involved
environment variable names, never the expanded key, environment value,
fallback text, or original scalar. Add a sentinel-secret test for this rule and
for all other parser errors.

- [ ] **Step 3: Run the parser tests and verify the parser is absent**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^TestParseDocument" -count=1'`

Expected: FAIL with undefined `parseDocument` and node kind names.

- [ ] **Step 4: Implement the internal node and exact scalar conversion**

```go
type nodeKind uint8

const (
	nodeNull nodeKind = iota
	nodeScalar
	nodeMapping
	nodeSequence
)

type valueNode struct {
	kind     nodeKind
	scalar   any
	mapping  map[string]*valueNode
	sequence []*valueNode
	source   FieldSource
	pathBase string
}
```

`parseDocument` sets `pathBase=filepath.Dir(source.Origin)` on every node from a default or override file and preserves it when APISIX expansion changes the public source to an environment variable; `cloneNode` and merge must retain it. This internal-only base lets a relative runtime path from a `${{...}}` value resolve against the file that contained the template without exposing a second value in provenance.

In `document.go`, decode through `yaml.Node`, treat EOF before content as an
empty mapping/no-op layer, and otherwise require exactly one document,
reject duplicate literal and expanded mapping keys under the rules above, and
retain explicit `!!null`. Reject anchors, aliases, and nodes tagged as YAML
merge keys explicitly rather
than letting library-specific expansion bypass duplicate-key or provenance
checks; do not reject a quoted or explicit-string `<<` key. Convert `!!int` lexically with `math/big.Int` so YAML base prefixes
become an exact base-10 `json.Number`. Normalize finite `!!float` lexically into
JSON number syntax (remove underscores and add a missing leading/trailing zero)
without `float64`; reject all infinity and NaN spellings with only the field
path. Convert booleans with `strconv.ParseBool`; leave timestamps and all other
scalar tags as strings so configuration decode remains explicit.

Traverse mapping keys as well as values with a scanner that supports multiple
`${{...}}` occurrences. It must accept whitespace immediately inside the braces,
require an identifier matching `[A-Za-z_][A-Za-z0-9_]*`, recognize optional
`:=fallback`, trim the fallback as pinned APISIX does, and preserve the first
missing-variable error instead of overwriting it in a replacement callback.
Do not use the earlier single-regexp sketch: it neither models the upstream
whitespace/fallback contract nor safely preserves the first error. Apply key
template source names recursively to descendant leaves, union them with value
template names, and never include substituted or fallback values in `Origin` or
errors.

`cloneNode(nil)` returns nil. Every other node is deeply copied, including maps,
sequences, `FieldSource`, and `pathBase`, so Task 3 does not alias parser-owned
state.

Implement the file boundary with no ambient reads:

```go
func readConfigDocument(path string, source FieldSource, env map[string]string) (*valueNode, error) {
	data, err := os.ReadFile(path)
	if err != nil { return nil, fmt.Errorf("read static configuration %s: %w", path, err) }
	doc, err := parseDocument(data, source, env)
	if err != nil { return nil, fmt.Errorf("parse static configuration %s: %w", path, err) }
	return doc, nil
}
```

- [ ] **Step 5: Run parser tests**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^Test(ParseDocument|ReadConfigDocument|NodeToAny|CloneNode)" -count=1'`

Expected: PASS.

- [ ] **Step 6: Commit the inert parser**

```bash
git add pkg/config/node.go pkg/config/document.go pkg/config/document_test.go
git commit -m "feat(config): preserve static configuration presence"
```

### Task 3: Merge Layers and Record Field Provenance

**Files:**
- Create: `pkg/config/merge.go`
- Create: `pkg/config/extension_env.go`
- Test: `pkg/config/merge_test.go`
- Test: `pkg/config/extension_env_test.go`

**Interfaces:**
- Consumes: `valueNode`, `cloneNode`, `Config`, and `Provenance`; `LoadEffective` in Task 4 passes the two override maps from `LoadRequest`, but no Task 3 function consumes `LoadRequest` directly.
- Produces: `mergeNodes(lower, upper *valueNode) *valueNode`, `flattenProvenance(*valueNode) Provenance`, `nodeFromAny(any, FieldSource) (*valueNode, error)`, `mustNodeFromAny(any, FieldSource) *valueNode`, `ValidateStaticOverridePath(string) error`, `applyAPISIXGO(*valueNode, map[string]string) error`, and `applyCLIOverrides(*valueNode, map[string]any) error`.

- [ ] **Step 1: Write the precedence, recursive-map, sequence, and null tests**

```go
func TestMergePresenceUsesExplicitPrecedence(t *testing.T) {
	lower := mustParseDocument(t, `section: {keep: lower, replace: lower, list: [a, b], nullable: lower}`,
		FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"})
	upper := mustParseDocument(t, `section: {replace: "", list: [c], nullable: null}`,
		FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true})
	merged := mergeNodes(lower, upper)
	section := merged.mapping["section"]
	if section.mapping["keep"].scalar != "lower" { t.Fatal("recursive map lost lower key") }
	if section.mapping["replace"].scalar != "" { t.Fatal("explicit empty did not replace") }
	if len(section.mapping["list"].sequence) != 1 { t.Fatal("sequence did not replace") }
	if section.mapping["nullable"].kind != nodeNull { t.Fatal("null did not replace") }
	provenance := flattenProvenance(merged)
	if provenance["section.keep"].Kind != SourceDefaultFile { t.Fatalf("keep = %+v", provenance["section.keep"]) }
	if provenance["section.nullable"].Kind != SourceOverrideFile { t.Fatalf("nullable = %+v", provenance["section.nullable"]) }
}
```

- [ ] **Step 2: Write the APISIXGO and CLI overlay tests**

```go
func TestExtensionAndCLIOverridesWinWithoutAmbientEnvironment(t *testing.T) {
	root := mustParseDocument(t, `proxy: {max_in_flight: 32}
plugins: [request-id, cors]
`, FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"})
	err := applyAPISIXGO(root, map[string]string{
		"APISIXGO_PROXY_MAX_IN_FLIGHT": "64",
		"UNRELATED": "ignored",
	})
	if err != nil { t.Fatal(err) }
	if err := applyCLIOverrides(root, map[string]any{"proxy.max_in_flight": 96}); err != nil { t.Fatal(err) }
	provenance := flattenProvenance(root)
	if got := provenance["proxy.max_in_flight"];
		got != (FieldSource{Kind: SourceCLI, Origin: "proxy.max_in_flight", Explicit: true}) {
		t.Fatalf("source = %+v", got)
	}
}

func TestRuntimePathExtensionUsesReservedAliasAndFullCLIPath(t *testing.T) {
	root := mustNodeFromAny(map[string]any{"apisix_go": map[string]any{"runtime_paths": map[string]any{
		"data_dir": "/default/data",
	}}}, FieldSource{Kind: SourceBuiltin, Origin: "test-defaults"})
	if err := applyAPISIXGO(root, map[string]string{
		"APISIXGO_RUNTIME_PATHS_DATA_DIR": "relative/env-data",
	}); err != nil { t.Fatal(err) }
	if err := applyCLIOverrides(root, map[string]any{
		"apisix_go.runtime_paths.data_dir": "relative/cli-data",
	}); err != nil { t.Fatal(err) }
	got := flattenProvenance(root)["apisix_go.runtime_paths.data_dir"]
	if got != (FieldSource{Kind: SourceCLI, Origin: "apisix_go.runtime_paths.data_dir", Explicit: true}) {
		t.Fatalf("source = %+v", got)
	}
}

func TestAPISIXGORejectsUnknownAndCollidingPaths(t *testing.T) {
	root := &valueNode{kind: nodeMapping, mapping: map[string]*valueNode{}}
	err := applyAPISIXGO(root, map[string]string{"APISIXGO_NOT_A_CONFIG_FIELD": "x"})
	if err == nil || err.Error() != "APISIXGO_NOT_A_CONFIG_FIELD does not map to a static configuration field" {
		t.Fatalf("applyAPISIXGO() error = %v", err)
	}
}
```

Extend these abbreviated examples with table-driven coverage for all merge
kind pairs, empty mappings and sequences, sequence replacement, nil arguments,
deep non-aliasing in both directions, and `pathBase` retention. Flattened
provenance records every non-root node: mapping containers (including empty
mappings), sequence containers (including empty sequences), sequence element
containers such as `apisix.node_listen[0]`, scalar leaves, and explicit nulls.
When two mappings merge recursively, the resulting mapping container takes the
upper mapping's `source` and `pathBase`; each child still records the source of
the layer that actually won that child.

Canonical provenance paths use dotted safe mapping segments and decimal
sequence brackets. A safe segment matches `[A-Za-z_][A-Za-z0-9_-]*`. Encode any
other mapping key as a bracketed JSON string, for example
`plugin_attr["tenant.a"].token`, so a literal dot or bracket in a dynamic key
cannot collide with a nested field path. Root unsafe keys begin with the same
bracket form. Encode the string with `encoding/json`, not `strconv.Quote`:
Go's `\xNN` escapes are not JSON. Include U+0001 in the tests and prove the
bracket content round-trips through `encoding/json`. Task 5's schema matcher
must consume this exact syntax.

Test `nodeFromAny` with every signed/unsigned integer width including
`math.MaxUint64`, named primitive types, `json.Number`, string-keyed named maps,
slices, arrays, nil, and typed-nil maps/slices. Named primitives are accepted by
underlying kind; typed nil becomes explicit null. Validate `json.Number`
immediately as a finite JSON number. Reject floats, pointers, structs, malformed
numbers, and non-string-key maps without printing their values. Verify that
`mustNodeFromAny` panics only for an invalid compile-time builtin. Reject invalid
UTF-8 scalar strings and map keys so `encoding/json` cannot replace distinct
byte strings with the same U+FFFD provenance path; include the invalid-byte vs
literal-U+FFFD collision case.

Both overlay functions are failure-atomic: sort and completely validate/convert
all selected inputs into detached nodes before changing `root`. On any error,
`root` and the caller-owned environment/CLI maps remain byte-for-byte and
alias-for-alias unchanged. APISIXGO ignores names without the `APISIXGO_`
prefix, rejects unknown prefixed names in sorted order, and never reads ambient
environment. CLI paths are sorted before validation, reject empty or empty
segments, and must be exact known static-schema leaves; compatibility-mode
retention of unknown *file* fields does not permit unknown CLI paths.

Add a reflected-type index-builder test seam so a synthetic struct can prove
alias collision rejection, including collision with one of the four reserved
runtime-path aliases. Test the exact four short reserved aliases and rejection
of `APISIXGO_APISIX_GO_RUNTIME_PATHS_*`. Recognize
`APISIXGO_DEPLOYMENT_PROFILE` and `deployment.profile` specially and return the
same error required for file input:

```text
deployment.profile was removed; use compatibility_target, security_profile, and qualification_profile
```

They are removed tombstones, not valid schema entries.

- [ ] **Step 3: Run the merge tests and confirm the functions are absent**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^(TestMerge|TestFlattenProvenance|TestNodeFromAny|TestMustNodeFromAny|TestExtension|TestAPISIXGO|TestCLI|TestOverlay|TestSetPath)" -count=1'`

Expected: FAIL with undefined merge and overlay functions.

- [ ] **Step 4: Implement immutable merge and provenance flattening**

`mergeNodes` must deep-clone the winning nodes so `LoadEffective` never aliases request-owned maps. Only two mapping nodes recurse; all other upper kinds replace the lower node. Empty upper mappings recurse and therefore do not erase lower children; empty upper sequences replace lower sequences. `nil` means an absent function argument, never explicit null. For recursive mappings, update the merged container metadata to the upper mapping's `source` and `pathBase` before merging children. `flattenProvenance` emits every non-root node under the canonical escaped-path contract above.

```go
func mergeNodes(lower, upper *valueNode) *valueNode {
	if lower == nil { return cloneNode(upper) }
	if upper == nil { return cloneNode(lower) }
	if lower.kind != nodeMapping || upper.kind != nodeMapping { return cloneNode(upper) }
	merged := cloneNode(lower)
	merged.source = upper.source
	merged.pathBase = upper.pathBase
	for key, incoming := range upper.mapping {
		merged.mapping[key] = mergeNodes(merged.mapping[key], incoming)
	}
	return merged
}
```

- [ ] **Step 5: Implement the deterministic APISIXGO index and CLI paths**

Walk `reflect.TypeFor[Config]()` through `mapstructure` tags. Recurse only into
struct fields; pointers, maps, slices, arrays, and scalar fields are schema
leaves. Skip unexported fields and `mapstructure:"-"`; use the field name only
when the tag name before comma options is empty. A leaf path
`proxy.max_in_flight` maps to `APISIXGO_PROXY_MAX_IN_FLIGHT`. Normalize both `.`
and `-` to `_`, so `ext-plugin.cmd` maps to
`APISIXGO_EXT_PLUGIN_CMD`; duplicate-alias rejection is mandatory because this
normalization is lossy.

Add exactly four reserved schema paths which are intentionally not fields on
`Config`: `apisix_go.runtime_paths.data_dir`, `.runtime_dir`, `.log_dir`, and
`.temp_dir`. Their environment aliases are
`APISIXGO_RUNTIME_PATHS_DATA_DIR`, `APISIXGO_RUNTIME_PATHS_RUNTIME_DIR`,
`APISIXGO_RUNTIME_PATHS_LOG_DIR`, and `APISIXGO_RUNTIME_PATHS_TEMP_DIR`; do not
generate `APISIXGO_APISIX_GO_*`. CLI keys use the complete
`apisix_go.runtime_paths.*` path. Build the reflected and reserved entries in one
index and reject all path or environment-name collisions deterministically.
Expose `ValidateStaticOverridePath` over that same immutable index; both
`applyCLIOverrides` and Task 5's `--set` parser use it, so there is no duplicate
schema source of truth.

APISIXGO values remain string scalar nodes and are converted only by typed
decode; an empty environment value is therefore explicit empty, not absence.
CLI values are converted recursively without a JSON float round trip. Implement
that conversion once as `nodeFromAny`; it accepts the exact types and semantics
defined in the test contract above. `mustNodeFromAny` wraps it only for
compile-time builtin literals and panics on programmer error; request data
always uses the error-returning function.

```go
func extensionEnvName(path string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return "APISIXGO_" + strings.ToUpper(replacer.Replace(path))
}

func mustNodeFromAny(value any, source FieldSource) *valueNode {
	node, err := nodeFromAny(value, source)
	if err != nil { panic(fmt.Sprintf("invalid builtin configuration: %v", err)) }
	return node
}

func setPath(root *valueNode, path string, value *valueNode) error {
	if root == nil || root.kind != nodeMapping { return fmt.Errorf("configuration root must be a mapping") }
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if segment == "" { return fmt.Errorf("configuration path %q contains an empty segment", path) }
	}
	current := root
	for _, segment := range segments[:len(segments)-1] {
		next := current.mapping[segment]
		if next == nil {
			next = &valueNode{
				kind: nodeMapping, mapping: make(map[string]*valueNode),
				source: value.source, pathBase: value.pathBase,
			}
			current.mapping[segment] = next
		}
		if next.kind != nodeMapping { return fmt.Errorf("configuration path %s crosses a non-mapping value", path) }
		current = next
	}
	current.mapping[segments[len(segments)-1]] = cloneNode(value)
	return nil
}
```

A created intermediate mapping inherits the inserted node's `source` and
`pathBase`; an existing mapping retains its metadata. Flatten provenance after
both APISIXGO and CLI create a missing container and assert that container and
leaf sources are valid. This is part of the failure-atomic overlay contract,
not a Task 5 repair.

- [ ] **Step 6: Run the layer tests**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^(TestMerge|TestFlattenProvenance|TestNodeFromAny|TestMustNodeFromAny|TestExtension|TestAPISIXGO|TestCLI|TestOverlay|TestSetPath)" -count=1'`

Expected: PASS.

- [ ] **Step 7: Commit the inert merge engine**

```bash
git add pkg/config/merge.go pkg/config/extension_env.go pkg/config/merge_test.go pkg/config/extension_env_test.go
git commit -m "feat(config): merge explicit configuration layers"
```

### Task 4: Atomically Cut Runtime Over to Effective Config and Explicit Encryption

**Files:**
- Create: `pkg/config/defaults.go`
- Create: `pkg/config/decode.go`
- Create: `pkg/config/validation.go`
- Create: `pkg/config/effective_test.go`
- Create: `pkg/data_encryption/service.go`
- Create: `pkg/data_encryption/service_test.go`
- Modify: `go.mod`
- Modify: `pkg/config/types.go`
- Rewrite: `pkg/config/init.go`
- Modify: `pkg/config/init_test.go`
- Modify: `pkg/config/release_gate_test.go`
- Modify: `pkg/config/trusted_addresses_validation_test.go`
- Modify: `pkg/config/standalone.go`
- Modify: `pkg/config/standalone_test.go`
- Modify: `pkg/data_encryption/data_encryption.go`
- Modify: `pkg/data_encryption/data_encryption_test.go`
- Modify: `pkg/data_encryption/resolver.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/reload.go`
- Modify: `pkg/server/tls.go`
- Modify tests: `pkg/server/*_test.go`
- Modify: `pkg/route/builder.go`
- Modify: `pkg/route/extra.go`
- Modify: `pkg/route/production_policy.go`
- Modify: `pkg/route/upstream_options.go`
- Modify tests: `pkg/route/*_test.go`
- Modify: `pkg/plugin/types.go`
- Modify: `pkg/plugin/init.go`
- Modify: `pkg/plugin/base/types.go`
- Modify: `pkg/plugin/proxy_cache/zones.go`
- Modify: `pkg/plugin/node_status/plugin.go`
- Modify: `pkg/plugin/request_context/plugin.go`
- Modify: `pkg/plugin/graphql_proxy_cache/plugin.go`
- Modify: `pkg/plugin/graphql_limit_count/plugin.go`
- Modify: `pkg/plugin/dubbo_proxy/transport.go`
- Modify: `pkg/plugin/server_info/plugin.go`
- Modify: `pkg/plugin/otel/provider.go`
- Modify: `pkg/plugin/skywalking/plugin.go`
- Modify: `pkg/plugin/log_rotate/plugin.go`
- Modify: `pkg/plugin/redirect/plugin.go`
- Modify the 16 plugin implementations returned by `rg -l 'data_encryption\.Keyring\(' pkg/plugin --glob '*.go' --glob '!*_test.go'`
- Modify focused tests in the corresponding plugin packages
- Modify: `pkg/store/store.go`
- Modify: `pkg/store/getter.go`
- Modify tests: `pkg/store/*_test.go`
- Modify: `pkg/apisix/id/id.go`
- Modify: `pkg/apisix/id/id_test.go`
- Modify: `pkg/observability/metrics/prometheus.go`
- Modify: `pkg/observability/metrics/prometheus_test.go`
- Modify every focused test and fixture affected by the Builder, Store, standalone-watcher, plugin-factory, metrics, ID, and handler signature changes; the exact inventory comes from the preflight `rg` commands below, not only the wildcard rows above.

**Interfaces:**
- Consumes: Tasks 1–3, `capability.Load() (*capability.Manifest, error)`, current `config.Config`, and current data-encryption primitives.
- Produces: `config.LoadEffective(config.LoadRequest) (*config.EffectiveConfig, error)`, `data_encryption.Service`, `base.Dependencies`, `plugin.New(string, base.Dependencies) Plugin`, `server.NewServer(*config.EffectiveConfig, data_encryption.Service) (*server.Server, error)`, and a runtime with no static-config or encryption global.

This is deliberately one implementation unit. Do not commit after adding `LoadEffective` while production still calls `config.Load`, and do not commit after injecting only some global readers. Temporary coexistence is allowed only inside the uncommitted worktree while the worker keeps the dependency order below; the final commit must compile with no adapter, overload, nil fallback, or old global path.

Implement in this internal order: (1) loader/decode/validation and the immutable-by-ownership encryption service; (2) plugin dependencies and factory; (3) Store plus standalone watcher; (4) all static/encryption readers and tests; (5) server and `cmd.Start`; (6) delete the old loader/globals and run the exact zero scans. Do not publish or checkpoint an intermediate stage.

Plan 04 consumes the immutable `data_encryption.Service` produced here. During its compiler cutover it atomically replaces the route/plugin `base.Dependencies` resolver field with its `secret.Materializer` and removes every direct plugin/base dependency on the raw plan-02 service or resolver; it must not retain both injection paths.

- [ ] **Step 1: Write the end-to-end loader tests**

```go
func TestLoadEffectiveAppliesAllLayersAndProvenance(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil { t.Fatal(err) }
	profile, ok := manifest.Qualification("http-data-plane-v1")
	if !ok { t.Fatal("http-data-plane-v1 qualification is missing") }
	defaults := writeConfigFile(t, "default.yaml", `
apisix: {node_listen: [{port: 9080}]}
proxy: {max_idle_conns: 10, max_idle_conns_per_host: 10, max_conns_per_host: 10, max_in_flight: 10}
nginx_config: {http: {client_max_body_size: 1024, client_body_timeout: 60s}}
plugins: `+mustYAMLInline(t, profile.RequiredPlugins)+`
deployment: {role: data_plane, role_data_plane: {config_provider: yaml}}
`)
	override := writeConfigFile(t, "override.yaml", `
proxy: {max_in_flight: 20}
compatibility_target: apisix-3.17
security_profile: compat
qualification_profile: http-data-plane-v1
`)
	effective, err := LoadEffective(LoadRequest{
		DefaultPath: defaults, OverridePath: override,
		Environment: map[string]string{"APISIXGO_PROXY_MAX_IN_FLIGHT": "30"},
		CLIOverrides: map[string]any{"proxy.max_in_flight": 40}, Manifest: manifest,
	})
	if err != nil { t.Fatal(err) }
	if effective.Config.Proxy.MaxInFlight != 40 { t.Fatalf("max_in_flight = %d", effective.Config.Proxy.MaxInFlight) }
	if got := effective.Provenance["proxy.max_in_flight"];
		got.Kind != SourceCLI || got.Origin != "proxy.max_in_flight" || !got.Explicit { t.Fatalf("source = %+v", got) }
	if effective.Profiles.Compatibility != CompatibilityAPISIX317 ||
		effective.Profiles.Security != SecurityCompat ||
		effective.Profiles.Qualification != QualificationHTTPDataPlaneV1 { t.Fatalf("profiles = %+v", effective.Profiles) }
}

func TestLoadEffectiveDistinguishesNullFalseZeroEmptyAndAbsent(t *testing.T) {
	effective := loadEffectiveFixture(t, `
debug: false
apisix: {id: "", node_listen: [{port: 9080}]}
proxy: {max_idle_conns: 1, max_idle_conns_per_host: 1, max_conns_per_host: 1, max_in_flight: 1}
nginx_config: {http: {client_max_body_size: 1, client_body_timeout: 1s}}
plugins: [request-id]
compatibility_target: apisix-3.17
security_profile: compat
deployment: {role: data_plane, role_data_plane: {config_provider: yaml}}
graphql: null
`)
	for _, path := range []string{"debug", "apisix.id", "graphql"} {
		if _, ok := effective.Provenance[path]; !ok { t.Fatalf("%s has no provenance", path) }
	}
	if _, ok := effective.Provenance["apisix.enable_dev_mode"]; ok { t.Fatal("absent field became explicit") }
}

func TestLoadEffectivePreservesExactUntypedNumber(t *testing.T) {
	effective := loadEffectiveFixture(t, validConfigWith(`plugin_attr: {prometheus: {large: 9007199254740993}}`))
	got := effective.Config.PluginAttr["prometheus"]["large"]
	if got != json.Number("9007199254740993") { t.Fatalf("large = %#v (%T)", got, got) }
}

func TestLoadEffectiveResolvesCompatRelativeRuntimePathAgainstOwningFile(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, `apisix_go: {runtime_paths: {data_dir: relative-data}}`)
	effective, err := LoadEffective(req)
	if err != nil { t.Fatal(err) }
	want := filepath.Join(filepath.Dir(req.OverridePath), "relative-data")
	if effective.Paths.DataDir != want { t.Fatalf("data_dir = %q, want %q", effective.Paths.DataDir, want) }
	if !filepath.IsAbs(effective.Paths.DataDir) { t.Fatalf("data_dir is not absolute: %q", effective.Paths.DataDir) }
}

func TestLoadEffectiveQualificationRequiresFourAbsoluteRuntimePaths(t *testing.T) {
	req := qualifiedLoadRequestFixture(t, `apisix_go: {runtime_paths: {log_dir: ""}}`)
	_, err := LoadEffective(req)
	if err == nil || err.Error() != "qualification_profile http-data-plane-v1: runtime path log_dir must be a non-empty absolute path" {
		t.Fatalf("LoadEffective() error = %v", err)
	}
}
```

All `loadRequestFixture`, `qualifiedLoadRequestFixture`, and `loadEffectiveFixture` helpers in this task must call `capability.Load()` explicitly, write files below `t.TempDir()` so their paths are absolute, and populate all four `DefaultPaths` with absolute child directories. They must not call `DefaultRuntimePaths()` or read the developer machine's environment.

- [ ] **Step 2: Write profile, unknown-field, removed-field, and nil-manifest tests**

```go
func TestLoadEffectiveUnknownFieldsFollowSecurityProfile(t *testing.T) {
	compat := loadRequestFixture(t, SecurityCompat, "unknown_section: {token: must-not-appear}\n")
	effective, err := LoadEffective(compat)
	if err != nil { t.Fatal(err) }
	if _, ok := effective.Provenance["unknown_section.token"]; !ok { t.Fatal("ignored field missing provenance") }
	strict := loadRequestFixture(t, SecurityStrict, "unknown_section: {token: must-not-appear}\n")
	if _, err := LoadEffective(strict); err == nil || !strings.Contains(err.Error(), "unknown_section.token") {
		t.Fatalf("strict LoadEffective() error = %v", err)
	}
}

func TestLoadEffectiveRejectsRemovedDeploymentProfile(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, "deployment: {profile: http-data-plane-v1}\n")
	_, err := LoadEffective(req)
	want := "deployment.profile was removed; use compatibility_target, security_profile, and qualification_profile"
	if err == nil || !strings.Contains(err.Error(), want) { t.Fatalf("LoadEffective() error = %v", err) }
}

func TestLoadEffectiveRequiresExplicitManifest(t *testing.T) {
	req := loadRequestFixture(t, SecurityCompat, "")
	req.Manifest = nil
	_, err := LoadEffective(req)
	if err == nil || err.Error() != "load effective config: capability manifest is required" { t.Fatalf("error = %v", err) }
}
```

- [ ] **Step 3: Run the new loader tests and confirm they fail**

Run: `bash -lc 'source .envrc && go test ./pkg/config -run "^TestLoadEffective" -count=1'`

Expected: FAIL because `LoadEffective` is not implemented; the root profile fields from plan 01 compile.

- [ ] **Step 4: Preserve plan-01 profile fields and tag static secrets**

Plan 01 has already deleted `HTTPDataPlaneV1Profile` and `Deployment.Profile`, added the following root `Config` fields, and implemented `Config.Profiles()`. Treat those symbols as inputs and do not move them back under `Deployment`:

```go
CompatibilityTarget CompatibilityTarget `mapstructure:"compatibility_target"`
SecurityProfile SecurityProfile `mapstructure:"security_profile"`
QualificationProfile QualificationProfile `mapstructure:"qualification_profile"`
```

Mark static secrets for the redacted renderer:

```go
type DataEncryption struct {
	EnableEncryptFields bool     `mapstructure:"enable_encrypt_fields"`
	Keyring             []string `mapstructure:"keyring" secret:"true"`
}
type AdminKey struct {
	Name string `mapstructure:"name"`
	Key  string `mapstructure:"key" secret:"true"`
	Role string `mapstructure:"role"`
}
type Etcd struct {
	Password string `mapstructure:"password" secret:"true"`
}
```

Modify the existing `Config.PluginAttr` declaration to include `secret:"container"` without changing its Go type. Redacted output lists plugin names but replaces each plugin's entire attribute value with `[REDACTED]`. This conservative static rule prevents an unregistered plugin attribute from leaking credentials.

- [ ] **Step 5: Implement builtin defaults, typed decode, and validation**

`defaults.go` owns exactly the current Go-only defaults, profile defaults, and bootstrap-injected runtime-path defaults:

```go
func builtinDefaults(paths RuntimePaths) *valueNode {
	return mustNodeFromAny(map[string]any{
		"nginx_config": map[string]any{"http": map[string]any{
			"client_max_body_size": int64(10 * 1024 * 1024),
			"client_body_timeout": "60s",
		}},
		"compatibility_target": string(CompatibilityAPISIX317),
		"security_profile": string(SecurityCompat),
		"apisix_go": map[string]any{"runtime_paths": map[string]any{
			"data_dir": paths.DataDir, "runtime_dir": paths.RuntimeDir,
			"log_dir": paths.LogDir, "temp_dir": paths.TempDir,
		}},
	}, FieldSource{Kind: SourceBuiltin, Origin: "apisix-go-runtime-defaults", Explicit: false})
}
```

Before typed `Config` decode, copy the top-level map, remove `apisix_go`, and decode that extension with the private exact shape `struct { RuntimePaths RuntimePaths ` + "`mapstructure:\"runtime_paths\"`" + ` }`; return its unused fields together with the main `Config` unused fields. This keeps `RuntimePaths` out of `Config` while retaining presence and provenance in the merged node tree. Use `mapstructure.Decoder` directly with `TagName: "mapstructure"`, `WeaklyTypedInput: true`, existing `configDecodeHook`, an added `json.Number` hook that calls `strconv.ParseInt/ParseUint` with the destination bit size, and `Metadata.Unused`. Move `github.com/go-viper/mapstructure/v2 v2.5.0` from the indirect block to the direct block in `go.mod`; do not change its version or rewrite vendor.

Define `resolveRuntimePaths(paths RuntimePaths, root *valueNode, req LoadRequest) (RuntimePaths, error)`. For each nonempty relative value, use the winning node's `pathBase`; CLI/APISIXGO nodes use the directory of `OverridePath`, or `DefaultPath` when no override exists. The chosen base must already be absolute; return a stable error instead of calling `filepath.Abs` or reading the working directory. Clean every nonempty result with `filepath.Clean`. `DataDir` is always required and absolute because it owns the journal. When qualification is selected, all four fields must be nonempty and absolute; compatibility mode may leave runtime/log/temp empty, and resolves any supplied relative value by the rule above.

`LoadEffective` is deterministic at its public boundary: `DefaultPath` is
required, nonempty, and absolute; a nonempty `OverridePath` must also be
absolute. It does not fall back to `DefaultConfigFile`, call `filepath.Abs`, or
interpret either path against the process working directory. `cmd.Start` and
Task 5 are the only bootstrap owners that turn their CLI defaults into absolute
paths. `sameConfigPath` compares already absolute, cleaned paths without cwd
access. Empty/comment-only files are empty mapping layers; explicit YAML null
remains an explicit replacing value.

Obtain `ProfileSelection` through `cfg.Profiles()`, call `selection.Validate(req.Manifest)`, then run existing general validations. Reuse plan 01's `validateSecurityProfile`; call `ValidateQualificationPlugins` plus HTTP-scope checks from `validateQualificationProfile`. Compatibility mode returns sorted unused paths; strict mode returns a redacted error naming the first sorted unknown path. Check `deployment.profile` in the merged tree before decode so it cannot be silently ignored.

- [ ] **Step 6: Implement the complete `LoadEffective` pipeline**

```go
func LoadEffective(req LoadRequest) (*EffectiveConfig, error) {
	if req.Manifest == nil { return nil, fmt.Errorf("load effective config: capability manifest is required") }
	if req.DefaultPath == "" || !filepath.IsAbs(req.DefaultPath) {
		return nil, fmt.Errorf("load effective config: default path must be a non-empty absolute path")
	}
	if req.OverridePath != "" && !filepath.IsAbs(req.OverridePath) {
		return nil, fmt.Errorf("load effective config: override path must be absolute")
	}
	defaultPath := filepath.Clean(req.DefaultPath)
	root := builtinDefaults(req.DefaultPaths)
	defaultDoc, err := readConfigDocument(defaultPath, FieldSource{Kind: SourceDefaultFile, Origin: defaultPath}, req.Environment)
	if err != nil { return nil, err }
	root = mergeNodes(root, defaultDoc)
	if req.OverridePath != "" && !sameConfigPath(defaultPath, req.OverridePath) {
		override, err := readConfigDocument(req.OverridePath,
			FieldSource{Kind: SourceOverrideFile, Origin: req.OverridePath, Explicit: true}, req.Environment)
		if err != nil { return nil, err }
		root = mergeNodes(root, override)
	}
	if err := applyAPISIXGO(root, req.Environment); err != nil { return nil, err }
	if err := applyCLIOverrides(root, req.CLIOverrides); err != nil { return nil, err }
	cfg, paths, unused, err := decodeConfig(root)
	if err != nil { return nil, err }
	profiles := cfg.Profiles()
	paths, err = resolveRuntimePaths(paths, root, req)
	if err != nil { return nil, err }
	effective := &EffectiveConfig{Config: cfg, Provenance: flattenProvenance(root), Profiles: profiles, Paths: paths}
	if err := validateEffective(effective, unused, req.Manifest); err != nil { return nil, err }
	return effective, nil
}
```

Delete the Viper import, `GlobalConfig`, `Load`, `loadConfigFiles`, `load(*viper.Viper)`, all `v.SetDefault`, `AutomaticEnv`, and the call to `data_encryption.Configure`. Remove the obsolete `--viper` flag in the same atomic cutover; it must not survive without semantics until Task 5. Move `github.com/go-viper/mapstructure/v2 v2.5.0` to the direct block and remove direct `github.com/spf13/viper`; do not run `go mod tidy` or rewrite vendor as incidental cleanup. Retain only helpers still called by the new decoder and validator; move them to the focused files listed above.

- [ ] **Step 7: Add an immutable data-encryption service and failing global-state tests**

```go
func TestServiceInstancesDoNotShareKeyrings(t *testing.T) {
	first := NewService(true, []string{"qeddd145sfvddff3"})
	second := NewService(false, nil)
	ciphertext, err := first.EncryptForContext("secret", "test.secret")
	if err != nil { t.Fatal(err) }
	if _, err := first.Resolver().ResolveForContext(ciphertext, "test.secret"); err != nil { t.Fatal(err) }
	if got, err := second.Resolver().ResolveForContext("plain", "test.secret"); err != nil || got != "plain" {
		t.Fatalf("disabled resolver = (%q, %v)", got, err)
	}
}
```

Implement an immutable value owner; do not expose its keyring:

```go
type Service struct {
	enabled bool
	keyring []string
}

func NewService(enabled bool, keyring []string) Service {
	return Service{enabled: enabled, keyring: append([]string(nil), keyring...)}
}
func (s Service) Resolver() Resolver { return NewResolver(s.enabled, s.keyring) }
func (s Service) EncryptPluginConfigs(configs map[string]any) error {
	if !s.enabled { return nil }
	return EncryptPluginConfigs(configs, s.keyring)
}
func (s Service) EncryptPluginMetadata(name string, metadata map[string]any) error {
	if !s.enabled { return nil }
	return EncryptPluginMetadata(name, metadata, s.keyring)
}
```

Add equivalent service methods for metadata/plugin decryption by delegating to the existing resolver-aware functions. Add `SameConfiguration(Service) bool`, implemented inside `data_encryption` with constant public semantics and no keyring exposure, so `store.GetStore` can reject a second caller that supplies a different service. Clone constructor input and never return the keyring. Delete `runtimeConfig`, `Configure`, and `Keyring` from `data_encryption.go`.

`EffectiveConfig` is immutable by runtime ownership, not by Go's type system:
the public struct necessarily contains maps and slices for typed compatibility.
The bootstrap constructs it once and every injected consumer treats it as
read-only; tests must prove two independent EffectiveConfig/Builder/Service
instances do not mutate or cross-read each other. `Service` itself never exposes
key material. The configured keyring remains present only in the read-only
static snapshot for Task 5's redacted rendering; this milestone must not claim
structural immutability or keyring removal from `EffectiveConfig`.

- [ ] **Step 8: Define explicit plugin dependencies**

```go
// pkg/plugin/base/types.go
type Dependencies struct {
	Config         *config.EffectiveConfig
	DataEncryption data_encryption.Resolver
}

type BasePlugin struct {
	Name string
	Priority int
	Schema string
	MetadataSchema string
	dependencies Dependencies
}

func (p *BasePlugin) SetDependencies(deps Dependencies) { p.dependencies = deps }
func (p *BasePlugin) StaticConfig() *config.EffectiveConfig { return p.dependencies.Config }
func (p *BasePlugin) DataEncryption() data_encryption.Resolver { return p.dependencies.DataEncryption }
```

```go
// pkg/plugin/types.go and pkg/plugin/init.go
type dependencyReceiver interface { SetDependencies(base.Dependencies) }

func New(name string, deps base.Dependencies) Plugin {
	factory, ok := pluginRegistry[name]
	if !ok { return nil }
	p := factory()
	receiver, ok := p.(dependencyReceiver)
	if !ok { panic("registered plugin does not embed base.BasePlugin") }
	receiver.SetDependencies(deps)
	return p
}
```

Update both `plugin.New` call sites in `pkg/route/builder.go`. There is no one-argument overload. Tests that need a plugin must pass an explicit zero `base.Dependencies{}` or the exact dependencies required by that behavior.

`plugin.New` continues to return `Plugin`, not `(Plugin, error)`. Zero
dependencies remain valid only for plugins whose behavior does not consume
them. A plugin that reads static configuration or encryption must return a
stable missing-dependency error from `Init`/`PostInit` before serving; production
Builder paths always supply both dependencies. Do not panic on a nil config or
invent an error-returning factory overload.

- [ ] **Step 9: Propagate immutable dependencies from `cmd` through server and route**

In `cmd.Start`, call `capability.Load()` and `config.DefaultRuntimePaths()` exactly once, normalize the selected default/override config file names to absolute paths before constructing the request, build an environment map from `os.Environ()`, and call `LoadEffective` with `CLIOverrides: nil`. Then create `data_encryption.NewService`, configure logging from `&effective.Config`, and call `server.NewServer(effective, encryption)`. Remove `cmd.globalConfig`. Task 5 adds repeatable `--set` and routes all three commands through one loader; Task 4 must not parse an unregistered flag.

Add `staticConfig *config.EffectiveConfig` and `dataEncryption data_encryption.Service` to the existing `Server` struct without renaming its other fields. Replace its constructor signature with `func NewServer(effective *config.EffectiveConfig, encryption data_encryption.Service) (*Server, error)`, return `effective config is required` before any resource creation when `effective == nil`, and make the remainder of the existing constructor read `&effective.Config` and the supplied encryption value. `cmd.Start` alone wraps that error once as `create server: %w`. Add `staticConfig *appconfig.EffectiveConfig` and `pluginDependencies base.Dependencies` to the existing `route.Builder` struct without renaming its other fields.

Replace constructor signatures with explicit required parameters; do not add variadic or nil-fallback overloads:

```go
func NewBuilder(storage *store.Store, effective *appconfig.EffectiveConfig, resolver data_encryption.Resolver) *Builder
func NewBuilderWithServerAddr(storage *store.Store, serverAddr string,
	effective *appconfig.EffectiveConfig, resolver data_encryption.Resolver) *Builder
func NewBuilderWithClusterRegistry(storage *store.Store, serverAddr string,
	registry *pxy.ClusterRegistry, effective *appconfig.EffectiveConfig,
	resolver data_encryption.Resolver) *Builder
```

Update every builder construction found by `rg -n 'route\.NewBuilder|NewBuilderWith(ServerAddr|ClusterRegistry)' pkg --glob '*.go'`. Production code always passes `s.staticConfig` and `s.dataEncryption.Resolver()`; tests use a shared route-package helper that creates a complete `EffectiveConfig` rather than assigning a package variable or repeating 138 ad-hoc literals.

- [ ] **Step 10: Replace every static-config global reader by its owner**

Apply this exact ownership table. Each row is a direct replacement, not a compatibility fallback:

| Current reader | Replacement |
| --- | --- |
| `pkg/server/server.go`, `pkg/server/tls.go` | `s.staticConfig.Config` or a helper receiving `*config.Config`; helpers such as `configuredListenAddresses`, `configuredTLSListenAddresses`, `pluginConfigured`, `newClusterObserver`, and TLS builders accept config explicitly. |
| `pkg/route/builder.go`, `extra.go`, `production_policy.go`, `upstream_options.go` | `b.staticConfig.Config`; free functions receive `*config.Config` or the needed value explicitly. |
| `pkg/plugin/graphql_proxy_cache`, `graphql_limit_count`, `otel`, `skywalking`, `log_rotate`, `redirect`, `request_context` | `p.StaticConfig()` supplied before `Init/PostInit`; consumers return a stable missing-dependency error, while focused unit tests pass explicit fixtures. |
| `pkg/plugin/dubbo_proxy/transport.go` | transport construction receives the plugin attribute map from the owning plugin instance; the transport helper no longer reads config. |
| `pkg/observability/metrics/prometheus.go` | `Init(attr map[string]any)`, `ConfiguredPublicEndpoint(attr map[string]any)`, and `ConfiguredExportServer(attr map[string]any)`; retain `StartExportServer(ExportServerConfig)`. Server and `route/extra.go` pass the same `effective.Config.PluginAttr["prometheus"]`; delete `prometheusPluginAttributes()`. |
| `pkg/apisix/id/id.go` | `Get(configuredID string)`; no zero-argument overload. |
| `pkg/plugin/node_status`, `server_info`, `request_context` | `StatusHandler(configuredID string) http.HandlerFunc`; `CurrentInfo(configuredID string)`, `InfoHandler(configuredID string) http.HandlerFunc`, `ReportTTL(attr map[string]any)`, and request-context `PostInit` receives the ID through `p.StaticConfig()`. Route/server pass the exact ID/attr snapshot. |
| `pkg/plugin/proxy_cache/zones.go` | an internal immutable zone snapshot owned by the proxy-cache registry; `RefreshConfiguredZones` publishes directly and never rewrites static config. At the start of every Builder candidate build, publish `b.staticConfig.Config.Apisix.ProxyCache.Zones` before validation so the first build cannot observe an empty stale registry. |

After each package edit, replace tests that mutate `GlobalConfig` with explicit constructor or function arguments. Do not preserve zero-argument helpers solely for old tests.

- [ ] **Step 11: Replace every encryption global reader**

Use these exact appended-argument signatures:

```go
// pkg/store
func Open(dbPath string, events chan *Event, encryption data_encryption.Service) (*Store, error)
func GetStore(dbPath string, events chan *Event, encryption data_encryption.Service) (*Store, error)

// pkg/config
func NewStandaloneFileWatcher(path, provider string, events chan *store.Event,
	encryption data_encryption.Service) *StandaloneFileWatcher
```

Store the service on each `Store`. Convert `decodePluginMetadata`,
`ParseRoute`, `ParseStreamRoute`, `ParseService`, `ParseConsumer`,
`ParseConsumerGroup`, `ParseGlobalRule`, `ParsePluginConfigRule`,
`decryptPluginConfigs`, and the validation paths that call them into Store
methods; they use only that receiver's Service/Resolver. Encryption-free
`ParseUpstream`, `ParseSSL`, and `ParseProto` may remain package functions. Do
not read `store.s` from a parser. Tests that parse an encrypted resource create
an explicit Store with the intended Service.

`GetStore` compares a supplied Service with an existing singleton using
`SameConfiguration`; a mismatch returns `global store already initialized with
a different data-encryption service` instead of first-caller-wins behavior.
Server passes `config.JournalPath(effective)` and the same immutable service to
Store and standalone watcher; remove every production literal or cwd fallback
for `apisix-go-store.db`.

Add explicit isolation tests, not only call-site compilation:

- two Stores built with different Services decrypt only their own fixture and
  never cross-read key material;
- a second `GetStore` call with a different Service returns the exact mismatch
  error, while the same configuration reuses the singleton;
- a standalone watcher decrypts through its supplied Service without changing
  another watcher or Store;
- `NewServer(nil, service)` fails before opening a database or starting a
  watcher;
- two Builders with different EffectiveConfigs retain their own plugin
  attributes, ID, and proxy-cache zones;
- the first candidate build publishes its configured proxy-cache zones before
  plugin validation can read the registry.

For the 16 plugin files reported by the required `rg` command, replace this pattern:

```go
keyring, enabled := data_encryption.Keyring()
resolved, err := data_encryption.NewResolver(enabled, keyring).ResolveForContext(value, context)
```

with:

```go
resolved, err := p.DataEncryption().ResolveForContext(value, context)
```

For helper methods, pass `data_encryption.Resolver` as an argument from the plugin instance. Update focused secret tests to construct plugins through `plugin.New(name, base.Dependencies{DataEncryption: service.Resolver()})` or set the embedded dependencies explicitly inside the same package. No test may call a process-wide configure/reset function.

- [ ] **Step 12: Confirm production configuration is already migrated**

`conf/config-production.yaml` already contains the three root axes on the
current base. Assert that shape through the migrated release-gate tests, but do
not touch the file or manufacture a no-op diff.

- [ ] **Step 13: Run the focused red/green gates for the atomic cutover**

Run in this order:

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/data_encryption ./pkg/store -count=1'
bash -lc 'source .envrc && go test ./cmd -run "(Start|Root|Config|Logger|Signal)" -count=1'
bash -lc 'source .envrc && go test ./pkg/apisix/id ./pkg/observability/metrics -run "(Configured|Prometheus|Metric|ID|Get|Init)" -count=1'
bash -lc 'source .envrc && go test ./pkg/server ./pkg/route -run "(Configured|Config|Builder|TLS|Plugin|Upstream|Production|Server|Store|Standalone)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin ./pkg/plugin/graphql_proxy_cache ./pkg/plugin/graphql_limit_count ./pkg/plugin/dubbo_proxy ./pkg/plugin/server_info ./pkg/plugin/node_status ./pkg/plugin/request_context ./pkg/plugin/otel ./pkg/plugin/skywalking ./pkg/plugin/log_rotate ./pkg/plugin/redirect ./pkg/plugin/proxy_cache -run "(Dependencies|Config|PostInit|Configured|Handler|TTL|ID|Zone|Transport)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/ai_rate_limiting ./pkg/plugin/clickhouse_logger ./pkg/plugin/csrf ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_log_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/kafka_proxy ./pkg/plugin/lago ./pkg/plugin/loggly ./pkg/plugin/response_rewrite ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls -run "(Secret|Resolve|PostInit|Materialize|Config)" -count=1'
bash -lc 'source .envrc && go test ./pkg/etcd ./pkg/plugin/authz_casbin ./pkg/plugin/basic_auth ./pkg/plugin/chaitin_waf ./pkg/plugin/file_logger ./pkg/plugin/grpc_transcode ./pkg/plugin/hmac_auth ./pkg/plugin/jwe_decrypt ./pkg/plugin/jwt_auth ./pkg/plugin/key_auth ./pkg/plugin/ldap_auth ./pkg/plugin/multi_auth ./pkg/plugin/wolf_rbac -run "^$" -count=1'
bash -lc 'source .envrc && make build'
git diff --check
```

The first command is deliberately package-wide because all loader, encryption,
and Store contracts change. The final compile-only command covers remaining
Store/signature call-site packages not already behavior-tested. Before running,
derive the live call-site package set with `rg`; add any newly discovered exact
package rather than broadening to `./pkg/...`. Expected: every command PASS and
every named behavior selector matches at least one test.

- [ ] **Step 14: Prove the old paths are gone**

Run:

```bash
rg -n 'GlobalConfig|data_encryption\.(Configure|Keyring)' cmd pkg --glob '*.go'
rg -n 'func (Load|loadConfigFiles)\b|AutomaticEnv|spf13/viper|--viper' pkg/config cmd go.mod --glob '*.go'
rg -n '^func Parse(Route|StreamRoute|Service|Consumer|ConsumerGroup|GlobalRule|PluginConfigRule)\b' pkg/store --glob '*.go'
rg -n 'plugin\.New\([^,\)]*\)' pkg --glob '*.go'
rg -n 'apisix-go-store\.db' cmd pkg --glob '*.go'
```

Expected: the first four commands print no production or test call site. The
database scan prints only `JournalPath` and exact path tests, never a
server/store cwd fallback. Separately inspect every changed constructor call
from the diff: `server.NewServer` requires EffectiveConfig and Service;
`plugin.New` requires Dependencies; all Builder, Store, and watcher calls pass
their exact new dependencies.

- [ ] **Step 15: Commit the atomic cutover**

```bash
git add go.mod cmd pkg/config pkg/data_encryption pkg/server pkg/route pkg/store pkg/plugin pkg/apisix/id pkg/observability/metrics
git commit -m "refactor(config): cut over to explicit effective configuration"
```

### Task 5: Add Side-Effect-Free Config Test and Redacted Dump Commands

**Files:**
- Create: `cmd/config.go`
- Create: `cmd/config_test.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`
- Modify: `cmd/version.go`
- Modify: `pkg/config/types.go`
- Create: `pkg/config/redact.go`
- Test: `pkg/config/effective_test.go`

**Interfaces:**
- Consumes: `capability.Load()`, `config.DefaultRuntimePaths() (RuntimePaths, error)`, `config.LoadEffective(LoadRequest) (*EffectiveConfig, error)`, `config.ValidateStaticOverridePath(string) error`, Task 3's canonical provenance-path syntax, root `--config/-c`, and root `--set path=value`.
- Produces: `config.RenderEffectiveRedacted(*EffectiveConfig) ([]byte, error)`, `newRootCommand() *cobra.Command`, `newVersionCommand() *cobra.Command`, `apisix config test`, and `apisix config dump --effective --redacted`.
- Boundary: bootstrap may snapshot the process environment, resolve platform paths, resolve absolute file names, and read static files and the embedded manifest. `config.LoadEffective` remains the deterministic request-owned loader. Config inspection commands stop after that loader and never configure logging, construct encryption/store/server owners, create a journal or runtime directory, bind a listener, start a provider, or create a goroutine.

- [ ] **Step 1: Write the command lifecycle and `--set` parser tests before registering the command**

Add table-driven tests in `cmd/config_test.go` and replace all package-global
`rootCmd`, `versionCmd`, and `cfgFile` use in `cmd/root_test.go` with a fresh
`newRootCommand()` per test. Cover these exact contracts:

- two root commands have independent `--config`, `--set`, output, error, and
  parsed-flag state;
- `version` works on two independently constructed roots, proving that no
  child `*cobra.Command` is reparented or reused;
- `--config/-c` and `--set` are persistent flags and work before or after the
  `config test` / `config dump` subcommand tokens;
- `--set` uses `StringArrayVar`, not `StringSliceVar`, so commas remain part of
  one value;
- split only the first `=` (`apisix.id=a=b` produces `a=b`), accept an empty
  value, and reject missing `=`, an empty path, duplicate paths, unknown paths,
  and the removed `deployment.profile` tombstone;
- every malformed-input error excludes the raw `--set` argument and its value.

The parser must return this value-free syntax error; do not append the raw
argument because it may contain a credential:

```go
func parseSetOverrides(values []string) (map[string]any, error) {
	result := make(map[string]any, len(values))
	for _, raw := range values {
		path, value, ok := strings.Cut(raw, "=")
		if !ok || path == "" {
			return nil, errors.New("--set must use path=value")
		}
		if _, exists := result[path]; exists {
			return nil, fmt.Errorf("--set path %q is repeated", path)
		}
		if err := config.ValidateStaticOverridePath(path); err != nil {
			return nil, err
		}
		result[path] = value
	}
	return result, nil
}
```

Use a sentinel such as `must-not-appear` in malformed values and assert that it
is absent from both `err.Error()` and the captured stdout/stderr. Unknown file
fields remain a compatibility-mode input feature; they never make an unknown
CLI override path valid. A schema leaf whose Go type cannot decode from a CLI
string is a valid path followed by a redacted typed-decode error, not an excuse
to add a second CLI schema.

- [ ] **Step 2: Write the command behavior and side-effect tests**

Every command uses `cobra.NoArgs`. `config test` prints exactly one line;
`config dump` succeeds only when both safety flags are true:

```go
func TestConfigCommandTestValidatesWithoutStartingServer(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "must-stay-absent")
	path := writeCommandConfig(t, validCommandConfigWithDataDir(dataDir))
	cmd := newRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout); cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"config", "test", "-c", path})
	if err := cmd.Execute(); err != nil { t.Fatal(err) }
	if got := stdout.String(); got != "configuration is valid\n" { t.Fatalf("stdout = %q", got) }
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config test created runtime path: %v", err)
	}
}

func TestConfigCommandDumpRequiresEffectiveAndRedacted(t *testing.T) {
	path := writeCommandConfig(t, validCommandConfigWithSecrets)
	cmd := newRootCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"config", "dump", "--effective", "--redacted", "-c", path,
		"--set", "proxy.max_in_flight=77",
		"--set", "apisix_go.runtime_paths.log_dir=relative-logs"})
	if err := cmd.Execute(); err != nil { t.Fatal(err) }
	output := stdout.String()
	for _, secret := range []string{
		"etcd-password", "url-password", "admin-secret", "encryption-key",
		"plugin-attr-secret", "discovery-provider-secret",
	} {
		if strings.Contains(output, secret) { t.Fatalf("dump leaked %q", secret) }
	}
	for _, want := range []string{
		`"max_in_flight": 77`, `"kind": "cli"`, `"profiles"`, `"paths"`, `"ignored_fields"`,
	} {
		if !strings.Contains(output, want) { t.Fatalf("dump missing %q: %s", want, output) }
	}
}
```

Also test all four dump flag combinations, positional-argument rejection for
`config`, `config test`, and `config dump`, an occupied configured listener,
and a temporary `apisix_go.runtime_paths.data_dir`. Successful inspection must
leave the listener owned by the test and must not create the data directory,
`apisix-go-store.db`, a standalone UID file, or any provider-owned artifact.
Checking only for the text `Starting server` is not side-effect evidence.

- [ ] **Step 3: Run command tests and verify the new factories and commands are absent**

Run:

```bash
bash -lc 'source .envrc && go test ./cmd -run "^(TestConfigCommand|TestParseSetOverrides|TestEnvironmentMap|TestRootCommand|TestVersionCommand)" -count=1'
```

Expected: FAIL because the fresh command factories and `config` subcommand are
not registered; the sentinel-leak assertions compile.

- [ ] **Step 4: Write the redacted-renderer schema, secret, and canonical-path tests**

Add `TestRenderEffectiveRedacted*` cases in `pkg/config/effective_test.go` before
creating `pkg/config/redact.go`. Lock the exact indented JSON schema to five
top-level keys in this order:

```json
{
  "config": {},
  "paths": {
    "data_dir": "",
    "runtime_dir": "",
    "log_dir": "",
    "temp_dir": ""
  },
  "profiles": {
    "compatibility_target": "",
    "security_profile": "",
    "qualification_profile": ""
  },
  "provenance": [
    {
      "path": "",
      "kind": "",
      "origin": "",
      "explicit": false
    }
  ],
  "ignored_fields": []
}
```

Use private dump DTOs; do not add JSON tags to or directly serialize
`ProfileSelection`, whose current exported Go field names would otherwise
produce unstable operator-facing `Compatibility`, `Security`, and
`Qualification` keys:

```go
type profileDump struct {
	Compatibility CompatibilityTarget  `json:"compatibility_target"`
	Security      SecurityProfile      `json:"security_profile"`
	Qualification QualificationProfile `json:"qualification_profile"`
}

type provenanceEntry struct {
	Path     string     `json:"path"`
	Kind     SourceKind `json:"kind"`
	Origin   string     `json:"origin"`
	Explicit bool       `json:"explicit"`
}

type effectiveDump struct {
	Config        map[string]any   `json:"config"`
	Paths         RuntimePaths     `json:"paths"`
	Profiles      profileDump      `json:"profiles"`
	Provenance    []provenanceEntry `json:"provenance"`
	IgnoredFields []string         `json:"ignored_fields"`
}
```

Tests must prove:

- nil input returns `render effective config: config is required`;
- config map keys and nested map keys are deterministic, arrays retain config
  order, provenance is sorted by rendered path, ignored fields are sorted, and
  empty provenance/ignored fields encode as `[]`, never `null`;
- exact `json.Number` values remain numbers;
- `time.Duration` values use the standard `encoding/json` representation of
  their typed `int64` nanoseconds; lock one non-zero example so a later switch
  to strings is an explicit schema migration;
- `secret:"true"` retains an empty string or empty slice and renders every
  non-empty scalar or list as the single JSON string `[REDACTED]`;
- `secret:"container"` preserves only first-level plugin names and renders
  each complete plugin attribute value as `[REDACTED]` without traversing its
  keys;
- URL userinfo, template-expanded dynamic keys, unknown field names, and
  secret-container descendants obey the fail-closed path contracts below;
- a dynamic config-map key whose winning provenance is `SourceAPISIXEnv` is
  replaced in the `config` object as well as in provenance; the sentinel key is
  absent from the complete JSON document;
- a sentinel secret is absent from the complete JSON bytes and every returned
  error.

Add a reflection inventory test over `Config` that fails on an unknown
`secret` tag value, a secret tag attached to an unsupported Go shape, or a
missing required secret path. The required registry after this task is:

| Static path | Tag | Renderer |
| --- | --- | --- |
| `apisix.data_encryption.keyring` | `secret:"true"` | redact the complete non-empty list |
| `deployment.admin.admin_key[].key` | `secret:"true"` | redact each non-empty key |
| `deployment.etcd.password` | `secret:"true"` | redact a non-empty password |
| `deployment.etcd.host` | `secret:"url-userinfo"` | sanitize every endpoint as specified below |
| `plugin_attr` | `secret:"container"` | preserve plugin names, redact each whole value |
| `discovery` | `secret:"container"` | preserve discovery provider names, redact each whole provider value |

`AdminAPIMTLS.AdminSSLCertKey` and `EtcdTLS.Key` are file paths in the current
typed/runtime contract, not inline private-key material. Do not tag them as
secret values in this task; changing that display policy requires a separate
operator-contract decision.

- [ ] **Step 5: Run renderer tests and confirm the renderer is absent**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/config -run "^TestRenderEffectiveRedacted" -count=1'
```

Expected: FAIL because `RenderEffectiveRedacted` and its schema matcher do not
exist. The complete secret registry test fails because
`deployment.etcd.host` does not yet carry `secret:"url-userinfo"`.

- [ ] **Step 6: Make every Cobra command and flag instance-local**

Define `newRootCommand() *cobra.Command` with a local options value captured by
its root and config handlers. `Execute` constructs exactly one fresh command.
Define `newVersionCommand() *cobra.Command`; delete the package-global
`versionCmd` and its `init()` registration. Reusing one child command across
fresh roots is forbidden because Cobra reparents and retains flag/output state.

Register `--config/-c` and `--set` as persistent flags on each new root. Keep
the current default `config.DefaultConfigFile`. Use `StringArrayVar` for
`--set`; `StringSliceVar` is forbidden because it treats commas in values as
separators. Task 4 already removed `--viper`; assert it remains absent.

Construct the config command tree through factories so every boolean is also
instance-local:

```go
type rootOptions struct {
	configPath string
	setValues  []string
}

func newConfigCommand(load func(string, []string) (*config.EffectiveConfig, error)) *cobra.Command
func newConfigTestCommand(load func(string, []string) (*config.EffectiveConfig, error)) *cobra.Command
func newConfigDumpCommand(load func(string, []string) (*config.EffectiveConfig, error)) *cobra.Command
```

The injected loader function is a narrow unit-test seam, not an alternate
production loader. Production always passes `loadEffectiveForCommand`.

- [ ] **Step 7: Implement the single bootstrap snapshot boundary**

The function name describes command/bootstrap ownership, not mathematical
purity. It may read only the ambient inputs listed here and must snapshot each
exactly once before entering `LoadEffective`:

```go
func loadEffectiveForCommand(configPath string, setValues []string) (*config.EffectiveConfig, error) {
	manifest, err := capability.Load()
	if err != nil { return nil, fmt.Errorf("load capability manifest: %w", err) }
	overrides, err := parseSetOverrides(setValues)
	if err != nil { return nil, err }
	paths, err := config.DefaultRuntimePaths()
	if err != nil { return nil, err }
	defaultPath, err := filepath.Abs(config.DefaultConfigFile)
	if err != nil { return nil, fmt.Errorf("resolve default config path: %w", err) }
	overridePath := ""
	if configPath != "" {
		overridePath, err = filepath.Abs(configPath)
		if err != nil { return nil, fmt.Errorf("resolve override config path: %w", err) }
	}
	return config.LoadEffective(config.LoadRequest{
		DefaultPath: defaultPath,
		OverridePath: overridePath,
		DefaultPaths: paths,
		Environment: environmentMap(os.Environ()),
		CLIOverrides: overrides,
		Manifest: manifest,
	})
}
```

Root server startup, `config test`, and `config dump` all call this exact
function. Root startup creates the immutable encryption service, configures
logging, opens store/server owners, and calls `runServer` only after the
function returns successfully. Inspection handlers return or render immediately
and never enter those branches.

Define the environment snapshot with a first-`=` split and no subsequent `os.Getenv` calls:

```go
func environmentMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok { result[name] = value }
	}
	return result
}
```

- [ ] **Step 8: Add the URL-userinfo tag and implement deterministic redacted values**

Add `secret:"url-userinfo"` only to `Etcd.Host`, and add
`secret:"container"` to `Config.Discovery` as well as the Task 4 tag on
`Config.PluginAttr`. `Discovery` is an open `map[string]any` provider
configuration surface and may contain credentials unknown to the static
schema; preserving only provider names is the same fail-closed boundary as
plugin attributes. For URL display, parse each
non-empty endpoint with `net/url` and emit a representation that cannot retain
credentials:

- a URL with scheme and host but no userinfo, path, raw query, or fragment is
  returned unchanged;
- a URL with userinfo is rendered exactly as
  `<scheme>://[REDACTED]@<host>` while retaining no username or password;
- any non-root path, raw query, or fragment appends exactly one fixed
  `/<redacted>` suffix to that scheme/host representation because those
  components are not required by the etcd endpoint contract and may contain
  credentials;
- an empty endpoint remains empty;
- a parse failure, missing scheme, or missing host returns the fixed
  `[REDACTED]` value rather than the input or a value-bearing error.

The sanitizer operates on a copy and never mutates `EffectiveConfig`. Its error
messages contain only the schema path and sequence index. Do not use
`url.URL.String()` until userinfo and non-host components have been replaced.

Build the config document by walking `Config` through `mapstructure` tags.
Recurse through structs, pointers, slices/arrays, and string-key maps; retain
zero values; copy `json.Number` exactly; apply the three supported secret tags
above before descending. No renderer reflection error may format the field
value. Carry the canonical raw schema path during this walk. Before emitting a
dynamic map key, look up the winning provenance for that map entry; when its
kind is `SourceAPISIXEnv`, emit the opaque ID allocated for the raw canonical
entry path instead of the key. `FieldSource` cannot distinguish key expansion
from value expansion, so this deliberately aliases some value-only environment
entries rather than risking an expanded secret in the JSON key. Fixed
struct/schema field names remain unchanged. Apply the same rule to a
first-level plugin name before the `secret:"container"` value is replaced.

- [ ] **Step 9: Implement the canonical provenance tokenizer and schema matcher**

Do not classify provenance with `strings.Split(path, ".")`. Implement one
tokenizer for the exact Task 3 grammar:

```text
safe mapping key: [A-Za-z_][A-Za-z0-9_-]*
unsafe mapping key: [<one valid JSON string>]
sequence index: [0] or another canonical base-10 non-negative integer
```

An unsafe root key starts directly with the bracketed JSON string. A mapping
key containing dot, quote, backslash, or brackets must round-trip through its
JSON string, for example
`plugin_attr["tenant.\\\"prod\\\\blue"].token`. Reject invalid JSON escapes,
trailing bytes inside brackets, a bracket-string encoding of a safe key,
negative/overflow indices, and non-canonical indices such as `[00]`.

Match the parsed tokens against a schema descriptor derived from
`reflect.TypeFor[Config]()` plus the four synthetic leaves under
`apisix_go.runtime_paths`. The matcher rules are exact:

- a struct accepts its exported `mapstructure` names and skips `-`;
- a map accepts one mapping-key token and continues with its element type;
- a slice/array accepts one numeric-index token and continues with its element;
- an interface accepts any remaining syntactically valid canonical tokens;
- a scalar accepts only end-of-path;
- every mapping, map, sequence, element container, scalar, and explicit-null
  position is known when traversal can end at that position.

This recognizes `apisix`, `apisix.node_listen`,
`apisix.node_listen[0]`, and `apisix.node_listen[0].port` without treating a
literal dynamic key such as `plugin_attr["tenant.a"]` as nested fields.

- [ ] **Step 10: Implement fail-closed provenance and ignored-field display paths**

Classification uses the raw canonical path internally. The output never emits
an untrusted raw path or a digest derived from it when its safety cannot be
proven. Before rendering, collect every raw path/key that requires masking,
sort the unique raw strings internally, and assign sequential opaque IDs:

```go
func buildOpaquePathIDs(paths []string) map[string]string {
	// Sort and deduplicate raw strings internally, then assign opaque:0001,
	// opaque:0002, ... without serializing the raw string or a derived digest.
}
```

The numeric width is at least four digits and expands when necessary. The same
raw string receives the same ID everywhere in one dump, so config, provenance,
and ignored fields remain correlatable. For the same effective configuration
the allocation is deterministic. Adding or removing an unsafe path may
renumber IDs; these are inspection-local handles, not durable identifiers. Do
not use an unsalted hash: low-entropy secret-bearing field names are
dictionary-guessable even when the digest is one-way.

Render paths with these ordered rules:

1. A raw path rejected by the schema matcher is added to `ignored_fields` as
   `unknown:` plus its opaque ID. Its provenance entry uses that same safe
   display path. Never output the unknown field name.
2. Any provenance entry with `Kind == SourceAPISIXEnv` is displayed as
   `apisix_env:` plus its opaque ID. `FieldSource` cannot currently prove
   whether the environment expansion occurred in a key or only in a value, so
   aliasing every such path is the required fail-closed behavior. Retain the
   safe environment-variable name(s) in `origin`.
3. For a path below a `secret:"container"` field, retain the container path and
   its first-level plugin name, but replace the remainder with
   `redacted:` plus its opaque ID. Do not emit descendant attribute keys.
   An unsafe first-level plugin name is itself replaced with
   `plugin:` plus its opaque ID.
4. Other known canonical paths are emitted unchanged.

Sort after converting to display paths. Preserve entries ordered by display
path, then kind, origin, and explicit; do not use a map that silently drops
provenance. `ignored_fields` is deduplicated after conversion and sorting.
Opaque IDs are diagnostic correlation handles only and are never accompanied
by the raw string or any value derived from it.

The fixed marker `[REDACTED]`, opaque IDs, schema paths, builtin IDs,
file paths, environment-variable names, and validated CLI paths are the only
data classes admitted to the dump. Environment or CLI values are never valid
origins.

- [ ] **Step 11: Implement the renderer and exact command semantics**

`RenderEffectiveRedacted` returns indented JSON with five top-level keys in a struct: `config`, `paths`, `profiles`, `provenance`, and `ignored_fields`. Reflect over `Config` using `mapstructure` names and add the four reserved `apisix_go.runtime_paths.*` schema paths. The schema matcher must recognize mapping containers, sequences, numeric sequence indices, and Task 3's bracketed JSON-string encoding for unsafe dynamic mapping keys so known container/index provenance is not mislabeled as unknown. For `secret:"true"`, retain empty as empty and render any nonempty scalar/list as `[REDACTED]`; for `secret:"container"`, preserve first-level plugin names with redacted values. Sort provenance keys and ignored paths before materializing output structs. Unknown paths are provenance paths not accepted by that matcher and are listed without their values.

```go
type effectiveDump struct {
	Config        map[string]any    `json:"config"`
	Paths         RuntimePaths      `json:"paths"`
	Profiles      profileDump       `json:"profiles"`
	Provenance    []provenanceEntry `json:"provenance"`
	IgnoredFields []string          `json:"ignored_fields"`
}

func RenderEffectiveRedacted(effective *EffectiveConfig) ([]byte, error) {
	if effective == nil { return nil, errors.New("render effective config: config is required") }
	dump, err := buildEffectiveDump(effective)
	if err != nil { return nil, err }
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil { return nil, fmt.Errorf("render effective config: %w", err) }
	return append(data, '\n'), nil
}
```

`apisix config test` accepts no positional arguments and prints exactly `configuration is valid`. `apisix config dump` accepts no positional arguments and errors unless both `--effective` and `--redacted` are true; no unredacted mode exists.

- [ ] **Step 12: Run the focused command, renderer, and build gates**

Run in this order:

```bash
bash -lc 'source .envrc && go test ./pkg/config -run "^(TestRenderEffectiveRedacted|TestLoadEffective)" -count=1'
bash -lc 'source .envrc && go test ./cmd -run "^(TestConfigCommand|TestParseSetOverrides|TestEnvironmentMap|TestRootCommand|TestVersionCommand|TestStart)" -count=1'
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: PASS. The focused tests prove side-effect absence with filesystem and
occupied-listener assertions, not log-string inspection.

- [ ] **Step 13: Prove global Cobra state and unsafe output paths are gone**

Run:

```bash
rg -n 'var (rootCmd|versionCmd|cfgFile)|--viper|StringSliceVar' cmd --glob '*.go'
rg -n 'got %q.*raw|ignored_fields.*raw|Path: raw|data_encryption\.Configure|config\.GlobalConfig' cmd pkg/config --glob '*.go'
```

Expected: no match. Function names such as `newRootCommand` and
`newVersionCommand` are allowed; package-global mutable Cobra commands and raw
secret-bearing path/value output are not.

- [ ] **Step 14: Commit the CLI**

```bash
git add cmd/config.go cmd/config_test.go cmd/root.go cmd/root_test.go cmd/version.go \
  pkg/config/types.go pkg/config/redact.go pkg/config/effective_test.go
git commit -m "feat(cli): inspect effective configuration safely"
```

### Task 6: Update the Operator Contract and Run the Milestone Gate

**Files:**
- Modify: `docs/configuration.md`
- Modify: `docs/production-profile.md`
- Modify: `docs/design.md`
- Modify: `cmd/capability-gen/main_test.go`
- Verify: `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`
- Verify: `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program.md`

**Interfaces:**
- Consumes: completed Tasks 1–5 and the central governance manifest.
- Produces: operator documentation for precedence, presence, environment namespaces, profiles, runtime paths, config commands, redaction, journal migration, and the static-config milestone evidence consumed by child plan 03. Plan 04 also consumes `data_encryption.Service` and replaces its temporary resolver injection atomically with `secret.Materializer`.
- Scope decision: do not edit `conf/config-production.yaml` or `Dockerfile` in this docs/governance task. The `/var/*` layout below is an operator-owned overlay example, not the checked-in image shape. A later deployment task may choose to make it an image default with non-root filesystem tests.

- [ ] **Step 1: Write governance tests for current truth and preserved history**

Extend `cmd/capability-gen/main_test.go` before editing docs. The tests must
distinguish active claims from the explicitly labelled superseded history in
`docs/design.md`; do not ban the string `deployment.profile` globally because
the existing governance test intentionally preserves that historical evidence.
Add assertions that the active documentation contains:

- builtin/default/override/APISIXGO/CLI precedence and file-local APISIX
  template expansion;
- the four runtime paths and short `APISIXGO_RUNTIME_PATHS_*` aliases;
- `config test` static-only limits and redacted-dump opaque-handle policy;
- journal relocation/migration guidance;
- the currently unqualified/fail-closed production snapshot.

Run:

```bash
bash -lc 'source .envrc && go test ./cmd/capability-gen -run "^TestGovernedDocsContainNoActiveLegacyClaims$" -count=1'
```

Expected: FAIL on the still-active legacy/static-config claims, while the
superseded-history oracle continues to pass.

- [ ] **Step 2: Replace obsolete operator examples without inventing an image contract**

Use this as an **operator overlay example**, never as the checked-in production
or container shape:

```yaml
compatibility_target: apisix-3.17
security_profile: strict
qualification_profile: http-data-plane-v1

apisix_go:
  runtime_paths:
    data_dir: /var/lib/apisix-go
    runtime_dir: /run/apisix-go
    log_dir: /var/log/apisix-go
    temp_dir: /var/tmp/apisix-go
```

State next to the example that the operator must create/mount and set ownership
and permissions on all four directories before startup. The current Dockerfile
creates only `/usr/local/apisix/{conf,logs,data}` and this task does not change
it. `conf/config-production.yaml` intentionally omits the runtime-path overlay
and etcd endpoint; do not claim otherwise and do not replace its existing
deployment/etcd example in `docs/production-profile.md`.

Document the exact precedence order, recursive-map/list-replacement/null behavior, provenance source kinds, APISIX `${{NAME}}` and `${{NAME:=fallback}}`, `APISIXGO_*`, repeatable `--set path=value`, strict unknown-field rejection, compatibility opaque ignored-path reporting, and the removed `deployment.profile` error. Explain that APISIX template expansion happens inside each parsed file layer; bootstrap injects platform defaults; qualification requires all four paths to be nonempty and absolute; compatibility resolves explicit relative paths against the selected override file directory, or the default file directory when no override exists; and the durable journal is always `filepath.Join(data_dir, "apisix-go-store.db")`.

Document the intentional parser limits: YAML anchors, aliases, and merge keys
fail closed, and LuaJIT hex-float template retyping is not qualified. Do not
generalize “official YAML shape” beyond those explicit limits.

Use `conf/config-example.yaml` for successful inspection examples:

```bash
apisix config test -c conf/config-example.yaml
apisix config dump --effective --redacted -c conf/config-example.yaml
apisix config test -c conf/config-example.yaml --set proxy.max_in_flight=2048
apisix config dump --effective --redacted -c conf/config-example.yaml --set apisix_go.runtime_paths.log_dir=relative-logs
```

Show the production command separately as **expected to fail closed on the
repository snapshot** until the deployment supplies an etcd endpoint and the
manifest records complete qualification evidence. Do not imply that a current
production dump emits JSON.

State that `config test` validates only static read/merge/decode/profile
contracts. It does not create/check directory permissions, open/migrate the
journal, bind ports, contact etcd/providers, configure logging, or prove runtime
readiness. State that dump has no unredacted mode. Its registered secret
contract covers encryption keyring, admin keys, etcd password, sanitized etcd
URL userinfo, plugin attributes, and discovery provider configuration. Unknown
and APISIX-environment-derived paths use opaque correlation handles; original
keys/values are absent. `AdminSSLCertKey` and `EtcdTLS.Key` are displayed as
file paths, not inline private-key contents. The output still contains approved
operational metadata (profiles, file paths, provider/plugin names, environment
variable names, and sanitized hosts) and must be handled as a sensitive
diagnostic artifact.

Add a journal migration runbook: stop the old process; back up the cwd
`apisix-go-store.db`; create and permission `data_dir`; copy/verify the database
to `data_dir/apisix-go-store.db`; start exactly one instance; validate resource
generation; retain the backup for rollback. Warn that starting without moving
the old journal presents an empty local state until providers repopulate it.

- [ ] **Step 3: Correct current design ownership while retaining superseded history**

Replace active claims that Viper defines merge semantics, environment variables are automatic field overrides, configuration is published globally, or `deployment.profile` combines security and qualification. Link the current static-configuration section to the program specification and central capability manifest. Update the active secret chain to `EffectiveConfig -> data_encryption.Service -> explicit resolver dependency`, and replace active `config.GlobalConfig.Plugins` wording with the startup plugin list. Preserve the explicitly labelled superseded history required by the governance oracle; do not rewrite unrelated historical evidence.

- [ ] **Step 4: Run governance, generated-document, symbol, and placeholder scans**

Run:

```bash
bash -lc 'source .envrc && go test ./cmd/capability-gen -run "^TestGovernedDocsContainNoActiveLegacyClaims$" -count=1'
bash -lc 'source .envrc && go run ./cmd/capability-gen -repo-root . -check'
rg -n 'GlobalConfig|data_encryption\.(Configure|Keyring)|HTTPDataPlaneV1Profile|AutomaticEnv|--viper' cmd pkg conf docs/configuration.md docs/production-profile.md docs/design.md --glob '*.go' --glob '*.yaml' --glob '*.md'
rg -n 'deployment\.profile' docs/configuration.md docs/production-profile.md docs/design.md cmd/capability-gen/main_test.go
rg -n 'apisix-go-store\.db' cmd pkg --glob '*.go'
rg -n 'T[B]D|T[O]DO|implement l[a]ter|fill in d[e]tails|similar to T[a]sk' docs/superpowers/plans/2026-08-23-static-effective-config.md
```

Expected: both commands PASS; active-legacy scan prints no line; the dedicated
`deployment.profile` scan contains only the current migration/tombstone text,
the explicitly labelled superseded-history block, and its governance oracle;
the journal scan contains `JournalPath`, exact path tests, and intentional
migration documentation references only; placeholder scan prints no line.

- [ ] **Step 5: Run the exact static-config milestone gate from the master plan**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/config ./pkg/data_encryption ./pkg/plugin/base ./pkg/plugin ./pkg/server ./pkg/route ./pkg/store ./pkg/observability/metrics ./cmd -run "^(TestLoadEffective|TestMergePresence|TestProfileSelection|TestConfigCommand|TestDependencies|TestServerConfig)" -count=1 && make build'
```

Expected: PASS. This is copied from the parent program and must not be narrowed.

- [ ] **Step 6: Run impact-scoped regression and formatting gates**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/data_encryption ./pkg/store ./pkg/server ./pkg/route ./pkg/observability/metrics -run "(Config|Configured|Secret|Encryption|Builder|Server|TLS|Prometheus)" -count=1'
bash -lc 'source .envrc && golangci-lint run ./cmd/... ./pkg/config/... ./pkg/data_encryption/... ./pkg/plugin/base/... ./pkg/plugin/... ./pkg/server/... ./pkg/route/... ./pkg/store/... ./pkg/observability/metrics/...'
git diff --check
```

Expected: PASS. Do not substitute `go test ./...` or `make test`.

- [ ] **Step 7: Perform the required dead-code and proxy-only audit**

List every deleted or renamed symbol from the diff, then run exact production-and-test call-site searches for `Load`, `loadConfigFiles`, `load`, `validateHTTPDataPlaneV1Profile`, `profileAwareRuntimeError`, `GlobalConfig`, `Configure`, `Keyring`, old `NewServer()`, old builder constructors, old `plugin.New(name)`, and zero-argument static config helpers. Delete unused imports, helpers, fixtures, and tests that only exercise removed globals. Do not preserve a wrapper for test convenience.

- [ ] **Step 8: Commit documentation and milestone evidence**

```bash
git add docs/configuration.md docs/production-profile.md docs/design.md cmd/capability-gen/main_test.go
git commit -m "docs(config): document effective configuration contract"
```

## Self-Review Checklist

- Every configuration/state requirement in the program spec maps to Tasks 1–6: profile axes, presence, exact numbers, APISIX/APISIXGO separation, provenance, unknown fields, CLI inspection, redaction, and global removal.
- The Task 2 base shape of `EffectiveConfig` plus `Provenance`, `ProfileSelection`, `RuntimePaths`, `LoadRequest`, `DefaultRuntimePaths`, `JournalPath`, and `LoadEffective` matches the master plan. Plans 05 and 07 add `EffectiveConfig.Runtime` and its lifecycle/safety/telemetry/diagnostics policy in their own compiling atomic milestones; this plan does not predeclare empty future policy types.
- `ProfileSelection.Validate` uses `Manifest.Qualification` plus `Manifest.QualifiedPlugins` to fail closed on incomplete required evidence; it does not read `Config`, and `capability` never imports `config`.
- `LoadEffective` has no ambient reads or runtime side effects; bootstrap alone supplies platform defaults and the environment snapshot.
- `apisix_go.runtime_paths.*`, their `APISIXGO_RUNTIME_PATHS_*` aliases, canonical resolution, qualification checks, redacted output, and `JournalPath` have focused tests.
- The only runtime cutover commit removes both old config and encryption global paths; no adapter remains.
- The redacted renderer cannot emit values or raw keys from registered secret
  containers/fields, APISIX-environment-derived dynamic paths, or ignored
  fields; docs list the approved operational metadata that remains visible.
- Operator docs separate checked-in configuration, bootstrap defaults, and
  operator overlays; they do not call static validation a runtime-readiness
  check and include the journal relocation/rollback boundary.
- All test and implementation steps contain exact code, commands, and expected red/green results.
- The final symbol scan and dead-code audit cover production and tests.

Plan complete and saved to `docs/superpowers/plans/2026-08-23-static-effective-config.md`. Execute it through the parent program's selected subagent-driven workflow, preserving the dependency order: governance first, then this plan, then durable generation journal; plan 04 consumes the static encryption service and removes its raw plugin/base injection during the `secret.Materializer` cutover.
