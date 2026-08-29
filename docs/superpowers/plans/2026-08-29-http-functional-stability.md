# HTTP Functional And Stability Qualification Implementation Plan

> **For Codex:** Execute this plan task by task with `superpowers:executing-plans`. Stop on an unexplained failing gate; do not convert a failure into a documented exception.

**Goal:** Make the already-defined `http-data-plane-v1` scope independently qualifiable for HTTP functionality and single-process runtime stability, without requiring upgrade, publication, or environment-specific deployment work.

**Architecture:** Keep the existing single-process `Server + GenerationEngine` architecture and the existing 110-plugin HTTP profile. Separate the reusable operational gates into (a) functional/stability gates and (b) optional upgrade/publication gates. Add a process-boundary smoke that proves an in-flight HTTP request completes during graceful termination, then use the existing verified-TLS etcd recovery harness and 30-minute proxy soak as the runtime stability evidence.

**Tech Stack:** Go 1.26, Bash, GitHub Actions reusable workflows, Docker/Podman, etcd 3.6.13.

**Non-goals:** Kubernetes/systemd manifests, environment-specific ingress validation, upgrade/rollback qualification, registry publication, signing, and release packaging.

---

### Task 1: Decouple Functional/Stability Qualification From Upgrade And Publication

**Files:**
- Modify: `scripts/release_gate_test.sh`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `.github/workflows/release-candidate.yml`
- Modify: `.github/workflows/release.yml`

**Step 1: Write the failing workflow-contract assertions**

Change `scripts/release_gate_test.sh` to require a `run-upgrade-rollback` reusable input, require a rollback tag only when upgrade/rollback or publication is requested, require RC qualification to run without rollback permissions or inputs, and require the final-release caller to explicitly retain upgrade/rollback.

**Step 2: Run the contract test and confirm RED**

Run: `bash scripts/release_gate_test.sh`

Expected: FAIL because the new input and caller separation do not exist yet.

**Step 3: Implement the smallest workflow separation**

- Add `run-upgrade-rollback` with default `false` to the reusable workflow.
- Keep `run-operational` as the switch for verified-TLS etcd recovery and the 30-minute soak.
- Run the upgrade/rollback job only when `run-upgrade-rollback` or publication is requested.
- Require `rollback-release-tag` only for upgrade/rollback or publication.
- Remove the rollback input and elevated read permissions from the RC workflow.
- Require the RC to rerun capability drift and the complete source/binary-bound
  APISIX 3.17 plugin differential before operational qualification.
- Set `run-upgrade-rollback: true` in the final-release workflow so its existing behavior does not silently change.

**Step 4: Run the contract and workflow syntax gates**

Run: `bash scripts/release_gate_test.sh`

Run: `actionlint .github/workflows/security-release-gates.yml .github/workflows/release-candidate.yml .github/workflows/release.yml`

Expected: PASS.

### Task 2: Prove Graceful Termination Preserves An In-Flight HTTP Request

**Files:**
- Modify: `scripts/container_smoke_test.sh`
- Modify: `scripts/container_smoke.sh`

**Step 1: Add failing source-contract assertions**

Require the smoke to create a deliberately slow local upstream response, start that request before sending TERM, confirm the request reached the upstream, wait for the request after TERM, and verify its response body.

**Step 2: Run the contract test and confirm RED**

Run: `bash scripts/container_smoke_test.sh`

Expected: FAIL because the current smoke terminates only while idle.

**Step 3: Implement the active-request smoke**

Use the existing BusyBox upstream to expose a gated CGI endpoint. Start one
request in the background, wait for the upstream to write an active marker,
send TERM to APISIX-Go, release the upstream response only after the signal was
sent, then require both a successful response and a zero gateway exit code
within the existing shutdown deadline.

**Step 4: Run source and real-process verification**

Run: `bash scripts/container_smoke_test.sh`

Run: `CONTAINER_BIN=podman bash scripts/container_smoke.sh`

Expected: PASS; the slow response completes after TERM and the gateway exits zero.

### Task 3: Run The Functional And Stability Gates

**Files:**
- Verify: `pkg/route/proxy_soak_test.go`
- Verify: `scripts/etcd_recovery_smoke.sh`
- Verify: `t/plugin/`

**Step 1: Run the focused functional assembly checks**

Run: `source .envrc && make test-plugin-harness`

Run the four RC smoke selectors individually:

```bash
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='key-auth/valid-consumer-schema'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='proxy-rewrite/rewrite-host'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='proxy-control/request-buffering-disabled'
source .envrc && make test-plugin-smoke PLUGIN_SMOKE_CASE='uri-blocker/one-rule-blocks-query'
```

Expected: PASS.

**Step 2: Run verified-TLS etcd recovery against a local candidate image**

Build one local image from the current tree, capture its immutable image ID, then run `scripts/etcd_recovery_smoke.sh` using Podman and validate the generated evidence.

Expected: PASS for startup, committed last-good service during outage, update after recovery, delete/re-add, compaction recovery, restart recovery, and two-replica convergence.

**Step 3: Run the canonical stability soak**

Run:

```bash
source .envrc && APISIX_GO_RUN_SOAK=1 APISIX_GO_SOAK_DURATION=30m \
  scripts/go_cache.sh run -- go test -json ./pkg/route \
  -run '^TestProxyRuntimeSoak$' -count=1 -timeout=40m
```

Expected: PASS with zero request errors and no configured goroutine/heap growth violation.

**Step 4: Run changed-scope completion gates**

Run: `source .envrc && make build`

Run: `source .envrc && make lint`

Expected: PASS, or report any pre-existing failure precisely without weakening the qualification result.

### Task 4: Publish The Exact Narrow Claim In Documentation

**Files:**
- Modify: `docs/production-profile.md`
- Modify: `docs/runbooks/production-release.md`

**Step 1: Replace the obsolete blocker list**

Document that the current milestone is `http-data-plane-v1` functional/stability qualification. State explicitly that deployment, upgrade/rollback, and formal publication are deferred and do not block this milestone.

**Step 2: Keep the claim bounded**

Do not call the whole project production ready. Permit only this claim after a post-merge immutable revision passes the functional/stability workflow: the HTTP data-plane profile is functionally and runtime-stability qualified within its documented plugin and dependency boundaries.

**Step 3: Review the final diff**

Run: `git diff --check`

Run: `git diff --stat && git diff -- . ':(exclude)docs/superpowers/plans/2026-08-29-http-functional-stability.md'`

Expected: only workflow separation, active-request smoke, and the narrowed qualification documentation changed.
