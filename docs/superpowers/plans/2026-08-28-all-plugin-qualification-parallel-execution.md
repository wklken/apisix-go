# APISIX 3.17 All-Plugin Qualification Parallel Execution Plan

> **For agentic workers:** REQUIRED SUB-SKILLS: Use `superpowers:subagent-driven-development` for bounded implementation tasks and `superpowers:dispatching-parallel-agents` only for the independent ownership lanes named below.

**Goal:** Reduce the remaining APISIX 3.17 plugin corpus from 323 pending/blocked plugin-owned blocks across 24 source files to zero, then complete real-process, differential, real-dependency, failure, and all-plugin behavior gates without mixing platform recovery into plugin qualification.

**Architecture:** Each implementer owns disjoint plugin packages and may add focused package tests or its lane's integration manifest. The controller alone owns shared truth files (`t/plugin/corpus_scope.yaml`, `pkg/capability/manifest.yaml`, generated projections, CI, and final qualification reports). Workers return a source-label-to-test evidence map; the controller verifies it against the pinned APISIX source before updating shared truth. Platform recovery, process supervision, stream TLS/UDP, security hardening, and release promotion remain a later production-readiness program.

**Tech Stack:** Go 1.26, `t/plugin`, YAML manifests, Bash qualification gates, Podman, Apache APISIX 3.17.0 source ref `refs/qualification/apisix-3.17.0` at commit `9ef2ecab67f652d38365049613610ef649bb4ad0`.

**Spec:** `docs/superpowers/plans/2026-08-28-all-plugin-behavior-qualification.md`

## Global Constraints

1. The pinned Apache source is read with `git -C "$APISIX_SOURCE_DIR" show refs/qualification/apisix-3.17.0:<path>`; the sparse checkout's working-tree `HEAD` is not authoritative.
2. Observable APISIX 3.17 behavior is the target. Lua/OpenResty/NGINX-native behavior may be classified as `non_plugin` or a concrete platform boundary only when no Go plugin contract exists; it must not be called converted.
3. Plugin evidence covers plugin schema, request/response behavior, plugin-owned dependencies, and plugin-owned failures. Generic config publication, etcd disconnect/compaction, journal replay, process restart, deletion recovery, and rollback are platform evidence and are excluded from plugin lanes.
4. The environment proxy remains cleared. No worker may restore `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, or lowercase variants to make a test pass.
5. The controller owns `t/plugin/corpus_scope.yaml`, `pkg/capability/manifest.yaml`, `docs/plugins.md`, README generated sections, `.github/workflows/ci.yml`, `Makefile`, and `scripts/qualification/*`. Workers must not modify these files unless a later task explicitly transfers ownership.
6. Parallel workers have exclusive ownership of the paths named in their task. They must not revert, reformat, or clean edits outside those paths. All workers share this dirty worktree.
7. Every behavior change starts with an exact failing focused test. Existing focused tests run before and after where practical. A worker may not replace a behavior requirement with a ledger classification.
8. Go commands run as `bash -lc 'source .envrc && scripts/go_cache.sh run -- ...'`. Do not use `go test ./...`, `go test ./pkg/...`, or `make test` as routine verification.
9. Only one real-process `t/plugin` test command runs at a time. The controller serializes those commands after parallel package work completes.
10. No worker commits, pushes, opens a PR, publishes an image, or changes external state. This plan produces local changes and evidence only.
11. A source block leaves pending/blocked only after the controller can name an executable test, a precise native/platform boundary, or an upstream test-helper-only reason. Difficulty is not a disposition.
12. Passing this plan does not make the project production ready. The final report must preserve that boundary.

## Current Checkpoint

The strict corpus command was run on 2026-08-28 from this worktree:

```bash
bash -lc 'source .envrc && APISIX_GO_REQUIRE_FULL_CORPUS=1 APISIX_SOURCE_DIR="$PWD/.cache/apache-apisix" scripts/go_cache.sh run -- go test ./t/plugin -run "^TestUpstreamCorpusCompletion$" -count=1 -v'
```

Observed result:

- 3,882 real-process qualification blocks
- 158 package-test blocks
- 2 dependency-test blocks
- 7 platform-test blocks
- 40 platform-gap blocks
- 17 post-target regression blocks
- 268 excluded post-target blocks
- 248 non-plugin blocks
- 323 pending/blocked plugin blocks across 24 source files

The unfinished Aliyun lane already converted source tests 1-2. `TestAPISIX317AllowsEmptyAndImageOnlyRequestsWithoutModeration` has been added but has not been run; it is not evidence until its RED/GREEN sequence is recorded.

## Parallelization Ruling

`subagent-driven-development` normally serializes implementers. The user explicitly authorized unrestricted subagent use to accelerate this plan, and `dispatching-parallel-agents` permits independent problems. Therefore, up to three implementers run concurrently because the runtime exposes four total slots including the controller. Parallelism is allowed only for the exclusive path sets below; all shared truth integration and all real-process commands remain serialized by the controller. The cost if this ruling is wrong is merge interference in the shared dirty worktree, mitigated by path ownership and controller diff review.

Workers do not commit because commit authority was not requested. Task review packages are path-scoped snapshots (`git diff -- <owned paths>` plus the worker report) rather than commit-range packages. The cost is weaker task-boundary history; the controller compensates with before/after path inventories and a persistent SDD ledger.

## Ownership Matrix

| Lane | Exclusive worker-owned paths | Shared outputs returned to controller, not edited |
| --- | --- | --- |
| Aliyun moderation | `pkg/plugin/ai_aliyun_content_moderation/**` | corpus dispositions for `ai-aliyun-content-moderation.t` |
| gRPC | `pkg/plugin/grpc_transcode/**`, `pkg/plugin/grpc_web/**` | four transcode sources and `grpc-web.t`; requested manifest cases |
| JWT/OIDC | `pkg/plugin/jwt_auth/**`, `pkg/plugin/openid_connect/**`, `t/plugin/jwt-auth.yaml`, `t/plugin/openid-connect.yaml` | JWT/OIDC source dispositions |
| Prometheus | `pkg/plugin/prometheus/**`, `t/plugin/prometheus.yaml` | seven Prometheus source dispositions, including AI metrics |
| Zipkin | `pkg/plugin/zipkin/**` | `zipkin.t`, `zipkin2.t`, `zipkin3.t`; requested manifest cases |
| Protocol/bounded-runtime | `pkg/plugin/dubbo/**`, `pkg/plugin/dubbo_proxy/**`, `pkg/plugin/serverless/**`, `pkg/plugin/rocketmq_logger/**`, `t/plugin/http-dubbo.yaml` | Dubbo, serverless, and RocketMQ security-warning dispositions |
| Controller | corpus ledger, capability manifest, generated docs, qualification scripts, CI, final reports | all worker evidence maps and review rulings |

## Worker Report Contract

Each implementer writes its assigned SDD report and returns only status, test summary, and concerns. The report must contain:

1. Every upstream source file and test number inspected.
2. For every test number, one proposed disposition: `converted`, `package_test`, `dependency_test`, `non_plugin`, or `platform_gap`.
3. For executable evidence, the exact local test symbol or manifest case and why it asserts the same observable behavior.
4. For boundaries, the exact APISIX-native mechanism and why no Go plugin contract exists.
5. RED command/output, production change, GREEN command/output, and focused regression command/output.
6. The exact files changed and confirmation that no shared-owned file was touched.
7. Remaining uncertainty. A worker must report `BLOCKED` rather than inventing evidence.

### Task 1: Initialize the Parallel Execution Ledger and Freeze Inputs

**Owner:** Controller only.

**Files:**
- Create through the skill: `.superpowers/sdd/2026-08-28-all-plugin-qualification-parallel-execution/progress.md`
- Read: this plan, its spec, current `git status`, and strict corpus output

**Steps:**
1. Resolve this plan's SDD workspace with `scripts/sdd-workspace` and create the identified ledger.
2. Record branch, worktree, `HEAD`, pinned APISIX ref/commit, current strict corpus counts, dirty-tree warning, and no-commit/no-push authority.
3. Record the ownership matrix and every shared-file interaction as the preflight conflict table.
4. Snapshot `git diff --name-only` and per-lane file lists so later task reviews can detect out-of-scope edits.
5. Mark Task 1 complete only after the ledger is sufficient to resume after compaction.

### Task 2: Complete Aliyun Content Moderation Parity

**Owner:** Aliyun worker.

**Files:**
- Modify: `pkg/plugin/ai_aliyun_content_moderation/plugin.go`
- Modify: `pkg/plugin/ai_aliyun_content_moderation/plugin_test.go`
- Modify: `pkg/plugin/ai_aliyun_content_moderation/response_phase_test.go`
- Modify only if required by focused behavior: other files under `pkg/plugin/ai_aliyun_content_moderation/`

**Requirements:**
1. Run the existing unexecuted empty/image-only request test first and preserve the failing output.
2. Match source tests 3-34 and 37-44: request allow/deny, response moderation, SSE final-packet/realtime modes, cache interval/size, upstream-error bypass, multimodal/tool-role extraction, and Responses API wire shape.
3. Preserve actual upstream model and usage fields when constructing denial responses where APISIX does; do not substitute zero usage or only the request model.
4. Keep invalid JSON and unknown protocols rejected while valid empty/image-only inputs bypass text moderation.
5. Investigate tests 35-36 as a cross-plugin active-connection metric. Return the owning package/test seam; do not edit Prometheus or AI proxy packages.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/ai_aliyun_content_moderation -run "^TestAPISIX317" -count=1 -v'
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/ai_aliyun_content_moderation -count=1'
```

### Task 3: Close gRPC Transcode and gRPC-Web Sources

**Owner:** gRPC worker.

**Files:**
- Modify: `pkg/plugin/grpc_transcode/**`
- Modify: `pkg/plugin/grpc_web/**`

**Requirements:**
1. Read and map `grpc-transcode-reload-bugfix.t`, `grpc-transcode.t`, `grpc-transcode2.t`, `grpc-transcode3.t`, and `grpc-web.t` at the pinned ref.
2. Prove descriptor reload, request/response conversion, error/status mapping, streaming/framing, content type, and invalid schema/config behavior at the closest package seam.
3. If a block is owned by NGINX HTTP/2 framing rather than the plugin, name that boundary precisely; do not classify general gRPC behavior as native merely because the original test uses NGINX.
4. Return proposed real-process manifest cases; the controller decides whether to create a shared manifest after package review.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/grpc_transcode ./pkg/plugin/grpc_web -count=1'
```

### Task 4: Close JWT and OIDC Sources

**Owner:** JWT/OIDC worker.

**Files:**
- Modify: `pkg/plugin/jwt_auth/**`
- Modify: `pkg/plugin/openid_connect/**`
- Modify: `t/plugin/jwt-auth.yaml`
- Modify: `t/plugin/openid-connect.yaml`

**Requirements:**
1. Resolve `jwt-auth-more-algo.t` test 3, `jwt-auth.t` test 26, the remaining `jwt-auth4.t` block, and `openid-connect5.t` tests 1-2 from source rather than retaining `blocked_design`.
2. For the JWT public sign endpoint, distinguish plugin behavior from APISIX control/public API infrastructure. Add plugin-package evidence only if the plugin owns the behavior.
3. For the 512-bit RSA case, preserve Go's security floor and write an explicit compatibility/security boundary test if exact APISIX behavior cannot be safely implemented.
4. For OIDC Redis session locking, determine whether the Go plugin exposes a same-process or distributed lock contract. Implement only an existing Go contract; otherwise return a precise native/session-platform boundary.
5. Add real-process cases for observable auth decisions that are owned by these plugins.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/jwt_auth ./pkg/plugin/openid_connect -count=1'
```

### Task 5: Integrate and Review Parallel Wave A

**Owner:** Controller only after Tasks 2-4 finish.

**Files:**
- Modify: `t/plugin/corpus_scope.yaml`
- Create/modify only when worker evidence requires it: lane-specific `t/plugin/*.yaml`

**Steps:**
1. Inspect every wave-A path diff and reject changes outside ownership.
2. Run one independent task review per lane against its brief, report, and path-scoped diff.
3. Route Critical/Important findings back to the same worker; do not repair worker-owned code in the controller.
4. Source-check every proposed disposition and update the shared corpus ledger in one controller patch.
5. Run focused package tests, corpus accounting, and then the newly added real-process cases serially.
6. Record exact remaining block/source counts; Wave B starts only after the count is reproducible.

**Verification:**
```bash
bash -lc 'source .envrc && APISIX_SOURCE_DIR="$PWD/.cache/apache-apisix" scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestUpstreamCorpusAccounting|TestCorpusEvidenceMatchesCompatibilityTarget|TestUpstreamCorpusCompletion)$" -count=1 -v'
```

### Task 6: Close Prometheus and AI-Proxy Metric Sources

**Owner:** Prometheus worker.

**Files:**
- Modify: `pkg/plugin/prometheus/**`
- Modify: `t/plugin/prometheus.yaml`

**Requirements:**
1. Map `prometheus-ai-proxy.t`, `prometheus-ai-proxy2.t`, `prometheus-metric-expire.t`, `prometheus.t`, `prometheus2.t`, `prometheus3.t`, and `prometheus4.t` at the pinned ref.
2. Cover each metric family, labels, status mapping, latency/bandwidth observations, metric expiry, and invalid configuration represented by those sources.
3. For AI proxy metrics, inspect the current AI runtime metric hooks without editing AI packages. Add Prometheus-side assertions or return the exact missing hook as a blocker.
4. Incorporate the Aliyun worker's conclusion for active-connection decrement tests 35-36.
5. Do not convert exporter-process internals that the Go plugin does not own; provide a precise observability-platform boundary instead.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/prometheus -count=1'
```

### Task 7: Close Zipkin Sources

**Owner:** Zipkin worker.

**Files:**
- Modify: `pkg/plugin/zipkin/**`

**Requirements:**
1. Map all blocks in `zipkin.t`, `zipkin2.t`, and `zipkin3.t` at the pinned ref.
2. Prove B3 extraction/injection, span naming/timing/tags, sampling, upstream-error reporting, batch/export behavior, and insecure endpoint warning where represented.
3. Separate Zipkin plugin behavior from OpenResty phase timestamps that have no Go equivalent.
4. Return concrete real-process and collector-fixture cases; the controller owns manifest/fixture integration.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/zipkin -count=1'
```

### Task 8: Close Dubbo, Bounded Serverless, and RocketMQ TLS Boundary

**Owner:** Protocol/bounded-runtime worker.

**Files:**
- Modify: `pkg/plugin/dubbo/**`
- Modify: `pkg/plugin/dubbo_proxy/**`
- Modify: `pkg/plugin/serverless/**`
- Modify: `pkg/plugin/rocketmq_logger/**`
- Modify: `t/plugin/http-dubbo.yaml`

**Requirements:**
1. Map `dubbo-proxy/route.t` and `dubbo-proxy/upstream.t` against current Dubbo transport behavior, including routing, serialization, upstream selection, errors, and connection lifecycle.
2. Map all 26 `serverless.t` blocks. Prove the declared bounded Go-compatible pre/post surface and classify Lua code execution, `ngx_lua` APIs, and OpenResty phase fidelity as native boundaries; do not create a general Lua runtime.
3. Revisit `security-warning2.t` tests 9-10. Verify the pinned RocketMQ client limitation from installed source and preserve fail-closed secure configuration if TLS cannot be provided.
4. Do not add or upgrade dependencies in this task. Report a dependency blocker if TLS requires one.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/dubbo ./pkg/plugin/dubbo_proxy ./pkg/plugin/serverless ./pkg/plugin/rocketmq_logger -count=1'
```

### Task 9: Integrate Wave B and Reach Zero Pending Plugin Blocks

**Owner:** Controller only after Tasks 6-8 finish.

**Files:**
- Modify: `t/plugin/corpus_scope.yaml`
- Create/modify as required: `t/plugin/*.yaml`

**Steps:**
1. Review each wave-B task independently and run fix loops before corpus integration.
2. Apply source-checked dispositions to all remaining source blocks.
3. Run accounting without strict mode first; investigate any unexpected count movement.
4. Run strict mode and require exactly zero pending/blocked plugin-owned blocks.
5. Do not reduce the count by changing a plugin block to `platform_gap` unless the source behavior is genuinely owned by a shared platform component and a named platform gate exists.

**Verification:**
```bash
bash -lc 'source .envrc && APISIX_GO_REQUIRE_FULL_CORPUS=1 APISIX_SOURCE_DIR="$PWD/.cache/apache-apisix" scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestUpstreamCorpusAccounting|TestCorpusEvidenceMatchesCompatibilityTarget|TestUpstreamCorpusCompletion)$" -count=1 -v'
```

### Task 10: Close Integration Manifest and Capability Evidence Coverage

**Owner:** Controller, with fresh bounded workers for disjoint missing manifest groups if the coverage test identifies more than one independent group.

**Files:**
- Modify/create: `t/plugin/*.yaml`
- Modify: `t/plugin/coverage_test.go`
- Modify: `pkg/capability/manifest.yaml`
- Regenerate: `docs/plugins.md`, README generated sections

**Steps:**
1. Derive selected capabilities from `apisix-3.17-all-plugins-v1`; require every Go-applicable factory to have a target-pinned manifest or an explicit native boundary test.
2. Add at least one success and one invalid/failure real-process case per selected plugin.
3. Run manifests one at a time while debugging; run the aggregate only after all focused cases pass.
4. Promote `converted_upstream`, `schema`, and `unit` claims only when the referenced tests exist and pass.
5. Regenerate projections and reject drift.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestCorpusEvidenceMatchesCompatibilityTarget)$" -count=1 -v'
bash -lc 'source .envrc && scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check'
```

### Task 11: Implement the Immutable APISIX 3.17 Differential Oracle

**Owner:** One integration worker; no parallel writer may touch `t/plugin` or qualification scripts during this task.

**Files:**
- Create/modify: `t/plugin/oracle.go`, `t/plugin/oracle_test.go`, `t/plugin/differential_test.go`, `t/plugin/normalization.yaml`
- Create/modify: `scripts/qualification/plugin_differential.sh`
- Modify as required: `t/plugin/case.go`, `t/plugin/runner_test.go`

**Requirements:**
1. Resolve `apache/apisix:3.17.0` to and record an immutable digest; mutable-tag-only execution fails.
2. Run equivalent candidate/oracle config, request, and deterministic dependency fixtures with all environment proxies cleared.
3. Compare semantic status, headers, body, route/upstream choice, retry count, Host, SNI, and auth/security decision.
4. Allow normalization only for reviewed volatile fields; status and asserted semantic fields are never normalized.
5. Record first-attempt results and fail on second-attempt-only success.

**Verification:**
```bash
CONTAINER_BIN=podman bash scripts/qualification/plugin_differential.sh
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestOracleIdentity|TestDifferentialNormalization|TestPluginDifferential)$" -count=1 -v'
```

### Task 12: Complete Plugin-Owned Real Dependency and Failure Evidence

**Owner:** Rolling independent workers by dependency family; controller serializes shared fixtures and manifest integration.

**Dependency families:** Redis, Kafka/RocketMQ, LDAP/OIDC, cloud APIs, log collectors, AI providers, tracing collectors, and protocol bridges.

**Requirements:**
1. A plugin with no external call receives a concrete `real_dependency: not_applicable` reason.
2. A plugin with an external call proves a pinned real dependency or protocol-faithful fixture plus connect failure, timeout, malformed response, rejection, and retry/cleanup behavior that the plugin owns.
3. Shared etcd/config/journal/process recovery is excluded.
4. Update `real_dependency` and `failure` manifest evidence only after executable artifacts pass.

**Verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./t/plugin -run "^TestPluginIntegration/<affected-manifest>/" -count=1 -v'
```

### Task 13: Build the Fail-Closed All-Plugin Gate and Audit

**Owner:** Controller, followed by one independent final code reviewer.

**Files:**
- Create/modify: `scripts/qualification/plugin_behavior_gate.sh`
- Create/modify: `scripts/qualification/plugin_behavior_gate_test.sh`
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`
- Modify: `pkg/capability/manifest.yaml`
- Regenerate: `docs/plugins.md`, README generated sections
- Create: `docs/qualification/apisix-3.17-all-plugins-v1.md`

**Steps:**
1. Compose capability validation, generator drift, focused plugin units, strict source corpus, real-process manifests, differential oracle, and dependency/failure suites into one fail-closed gate.
2. Emit deterministic JSON with candidate commit, profile, APISIX source commit, oracle digest, case counts, first-attempt result, and output hashes.
3. Fail on skips, mutable identities, partial/deferred selected capabilities, selected known gaps, stale/missing/flaky evidence, pending corpus, or retry-only success.
4. Run impact-scoped units, strict corpus, gate, build, lint, and `git diff --check`.
5. Dispatch one independent whole-change reviewer. Route concrete findings through one bounded fix worker and one scoped re-review.
6. Publish the qualification report only if all evidence passes. State explicitly that production readiness is still pending the separate platform/release plan.

**Final verification:**
```bash
bash -lc 'source .envrc && scripts/go_cache.sh run -- go test ./pkg/plugin/... -count=1'
bash -lc 'source .envrc && APISIX_GO_REQUIRE_FULL_CORPUS=1 APISIX_SOURCE_DIR="$PWD/.cache/apache-apisix" scripts/go_cache.sh run -- go test ./t/plugin -run "^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestUpstreamCorpusAccounting|TestCorpusEvidenceMatchesCompatibilityTarget|TestUpstreamCorpusCompletion)$" -count=1 -v'
CONTAINER_BIN=podman bash scripts/qualification/plugin_behavior_gate.sh
bash -lc 'source .envrc && make build'
bash -lc 'source .envrc && GOLANGCI_LINT_CACHE="$PWD/.cache/tmp/golangci-lint-all-plugin" make lint'
git diff --check
```

## Completion Boundary and Next Goal

Completion of Tasks 1-13 proves the Go-applicable APISIX 3.17 plugin behavior program. The next goal must consume the exact same candidate image digest and separately close: `security_profile: strict` as the production contract, environment-proxy isolation, production plugin allow-listing, supervisor/worker lifecycle, stream chain/TLS/UDP decisions, capacity/soak, platform recovery, security review, upgrade/rollback, provenance/signing, canary, release protection, and same-digest RC-to-final promotion. Only that later program may change the project-level production-readiness statement.
