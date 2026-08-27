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
- Production qualification outside the six-plugin profile; APISIX 3.17 parity
  implementation continues plugin-by-plugin in later corpus waves.
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

## Execution Revision: Global APISIX 3.17 Plugin Parity

This revision supersedes the original Task 3–10 design. The long-term product
goal is observable APISIX 3.17 parity for every applicable plugin; the six
HTTP-profile plugins are the first migration and release wave. Do not create
`pkg/qualification` or a second profile-specific corpus.

### Canonical Source Ledger

| Contract | Canonical source | Derived or consuming surfaces |
| --- | --- | --- |
| APISIX compatibility target | `pkg/capability/manifest.yaml.target.source_commit` | corpus freshness tests, generated docs |
| Upstream block accounting and migration progress | `t/plugin/corpus_scope.yaml` | `t/plugin/*.yaml source.commit` and source selections |
| Executable converted behavior | `t/plugin/*.yaml` | focused real-process integration results |
| Plugin behavior/evidence status | `pkg/capability/manifest.yaml` | registry, `docs/plugins.md`, README summaries |
| Release/operational acceptance | existing scripts and GitHub workflows | immutable CI evidence artifacts |

The corpus remains one global ledger. Its top-level `commit` is the default
historical commit for rows not yet migrated. An optional row-level `commit`
records the actual APISIX source commit for that exact file/test-number set.
The effective row commit is `source.commit` when present, otherwise the
top-level default. A converted standalone manifest must use the same effective
commit as every ledger row it implements. Freshness is computed by comparing
those effective commits with the capability target; it is never inferred from
the top-level default alone.

This schema permits reviewable plugin waves while preserving one source of
truth. The end state is reached when every applicable row and executable
manifest uses the APISIX 3.17 target commit; only then may the historical
default be removed or advanced.

## Implementation Order

### Task 1: Establish a Clean Green Baseline

**Files:**
- Modify: `pkg/plugin/serverless/plugin.go`
- Test: `pkg/json/imports_test.go`

- [x] **Step 1: Create the isolated implementation worktree**

```bash
git fetch origin master
git worktree add ../apisix-go-six-plugin-rollout -b codex/six-plugin-http-rollout origin/master
cd ../apisix-go-six-plugin-rollout
source .envrc
git status --short
git rev-parse HEAD
```

Expected: clean status and `HEAD` equal to the freshly fetched `origin/master` SHA.

- [x] **Step 2: Reproduce the current master failure**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/json -run '^TestProductionCodeUsesProjectJSON$' -count=1
```

Expected: FAIL because `pkg/plugin/serverless/plugin.go` imports `encoding/json` directly.

- [x] **Step 3: Make the minimum source fix**

Remove the `stdjson "encoding/json"` import and use the project package’s `json.Number` alias at the existing number conversion site. Do not change serverless behavior or reformat unrelated code.

- [x] **Step 4: Verify the baseline repair and current source build**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/json -run '^TestProductionCodeUsesProjectJSON$' -count=1
source .envrc && make build
git diff --check
```

Expected: all commands pass. If another fresh `origin/master` failure appears, record it separately and stop before qualification work.

- [x] **Step 5: Commit the baseline-only repair**

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

- [x] **Step 1: Replace the reorder-pass regression with a reorder-fail test**

Add a table case that reverses the six required plugins and expects an error containing:

```text
qualification_profile http-data-plane-v1: plugins must exactly match required order
```

Keep separate assertions for duplicate, missing, and unexpected plugins so error categories stay diagnosable.

- [x] **Step 2: Add release-tuple failure tests**

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

- [x] **Step 3: Run the focused tests and observe failure**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/config -run '^(TestValidateQualificationPlugins|TestProductionReleaseGate)' -count=1
bash scripts/release_gate_test.sh
```

Expected: reordered membership and at least the newly added release-tuple cases fail before implementation.

- [x] **Step 4: Implement ordered equality without coupling the profile axes**

In `ValidateQualificationPlugins`, preserve the original slices for `slices.Equal(want, got)`. Use separately sorted copies only for duplicate/missing/unexpected diagnostics. Return the ordered-match error when membership is equal but sequence differs.

The release gate, not the general config loader, enforces the controlled tuple `apisix-3.17 + strict + http-data-plane-v1`.

- [x] **Step 5: Verify focused configuration behavior**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/config -run '^(TestValidateQualificationPlugins|TestProductionReleaseGate)' -count=1
bash scripts/release_gate_test.sh
source .envrc && make build
git diff --check
```

- [x] **Step 6: Commit the contract correction**

```bash
git add pkg/config/qualification.go pkg/config/effective_contract_test.go pkg/config/release_gate_test.go scripts/release_gate_test.sh docs/production-profile.md
git commit -m "fix(config): enforce ordered HTTP qualification profile"
```

### Task 3: Make the Single Global Corpus Incrementally Migratable

**Files:**
- Modify: `t/plugin/corpus_test.go`
- Modify: `t/plugin/coverage_test.go`
- Modify: `t/plugin/corpus_scope.yaml`
- Modify: `t/plugin/README.md`

- [ ] **Step 1: Add failing mixed-commit ledger tests**

Tests must prove that a row-level commit is a lowercase 40-character object ID,
that converted manifest selections match the effective commit for every source
label, and that a manifest cannot combine labels whose ledger rows disagree on
commit. The old single-commit fixture must continue to pass unchanged.

- [ ] **Step 2: Observe RED against the current loader**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run '^(TestCorpusScopeAllowsPerSourceMigration|TestManifestSelectionsUseEffectiveCorpusCommit|TestCorpusEvidenceMatchesCompatibilityTarget)$' -count=1
```

- [ ] **Step 3: Implement effective-commit resolution**

Add optional `commit` to `corpusSourceScope`. Keep strict YAML and duplicate
label validation. Replace global-commit manifest comparison with a lookup by
`file + test number`; require every selected label to exist, name the same
manifest, and have the same effective commit as the manifest source.

- [ ] **Step 4: Make freshness plugin-specific**

`TestCorpusEvidenceMatchesCompatibilityTarget` must inspect the manifests named
by each plugin's converted-upstream refs. A claim is fresh only when every
source in every referenced manifest uses the capability target commit. Other
plugins remain stale while the first six migrate.

- [ ] **Step 5: Verify legacy and mixed-commit accounting**

```bash
source .envrc && make test-plugin-harness
source .envrc && make test-capability-status
git diff --check
```

- [ ] **Step 6: Commit the ledger migration seam**

```bash
git add t/plugin/corpus_test.go t/plugin/coverage_test.go t/plugin/corpus_scope.yaml t/plugin/README.md
git commit -m "test(corpus): support incremental APISIX source migration"
```

### Task 4: Migrate the First Six Plugins to the APISIX 3.17 Corpus

**Files:**
- Create: `qualification/oracle.yaml`
- Create: `scripts/qualification/resolve_oracle.sh`
- Create: `scripts/qualification/resolve_oracle_test.sh`
- Modify: `t/plugin/corpus_scope.yaml`
- Modify: `t/plugin/basic-auth.yaml`
- Modify: `t/plugin/cors.yaml`
- Modify: `t/plugin/jwt-auth.yaml`
- Modify: `t/plugin/key-auth.yaml`
- Create: `t/plugin/prometheus.yaml`
- Modify: `t/plugin/request-id.yaml`

- [ ] **Step 1: Lock the official APISIX 3.17 oracle**

Resolve `apache/apisix:3.17.0-debian` to an immutable registry digest and verify
the running image reports APISIX 3.17.0. The lock also records source commit
`9ef2ecab67f652d38365049613610ef649bb4ad0`. Mutable tags or unverifiable
source/version identity fail closed.

- [ ] **Step 2: Fetch the target source and enumerate six-plugin blocks**

Use a temporary checkout at the exact target commit. For every basic-auth,
cors, jwt-auth, key-auth, prometheus, and request-id upstream plugin test file,
compare file presence and `=== TEST` labels with the historical manifest and
ledger. Update row-level commits and selections only after exact accounting.

- [ ] **Step 3: Resolve JWT boundaries explicitly**

The sign endpoint is outside the data-plane request contract. Insecure 512-bit
RSA rejection requires an accepted strict-security divergence. Ed448 remains
blocking unless implemented or covered by an accepted algorithm-subset ADR.
No unresolved applicable block may be called converted.

- [ ] **Step 4: Add the missing Prometheus standalone manifest**

Cover profile metrics scrape, bounded route/service labels, series overflow,
and invalid configuration through the real process. Volatile timestamps,
runtime internals, and exposition order are not asserted.

- [ ] **Step 5: Run six exact real-process cases sequentially**

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

Run the six real-process commands sequentially. `prometheus/profile-metrics-scrape` is the exact case created in Step 4; the other five names already exist at the implementation baseline.

- [ ] **Step 6: Commit the first global-corpus migration wave**

```bash
git add qualification/oracle.yaml scripts/qualification/resolve_oracle.sh scripts/qualification/resolve_oracle_test.sh t/plugin/corpus_scope.yaml t/plugin/basic-auth.yaml t/plugin/cors.yaml t/plugin/jwt-auth.yaml t/plugin/key-auth.yaml t/plugin/prometheus.yaml t/plugin/request-id.yaml
git commit -m "test(qualification): refresh six-plugin APISIX corpus"
```

### Task 5: Close First-Wave Plugin Parity Gaps

**Files:**
- Modify only the owning `pkg/plugin/<name>` packages exposed by fresh cases
- Modify the corresponding focused unit tests
- Modify the six `t/plugin/*.yaml` manifests only when upstream mapping requires it

- [ ] **Step 1: Run each fresh case and record concrete parity failures**

Do not change production code for a fixture/harness defect. For every real
behavior mismatch, add the smallest owning-package test that reproduces it and
observe RED before implementation.

- [ ] **Step 2: Implement minimum APISIX 3.17 observable behavior**

Preserve strict-security divergences only when accepted and recorded. Do not
approximate Lua/OpenResty internals when the repository contract treats them as
native-only.

- [ ] **Step 3: Verify each plugin independently**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/basic_auth -count=1
source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/cors -count=1
source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/jwt_auth -count=1
source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/key_auth -count=1
source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/prometheus -count=1
source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/request_id -count=1
```

- [ ] **Step 4: Re-run the exact six standalone cases and build**

Use the Task 4 selectors, then run `make build` and `git diff --check`.

### Task 6: Turn Verified-TLS Etcd Recovery into Six-Plugin Recovery Evidence

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

### Task 7: Promote Capability Evidence Only After It Exists

**Files:**
- Modify: `pkg/capability/manifest.yaml`
- Modify: `pkg/capability/manifest_test.go`
- Generated: `pkg/plugin/registry_gen.go`
- Generated: `docs/plugins.md`
- Generated: README capability summaries
- Modify: `docs/production-profile.md`

- [ ] **Step 1: Add a fail-closed evidence-reference test**

For every `verified` six-plugin converted-upstream entry, assert that every
referenced manifest source uses the capability target commit. Other evidence
states retain their existing source-specific validation. `not_applicable`
requires non-empty owner/reason.

- [ ] **Step 2: Run the test and observe current missing/stale states**

```bash
source .envrc && scripts/go_cache.sh run -- go test ./pkg/capability -run '^TestHTTPDataPlaneV1EvidenceReferences$' -count=1
source .envrc && make test-capability-status
```

Expected before manifest promotion: the six plugins remain unqualified due to stale/missing evidence.

- [ ] **Step 3: Update only evidence that is proven by Tasks 3–6**

- `converted_upstream`: six fresh standalone manifests and complete current-pin corpus mapping.
- `differential`: record only when the release scripts execute equivalent
  oracle/candidate cases; a fresh standalone conversion alone is insufficient.
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

### Task 8: Make RC and Final Workflows Enforce Existing Evidence Sources

**Files:**
- Modify: `.github/workflows/release-candidate.yml`
- Modify: `.github/workflows/release.yml`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `scripts/release_metadata.sh`
- Modify: `scripts/release_metadata_test.sh`
- Create: `scripts/qualification/http_data_plane_v1_workflow_test.sh`
- Create: `scripts/qualification/http_data_plane_v1_differential.sh`
- Create: `scripts/qualification/http_data_plane_v1_differential_test.sh`
- Create: `scripts/qualification/http_data_plane_v1_prometheus.sh`
- Create: `scripts/qualification/http_data_plane_v1_prometheus_test.sh`

- [ ] **Step 1: Add workflow-structure tests**

The shell test parses the three workflow files and rejects:

- a release matrix missing any of the six plugin smokes;
- use of a non-production profile fixture in operational jobs;
- publication without capability/corpus, immutable image, recovery, soak, and
  rollback evidence verification;
- a final release that rebuilds after qualification;
- source commit, image digest, config hash, or oracle digest not propagated into the bundle;
- artifact download with warning/ignore behavior for required evidence;
- release notes that omit the narrow controlled-rollout label.

- [ ] **Step 2: Replace the unrelated four-case matrix**

Both RC and final release run one exact case per required plugin. Keep broader CI elsewhere; the release matrix’s purpose is the declared six-plugin profile, not representative unrelated plugins.

- [ ] **Step 3: Add script-owned differential and real Prometheus evidence**

Run equivalent declarative requests against the immutable APISIX oracle and
candidate image, with reviewed normalization limited to volatile fields. Run a
digest-pinned Prometheus consumer that scrapes and queries the candidate. Both
scripts emit source/image/oracle/config identities and fail on missing cases,
empty observations, or mutable-only images.

- [ ] **Step 4: Add an evidence-archive job**

After the existing image, etcd, and soak jobs, download all required evidence,
verify their source commit/image/config/oracle identities with release scripts,
and upload one immutable archive plus checksum. Required files use
`if-no-files-found: error`. Publication consumes the same loaded image and must
not rebuild. No Go qualification package is introduced.

- [ ] **Step 5: Preserve RC/final separation**

RC builds and qualifies without publishing a production tag. Final release accepts only the previously qualified source/image identity, re-verifies the bundle and protected environment, signs/attests, then promotes that digest. If the current pipeline cannot promote the RC digest without rebuilding, stop at RC until same-digest promotion is implemented; do not call a rebuilt final image qualified.

- [ ] **Step 6: Verify scripts and workflow structure locally**

```bash
bash scripts/release_metadata_test.sh
bash scripts/qualification/http_data_plane_v1_differential_test.sh
bash scripts/qualification/http_data_plane_v1_prometheus_test.sh
bash scripts/qualification/http_data_plane_v1_workflow_test.sh
git diff --check
```

- [ ] **Step 7: Commit the fail-closed workflow gate**

```bash
git add .github/workflows/release-candidate.yml .github/workflows/release.yml .github/workflows/security-release-gates.yml scripts/release_metadata.sh scripts/release_metadata_test.sh scripts/qualification/http_data_plane_v1_differential.sh scripts/qualification/http_data_plane_v1_differential_test.sh scripts/qualification/http_data_plane_v1_prometheus.sh scripts/qualification/http_data_plane_v1_prometheus_test.sh scripts/qualification/http_data_plane_v1_workflow_test.sh
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
source .envrc && scripts/go_cache.sh run -- go test ./pkg/json ./pkg/config ./pkg/capability -count=1
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

`GO` is allowed only when all ten tasks are complete and every existing
capability, corpus, release-script, workflow, and environment gate passes. The following are hard stops:

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
