# Six-Plugin HTTP Data Plane Controlled Rollout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Qualify and release one immutable Linux `amd64` apisix-go image for a named, protected environment running only the `http-data-plane-v1` profile: HTTP data plane, strict security, TLS-verified etcd, and exactly `basic-auth`, `cors`, `jwt-auth`, `key-auth`, `prometheus`, and `request-id` in the manifest-declared order.

**Architecture:** Keep the existing single-process `Server + GenerationEngine`, the global `security_profile: compat` default, and the independent compatibility/security/qualification axes. Add a fail-closed controlled-rollout gate around the existing `http-data-plane-v1` contract: ordered configuration validation, fresh APISIX 3.17 corpus accounting, six-plugin differential/failure/recovery evidence, immutable image-bound operational evidence, protected release policy, and a staged canary/rollback record. Passing this plan means only “qualified for this named controlled rollout”; it does not make the entire project production ready.

**Tech Stack:** Go 1.26, YAML, Bash, GitHub Actions, Docker/OCI on Linux `amd64`, official Apache APISIX 3.17 oracle, TLS-enabled etcd, Prometheus, Syft/Anchore SBOM, Trivy, Cosign, GitHub artifact attestations, and the existing `t/plugin` real-process harness.

**Spec:** `docs/production-profile.md`, `docs/runbooks/production-release.md`, `docs/architecture/compatibility-contract.md`, and `pkg/capability/manifest.yaml`.

## Controlled-Rollout Completion Contract

The milestone is complete only when one release evidence bundle proves all of the following for the same source commit and immutable image digest:

1. Effective configuration has `compatibility_target: apisix-3.17`, `security_profile: strict`, `qualification_profile: http-data-plane-v1`, `deployment.role: data_plane`, `apisix.proxy_mode: http`, no stream listener, no Admin API, and exactly the six required plugins in manifest order.
2. Every six-plugin required evidence kind is `verified` or an explicitly allowed `not_applicable`; no `missing`, `stale`, `deferred`, skipped, flaky, or blocked record is accepted.
3. The current APISIX 3.17 source corpus is accounted for, and profile-applicable behavior is exercised against an immutable official APISIX oracle.
4. The exact candidate image passes non-root container smoke, focused race/vulnerability gates, verified-TLS etcd recovery, six-plugin behavior checks, Prometheus scrape/query, 30-minute proxy soak, and rollback to a distinct known-good digest.
5. Repository and `production-release` environment policies prevent self-approved or check-bypassing publication.
6. A named staging/canary environment satisfies predeclared error, latency, resource, and recovery thresholds, with ingress access logs captured outside the process.

The release note and manifest must continue to say the project as a whole is not production ready. The only permitted positive claim is:

> `http-data-plane-v1` controlled-rollout qualified on Linux `amd64` for `<environment>`, source `<commit>`, image `<repository>@sha256:<digest>`, evidence bundle `<URL or checksum>`.

## Explicitly Deferred to the Next Production-Readiness Goal

- External supervisor/worker, listener inheritance, IPC activation, worker probation/restart, and multi-process zero-downtime lifecycle.
- Stream TLS, UDP, PROXY protocol, discovery, and general stream plugin chaining.
- Qualification of plugins outside the six-plugin profile.
- Linux `arm64`, multi-architecture OCI promotion, native macOS server qualification, and Windows release artifacts.
- Generic “production ready” wording, a broad public support matrix, and removal of the repository’s project-level warning.
- The complete cross-platform qualification framework in `2026-08-23-parity-qualification-release.md`; this plan implements only the subset required by the controlled HTTP milestone.

## Global Constraints

- Start implementation from a fresh fetched `origin/master` in an isolated worktree. The current main checkout contains user-owned `go.mod`, `go.sum`, configuration, and review-file changes; do not reset, clean, stage, or reuse them.
- Before any Go or Make command, run `source .envrc`; direct Go commands must run through `scripts/go_cache.sh run --`.
- Do not change the built-in `security_profile: compat` default. Production selection is an explicit deployment/release contract, not a global compatibility change.
- Do not couple `security_profile` and `qualification_profile` in the general loader. The release gate validates their exact joint values only for the controlled-rollout artifact.
- `pkg/capability/manifest.yaml` remains the editable source for profile membership and evidence. Never hand-edit `pkg/plugin/registry_gen.go`, `docs/plugins.md`, or generated README blocks.
- Preserve the six-plugin sequence exactly as `basic-auth`, `cors`, `jwt-auth`, `key-auth`, `prometheus`, `request-id`. Membership equality with a different order is a validation error.
- An applicable APISIX behavior block that remains unsupported blocks qualification. A `not_applicable` classification requires a reviewed profile boundary, owner, and concrete reason; a security-hardening divergence requires an ADR and explicit owner acceptance.
- Qualification evidence is append-only per attempt. A rerun is a new attempt and must not overwrite the original failure. Required evidence accepts only attempt `1` passing or a reviewed rerun policy recorded in the bundle.
- No secret, token, private key, etcd credential, or plaintext plugin credential may enter committed fixtures, logs, or uploaded evidence.
- Routine verification stays impact-scoped. Do not add `go test ./...`, `go test ./pkg/...`, `make test`, or the entire `t/plugin` suite as a default local gate.
- External repository settings, environment settings, tag creation, image publication, staging deployment, canary traffic, and rollback are separate authorization boundaries. Scripts may verify them read-only; mutation requires explicit authorization and the required human reviewer/operator.
- Under the repository’s subagent policy, execute this plan in the current agent unless the user explicitly authorizes delegation. The generic worker note above does not override `AGENTS.md`.

## Stable Narrow Evidence Contract

Create a versioned controlled-rollout record that the release workflow can validate without parsing log prose:

```go
// package qualification
type Outcome string

const (
	OutcomePass          Outcome = "pass"
	OutcomeFail          Outcome = "fail"
	OutcomeNotApplicable Outcome = "not_applicable"
)

type Observation struct {
	Status       int                 `json:"status" yaml:"status"`
	Headers      map[string][]string `json:"headers" yaml:"headers"`
	BodyBase64   string              `json:"body_base64" yaml:"body_base64"`
	MetricFamily map[string][]string `json:"metric_family,omitempty" yaml:"metric_family,omitempty"`
}

type EvidenceRecord struct {
	ID             string   `json:"id"`
	Plugin         string   `json:"plugin,omitempty"`
	Kind           string   `json:"kind"`
	Outcome        Outcome  `json:"outcome"`
	SourceCommit   string   `json:"source_commit"`
	ImageDigest    string   `json:"image_digest"`
	OracleDigest   string   `json:"oracle_digest,omitempty"`
	Command        []string `json:"command"`
	OutputSHA256   string   `json:"output_sha256"`
	Attempt        int      `json:"attempt"`
	Owner          string   `json:"owner,omitempty"`
	Reason         string   `json:"reason,omitempty"`
}

type ControlledRolloutResult struct {
	SchemaVersion        int              `json:"schema_version"`
	QualificationProfile string           `json:"qualification_profile"`
	Environment          string           `json:"environment"`
	SourceCommit         string           `json:"source_commit"`
	ImageReference       string           `json:"image_reference"`
	ConfigSHA256         string           `json:"config_sha256"`
	Outcome              Outcome          `json:"outcome"`
	Evidence             []EvidenceRecord `json:"evidence"`
}

func EvaluateHTTPDataPlaneV1(manifest *capability.Manifest, result *ControlledRolloutResult) error
func WriteHTTPDataPlaneV1Bundle(root string, result *ControlledRolloutResult, files []string) error
func VerifyHTTPDataPlaneV1Bundle(root string) (*ControlledRolloutResult, error)
```

`EvaluateHTTPDataPlaneV1` requires schema `1`, profile `http-data-plane-v1`, a 40-character source commit, a digest-qualified `linux/amd64` image, a non-empty named environment, the exact profile plugin/evidence matrix from the capability manifest, and all mandatory operational records. It rejects duplicate IDs, unknown kinds/outcomes, empty commands, invalid SHA-256 values, artifact/oracle identity drift, attempt `0`, a pass result containing any failed or absent requirement, and `not_applicable` without both owner and reason. `WriteHTTPDataPlaneV1Bundle` writes `result.json`, hashes sorted relative regular files, rejects symlinks/path escape, and writes `bundle-manifest.json`; verification recomputes every hash before evaluation.

The narrow types intentionally cover only this milestone. The later production-readiness plan may extend them for multi-platform results, but must not reinterpret version `1` records.

## Implementation Order

### Task 1: Establish a Clean Green Baseline

**Files:**
- Modify: `pkg/plugin/serverless/plugin.go`
- Test: `pkg/json/imports_test.go`

- [ ] **Step 1: Create the isolated implementation worktree**

```bash
git fetch origin master
git worktree add ../apisix-go-six-plugin-rollout -b codex/six-plugin-http-rollout origin/master
cd ../apisix-go-six-plugin-rollout
source .envrc
git status --short
git rev-parse HEAD
```

Expected: clean status and `HEAD` equal to the freshly fetched `origin/master` SHA.

- [ ] **Step 2: Reproduce the current master failure**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/json -run '^TestProductionCodeUsesProjectJSON$' -count=1
```

Expected: FAIL because `pkg/plugin/serverless/plugin.go` imports `encoding/json` directly.

- [ ] **Step 3: Make the minimum source fix**

Remove the `stdjson "encoding/json"` import and use the project package’s `json.Number` alias at the existing number conversion site. Do not change serverless behavior or reformat unrelated code.

- [ ] **Step 4: Verify the baseline repair and current source build**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/json -run '^TestProductionCodeUsesProjectJSON$' -count=1
source .envrc && make build
git diff --check
```

Expected: all commands pass. If another fresh `origin/master` failure appears, record it separately and stop before qualification work.

- [ ] **Step 5: Commit the baseline-only repair**

```bash
git add pkg/plugin/serverless/plugin.go
git commit -m "fix(json): use project JSON number alias"
```

### Task 2: Make the Production Profile Exact and Ordered

**Files:**
- Modify: `pkg/config/qualification.go`
- Modify: `pkg/config/effective_contract_test.go`
- Modify: `pkg/config/release_gate_test.go`
- Modify: `scripts/release_gate_test.sh`
- Modify: `docs/production-profile.md` only if executable behavior text needs correction after the tests are final

- [ ] **Step 1: Replace the reorder-pass regression with a reorder-fail test**

Add a table case that reverses the six required plugins and expects an error containing:

```text
qualification_profile http-data-plane-v1: plugins must exactly match required order
```

Keep separate assertions for duplicate, missing, and unexpected plugins so error categories stay diagnosable.

- [ ] **Step 2: Add release-tuple failure tests**

Extend the release gate fixtures so each of these independently fails:

- `security_profile: compat`
- empty or different `qualification_profile`
- compatibility target other than `apisix-3.17`
- reordered, missing, duplicate, or additional plugin
- non-HTTP proxy mode or any stream listener
- Admin API enabled
- etcd TLS verification disabled
- process-level request access logging enabled

Also keep one production fixture that passes with the exact tuple and six-plugin order.

- [ ] **Step 3: Run the focused tests and observe failure**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/config -run '^(TestValidateQualificationPlugins|TestProductionReleaseGate)' -count=1
bash scripts/release_gate_test.sh
```

Expected: reordered membership and at least the newly added release-tuple cases fail before implementation.

- [ ] **Step 4: Implement ordered equality without coupling the profile axes**

In `ValidateQualificationPlugins`, preserve the original slices for `slices.Equal(want, got)`. Use separately sorted copies only for duplicate/missing/unexpected diagnostics. Return the ordered-match error when membership is equal but sequence differs.

The release gate, not the general config loader, enforces the controlled tuple `apisix-3.17 + strict + http-data-plane-v1`.

- [ ] **Step 5: Verify focused configuration behavior**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/config -run '^(TestValidateQualificationPlugins|TestProductionReleaseGate)' -count=1
bash scripts/release_gate_test.sh
source .envrc && make build
git diff --check
```

- [ ] **Step 6: Commit the contract correction**

```bash
git add pkg/config/qualification.go pkg/config/effective_contract_test.go pkg/config/release_gate_test.go scripts/release_gate_test.sh docs/production-profile.md
git commit -m "fix(config): enforce ordered HTTP qualification profile"
```

### Task 3: Lock the Oracle and Refresh Only the Six-Plugin Corpus

**Files:**
- Create: `qualification/oracle.yaml`
- Create: `qualification/http-data-plane-v1.yaml`
- Create: `scripts/qualification/resolve_oracle.sh`
- Create: `scripts/qualification/resolve_oracle_test.sh`
- Modify: `t/plugin/corpus_scope.yaml`
- Create: `t/plugin/prometheus.yaml`
- Modify as required by fresh upstream mapping: `t/plugin/basic-auth.yaml`
- Modify as required by fresh upstream mapping: `t/plugin/cors.yaml`
- Modify as required by fresh upstream mapping: `t/plugin/jwt-auth.yaml`
- Modify as required by fresh upstream mapping: `t/plugin/key-auth.yaml`
- Modify as required by fresh upstream mapping: `t/plugin/request-id.yaml`

- [ ] **Step 1: Write the oracle-lock validator tests**

Fixtures must reject a mutable-only image tag, uppercase or malformed digest, wrong APISIX version/source commit, unknown YAML fields, multiple YAML documents, and a registry result whose manifest digest differs from the committed lock.

- [ ] **Step 2: Resolve the official oracle identity**

`qualification/oracle.yaml` must contain APISIX `3.17.0`, source commit `9ef2ecab67f652d38365049613610ef649bb4ad0`, repository `apache/apisix`, tag `3.17.0-debian`, and the real registry manifest digest returned by `docker buildx imagetools inspect`. The script must fail if the digest cannot be resolved or the image cannot prove the expected APISIX version; it must never substitute a community image.

- [ ] **Step 3: Enumerate fresh upstream blocks for the six plugins**

Fetch the pinned Apache APISIX source commit into a temporary directory and enumerate test blocks from only:

```text
t/plugin/basic-auth.t
t/plugin/cors.t and cors2.t when present at the pin
t/plugin/jwt-auth.t and jwt-auth-*.t when present at the pin
t/plugin/key-auth.t
t/plugin/prometheus.t and prometheus*.t when present at the pin
t/plugin/request-id.t
```

Update the corpus pin to the compatibility target and classify every enumerated block as `converted`, `not_applicable`, or `deferred`. `not_applicable` and `deferred` require an owner and substantive reason; any profile-applicable `deferred` block stops the milestone.

- [ ] **Step 4: Resolve the known JWT boundary explicitly**

- The APISIX sign endpoint is outside the data-plane route execution contract and may be `not_applicable` with that exact reason.
- Rejection of insecure 512-bit RSA keys requires a reviewed strict-security divergence ADR before it can be `not_applicable`; without the ADR, qualification stops.
- Ed448 remains blocking unless implemented or a reviewed profile-specific compatibility boundary records the algorithm subset. Do not silently mark the whole jwt-auth corpus converted.

- [ ] **Step 5: Add the missing Prometheus standalone manifest**

Cover at minimum a successful scrape with required APISIX-Go series, route/service labels under the bounded profile contract, series-limit behavior, and invalid configuration. Do not compare volatile timestamps, Go runtime internals, or exposition order.

- [ ] **Step 6: Verify corpus completeness and six standalone manifests**

```bash
source .envrc && make test-capability-status
source .envrc && make test-plugin-harness
source .envrc && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='basic-auth/valid-consumer-schema'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='cors/default-wildcard'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='jwt-auth/jwt-auth-sanity-test-1'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='key-auth/valid-consumer-schema'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='prometheus/profile-metrics-scrape'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='request-id/default-uuid-v4'
```

Run the six real-process commands sequentially. `prometheus/profile-metrics-scrape` is the exact case created in Step 5; the other five names already exist at the implementation baseline.

- [ ] **Step 7: Commit oracle/corpus scope as evidence, not a readiness claim**

```bash
git add qualification/oracle.yaml qualification/http-data-plane-v1.yaml scripts/qualification/resolve_oracle.sh scripts/qualification/resolve_oracle_test.sh t/plugin/corpus_scope.yaml t/plugin/basic-auth.yaml t/plugin/cors.yaml t/plugin/jwt-auth.yaml t/plugin/key-auth.yaml t/plugin/prometheus.yaml t/plugin/request-id.yaml
git commit -m "test(qualification): refresh six-plugin APISIX corpus"
```

### Task 4: Add Versioned Differential and Failure Evidence

**Files:**
- Create: `pkg/qualification/differential.go`
- Create: `pkg/qualification/normalize.go`
- Create: `pkg/qualification/differential_test.go`
- Create: `pkg/qualification/normalize_test.go`
- Modify: `qualification/http-data-plane-v1.yaml`
- Create: `scripts/qualification/http_data_plane_v1_differential.sh`
- Create: `scripts/qualification/http_data_plane_v1_differential_test.sh`

- [ ] **Step 1: Write strict manifest-loader tests**

Require schema `1`, the exact oracle identity, sorted unique case IDs, one of the six plugin names, a source block mapping, subject-equivalent configuration, request input, expected observation, normalization version, and evidence kind `differential` or `failure`. Reject unknown fields, second YAML documents, missing fixtures, and case IDs that are absent from fresh corpus accounting.

- [ ] **Step 2: Write normalization tests before the runner**

Normalization may remove only generated `Date`, request IDs explicitly designated as nondeterministic, Prometheus timestamps, and exposition order. Tests must prove it does not change status, asserted headers, body bytes, authentication decisions, selected consumer, metric label values, or series-limit results.

- [ ] **Step 3: Implement same-input dual execution**

The script starts the immutable official APISIX oracle and the locally loaded candidate image with logically equivalent routes/consumers/plugin config and the same upstream/TLS fixtures. It records raw request, both raw observations, normalized observations, container identities, source commit, case ID, command, attempt, and SHA-256 values under `.cache/release-evidence/http-data-plane-v1/differential/`.

- [ ] **Step 4: Cover minimum profile behavior and failure cases**

For each plugin, include at least one successful and one rejected/edge request:

- `basic-auth`: valid consumer; wrong or absent credentials.
- `cors`: allowed preflight/actual response; disallowed origin or invalid config boundary.
- `jwt-auth`: valid token; expired/bad signature/absent token under the declared algorithm subset.
- `key-auth`: valid consumer; wrong/absent/duplicate credential source.
- `prometheus`: bounded expected metric families; series overflow or invalid label/config boundary.
- `request-id`: preserved valid incoming ID or generated output contract; invalid/absent input behavior.

- [ ] **Step 5: Run the package and script contract tests**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/qualification -run '^(TestLoadDifferentialManifest|TestNormalize|TestCompare)' -count=1
bash scripts/qualification/http_data_plane_v1_differential_test.sh
git diff --check
```

- [ ] **Step 6: Run the real differential gate on a Docker host**

```bash
bash scripts/qualification/http_data_plane_v1_differential.sh
```

Expected: one pass record per required case, bound to the committed oracle digest and candidate image ID. Any unavailable Docker/registry prerequisite is reported as blocked and does not permit evidence promotion.

- [ ] **Step 7: Commit differential infrastructure and cases**

```bash
git add pkg/qualification qualification/http-data-plane-v1.yaml scripts/qualification/http_data_plane_v1_differential.sh scripts/qualification/http_data_plane_v1_differential_test.sh
git commit -m "test(qualification): add six-plugin differential gate"
```

### Task 5: Turn Verified-TLS Etcd Recovery into Six-Plugin Recovery Evidence

**Files:**
- Modify: `scripts/etcd_recovery_smoke.sh`
- Modify: `scripts/etcd_recovery_smoke_test.sh`
- Create: `scripts/qualification/http_data_plane_v1_recovery_test.sh`
- Modify: `docs/runbooks/production-release.md`

- [ ] **Step 1: Add shell-fixture failures before changing the harness**

Tests must fail when the recovery script omits any required plugin, uses plaintext etcd, disables certificate verification, accepts only one data-plane replica, restores an uncommitted generation, loses a committed route after compaction/restart, or writes evidence without source/image identities.

- [ ] **Step 2: Define one six-plugin route set and expected probes**

The TLS etcd fixture provisions two apisix-go replicas with the exact production profile. It writes routes/consumers that exercise all six plugins, verifies both replicas, updates credentials/CORS/JWT/request-id/Prometheus-visible route data, compacts etcd, disconnects/reconnects, restarts one replica, deletes and re-adds resources, and checks that committed state is preserved while deleted state never falls back.

- [ ] **Step 3: Record recovery evidence per plugin**

Write one JSON record per plugin plus journal/generation records under `.cache/release-evidence/etcd-recovery/http-data-plane-v1/`. Each record includes source commit, candidate image ID/digest, profile, plugin, before/after generation, probe result, etcd TLS peer/certificate metadata without secrets, command, attempt, and output hash.

`request-id` may remain `not_applicable` for plugin-owned recovery because it has no durable external dependency, but route publication/recovery must still exercise it. The other five plugins require passing recovery records.

- [ ] **Step 4: Run focused shell and package verification**

```bash
bash scripts/etcd_recovery_smoke_test.sh
bash scripts/qualification/http_data_plane_v1_recovery_test.sh
source .envrc && scripts/go_cache.sh run -- go test -race ./pkg/etcd ./pkg/store ./pkg/generation ./pkg/server ./pkg/route -count=1
```

- [ ] **Step 5: Run the immutable-image recovery gate**

```bash
source .envrc && make release-etcd-recovery APISIX_IMAGE='<candidate-image-id-or-digest>'
```

Expected: both replicas and all profile probes pass through compaction, disconnect, restart, delete, and re-add. A local source run or Docker-free fixture test is not qualification evidence.

- [ ] **Step 6: Commit recovery-gate changes**

```bash
git add scripts/etcd_recovery_smoke.sh scripts/etcd_recovery_smoke_test.sh scripts/qualification/http_data_plane_v1_recovery_test.sh docs/runbooks/production-release.md
git commit -m "test(recovery): qualify six-plugin HTTP generations"
```

### Task 6: Add the Real Prometheus Consumer and Evidence Bundle Evaluator

**Files:**
- Create: `pkg/qualification/types.go`
- Create: `pkg/qualification/evaluate.go`
- Create: `pkg/qualification/bundle.go`
- Create: `pkg/qualification/evaluate_test.go`
- Create: `pkg/qualification/bundle_test.go`
- Create: `scripts/qualification/http_data_plane_v1_prometheus.sh`
- Create: `scripts/qualification/http_data_plane_v1_prometheus_test.sh`
- Create: `qualification/http-data-plane-v1/required-evidence.yaml`

- [ ] **Step 1: Implement the Stable Narrow Evidence Contract test-first**

Write tests that reject a wrong profile, mutable image tag, missing plugin/evidence kind, failed record, stale source/oracle digest, duplicate record ID, invalid attempt, missing command/output hash, invalid `not_applicable`, symlink/path escape, extra bundle file, and an overall pass with any missing operational record.

- [ ] **Step 2: Declare the exact required matrix**

`required-evidence.yaml` maps the manifest’s seven evidence kinds for each six-plugin member and mandatory operational IDs:

```text
config-exact
container-nonroot-smoke
focused-race
reachable-vulnerability-scan
tls-etcd-recovery
prometheus-real-consumer
proxy-soak-30m
external-access-log
capacity-envelope
failure-injection
rollback-distinct-digest
repository-policy
release-environment-policy
```

The evaluator cross-checks plugin kinds against `pkg/capability/manifest.yaml` so this file cannot weaken the manifest requirement.

- [ ] **Step 3: Add a pinned real Prometheus check**

Start the candidate image and a digest-pinned Prometheus container, configure Prometheus to scrape the gateway, generate bounded six-plugin traffic, query required series through the Prometheus API, and record both the scrape target health and query results. The test script must reject a tag-only Prometheus image, an empty query result, unbounded cardinality, or evidence tied to another candidate image.

- [ ] **Step 4: Verify evaluator and real-consumer contract**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/qualification -run '^(TestEvaluateHTTPDataPlaneV1|TestWriteHTTPDataPlaneV1Bundle|TestVerifyHTTPDataPlaneV1Bundle)' -count=1
bash scripts/qualification/http_data_plane_v1_prometheus_test.sh
bash scripts/qualification/http_data_plane_v1_prometheus.sh '<candidate-image-id-or-digest>'
```

- [ ] **Step 5: Commit the evidence evaluator**

```bash
git add pkg/qualification scripts/qualification/http_data_plane_v1_prometheus.sh scripts/qualification/http_data_plane_v1_prometheus_test.sh qualification/http-data-plane-v1/required-evidence.yaml
git commit -m "feat(qualification): evaluate controlled HTTP rollout evidence"
```

### Task 7: Promote Capability Evidence Only After It Exists

**Files:**
- Modify: `pkg/capability/manifest.yaml`
- Modify: `pkg/capability/manifest_test.go`
- Generated: `pkg/plugin/registry_gen.go`
- Generated: `docs/plugins.md`
- Generated: README capability summaries
- Modify: `docs/production-profile.md`

- [ ] **Step 1: Add a fail-closed evidence-reference test**

For every `verified` six-plugin evidence entry, assert that each referenced committed path exists and is represented in `qualification/http-data-plane-v1/required-evidence.yaml`. For `not_applicable`, assert non-empty owner/reason and prohibit references to passing runtime records.

- [ ] **Step 2: Run the test and observe current missing/stale states**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/capability -run '^TestHTTPDataPlaneV1EvidenceReferences$' -count=1
source .envrc && make test-capability-status
```

Expected before manifest promotion: the six plugins remain unqualified due to stale/missing evidence.

- [ ] **Step 3: Update only evidence that is proven by Tasks 3–6**

- `converted_upstream`: six fresh standalone manifests and complete current-pin corpus mapping.
- `differential`: versioned oracle/candidate cases for all six plugins.
- `failure`: named negative cases for all six plugins.
- `real_dependency`: Prometheus is `verified` by the real consumer; plugins without an outbound dependency may be `not_applicable` only with a concrete reason and owner.
- `recovery`: five state-bearing profile paths are `verified`; request-id is `not_applicable` only for plugin-owned recovery while its route recovery probe remains mandatory.
- `schema` and `unit`: retain current verified references unless fresh inspection shows drift.

Do not promote a state from CI intent, a local source-only run, or a planned script.

- [ ] **Step 4: Regenerate projections from the manifest**

```bash
source .envrc && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root .
source .envrc && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check
```

- [ ] **Step 5: Verify the profile becomes selectable and remains narrow**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/capability ./pkg/config -count=1
source .envrc && make test-capability-status
source .envrc && make check-capability-drift
source .envrc && make build
git diff --check
```

- [ ] **Step 6: Commit the evidence promotion and generated projections together**

```bash
git add pkg/capability/manifest.yaml pkg/capability/manifest_test.go pkg/plugin/registry_gen.go docs/plugins.md docs/production-profile.md README.md
git commit -m "docs(capability): qualify six-plugin HTTP rollout profile"
```

### Task 8: Make RC and Final Workflows Enforce the Same Evidence Bundle

**Files:**
- Modify: `.github/workflows/release-candidate.yml`
- Modify: `.github/workflows/release.yml`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `scripts/release_metadata.sh`
- Modify: `scripts/release_metadata_test.sh`
- Create: `scripts/qualification/http_data_plane_v1_workflow_test.sh`

- [ ] **Step 1: Add workflow-structure tests**

The shell test parses the three workflow files and rejects:

- a release matrix missing any of the six plugin smokes;
- use of a non-production profile fixture in operational jobs;
- publication without bundle verification;
- a final release that rebuilds after qualification;
- source commit, image digest, config hash, or oracle digest not propagated into the bundle;
- artifact download with warning/ignore behavior for required evidence;
- release notes that omit the narrow controlled-rollout label.

- [ ] **Step 2: Replace the unrelated four-case matrix**

Both RC and final release run one exact case per required plugin. Keep broader CI elsewhere; the release matrix’s purpose is the declared six-plugin profile, not representative unrelated plugins.

- [ ] **Step 3: Add a qualification-bundle job**

After the existing image, etcd, and soak jobs, download all required evidence, run `VerifyHTTPDataPlaneV1Bundle`, and upload one immutable bundle named with the source commit and image digest. Required files use `if-no-files-found: error`. Publication depends on this job and consumes the same loaded image archive/digest; it must not rebuild.

- [ ] **Step 4: Preserve RC/final separation**

RC builds and qualifies without publishing a production tag. Final release accepts only the previously qualified source/image identity, re-verifies the bundle and protected environment, signs/attests, then promotes that digest. If the current pipeline cannot promote the RC digest without rebuilding, stop at RC until same-digest promotion is implemented; do not call a rebuilt final image qualified.

- [ ] **Step 5: Verify scripts and workflow structure locally**

```bash
bash scripts/release_metadata_test.sh
bash scripts/qualification/http_data_plane_v1_workflow_test.sh
source .envrc && scripts/go_cache.sh run -- go test ./pkg/qualification -count=1
git diff --check
```

- [ ] **Step 6: Commit the fail-closed workflow gate**

```bash
git add .github/workflows/release-candidate.yml .github/workflows/release.yml .github/workflows/security-release-gates.yml scripts/release_metadata.sh scripts/release_metadata_test.sh scripts/qualification/http_data_plane_v1_workflow_test.sh
git commit -m "ci(release): gate six-plugin controlled rollout"
```

### Task 9: Verify and Harden Repository/Environment Policy

**Files:**
- Create: `qualification/http-data-plane-v1/repository-policy.json`
- Create: `scripts/qualification/repository_policy.sh`
- Create: `scripts/qualification/repository_policy_test.sh`
- Modify: `docs/runbooks/production-release.md`

- [ ] **Step 1: Add API-response fixture tests**

Tests reject a default-branch ruleset with zero approvals, no required status checks, unresolved conversations allowed, deletion/force-push allowed, or a bypass actor. They also reject `production-release` when admins can bypass, self-review is allowed, no independent required reviewer exists, or the wait timer is below five minutes.

- [ ] **Step 2: Commit the exact required policy**

The JSON contract requires at least one non-author approval, resolved review threads, no force-push/deletion, no bypass, and the exact green contexts emitted by the final workflow after Task 8. The protected environment requires an independent reviewer, `prevent_self_review: true`, `can_admins_bypass: false`, and a wait timer of at least five minutes.

- [ ] **Step 3: Run a read-only live audit**

```bash
bash scripts/qualification/repository_policy_test.sh
bash scripts/qualification/repository_policy.sh --repository "$GITHUB_REPOSITORY" --environment production-release --check
```

Expected on the current repository snapshot: FAIL, because required approvals/status checks and environment anti-bypass controls are not yet sufficient.

- [ ] **Step 4: Apply settings only after explicit authorization**

Use GitHub’s ruleset/environment APIs to apply the committed JSON contract. This step requires the repository owner to name the independent reviewer and authorize external mutation. Capture redacted before/after API responses as rollout evidence.

- [ ] **Step 5: Re-run the read-only audit until it passes**

```bash
bash scripts/qualification/repository_policy.sh --repository "$GITHUB_REPOSITORY" --environment production-release --check
```

- [ ] **Step 6: Commit verifier and runbook only; do not commit live API payloads**

```bash
git add qualification/http-data-plane-v1/repository-policy.json scripts/qualification/repository_policy.sh scripts/qualification/repository_policy_test.sh docs/runbooks/production-release.md
git commit -m "ci(policy): require protected controlled releases"
```

### Task 10: Run RC, Staging Canary, Rollback, and Final Promotion

**Files:**
- Create: `qualification/http-data-plane-v1/rollout-contract.schema.json`
- Create: `scripts/qualification/verify_rollout_contract.sh`
- Create: `scripts/qualification/verify_rollout_contract_test.sh`
- Modify: `docs/runbooks/production-release.md`
- Modify: `README.md` only for the narrow release-status link; retain project-level not-ready wording

- [ ] **Step 1: Validate operator inputs before deployment**

The rollout contract requires: environment name; source commit; digest-qualified candidate and distinct rollback images; effective config SHA-256; replica count at least two; canary percentage/duration; maximum 5xx rate; p99 latency ceiling; CPU/memory ceilings; readiness and recovery deadlines; external ingress access-log evidence location; deployment/rollback command identities; approver; and evidence destination. It rejects secrets and tag-only images.

- [ ] **Step 2: Define thresholds before the RC run**

The operator supplies numeric thresholds from the target environment’s existing SLO/capacity contract. Once the RC starts, threshold changes invalidate the attempt rather than making a failure pass.

- [ ] **Step 3: Run the RC workflow on an immutable commit**

Required outcome: green lint/build/unit/harness, exact six-plugin smokes, differential/failure evidence, SBOM/Trivy, focused race/vulnerability scan, real Prometheus, verified-TLS etcd recovery, 30-minute soak, capacity/failure injection, and a verified evidence bundle tied to one image digest.

- [ ] **Step 4: Deploy the exact RC digest to staging**

Render and hash the effective config; verify `strict`, the exact profile tuple and plugin order, no environment proxy variables, no stream/Admin API, trusted ingress CIDRs only, verified etcd TLS, and external ingress request logging. Deploy two or more replicas and run the six-plugin probe set.

- [ ] **Step 5: Run canary and failure drills**

Shift only the predeclared traffic percentage. During the fixed duration, record requests, 5xx rate, p50/p95/p99, CPU, memory, goroutines, file descriptors, active/retiring generations, etcd reconnect/compaction behavior, replica restart, upstream failure, certificate/credential rejection, and ingress logs. Any missing signal or exceeded threshold fails the attempt.

- [ ] **Step 6: Prove rollback and re-upgrade**

Roll back to the distinct known-good digest within the declared recovery objective, verify six-plugin traffic and state, then re-deploy the same candidate digest and repeat the probe set. A restart of the same digest is not rollback evidence.

- [ ] **Step 7: Approve and promote the same digest**

After an independent reviewer approves the protected environment, promote/sign/attest the exact qualified digest. The final tag, release metadata, bundle, SBOM, vulnerability report, config hash, source commit, and image reference must agree byte-for-byte on identity.

- [ ] **Step 8: Verify the final release externally**

```bash
gh release view '<final-tag>' --json tagName,targetCommitish,url,assets
gh run list --workflow release.yml --limit 5
docker buildx imagetools inspect '<repository>@sha256:<qualified-digest>'
bash scripts/qualification/repository_policy.sh --repository '<owner/repo>' --environment production-release --check
bash scripts/qualification/verify_rollout_contract.sh '<private-rollout-contract.json>'
```

Read back the published release and artifact identities. Do not claim success from the local tag or workflow dispatch alone.

- [ ] **Step 9: Update only the controlled-rollout status**

Record the environment, source SHA, image digest, evidence checksum/link, date, expiry/requalification trigger, and rollback digest in the release/runbook record. Keep supervisor/worker, stream, multiarch, and overall production readiness open.

- [ ] **Step 10: Commit documentation/schema changes after evidence exists**

```bash
git add qualification/http-data-plane-v1/rollout-contract.schema.json scripts/qualification/verify_rollout_contract.sh scripts/qualification/verify_rollout_contract_test.sh docs/runbooks/production-release.md README.md
git commit -m "docs(release): record controlled HTTP rollout qualification"
```

## Final Verification Matrix

Run these only after Tasks 1–9 have landed on the same candidate branch and the relevant Docker/external prerequisites are available:

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/json ./pkg/config ./pkg/capability ./pkg/qualification -count=1
source .envrc && scripts/go_cache.sh run -- go test -race ./pkg/etcd ./pkg/store ./pkg/generation ./pkg/server ./pkg/route ./pkg/proxy -count=1
source .envrc && make test-capability-status
source .envrc && make test-plugin-harness
source .envrc && make check-capability-drift
bash scripts/release_gate_test.sh
bash scripts/release_metadata_test.sh
bash scripts/etcd_recovery_smoke_test.sh
bash scripts/qualification/resolve_oracle_test.sh
bash scripts/qualification/http_data_plane_v1_differential_test.sh
bash scripts/qualification/http_data_plane_v1_recovery_test.sh
bash scripts/qualification/http_data_plane_v1_prometheus_test.sh
bash scripts/qualification/http_data_plane_v1_workflow_test.sh
bash scripts/qualification/repository_policy_test.sh
bash scripts/qualification/verify_rollout_contract_test.sh
source .envrc && make lint
source .envrc && make build
git diff --check
```

Then execute the Docker/operational gates against the same immutable candidate image:

```bash
bash scripts/qualification/http_data_plane_v1_differential.sh
bash scripts/qualification/http_data_plane_v1_prometheus.sh '<candidate-image-id-or-digest>'
source .envrc && make release-etcd-recovery APISIX_IMAGE='<candidate-image-id-or-digest>'
source .envrc && APISIX_GO_RUN_SOAK=1 APISIX_GO_SOAK_DURATION=30m scripts/go_cache.sh run -- go test -json ./pkg/route -run '^TestProxyRuntimeSoak$' -count=1 -timeout=40m
```

Finally, verify the generated bundle, live repository policy, protected environment, staging/canary record, rollback record, signature, attestation, and published digest. If any command is skipped, narrowed beyond the declared case set, blocked, flaky, or fails, report the milestone as incomplete.

## Stop/Go Decision

`GO` is allowed only when all ten tasks are complete and the final bundle evaluator returns `pass`. The following are hard stops:

- any of the six plugin evidence kinds remains stale/missing/deferred;
- a profile-applicable APISIX test block is unimplemented or unclassified;
- the candidate and oracle are tag-only or change digest;
- RC and final images differ;
- strict config, exact plugin order, TLS verification, or environment-proxy clearing is not proven from effective runtime state;
- repository/environment policy still permits bypass or self-approval;
- canary thresholds were not predeclared or were exceeded;
- rollback was not executed with a distinct digest;
- evidence exists only in local logs rather than a durable, hash-verified bundle.

After `GO`, the status is still “controlled six-plugin HTTP rollout qualified,” not “apisix-go production ready.”
