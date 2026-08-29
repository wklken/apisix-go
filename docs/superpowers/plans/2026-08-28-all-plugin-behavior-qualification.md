# APISIX 3.17 All-Plugin Behavior Qualification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Do not use subagents unless the user explicitly authorizes delegation.

**Goal:** Prove, with fail-closed and source-pinned evidence, that every Go-applicable Apache APISIX 3.17 plugin capability implemented by apisix-go has correct externally observable behavior.

**Architecture:** `pkg/capability/manifest.yaml` remains the only plugin capability and evidence registry. Plugin qualification consumes schema/unit tests, pinned upstream corpus cases, candidate real-process cases, differential oracle results, dependency cases, and failure cases. Generic configuration publication, etcd lifecycle, process restart, journal replay, resource deletion, and rollback remain platform evidence under `platform-recovery-v1`; they are never projected onto plugin rows. This plan does not create `pkg/qualification` or a second plugin registry.

**Tech Stack:** Go 1.26, `t/plugin`, YAML manifests, Bash qualification gates, Apache APISIX 3.17.0 source commit `9ef2ecab67f652d38365049613610ef649bb4ad0`, immutable `apache/apisix:3.17.0@sha256:...` oracle image, Docker or Podman.

**Supersedes:** The plugin-evidence portions of `2026-08-23-parity-qualification-release.md` that propose a generic `pkg/qualification` package or require generic recovery evidence for every plugin. Artifact, release, and platform gates remain separate production-readiness work.

## Success Contract

- Every APISIX-namespace capability with a Go factory is selected by `apisix-3.17-all-plugins-v1`; selection drift fails a test.
- Every selected capability is `behavior: full`, has no known gaps, and has `verified` or concretely `not_applicable` schema, unit, converted-upstream, differential, real-dependency, and failure evidence.
- Native/OpenResty/NGINX-only capability rows remain visible. Each has a concrete ownership boundary and a fail-closed or platform-owner test; it is not silently counted as a passing Go plugin.
- Every plugin-owned upstream source block at the pinned APISIX commit is converted. `APISIX_GO_REQUIRE_FULL_CORPUS=1` passes with zero pending or blocked plugin-owned blocks.
- Every qualification source in a converted integration manifest uses the exact compatibility target commit. Later Apache source blocks may remain as explicitly marked `regression_only` cases, but they never count as APISIX 3.17 evidence.
- Every selected plugin has at least one real-process behavior case, including success and invalid/failure behavior. Plugins that directly own an external protocol or service also prove those plugin-specific interactions inside `real_dependency`/`failure`; shared etcd/config/journal recovery remains platform evidence.
- Differential cases run the same logical request and dependency fixtures against apisix-go and the immutable APISIX oracle. Normalization may remove only reviewed volatile fields and never status, asserted headers, body, route/upstream choice, auth decisions, retry count, Host, or SNI.
- The complete unit, corpus, real-process, differential, dependency, and failure suite passes from a clean tree and emits evidence bound to the candidate source commit and oracle digest.
- This plan proves plugin behavior only. It must not change the project to `production ready`; that requires the separate platform and release program after this plan passes.

## Observed Baseline on 2026-08-28

- 119 capability rows: 118 APISIX namespace rows and one Go-native row.
- 115 registered factory keys; 100 current integration manifests cover 100 factory keys.
- 29 capabilities are `partial`; seven are native/runtime `not_applicable` rows.
- Current corpus accounting reports 4,032 converted blocks, 50 non-plugin blocks, and 968 pending/blocked plugin-owned blocks across 79 sources.
- Six converted-upstream claims use the target commit and 93 claims are stale.
- The current plugin unit package aggregation passes. This proves only the current unit suite, not APISIX parity.
- The current real-process suite may pass while the full-corpus and evidence gates remain red; a green real-process run alone is not qualification.

### Task 1: Separate Plugin Evidence from Platform Recovery

**Files:**
- Modify: `pkg/capability/types.go`
- Modify: `pkg/capability/load.go`
- Modify: `pkg/capability/load_test.go`
- Modify: `pkg/capability/manifest.yaml`
- Modify: `cmd/capability-gen/main.go`
- Modify: `cmd/capability-gen/main_test.go`
- Regenerate: `docs/plugins.md`
- Create: `scripts/qualification/platform_recovery_test.sh`
- Modify: `scripts/etcd_recovery_smoke.sh`
- Modify: `scripts/etcd_recovery_smoke_test.sh`

- [x] Add a failing test proving plugin qualification does not require generic recovery evidence.
- [x] Remove the plugin `recovery` evidence kind and generated column.
- [x] Keep plugin-owned external-service behavior under `real_dependency` and `failure`, without importing shared etcd/config/journal recovery.
- [x] Emit and validate only platform-owned `journal` and `generation` records from `platform-recovery-v1`.
- [x] Run the platform fixture, capability tests, generator drift check, focused generation/etcd tests, race gate, and build.

Verification:

```bash
bash scripts/etcd_recovery_smoke_test.sh
bash scripts/qualification/platform_recovery_test.sh
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen ./pkg/generation ./pkg/etcd -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check'
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test -race ./pkg/generation ./pkg/etcd ./pkg/store ./pkg/server ./pkg/route -count=1'
bash -lc 'source .envrc && make build'
```

### Task 2: Add the Complete APISIX 3.17 Plugin Qualification Profile

**Files:**
- Modify: `pkg/capability/manifest.yaml`
- Modify: `pkg/capability/load_test.go`
- Modify: `cmd/capability-gen/main_test.go`
- Regenerate: `docs/plugins.md`

- [x] Add `apisix-3.17-all-plugins-v1` with every APISIX-namespace capability that has a Go factory, identified by its canonical/primary factory key.
- [x] Add a test deriving the expected set from the manifest and rejecting omissions, extras, aliases counted twice, native-only rows, and Go-native extensions.
- [x] Require `converted_upstream`, `differential`, `failure`, `real_dependency`, `schema`, and `unit`; do not add platform recovery.
- [x] Keep qualification fail-closed: `partial`, `deferred`, `missing`, `stale`, `flaky`, or unexplained applicability cannot qualify.
- [x] Regenerate projections and assert the profile denominator equals the derived set.

Verification:

```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen -count=1'
bash -lc 'source .envrc && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check'
```

### Task 3: Pin and Reconcile the Complete Upstream Corpus

**Files:**
- Modify: `t/plugin/corpus_scope.yaml`
- Modify: `t/plugin/corpus_test.go`
- Modify as required: `t/plugin/*.yaml`
- Create: `scripts/qualification/fetch_apisix_317_source.sh`

- [x] Fetch only the official Apache APISIX repository and verify the exact target commit before reading corpus files.
- [ ] Rebuild the `t/plugin/*.t` file and test-label inventory from the target commit rather than replacing the old commit string mechanically.
- [ ] Reconcile added, removed, split, and renumbered test blocks against the old ledger.
- [ ] Keep post-3.17 Apache cases as explicit `regression_only` sources where useful; run them separately and exclude them from 3.17 qualification evidence.
- [ ] Classify true test-infrastructure or native-runtime-only blocks as `non_plugin` with a concrete reason and owning platform boundary.
- [ ] Keep every Go-applicable behavior block as plugin-owned until converted; do not use `blocked_*` as a passing disposition.
- [ ] Make full-corpus completion mandatory in the qualification command.

Verification:

```bash
bash -lc 'source .envrc && APISIX_SOURCE_DIR="$PWD/.cache/apache-apisix" scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestUpstreamCorpusAccounting|TestCorpusEvidenceMatchesCompatibilityTarget|TestUpstreamCorpusCompletion)$" -count=1 -v'
bash -lc 'source .envrc && APISIX_GO_REQUIRE_FULL_CORPUS=1 APISIX_SOURCE_DIR="$PWD/.cache/apache-apisix" scripts/go_cache.sh run -- go test ./t/plugin -run "^TestUpstreamCorpusCompletion$" -count=1 -v'
```

Expected final output includes zero pending/blocked plugin-owned blocks and zero stale converted-upstream claims.

### Task 4: Close the Missing Integration Manifest Set

**Files:**
- Create/modify as required: `t/plugin/*.yaml`
- Modify: `t/plugin/coverage_test.go`
- Modify: `pkg/capability/manifest.yaml`

- [ ] Add target-pinned manifests for every Go-applicable capability currently lacking one, including bounded serverless, gRPC, Dubbo, tracing, AI, MCP, and MQTT capabilities where they have Go contracts.
- [ ] For native/runtime-only rows, add an explicit boundary test instead of a fake plugin manifest.
- [ ] Require every selected capability to have at least one manifest and every manifest to identify its target plugin.
- [ ] Promote `converted_upstream` only after source labels, target commit, and real-process cases agree.

Verification:

```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestCorpusEvidenceMatchesCompatibilityTarget)$" -count=1 -v'
```

### Task 5: Close All Partial Behavior Gaps

**Files:**
- Modify as required: `pkg/plugin/**`
- Modify as required: `pkg/route/**`, `pkg/proxy/**`, `pkg/stream/**`
- Modify as required: `t/plugin/*.yaml`
- Modify: `pkg/capability/manifest.yaml`

Execute in reviewable waves; every gap starts with a failing focused test and ends with source-pinned behavior evidence:

- [ ] Schema, variable, PCRE/RE2, JSONPath, and template gaps: redirect, response/proxy rewrite, mocking, body transformer, ACL/restrictions, data mask, traffic/workflow, prompt guard.
- [ ] Auth/session gaps: DingTalk, Feishu, and any other capability whose security contract is currently weaker or observably different.
- [ ] Protocol/transport gaps: gRPC transcode/web, proxy mirror, RocketMQ logger TLS, stream MQTT, and other declared transport differences.
- [ ] State/selection/observability gaps: API breaker, traffic split/label, Zipkin, OpenTelemetry, and AI proxy multi.
- [ ] Bounded Lua/serverless gaps: either implement the declared Go-compatible surface or record an owner-approved divergence without claiming full Lua/OpenResty parity.
- [ ] Regenerate the manifest projection after each wave; a `full` capability must have no known gaps.

Each wave verification:

```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/<affected_package> -count=1'
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run "^TestPluginIntegration/<affected-manifest>/" -count=1 -v'
bash -lc 'source .envrc && make build'
```

### Task 6: Add Immutable APISIX Differential Oracle Execution

**Files:**
- Create: `t/plugin/oracle.go`
- Create: `t/plugin/oracle_test.go`
- Create: `t/plugin/differential_test.go`
- Create: `t/plugin/normalization.yaml`
- Create: `scripts/qualification/plugin_differential.sh`
- Modify: `t/plugin/case.go`
- Modify: `t/plugin/runner_test.go`
- Modify: `pkg/capability/manifest.yaml`

- [ ] Resolve `apache/apisix:3.17.0` to an immutable digest and reject mutable-tag-only execution.
- [ ] Run the same logical config, request, deterministic values, and dependency fixtures against candidate and oracle adapters.
- [ ] Version and allow-list normalization. Reject normalization of semantic fields.
- [ ] Record first-attempt candidate/oracle observations and compare status, asserted headers, body bytes, route/upstream choice, retries, Host, SNI, and security decisions.
- [ ] Support a concrete not-applicable reason only where no equivalent Go plugin contract exists.
- [ ] Promote each plugin's `differential` evidence only after its cases pass against the pinned oracle.

Verification:

```bash
CONTAINER_BIN=podman bash scripts/qualification/plugin_differential.sh
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestOracleIdentity|TestDifferentialNormalization|TestPluginDifferential)$" -count=1 -v'
```

### Task 7: Classify and Verify Dependency and Failure Behavior

**Files:**
- Modify as required: `t/plugin/*.yaml`
- Modify as required: `t/plugin/fixture_*_test.go`
- Modify: `pkg/capability/manifest.yaml`

- [ ] Mark `real_dependency` not applicable only for plugins that make no external network/storage call, with a concrete reason.
- [ ] For Redis, Kafka, RocketMQ, LDAP, OIDC, cloud APIs, log collectors, AI providers, tracing backends, and protocol bridges, run pinned real dependency cases.
- [ ] Test plugin-owned connect failure, timeout, malformed response, rejection, retry boundary, and cleanup where implemented; do not duplicate shared client or platform recovery scenarios.
- [ ] Mark `failure` verified only when invalid configuration and runtime failure behavior are both covered or concretely inapplicable.
- [ ] Keep etcd disconnect/compaction and process restart out of these records.

### Task 8: Add One Fail-Closed All-Plugin Behavior Gate

**Files:**
- Create: `scripts/qualification/plugin_behavior_gate.sh`
- Create: `scripts/qualification/plugin_behavior_gate_test.sh`
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

- [ ] Run capability/generator validation, all plugin units, source accounting, mandatory full corpus, all real-process manifests, differential oracle, and real-dependency/failure suites.
- [ ] Clear inherited HTTP/HTTPS/ALL proxy variables for candidate, fixtures, and oracle.
- [ ] Emit a deterministic JSON summary containing candidate source commit, oracle source commit/image digest, profile, case counts, first attempt, output hashes, and pass/fail state.
- [ ] Fail on skips, stale/missing/flaky/deferred evidence, partial behavior, pending corpus blocks, mutable oracle identity, or a second-attempt-only pass.
- [ ] Add the gate to CI without weakening it to inventory or compile-only coverage.

Verification:

```bash
bash scripts/qualification/plugin_behavior_gate_test.sh
CONTAINER_BIN=podman bash scripts/qualification/plugin_behavior_gate.sh
```

### Task 9: Final All-Plugin Qualification Audit

**Files:**
- Modify: `pkg/capability/manifest.yaml`
- Regenerate: `docs/plugins.md`
- Create: `docs/qualification/apisix-3.17-all-plugins-v1.md`

- [ ] Confirm the profile selection exactly matches every Go-applicable APISIX 3.17 plugin capability.
- [ ] Confirm zero `partial`/`deferred` selected capabilities and zero selected known gaps.
- [ ] Confirm every required evidence claim is current and points to an existing auditable artifact.
- [ ] Run the complete gate from a clean tree on Linux amd64 and arm64 candidate artifacts.
- [ ] Record failures and unsupported native boundaries; do not convert uncertainty into `not_applicable`.
- [ ] Publish the qualification report only if every requirement above passes on the first recorded attempt.

Final commands:

```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/... -count=1'
bash -lc 'source .envrc && APISIX_GO_REQUIRE_FULL_CORPUS=1 scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestUpstreamCorpusAccounting|TestCorpusEvidenceMatchesCompatibilityTarget|TestUpstreamCorpusCompletion)$" -count=1 -v'
CONTAINER_BIN=podman bash scripts/qualification/plugin_behavior_gate.sh
bash -lc 'source .envrc && make build'
bash -lc 'source .envrc && GOLANGCI_LINT_CACHE="$PWD/.cache/tmp/golangci-lint-all-plugin" make lint'
git diff --check
```

## Production-Readiness Handoff

Passing this plan establishes all-plugin behavior qualification, not project production readiness. The next plan must consume the exact same candidate digest and close strict configuration defaults, environment-proxy isolation, plugin allow-listing, supervisor/worker lifecycle, general stream chain/TLS/UDP scope, capacity/soak, platform recovery, security, real dependencies, upgrade/rollback, provenance, signing, canary, and release protection before changing the project status.
