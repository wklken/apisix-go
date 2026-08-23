# Parity Qualification and Same-Digest Release Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn APISIX 3.17 compatibility claims into fail-closed, immutable qualification evidence and release only the exact Linux artifact digest that passed parity, operational, recovery, security, upgrade, rollback, and provenance gates.

**Architecture:** `pkg/qualification` owns a versioned result and evidence-bundle contract. A differential harness executes the same declarative case, inputs, and pinned dependency fixtures against apisix-go and an immutable official APISIX oracle, then compares results through a reviewed versioned normalizer. Pull requests run hermetic gates; scheduled and release workflows add pinned real dependencies and native platform smoke. Release builds happen once, every record is bound to the resulting digest, and promotion verifies, signs, and publishes that same digest without rebuilding.

**Tech Stack:** Go 1.26, Cobra, YAML, Bash, GitHub Actions, Docker Buildx/OCI, native macOS Go packaging, Syft/Anchore SBOM, Trivy, Cosign, GitHub artifact attestations, `t/plugin` real-process harness.

**Spec:** `docs/superpowers/plans/2026-08-23-apisix-go-convergence-program-spec.md`

## Global Constraints

- Compatibility target is exactly APISIX `3.17.0` at source commit `9ef2ecab67f652d38365049613610ef649bb4ad0`. The oracle image must be an official `apache/apisix` image resolved to `repository@sha256:<64 lowercase hex>`; a mutable tag is never accepted as evidence.
- `pkg/capability` remains the source of behavior/evidence requirements. Qualification consumes `capability.Load()` and never invents a second plugin or profile registry.
- Every upstream test block at the pinned commit is exactly one of `converted`, `not_applicable`, or `deferred`. `not_applicable` and `deferred` require non-empty owner and reason; `deferred` cannot qualify a required capability.
- A differential case uses the same logical APISIX configuration, HTTP input, dependency fixture behavior, clock/seed values, and trust material on both subjects. Subject-specific adapters may translate syntax, but may not change semantics.
- Normalization is versioned, allow-listed, and recorded in every differential result. It may remove only explicitly volatile fields; it may never normalize status, asserted headers, body bytes, route/upstream choice, attempt count, Host, SNI, or security decisions.
- There are no hidden retries. A failed, timed-out, flaky, stale, skipped, or blocked required record makes the qualification result non-passing. A repeated diagnostic run is a new recorded attempt and cannot replace the first result.
- Pull requests use hermetic fixtures. Scheduled and release qualification use pinned real etcd/upstream/TLS dependencies. Only the release workflow may promote.
- Build once: qualification, SBOM, vulnerability scan, signature, attestation, and promotion all consume the same OCI index digest. Linux has no standalone binary/archive/checksum release files: the index and its `linux/amd64` and `linux/arm64` child manifests are the only Linux artifacts. No job after artifact construction may invoke `docker build`, `docker buildx build`, `go build`, or `goreleaser build/release` for Linux.
- Linux `amd64` and `arm64` are production OCI child manifests only. macOS `amd64` and `arm64` are published release archives only after native-runner smoke; they are supported development artifacts, not production-qualified server platforms. Windows is cross-build-only, experimental, and produces no downloadable or official release artifact.
- All Go/Make commands run through `bash -lc 'source .envrc && ...'`. Tests remain impact-scoped; never add routine `go test ./...`, `go test ./pkg/...`, or `make test` gates.
- Decision 196C applies: remove the previous build/test/publish release paths atomically. Do not retain a compatibility workflow, dual publisher, optional legacy gate, or rebuild fallback.

## Dependency Order and File Ownership

1. Plan 01 supplies `capability.Manifest`, evidence kinds/states, the qualification profile, target source commit, and complete corpus accounting.
2. Plan 02 supplies deterministic effective configuration, selected profiles, runtime paths, and redacted provenance output.
3. Plans 03–05 supply durable revision publication, rollback/finalization, worker generation lifecycle, drain, crash, and readiness evidence.
4. Plan 06 supplies `t/plugin/http-protocol.yaml`, shared differential case support, and versioned HTTP normalization candidates.
5. Plan 07 supplies capacity, overload, security, diagnostics, telemetry, upgrade, and rollback acceptance signals.
6. This plan owns `pkg/qualification`, the qualification scripts/workflows, release pipeline cutover, and the immutable evidence bundle. It does not redefine upstream subsystem interfaces.
7. Plan 09 consumes only a passing `qualification.Result` whose exact artifact digest is signed and promoted.

## Stable Interfaces

Create these interfaces before changing workflows. Downstream plans may depend on them without parsing CI log text.

```go
// package qualification
type Outcome string

const (
	OutcomePass    Outcome = "pass"
	OutcomeFail    Outcome = "fail"
	OutcomeBlocked Outcome = "blocked"
)

type GateKind string

const (
	GateManifest     GateKind = "manifest"
	GateCorpus       GateKind = "corpus"
	GateDifferential GateKind = "differential"
	GateHermetic     GateKind = "hermetic"
	GateRealDeps     GateKind = "real_dependencies"
	GateCapacity     GateKind = "capacity"
	GateRecovery     GateKind = "recovery"
	GateSecurity     GateKind = "security"
	GateUpgrade      GateKind = "upgrade"
	GateRollback     GateKind = "rollback"
	GateProvenance   GateKind = "provenance"
	GatePlatform     GateKind = "platform"
)

type Platform string

const (
	PlatformLinuxAMD64 Platform = "linux/amd64"
	PlatformLinuxARM64 Platform = "linux/arm64"
	PlatformDarwinAMD64 Platform = "darwin/amd64"
	PlatformDarwinARM64 Platform = "darwin/arm64"
	PlatformWindowsAMD64 Platform = "windows/amd64"
)

type OracleIdentity struct {
	Version      string `json:"version"`
	SourceCommit string `json:"source_commit"`
	Image        string `json:"image"`
	ImageDigest  string `json:"image_digest"`
}

type OracleLock struct {
	SchemaVersion  int    `yaml:"schema_version"`
	Name           string `yaml:"name"`
	Version        string `yaml:"version"`
	SourceRepository string `yaml:"source_repository"`
	SourceCommit   string `yaml:"source_commit"`
	ImageRepository string `yaml:"image_repository"`
	ImageTag       string `yaml:"image_tag"`
	ImageDigest    string `yaml:"image_digest"`
}

type ArtifactIdentity struct {
	SourceCommit string     `json:"source_commit"`
	Image        string     `json:"image"`
	Digest       string     `json:"digest"`
	Platforms    []Platform `json:"platforms"`
}

type BuildMetadata struct {
	SchemaVersion   int               `json:"schema_version"`
	SourceCommit    string            `json:"source_commit"`
	OCIIndexDigest  string            `json:"oci_index_digest"`
	PlatformDigests map[Platform]string `json:"platform_digests"`
	MacOSFiles      map[string]string `json:"macos_files"`
}

type InputIdentity struct {
	CaseID              string            `json:"case_id"`
	ConfigDigest        string            `json:"config_digest"`
	RequestDigest       string            `json:"request_digest"`
	DependencyDigest    string            `json:"dependency_digest"`
	Normalization       string            `json:"normalization"`
	DeterministicValues map[string]string `json:"deterministic_values"`
}

type EvidenceRecord struct {
	ID             string           `json:"id"`
	Gate           GateKind         `json:"gate"`
	Capability     string           `json:"capability,omitempty"`
	Platform       Platform         `json:"platform,omitempty"`
	Outcome        Outcome          `json:"outcome"`
	ArtifactDigest string           `json:"artifact_digest"`
	ArtifactFile   string           `json:"artifact_file,omitempty"`
	ArtifactFileDigest string       `json:"artifact_file_digest,omitempty"`
	Inputs         *InputIdentity   `json:"inputs,omitempty"`
	Command        []string         `json:"command"`
	OutputDigest   string           `json:"output_digest"`
	Owner          string           `json:"owner,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Attempt        int              `json:"attempt"`
}

type Result struct {
	SchemaVersion        int               `json:"schema_version"`
	QualificationProfile string            `json:"qualification_profile"`
	Artifact             ArtifactIdentity  `json:"artifact"`
	Oracle               OracleIdentity    `json:"oracle"`
	Outcome              Outcome           `json:"outcome"`
	Evidence             []EvidenceRecord  `json:"evidence"`
}

type FileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type BundleManifest struct {
	SchemaVersion int          `json:"schema_version"`
	ResultSHA256  string       `json:"result_sha256"`
	Files         []FileDigest `json:"files"`
}

func Evaluate(manifest *capability.Manifest, result *Result) error
func LoadOracleLock(path string) (*OracleLock, error)
func ValidateBuildMetadata(metadata *BuildMetadata, artifact ArtifactIdentity) error
func WriteBundle(root string, result *Result, files []string) (*BundleManifest, error)
func VerifyBundle(root string) (*Result, error)
```

`Evaluate` requires schema version `1`, exact target/oracle identity, a digest-qualified oracle and candidate OCI index, Linux `amd64/arm64` child-manifest records with no `ArtifactFile`, native macOS `amd64/arm64` archive-smoke records, every profile-required plugin/evidence kind, and one passing record for every mandatory operational gate. It rejects duplicate record IDs, unknown enums, unsorted records, `Attempt != 1` for required evidence, an evidence digest different from `Artifact.Digest`, any Linux or Windows `ArtifactFile`, a missing/mismatched macOS `ArtifactFileDigest`, a non-pass required record, and a pass result with any missing requirement. `blocked` is used only when an external prerequisite did not execute; it is not success. `WriteBundle` creates `result.json`, hashes every relative regular file including published macOS archives, sorts by slash-normalized path, rejects symlinks/path escape, and atomically writes `bundle-manifest.json`. `VerifyBundle` recomputes every digest, rejects extra/missing files, then calls `Evaluate`.

`LoadOracleLock` uses strict YAML decoding, rejects a second document, and requires schema `1`, the exact name/version/source repository/source commit/image repository/tag above, and a lowercase SHA-256 registry digest. Task 2 resolves the official registry manifest and writes the real digest in the same commit; if the official tag is unavailable or its source revision cannot be proven, the task and release remain blocked rather than substituting a community image or fabricated digest.

The differential protocol is also versioned:

```go
// package qualification
type DifferentialSource struct {
	Repository string `yaml:"repository"`
	Commit     string `yaml:"commit"`
	File       string `yaml:"file"`
}

type DifferentialManifest struct {
	SchemaVersion int                `yaml:"schema_version"`
	Source        DifferentialSource `yaml:"source"`
	Cases         []DifferentialCase `yaml:"cases"`
}

type SourceSelection struct {
	File  string `yaml:"file"`
	Tests []int `yaml:"tests"`
}

type DifferentialRequest struct {
	Method     string              `yaml:"method"`
	Path       string              `yaml:"path"`
	Headers    map[string][]string `yaml:"headers"`
	BodyBase64 string              `yaml:"body_base64"`
}

type FixtureResponse struct {
	Status     int                 `yaml:"status"`
	Headers    map[string][]string `yaml:"headers"`
	BodyBase64 string              `yaml:"body_base64"`
	Delay      time.Duration       `yaml:"delay"`
}

type DependencyFixture struct {
	ID        string            `yaml:"id"`
	Kind      string            `yaml:"kind"`
	Address   string            `yaml:"address"`
	Responses []FixtureResponse `yaml:"responses"`
}

type DifferentialCase struct {
	ID            string                 `yaml:"id"`
	Capability    string                 `yaml:"capability"`
	Source        SourceSelection        `yaml:"source"`
	Config        map[string]any         `yaml:"config"`
	Request       DifferentialRequest    `yaml:"request"`
	Dependencies  []DependencyFixture    `yaml:"dependencies"`
	Expected      Observation            `yaml:"expected"`
	Determinism   map[string]string      `yaml:"determinism"`
	Normalization string                 `yaml:"normalization"`
}

type Observation struct {
	Status        int                 `json:"status" yaml:"status"`
	Headers       map[string][]string `json:"headers" yaml:"headers"`
	BodyBase64    string              `json:"body_base64" yaml:"body_base64"`
	RouteID       string              `json:"route_id" yaml:"route_id"`
	UpstreamID    string              `json:"upstream_id" yaml:"upstream_id"`
	AttemptCount  int                 `json:"attempt_count" yaml:"attempt_count"`
	Host          string              `json:"host" yaml:"host"`
	ServerName    string              `json:"server_name" yaml:"server_name"`
}

type Normalizer interface {
	Version() string
	Normalize(Observation) (Observation, error)
}

func LoadDifferentialManifest(path string) (*DifferentialManifest, error)
func (m *DifferentialManifest) Case(id string) (DifferentialCase, bool)
func Compare(normalizer Normalizer, want, got Observation) error
```

`LoadDifferentialManifest` uses `yaml.Decoder.KnownFields(true)`, requires one YAML document, schema version `1`, the exact oracle repository/commit, unique sorted case IDs, non-empty capability/source tests/config/request/dependencies/normalization, and a known dependency kind. A blank case `source.file` uses the manifest source file; a non-blank value names another file at the same pinned repository/commit. `Case` returns a deep copy. There is no adapter that guesses `name` versus `id`, `input` versus `request`, `upstream` versus `dependencies`, or `output` versus `expected`.

### Task 1: Add the Fail-Closed Qualification Result and Bundle Contract

**Files:**
- Create: `pkg/qualification/types.go`
- Create: `pkg/qualification/evaluate.go`
- Create: `pkg/qualification/bundle.go`
- Create: `pkg/qualification/evaluate_test.go`
- Create: `pkg/qualification/bundle_test.go`

**Interfaces:** Produces every type and function under Stable Interfaces. Consumes only `pkg/capability`; it must not import `cmd`, `server`, workflow packages, or global state.

- [ ] **Step 1: Write failing validation tests**

```go
func TestEvaluateRejectsEvidenceForAnotherArtifact(t *testing.T) {
	m := qualificationManifestFixture(t)
	r := passingResultFixture(t, m)
	r.Evidence[0].ArtifactDigest = "sha256:" + strings.Repeat("b", 64)
	err := Evaluate(m, r)
	if err == nil || !strings.Contains(err.Error(), "artifact digest") {
		t.Fatalf("expected artifact digest error, got %v", err)
	}
}

func TestEvaluateRejectsFlakyRequiredEvidence(t *testing.T) {
	m := qualificationManifestFixture(t)
	r := passingResultFixture(t, m)
	r.Evidence[0].Outcome = OutcomeFail
	r.Evidence[0].Reason = "first execution failed; diagnostic rerun passed"
	err := Evaluate(m, r)
	if err == nil || !strings.Contains(err.Error(), r.Evidence[0].ID) {
		t.Fatalf("expected fail-closed evidence error, got %v", err)
	}
}

func TestEvaluateRejectsStandaloneLinuxArtifactFile(t *testing.T) {
	m := qualificationManifestFixture(t)
	r := passingResultFixture(t, m)
	for i := range r.Evidence {
		if r.Evidence[i].Platform == PlatformLinuxAMD64 {
			r.Evidence[i].ArtifactFile = "apisix_linux_amd64.tar.gz"
			r.Evidence[i].ArtifactFileDigest = "sha256:" + strings.Repeat("c", 64)
			break
		}
	}
	err := Evaluate(m, r)
	if err == nil || !strings.Contains(err.Error(), "Linux artifact file") {
		t.Fatalf("expected OCI-only Linux error, got %v", err)
	}
}
```

Run: `bash -lc 'source .envrc && go test ./pkg/qualification -run "^(TestEvaluateRejectsEvidenceForAnotherArtifact|TestEvaluateRejectsFlakyRequiredEvidence|TestEvaluateRejectsStandaloneLinuxArtifactFile)$" -count=1'`

Expected: RED because `pkg/qualification` does not exist.

- [ ] **Step 2: Implement strict enums, canonical sorting, and `Evaluate`**

Define the stable types verbatim. Derive plugin requirements from `manifest.Qualification(result.QualificationProfile)` and `manifest.Plugin(key)`; do not duplicate required plugin names. Map each required `capability.EvidenceKind` to the corresponding evidence record ID prefix. Require all operational gates listed under Stable Interfaces, exact two Linux platforms represented only by OCI child-manifest digests, no Linux `ArtifactFile`, and no Windows release platform/file.

- [ ] **Step 3: Write bundle tamper and path-escape tests**

```go
func TestVerifyBundleRejectsTamperedEvidence(t *testing.T) {
	root := writePassingBundleFixture(t)
	p := filepath.Join(root, "evidence", "differential.json")
	require.NoError(t, os.WriteFile(p, []byte("tampered\n"), 0o600))
	_, err := VerifyBundle(root)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch, got %v", err)
	}
}

func TestWriteBundleRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	r := passingResultFixture(t, qualificationManifestFixture(t))
	_, err := WriteBundle(root, r, []string{"../outside"})
	if err == nil || !strings.Contains(err.Error(), "escapes bundle root") {
		t.Fatalf("expected path escape error, got %v", err)
	}
}
```

Run: `bash -lc 'source .envrc && go test ./pkg/qualification -run "^(TestVerifyBundleRejectsTamperedEvidence|TestWriteBundleRejectsPathEscape)$" -count=1'`

Expected: RED until bundle hashing exists.

- [ ] **Step 4: Implement atomic immutable bundle writing and verification**

Use `os.Lstat`, `filepath.Rel`, `filepath.IsLocal`, SHA-256, JSON `DisallowUnknownFields`, a second-document EOF check, `os.CreateTemp(root, ".bundle-manifest-*")`, `Sync`, `Chmod(0o444)`, and `Rename`. Reject symlinks, non-regular files, duplicate normalized paths, and files absent from the manifest.

Run: `bash -lc 'source .envrc && go test ./pkg/qualification -count=1'`

Expected: GREEN.

- [ ] **Step 5: Commit the contract**

```bash
git add pkg/qualification
git commit -m "feat(qualification): define immutable evidence contract"
```

### Task 2: Pin the Official APISIX Oracle and Account for Every Upstream Block

**Files:**
- Create: `qualification/oracle.yaml`
- Create: `scripts/qualification/resolve_oracle.sh`
- Create: `scripts/qualification/oracle_lock_test.sh`
- Modify: `pkg/capability/manifest.yaml`
- Modify: `t/plugin/corpus_scope.yaml`
- Modify: `t/plugin/corpus_test.go`

**Interfaces:** Consumes `capability.Manifest.Target`. Produces a digest-qualified `qualification/oracle.yaml` and exact per-block corpus disposition.

- [ ] **Step 1: Add a failing oracle-lock test**

```bash
#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
lock="$root/qualification/oracle.yaml"
yq -e '.schema_version == 1' "$lock" >/dev/null
yq -e '.version == "3.17.0"' "$lock" >/dev/null
yq -e '.source_commit == "9ef2ecab67f652d38365049613610ef649bb4ad0"' "$lock" >/dev/null
yq -e '.image_repository == "docker.io/apache/apisix"' "$lock" >/dev/null
digest=$(yq -r '.image_digest' "$lock")
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
[[ "$(yq -r '.image_tag' "$lock")" == "3.17.0" ]]
```

Run: `bash scripts/qualification/oracle_lock_test.sh`

Expected: RED because the lock does not exist.

- [ ] **Step 2: Resolve and commit the real official manifest digest**

`resolve_oracle.sh` obtains a Docker Hub bearer token for `repository:apache/apisix:pull`, requests the OCI/Docker manifest list for tag `3.17.0`, verifies the repository name, resolves both Linux `amd64` and `arm64` child manifests, and writes the top-level digest returned by the registry `Docker-Content-Digest` header. It then runs the image and verifies the reported APISIX version/source metadata where the official image exposes it. Do not accept `docker inspect`'s local image ID as a registry digest.

Run: `bash scripts/qualification/resolve_oracle.sh qualification/oracle.yaml`

Expected: GREEN and a real lowercase SHA-256 digest. If the official tag is unavailable or revision proof fails, expected outcome is a loud blocked task; do not proceed to qualification.

- [ ] **Step 3: Make the capability target use the immutable reference**

Read the digest from `qualification/oracle.yaml`, set `target.image` to the exact concatenation `docker.io/apache/apisix@` plus that digest, and add a manifest test that rejects tags or a mismatched oracle source commit. Both committed YAML files contain the same real resolved digest.

- [ ] **Step 4: Expand corpus accounting to per-block dispositions**

Use this strict shape for every upstream `.t` test block at the pinned source commit:

```yaml
schema_version: 2
source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
blocks:
  - id: t/plugin/request-id.t#1
    disposition: converted
    case: request-id/default
    owner: request-id
    reason: exact schema and request behavior covered by the named case
```

Allowed dispositions are `converted`, `not_applicable`, and `deferred`. `converted` requires an existing case selector; the other two require owner/reason and no case. Add tests that compare the ledger to block IDs extracted from the pinned sparse checkout, reject duplicates/unknown/missing blocks, and require `deferred` evidence to remain non-qualifying.

Run: `bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./pkg/capability ./t/plugin -run "^(TestOracleMatchesManifestTarget|TestCorpusAccountsForEveryPinnedUpstreamBlock|TestCorpusDispositionSemantics)$" -count=1'`

Expected: RED before the complete ledger; GREEN only after every block is represented.

- [ ] **Step 5: Commit the immutable oracle and ledger**

```bash
git add qualification/oracle.yaml scripts/qualification/resolve_oracle.sh scripts/qualification/oracle_lock_test.sh pkg/capability/manifest.yaml t/plugin/corpus_scope.yaml t/plugin/corpus_test.go
git commit -m "test(qualification): pin oracle and account upstream corpus"
```

### Task 3: Build the Same-Input Differential Runner and Versioned Normalizer

**Files:**
- Create: `pkg/qualification/differential.go`
- Create: `pkg/qualification/normalize.go`
- Create: `pkg/qualification/differential_test.go`
- Create: `cmd/qualification/main.go`
- Create: `cmd/qualification/main_test.go`
- Create: `scripts/qualification/differential.sh`
- Modify: `t/plugin/case.go`
- Modify: `t/plugin/runner_test.go`
- Modify: `t/plugin/http-protocol.yaml`

**Interfaces:** Implements `DifferentialManifest`, `DifferentialCase`, `Observation`, `Normalizer`, `LoadDifferentialManifest`, `(*DifferentialManifest).Case`, and `Compare`. The internal `cmd/qualification compare` command requires `--manifest`, `--case-id`, `--normalization`, `--want`, `--got`, `--artifact-digest`, and `--out` and returns nonzero on mismatch. Consumes plan 06 cases and oracle lock. Produces one raw and one normalized observation per subject plus a digest-bound evidence record.

- [ ] **Step 1: Write strict loading and forbidden-normalization tests**

```go
func TestCompareNeverNormalizesSemanticFields(t *testing.T) {
	n := NewNormalizerV1()
	want := Observation{Status: 200, BodyBase64: "b2s=", RouteID: "r1", AttemptCount: 1, Host: "api.example", ServerName: "api.example"}
	fields := []func(*Observation){
		func(o *Observation) { o.Status = 201 },
		func(o *Observation) { o.BodyBase64 = "bm8=" },
		func(o *Observation) { o.RouteID = "r2" },
		func(o *Observation) { o.AttemptCount = 2 },
		func(o *Observation) { o.Host = "other.example" },
		func(o *Observation) { o.ServerName = "other.example" },
	}
	for i, mutate := range fields {
		got := want
		mutate(&got)
		if err := Compare(n, want, got); err == nil {
			t.Fatalf("mutation %d was hidden by normalization", i)
		}
	}
}
```

Run: `bash -lc 'source .envrc && go test ./pkg/qualification -run "^(TestCompareNeverNormalizesSemanticFields|TestLoadDifferentialManifestRejectsUnknownField)$" -count=1'`

Expected: RED because the differential types do not exist.

- [ ] **Step 2: Implement normalization version `obs-v1`**

`obs-v1` may remove `Date`, the oracle's unasserted `Server` header, generated request IDs when the case declares them nondeterministic, and loopback ephemeral port numbers. It sorts header values only for headers whose HTTP semantics are order-insensitive. It must return an error if a case asks to ignore any protected semantic field.

- [ ] **Step 3: Encode one canonical input and two subject adapters**

The runner serializes canonical config/request/dependency fixtures once, computes `InputIdentity`, and passes the same immutable fixture directory to `run_apisix_go` and `run_oracle`. Both receive fixed UTC clock, RNG seed, DNS answers, certificates, and upstream script. Adapters may map canonical keys into each subject's startup shape but must emit an adapter translation digest and may not alter fixture files.

```bash
run_subject apisix-go "$candidate_ref" "$case_file" "$fixture_dir" "$go_raw"
run_subject apisix "$oracle_ref" "$case_file" "$fixture_dir" "$oracle_raw"
go run ./cmd/qualification compare \
  --manifest "$case_file" --case-id "$case_id" --normalization obs-v1 \
  --want "$oracle_raw" --got "$go_raw" \
  --artifact-digest "$candidate_digest" --out "$record"
```

- [ ] **Step 4: Extend plan 06 protocol cases and plugin corpus cases**

Every `converted` block must point to a differential selector or explicitly remain converted-upstream-only and therefore non-qualified when differential evidence is required. Add route, phase, proxy, TLS, real-IP, retry, streaming, trailer, cancellation, auth, rate-limit, and logging observations without broadening normalization.

Run: `bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./pkg/qualification ./t/plugin -run "^(TestDifferential|TestHTTPProtocolManifest|TestCorpusDispositionSemantics)$" -count=1'`

Expected: GREEN.

- [ ] **Step 5: Commit the runner**

```bash
git add pkg/qualification cmd/qualification t/plugin scripts/qualification/differential.sh
git commit -m "test(qualification): compare pinned APISIX observations"
```

### Task 4: Split Hermetic Pull-Request Evidence from Real-Dependency Qualification

**Files:**
- Create: `.github/workflows/qualification.yml`
- Create: `scripts/qualification/run.sh`
- Create: `scripts/qualification/flaky_policy_test.sh`
- Modify: `.github/workflows/unit-test.yml`
- Modify: `scripts/release_gate_test.sh`
- Modify: `Makefile`

**Interfaces:** Reusable workflow input is `{mode, source-commit, candidate-image, candidate-digest, oracle-lock}`; output is one immutable evidence artifact name and bundle SHA-256. Modes are `hermetic`, `scheduled`, and `release`.

- [ ] **Step 1: Write failing workflow contract tests**

```bash
require_job_fixed "$qualification_workflow" hermetic 'strategy:'
require_job_fixed "$qualification_workflow" hermetic 'max-parallel: 1'
require_job_fixed "$qualification_workflow" hermetic 'bash scripts/qualification/run.sh hermetic'
reject_pattern 'continue-on-error:[[:space:]]*true' "$qualification_workflow"
reject_pattern '(retry|rerun|until)[-_ ]' "$qualification_workflow"
require_job_fixed "$unit_workflow" qualification 'mode: hermetic'
```

Run: `bash scripts/release_gate_test.sh`

Expected: RED because the reusable workflow is absent.

- [ ] **Step 2: Implement the hermetic PR mode**

Hermetic mode runs strict manifest/corpus/unit tests and differential cases using repository-owned deterministic DNS, clock, TLS, upstream, and datastore fixtures. It runs real-process `t/plugin` selectors serially. It does not require Docker Hub, public DNS, a cloud service, or a shared etcd.

- [ ] **Step 3: Implement explicit attempt recording and no-hidden-retry policy**

`run.sh` creates an evidence record before starting each command, writes `attempt: 1`, captures stdout/stderr separately, and finalizes pass/fail/blocked. A trap finalizes interrupted commands as fail. The workflow uploads evidence with `if: always()` but its gate job exits non-zero for every non-pass outcome. `flaky_policy_test.sh` scans workflow/scripts for retry loops and proves a fail-then-pass fixture remains overall fail with two immutable attempts.

Run: `bash scripts/qualification/flaky_policy_test.sh`

Expected: GREEN.

- [ ] **Step 4: Wire the focused PR gate**

Add `make test-qualification-hermetic` invoking only `./pkg/qualification`, selected capability/corpus tests, and hermetic differential selectors. Replace duplicated plugin/release assertions in `unit-test.yml`; do not add a repository-wide Go test.

Run: `bash -lc 'source .envrc && make test-qualification-hermetic'`

Expected: GREEN.

- [ ] **Step 5: Commit the PR qualification split**

```bash
git add .github/workflows/qualification.yml .github/workflows/unit-test.yml scripts/qualification scripts/release_gate_test.sh Makefile
git commit -m "ci(qualification): add fail-closed hermetic gate"
```

### Task 5: Add Scheduled and Release Real-Dependency and Operational Gates

**Files:**
- Modify: `.github/workflows/qualification.yml`
- Modify: `.github/workflows/security-release-gates.yml`
- Create: `scripts/qualification/operational.sh`
- Create: `scripts/qualification/operational_test.sh`
- Create: `qualification/policy.json`
- Modify: `scripts/etcd_recovery_smoke.sh`
- Modify: `scripts/container_smoke.sh`
- Modify: `scripts/runtime_capacity.sh`
- Modify: `scripts/runtime_capacity_test.sh`

**Interfaces:** Consumes the exact candidate digest; plan 05's `lifecycle.StatusProvider`, `lifecycle.Status`, stable JSON status command, command IDs, revision fence, state, generation, readiness, terminal flag, and reason code; plan 07's `runtime.BudgetManager.Snapshot()`, `telemetry.Aggregator.Handler()`, unified `/livez` and `/readyz` projections, and `scripts/runtime_capacity.sh`. Produces mandatory `GateRealDeps`, `GateCapacity`, `GateRecovery`, `GateSecurity`, `GateUpgrade`, `GateRollback`, and `GateProvenance` records. It creates no second lifecycle/health/metric state and does not parse free-form logs.

- [ ] **Step 1: Add a failing operational matrix test**

```bash
for gate in real_dependencies capacity recovery security upgrade rollback provenance; do
  jq -e --arg gate "$gate" '.required_gates | index($gate) != null' qualification/policy.json >/dev/null
done
require_job_fixed "$qualification_workflow" scheduled 'mode == '\''scheduled'\'' || inputs.mode == '\''release'\'''
require_job_fixed "$qualification_workflow" scheduled 'candidate-digest'
require_job_fixed "$qualification_workflow" scheduled 'oracle-lock'
```

Run: `bash scripts/qualification/operational_test.sh`

Expected: RED until the matrix and policy exist.

- [ ] **Step 2: Run pinned real dependencies only in scheduled/release modes**

Pin etcd and every dependency image by digest in workflow environment, record those digests in `InputIdentity.DependencyDigest`, and reject floating tags. Execute real TLS/network/etcd behavior, compaction/reconnect, external logging destinations, and upstream failure/recovery cases serially where they share ports.

- [ ] **Step 3: Bind operational gates to lifecycle and publication evidence**

`operational.sh` invokes plan 05's canonical lifecycle commands and stable JSON status, correlates command IDs with `lifecycle.Event`, and verifies the returned `lifecycle.Status` state/generation/revision fence/readiness/terminal/reason code. It consumes plan 07's Prometheus handler and unified health projections, while `scripts/runtime_capacity.sh` records `runtime.BudgetSnapshot`-derived pressure, workload, worker PID/generation transitions, and metrics snapshots under a unique `.cache/capacity` directory. Together they prove overload bounds, graceful WebSocket/hijack/goroutine drain, crash probation, restart, last-good rollback, journal recovery, upgrade from the previous promoted digest, rollback to it, readiness transitions, and provenance/redaction. Each assertion produces a record bound to the candidate digest; log scraping is not an interface.

- [ ] **Step 4: Make missing infrastructure blocked and non-passing**

Unavailable Docker, architecture runner, registry, real dependency, previous promoted digest, or signing identity creates `OutcomeBlocked`, uploads the partial bundle, and fails the job. Scheduled results may notify owners but never update capability evidence to verified automatically.

Run: `bash scripts/qualification/operational_test.sh`

Expected: GREEN.

- [ ] **Step 5: Commit real-dependency qualification**

```bash
git add .github/workflows/qualification.yml .github/workflows/security-release-gates.yml scripts/qualification scripts/etcd_recovery_smoke.sh scripts/container_smoke.sh scripts/runtime_capacity.sh scripts/runtime_capacity_test.sh
git commit -m "ci(qualification): gate real dependency and recovery evidence"
```

### Task 6: Build the Linux OCI Index Once and Prove Native/Experimental Platform Boundaries

**Files:**
- Delete: `.goreleaser.yaml`
- Modify: `Dockerfile`
- Create: `scripts/qualification/platform_matrix_test.sh`
- Create: `scripts/qualification/package_macos.sh`
- Create: `scripts/qualification/package_macos_test.sh`
- Modify: `.github/workflows/qualification.yml`
- Modify: `.github/workflows/release-candidate.yml`

**Interfaces:** Produces one OCI index digest containing the only Linux artifacts, child manifests for `amd64` and `arm64`; produces and publishes native-built macOS `amd64` and `arm64` archives after smoking their exact binaries; records only those macOS filename-to-digest entries in `BuildMetadata.MacOSFiles` and the corresponding `EvidenceRecord.ArtifactFileDigest`; produces a Windows cross-build record and no Windows artifact.

- [ ] **Step 1: Write failing platform policy tests**

```bash
test ! -e .goreleaser.yaml
! rg -n 'goreleaser|apisix_.*linux.*\.(tar\.gz|zip)|checksums\.txt' .github/workflows
require_job_fixed "$qualification_workflow" macos-smoke 'runs-on: ${{ matrix.runner }}'
require_job_fixed "$qualification_workflow" macos-smoke 'matrix:'
require_job_fixed "$qualification_workflow" macos-smoke 'bash scripts/qualification/package_macos.sh'
require_job_fixed "$qualification_workflow" macos-smoke 'actions/upload-artifact'
require_job_fixed "$qualification_workflow" windows-crossbuild 'GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build'
reject_job_pattern "$qualification_workflow" windows-crossbuild 'upload-artifact|release|packages: write'
```

Run: `bash scripts/qualification/platform_matrix_test.sh`

Run: `bash scripts/qualification/package_macos_test.sh`

Expected: RED because the legacy GoReleaser/Linux-download path still exists and native macOS archives plus explicit Windows boundaries do not.

- [ ] **Step 2: Build the Linux OCI index once**

The release-candidate/release build job checks out the exact source commit, uses pinned Buildx action versions, builds both Linux architectures, pushes to a staging repository by content digest, records the OCI index digest and per-platform child digests, and exports them as immutable workflow outputs. Candidate jobs consume `repository@digest`; no tarball load may replace multi-architecture identity. Delete `.goreleaser.yaml` and every GoReleaser workflow invocation in the same cutover: there is no Linux binary, archive, checksum file, or alternate downloadable subject to qualify independently.

- [ ] **Step 3: Build, smoke, and publish native macOS archives**

On `macos-13` (`amd64`) and `macos-14` (`arm64`), `package_macos.sh` runs a native `go build`, executes that exact binary for `apisix version`, `config test`, and `config dump --effective --redacted`, creates deterministic `apisix_${VERSION}_darwin_${GOARCH}.tar.gz`, hashes it, and emits an evidence record containing the archive filename/digest plus the common candidate OCI digest. Upload both archives and their records; release promotion publishes those exact downloaded archives without rebuilding. Windows uses `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o .cache/out/apisix.exe .` and records success only; it never uploads an executable. Linux packaging stops at the pushed OCI index and its two child manifests.

- [ ] **Step 4: Run static and focused platform checks**

Run: `bash scripts/qualification/platform_matrix_test.sh`

Expected: GREEN.

Run: `bash scripts/qualification/package_macos_test.sh`

Expected: GREEN with deterministic archive fixtures and digest-mismatch rejection.

Run: `bash -lc 'source .envrc && make build'`

Expected: GREEN as a source/build smoke only; it creates no release file. The Linux release identity remains the Buildx OCI index, while the two macOS archives are produced separately and natively by `package_macos.sh`.

- [ ] **Step 5: Commit platform boundaries**

```bash
git add .goreleaser.yaml Dockerfile scripts/qualification/platform_matrix_test.sh scripts/qualification/package_macos.sh scripts/qualification/package_macos_test.sh .github/workflows/qualification.yml .github/workflows/release-candidate.yml
git commit -m "build(release): qualify explicit platform matrix"
```

### Task 7: Bind Build Metadata, Tests, SBOM, Signature, Attestation, and Bundle to One Digest

**Files:**
- Modify: `pkg/qualification/types.go`
- Modify: `pkg/qualification/evaluate.go`
- Modify: `pkg/qualification/evaluate_test.go`
- Modify: `scripts/release_metadata.sh`
- Modify: `scripts/release_metadata_test.sh`
- Create: `scripts/qualification/verify_identity.sh`
- Create: `scripts/qualification/verify_identity_test.sh`
- Modify: `.github/workflows/security-release-gates.yml`

**Interfaces:** Implements `BuildMetadata` and `ValidateBuildMetadata`. `build-metadata.json` is the post-build authoritative metadata record; every test/evidence record, SBOM, signature, attestation, and promotion subject must match `Result.Artifact.Digest`.

- [ ] **Step 1: Add a failing build metadata identity test**

```go
func TestValidateBuildMetadataRejectsAnotherOCIIndex(t *testing.T) {
	artifact := ArtifactIdentity{
		SourceCommit: "0123456789012345678901234567890123456789",
		Image: "ghcr.io/wklken/apisix-go@sha256:" + strings.Repeat("a", 64),
		Digest: "sha256:" + strings.Repeat("a", 64),
		Platforms: []Platform{PlatformLinuxAMD64, PlatformLinuxARM64},
	}
	metadata := &BuildMetadata{
		SchemaVersion: 1,
		SourceCommit: artifact.SourceCommit,
		OCIIndexDigest: "sha256:" + strings.Repeat("b", 64),
		PlatformDigests: map[Platform]string{
			PlatformLinuxAMD64: "sha256:" + strings.Repeat("c", 64),
			PlatformLinuxARM64: "sha256:" + strings.Repeat("d", 64),
		},
	}
	err := ValidateBuildMetadata(metadata, artifact)
	if err == nil || !strings.Contains(err.Error(), "OCI index digest") {
		t.Fatalf("expected OCI index mismatch, got %v", err)
	}
}
```

Run: `bash -lc 'source .envrc && go test ./pkg/qualification -run "^TestValidateBuildMetadataRejectsAnotherOCIIndex$" -count=1'`

Expected: RED because `BuildMetadata` and its validator are absent.

- [ ] **Step 2: Write post-build metadata without a self-referential binary digest**

Immediately after Buildx returns the OCI index digest and both native macOS jobs return their archives, write `build-metadata.json` with schema `1`, source commit, index digest, exactly the two Linux child manifest digests, and exactly the two native macOS filename-to-digest entries in `MacOSFiles`. No separate Linux file field exists. Do not attempt to embed the OCI digest inside a binary or image layer: that would change the bytes being digested and create a circular identity. The existing binary version metadata continues to identify source version/commit/build time/Go version; the immutable post-build metadata is the artifact-digest authority. `ValidateBuildMetadata` requires exact source, index, Linux platform set, empty Linux `ArtifactFile` values, the two expected macOS archive names/digests, and digest equality with `ArtifactIdentity`/platform evidence.

- [ ] **Step 3: Upgrade release metadata to schema 3**

Schema 3 contains source commit, OCI index digest, Linux child platform digests, macOS archive hashes, oracle digest, qualification bundle hash, SBOM subject digest, signature subjects for the OCI index and two macOS archives, attestation subject digest, and promotion target digest. It includes `build_metadata_sha256`, contains no Linux file/archive/checksum entry, and embeds no mutable tag as identity. `release_metadata.sh` rejects any unequal subject.

- [ ] **Step 4: Prove tampering and cross-digest mixing fail**

```bash
if ARTIFACT_DIGEST="sha256:$(printf 'b%.0s' {1..64})" \
  bash scripts/qualification/verify_identity.sh qualification/evidence; then
  echo "cross-digest evidence unexpectedly passed" >&2
  exit 1
fi
```

Run: `bash scripts/release_metadata_test.sh && bash scripts/qualification/verify_identity_test.sh`

Expected: GREEN, including negative fixtures.

- [ ] **Step 5: Commit end-to-end identity**

```bash
git add pkg/qualification scripts/release_metadata.sh scripts/release_metadata_test.sh scripts/qualification/verify_identity.sh scripts/qualification/verify_identity_test.sh .github/workflows/security-release-gates.yml
git commit -m "feat(release): bind evidence to artifact digest"
```

### Task 8: Promote, Sign, and Attest the Qualified Digest Without Rebuilding

**Files:**
- Modify: `.github/workflows/release.yml`
- Modify: `.github/workflows/release-candidate.yml`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `scripts/release_gate_test.sh`
- Create: `scripts/qualification/promotion_test.sh`
- Delete: obsolete build/publish jobs and legacy release branches identified by the tests

**Interfaces:** Consumes verified `BundleManifest` and candidate OCI digest. Produces Cosign signature, SBOM, GitHub provenance attestation, and production tag all referencing that exact digest.

- [ ] **Step 1: Add a failing no-rebuild release test**

```bash
promotion=$(job_block .github/workflows/release.yml promote)
for forbidden in 'docker build' 'docker buildx build' 'build-push-action' 'go build' 'goreleaser release'; do
  if grep -Fq "$forbidden" <<<"$promotion"; then
    echo "promotion rebuilds with: $forbidden" >&2
    exit 1
  fi
done
for required in 'VerifyBundle' 'cosign sign --yes' 'cosign sign-blob --yes' 'attest-build-provenance' 'oras copy' 'apisix_${VERSION}_darwin_amd64.tar.gz' 'apisix_${VERSION}_darwin_arm64.tar.gz'; do
  grep -Fq "$required" <<<"$promotion"
done
if grep -Eq 'apisix_.*linux.*\.(tar\.gz|zip)|checksums\.txt' <<<"$promotion"; then
  echo "promotion publishes an unqualified standalone Linux file" >&2
  exit 1
fi
```

Run: `bash scripts/qualification/promotion_test.sh`

Expected: RED against the current release workflow.

- [ ] **Step 2: Reshape release into build, qualify, and promote**

The build job emits the staging OCI index digest once. Qualification pulls by digest and returns the immutable bundle; it never downloads or qualifies a standalone Linux file. Promotion downloads the bundle plus both digest-verified macOS archives, verifies hashes and `qualification.Result`, verifies the SBOM subject, signs the staging digest, signs each macOS archive digest with `cosign sign-blob --yes`, records all three signature subjects in release metadata, creates provenance for the OCI digest and macOS archive subjects, publishes only the exact native-smoked macOS archives/signatures as downloadable release assets, and uses registry-native digest copy/tagging (`oras copy` or `crane cp`) for Linux. It resolves the production tag afterward and requires equality with the qualified OCI digest; it never rebuilds an archive or publishes a Linux binary/archive/checksum file.

- [ ] **Step 3: Fail closed on every release gap**

Promotion requires all jobs and both Linux child digests. A cancelled/skipped matrix entry, unavailable macOS native runner, Windows crossbuild failure, blocked operational dependency, stale oracle, missing previous-release upgrade/rollback input, vulnerability failure, unsigned digest, absent SBOM, or evidence mismatch fails the release. Do not use `always()` on promotion; use it only for partial evidence upload.

- [ ] **Step 4: Atomically remove legacy paths**

Delete the old `publish-image` rebuild job, all GoReleaser configuration/invocations, Linux binary/archive/checksum publication, tarball-as-release-identity path, single-architecture `platforms: linux/amd64` qualification, mutable tag handoff, permissive vulnerability exit, and duplicated integration gate. Update `release_gate_test.sh` to reject all of them. Do not leave an input or conditional that can select the old route.

Run: `bash scripts/release_gate_test.sh && bash scripts/qualification/promotion_test.sh`

Expected: GREEN.

- [ ] **Step 5: Commit the atomic release cutover**

```bash
git add .github/workflows/release.yml .github/workflows/release-candidate.yml .github/workflows/security-release-gates.yml scripts/release_gate_test.sh scripts/qualification/promotion_test.sh
git commit -m "ci(release): promote the qualified digest without rebuild"
```

### Task 9: Close Evidence, Documentation, and Release Acceptance

**Files:**
- Create: `docs/qualification.md`
- Modify: `docs/architecture/compatibility-contract.md`
- Modify: `docs/design.md`
- Modify: `README.md`
- Modify: `cmd/capability-gen/main.go`
- Modify: `cmd/capability-gen/main_test.go`
- Verify: all files changed by Tasks 1–8

**Interfaces:** Produces the immutable qualification result consumed by plan 09 and generated status that distinguishes behavior, evidence, qualification, and promotion.

- [ ] **Step 1: Add generated qualification status**

Extend the generator from plan 01 to read only a verified bundle and render target commit, oracle digest, candidate digest, qualification profile, gate outcomes, platform boundary, and promoted digest. It must never infer pass from test names, workflow success text, or the presence of an artifact.

- [ ] **Step 2: Document operator-verifiable commands**

Document exact commands to verify bundle hashes, the OCI-index Cosign signature, SBOM subject, provenance subject, OCI child manifests, build metadata, both published macOS archive hashes/signatures, and production tag equality. State explicitly that Linux has no downloadable binary/archive/checksum asset: the signed OCI index and its two child manifests are the complete qualified Linux release. Explain that PR evidence is hermetic, scheduled/release evidence uses pinned real dependencies, macOS archives are native-smoked development artifacts rather than production server qualification, and Windows has no official artifact.

- [ ] **Step 3: Run the impact-scoped qualification gates**

```bash
bash -lc 'source .envrc && go test ./pkg/qualification ./pkg/version ./pkg/capability -count=1'
bash -lc 'source .envrc && APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 go test ./t/plugin -run "^(TestCorpus|TestDifferential|TestHTTPProtocolManifest|TestOracle)" -count=1'
bash scripts/qualification/oracle_lock_test.sh
bash scripts/qualification/flaky_policy_test.sh
bash scripts/qualification/operational_test.sh
bash scripts/qualification/platform_matrix_test.sh
bash scripts/release_metadata_test.sh
bash scripts/release_gate_test.sh
bash scripts/qualification/promotion_test.sh
bash -lc 'source .envrc && make build'
git diff --check
```

Expected: GREEN. Real-process `t/plugin` selectors remain serial and are run by their named workflow mode, not as a local broad aggregation.

- [ ] **Step 4: Run absence and consistency scans**

```bash
! rg -n 'apache/apisix:3\.17\.0([^@]|$)|continue-on-error:[[:space:]]*true|platforms:[[:space:]]*linux/amd64$' qualification scripts/qualification .github/workflows pkg/capability
! rg -n '(docker build|docker buildx build|build-push-action|go build|goreleaser (build|release))' <(awk '/^  promote:/{p=1} p{print}' .github/workflows/release.yml)
test ! -e .goreleaser.yaml
! rg -n 'apisix_.*linux.*\.(tar\.gz|zip)|checksums\.txt' .github/workflows
! rg -n '(legacy|fallback).*(release|publish|qualification)|publish-image.*rebuild' .github/workflows scripts/qualification
rg -n '9ef2ecab67f652d38365049613610ef649bb4ad0' qualification/oracle.yaml pkg/capability/manifest.yaml t/plugin/corpus_scope.yaml
```

Expected: first three scans return no matches; the final scan finds all three authoritative pins.

- [ ] **Step 5: Commit qualification documentation**

```bash
git add docs/qualification.md docs/architecture/compatibility-contract.md docs/design.md README.md cmd/capability-gen
git commit -m "docs(qualification): publish same-digest release contract"
```

## Plan Self-Review

- **Placeholder scan:** The `<64 lowercase hex>` text in the global constraint defines the accepted digest grammar; it is not a value to copy into a file. Task 2 resolves and commits the real registry digest. The placeholder-token and fake-digest scan returns no implementation placeholders.
- **Type consistency:** Tasks 1–9 use the exact `Outcome`, `GateKind`, `Platform`, `OracleIdentity`, `ArtifactIdentity`, `InputIdentity`, `EvidenceRecord`, `Result`, `FileDigest`, and `BundleManifest` definitions under Stable Interfaces. Capability evidence kinds remain owned by `pkg/capability`; runtime subsystems are consumed, not redefined.
- **Dependency consistency:** Plan 01 precedes oracle/corpus evaluation; plan 06 precedes differential completion; plans 03–05 and 07 precede operational qualification; plan 09 follows a passing signed/promoted result. There is no dependency from capability/config back into qualification.
- **Command consistency:** Every Go or Make command sources `.envrc`; no GoReleaser configuration or invocation remains; test commands are package/case scoped; real-process plugin tests are serialized; release-only external gates live in scheduled/release modes.
- **Path consistency:** All created paths are declared in task file lists. Workflow, script, Go package, corpus, and documentation paths exist today or are explicitly created by the task that first uses them.
- **Release consistency:** One signed/promoted OCI index digest covers the only Linux artifacts, child manifests for `amd64/arm64`; no independent Linux file can escape qualification. Both published macOS archives are built/smoked natively and bound by their archive digests to that release; Windows crossbuild produces no artifact; every evidence record, SBOM, signature, provenance statement, macOS archive, and production tag is verified before promotion, which has no rebuild route.

## Execution Handoff

Execute Tasks 1–9 in order. Stop at the first RED command that fails for a reason other than the expected missing implementation, at an unavailable/unverifiable official APISIX image, or at a blocked real dependency/platform/signing prerequisite. Preserve the partial immutable bundle and report the exact record ID, owner, reason, candidate digest, and command. Do not downgrade, retry away, or publish through a legacy path.
