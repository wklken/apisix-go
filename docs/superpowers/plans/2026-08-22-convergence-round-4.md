# Convergence Round 4 Implementation Plan

## Baseline and scope

- Base revision: `f5c34b7d21b8e6956bf0f4dfe10699f4fd5b2659` (`origin/master` after PR #151).
- Goal: remediate every newly confirmed non-security P1/P2 from the merged-master review, preserve APISIX 3.17 behavior, and keep configuration failures isolated where the existing ownership contract permits it.
- Security boundary: network-security and trust-boundary findings are recorded only. In particular, active HTTPS health-check certificate verification is not changed in this round.
- No dependency changes and no unrelated refactors.

## Confirmed findings and acceptance contracts

### R4-P1-01: passive health never recovers an unhealthy node

`HealthAwareLoadBalance.ReportHTTP` drops all reports for unhealthy targets. Parse `checks.passive.healthy.successes` with the APISIX default, count configured healthy responses while a target is unhealthy, and restore the target after the threshold, including targets using passive `type: tcp`. Healthy status classification takes precedence over an overlapping unhealthy set. Statuses in neither set are neutral and do not mutate counters.

Regression proof: one failure quarantines a target; fail-open still selects it; the configured number of healthy responses restores it and emits exactly one healthy transition.

### R4-P2-01: active/passive status classification treats neutral responses as failures

Active probes must return a neutral result for a status in neither set, leaving success/failure counters unchanged. If a status is present in both sets, healthy wins. Passive reporting follows the same precedence and neutral behavior.

Regression proof: active 418 with healthy `[200]` and unhealthy `[500]` is neutral; overlapping 200 is healthy in both active and passive paths.

### R4-P1-02: stream upstream drops priority and retries

Build stream selectors by descending node priority. Normal selection uses only the highest priority group. For a single connection attempt, exhaust untried targets in the current group before falling through to lower priorities, bounded by the explicitly configured non-negative `upstream.retries`; zero means one attempt. Preserve weighted round-robin and the configured node order used by existing chash mappings inside each priority group. Each retry gets the configured connect timeout and respects parent cancellation.

Regression proof: repeated normal selection never reaches a lower priority node; with `retries=1`, a closed high-priority target falls through to a live lower-priority target; with `retries=0`, the same connection fails without fallback.

### R4-P1-03: proxy-mirror launches unbounded detached goroutines

Use a fixed, documented per-plugin in-flight limit with non-blocking admission because mirroring is best-effort and must never delay the primary request. Every admitted send uses a plugin-owned context, is counted in a wait group, and releases its slot. `Stop` closes admission, cancels in-flight requests, and waits for them. Synchronize admission against `Stop` so no wait-group additions occur after shutdown begins. Saturated sends are dropped before request-body copying.

Regression proof: a blocked mirror endpoint cannot exceed the fixed in-flight limit; an additional request returns without starting a mirror; `Stop` cancels and joins all admitted sends; no post-stop send starts.

### R4-P2-02: terminal response plugins accept informational status codes

The terminal response contracts for `mocking`, AWS/Aliyun content moderation, serverless exit values, and exit-transformer must accept only integral HTTP status codes 200 through 599. Keep existing numeric-string compatibility for serverless/exit-transformer within that range.

Regression proof: schema/converter tests reject 100, 103, 199, fractional values, and values above 599 while retaining 200 and 599.

### R4-P2-03: ordinary CI focused selectors can pass with zero tests

The unit-test integration matrix uses exact `<plugin>/<case>` selectors through `make test-plugin-smoke`. The plugin-status workflow uses a Make target that first proves `TestSupportedPluginManifestSelection` exists via `go test -list`, then runs the exact test. Shell contract tests reject raw focused selectors in these workflows.

Regression proof: an unknown plugin smoke case fails non-zero; the plugin-status target fails when its exact test-name sentinel is absent; workflow contract scripts pass only with the fail-closed targets.

### R4-P2-04: active health counters survive passive state transitions

Active success counters must advance only while the target is currently unhealthy, and active failure counters must advance only while it is currently healthy. A passive transition to the opposite state clears stale active progress so the next active transition requires the full configured threshold.

Regression proof: successes observed while healthy cannot shorten recovery after passive quarantine, and failures observed while unhealthy cannot shorten quarantine after passive recovery.

## Record-only security item

Add a new security-deferred ledger entry for active HTTPS health checks ignoring APISIX `checks.active.https_verify_certificate` and its secure default. Do not modify TLS transports, schema parsing, or probe behavior.

## Work units and ownership

1. Health owner: `pkg/proxy/health.go`, `pkg/proxy/health_test.go`, `pkg/proxy/active_health.go`, `pkg/proxy/active_health_test.go`.
2. Stream owner: `pkg/stream/router.go`, `pkg/stream/router_test.go`.
3. Plugin owner: `pkg/plugin/proxy_mirror/**`, `pkg/plugin/mocking/**`, `pkg/plugin/ai_aws_content_moderation/**`, `pkg/plugin/ai_aliyun_content_moderation/**`, `pkg/plugin/serverless/**`, `pkg/plugin/exit_transformer/**`.
4. Main owner: `.github/workflows/unit-test.yml`, `.github/workflows/plugin-status.yml`, `Makefile`, `scripts/plugin_status_gate_test.sh`, `scripts/release_gate_test.sh` when needed, this plan, and `docs/reviews/convergence-decisions.md`.

Workers may implement and run focused tests only. They must not commit, push, open a PR, modify dependencies, or touch another owner's files.

## Verification gates

Run after merging all work units:

1. Exact new regression tests, then affected package tests.
2. `go test -race` for `pkg/proxy`, `pkg/stream`, and `pkg/plugin/proxy_mirror` plus other concurrency-affected packages as required.
3. `make test-plugin-harness`, one exact `make test-plugin-smoke`, and an unknown selector fail-closed check.
4. `bash scripts/plugin_status_gate_test.sh` and `bash scripts/release_gate_test.sh`.
5. `make lint`, `make build`, `make test`, `make test-cover`, and `git diff --check`.
6. Dead-code/proxy-only call-site scan for every new or changed helper and lifecycle method.
7. Independent merge-level review; repair only confirmed in-scope findings and rerun affected gates.

## Verification-discovered test repairs

- `TestProxyFaultActiveHealthRecovers` must return a configured healthy status from `/healthz`; the old 204 fixture only passed when the former misclassification quarantined every node and triggered fail-open.
- `TestRetrieverCoalescesConcurrentGets` must use Go 1.26 `testing/synctest` to wait until concurrent callers are durably blocked inside the singleflight wave before releasing the retrieval callback. The old pre-call counter allowed late callers to start a second wave under aggregate load.
