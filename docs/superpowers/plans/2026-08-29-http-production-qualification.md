# HTTP Data Plane Production Qualification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce repeatable platform-reliability and release-grade evidence for a Linux `amd64`, HTTP-only APISIX-Go data plane covering every APISIX 3.17 plugin already qualified for the HTTP domain.

**Architecture:** Keep the current single-process `Server + GenerationEngine`; Kubernetes or systemd owns process restart and replica availability. Keep `security_profile: compat` for APISIX-compatible behavior, while enforcing production transport, listener, privilege, and artifact rules directly in the `http-data-plane-v1` qualification and release gates. Bind every positive claim to one source commit and one immutable image digest.

**Tech Stack:** Go 1.26, Bash, YAML, Podman/Docker, Linux `amd64`, TLS etcd 3.6.13, GitHub Actions, BuildKit, CycloneDX, Trivy, Cosign, GitHub artifact attestations.

**Spec:** User decisions recorded on 2026-08-29, `docs/design.md`, `docs/production-profile.md`, and `docs/runbooks/production-release.md`.

## Global Constraints

- The supported production scope is HTTP only: `apisix.proxy_mode: http`, no TCP/UDP stream listeners, and no stream plugin chain.
- Differential qualification remains 111/111 plugins; `mqtt-proxy` is the only stream-only plugin, so `http-data-plane-v1` contains the other 110 qualified plugins.
- Production qualification uses `security_profile: compat`. Do not remove the compat/strict implementation in this change; deleting that axis is a separate future decision.
- `http-data-plane-v1` directly requires `debug: false`, data-plane etcd, HTTPS endpoints, explicit TLS verification, no Admin API, trusted CIDRs, absolute runtime paths, and cleared proxy variables in container qualification.
- The runtime remains one process. Do not add an internal supervisor, worker subprocess, IPC activation, or listener inheritance.
- Only Linux `amd64` is qualified. Stream, Linux `arm64`, macOS, Windows, and multi-architecture publication remain outside this claim.
- Plugin behavior evidence stays in `pkg/capability/manifest.yaml` and the differential corpus. Platform recovery evidence stays under `platform-recovery-v1`.
- A release claim requires the same source commit and immutable image digest across non-root smoke, vulnerability/SBOM, recovery, soak, upgrade, and rollback records.
- Registry publication, tag creation, production deployment, or repository-environment changes require separate explicit authorization.

---

### Task 1: Make HTTP Production Scope the Canonical Profile

**Files:**
- Modify: `pkg/capability/manifest.yaml`
- Modify: `pkg/capability/load_test.go`
- Modify: `pkg/config/profiles_test.go`
- Modify: `pkg/config/release_gate_test.go`
- Modify: `pkg/config/validation.go`
- Modify: `conf/config-production.yaml`
- Modify: `scripts/release_gate_test.sh`
- Regenerate: `docs/plugins.md`, `README.md`, `README.zh-CN.md`, `pkg/plugin/registry_gen.go`
- Modify: `docs/production-profile.md`

**Interfaces:**
- `http-data-plane-v1` remains the production qualification name.
- Its required plugin list equals the ordered `apisix-3.17-all-plugins-v1` list filtered to plugins declaring the `http` domain.
- `mqtt-proxy` remains qualified by the all-plugin differential profile but is excluded from HTTP production scope.

- [x] **Step 1: Add the failing profile relationship test**

Add `TestHTTPDataPlaneQualificationMatchesQualifiedHTTPSubset` in `pkg/capability/load_test.go`. It must load both profiles, filter the all-plugin required list through plugin domains, assert the result has 110 entries, assert `mqtt-proxy` is absent, and compare exact order with `http-data-plane-v1`.

- [x] **Step 2: Add failing production tuple and hardening tests**

Change the production tuple expectation to `apisix-3.17/compat/http-data-plane-v1`. Run the existing mutation table under `SecurityCompat`; HTTPS etcd, explicit TLS verification, `debug: false`, trusted CIDRs, HTTP-only shape, and disabled Admin API must still fail independently when mutated.

- [x] **Step 3: Observe RED**

```bash
source .envrc
export GOFLAGS=-mod=readonly
scripts/go_cache.sh run -- go test ./pkg/capability -run '^TestHTTPDataPlaneQualificationMatchesQualifiedHTTPSubset$' -count=1
scripts/go_cache.sh run -- go test ./pkg/config -run '^(TestHTTPDataPlaneProductionConfigSelectsControlledTuple|TestProductionPolicyRejectsOneMutatedFieldPerRow)$' -count=1
bash scripts/release_gate_test.sh
```

The first command must fail on the six-plugin profile; the config and shell gates must fail while the checked-in production config still selects `strict`.

- [x] **Step 4: Update only the canonical profile and production config**

Set `http-data-plane-v1.required_plugins` to the 110-plugin ordered HTTP subset. Set `conf/config-production.yaml` to `security_profile: compat` and the same 110-plugin order. Keep HTTPS etcd verification, trusted CIDRs, no Admin API, and HTTP-only listeners.

- [x] **Step 5: Move production hardening under qualification ownership**

In `validateQualificationProfile`, require the production invariants directly when `QualificationHTTPDataPlaneV1` is selected. Do not call `validateSecurityProfile` with a fabricated strict selection and do not change the behavior of standalone `compat` or `strict` selections.

- [x] **Step 6: Synchronize generated projections and documentation**

```bash
source .envrc
scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -write
```

Update `docs/production-profile.md` to say the production claim covers 110 HTTP-domain plugins under compatibility behavior, while the stream-only `mqtt-proxy` remains outside the first release.

- [x] **Step 7: Verify the source relationship and focused config contract**

```bash
source .envrc
export GOFLAGS=-mod=readonly
scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen -count=1
scripts/go_cache.sh run -- go test ./pkg/config -run '^(TestProfileSelection|TestHTTPDataPlane|TestProductionPolicy|TestUnsupportedRuntimeConfig)' -count=1
scripts/go_cache.sh run -- go run ./cmd/capability-gen -repo-root . -check
bash scripts/release_gate_test.sh
git diff --check
```

### Task 2: Finalize Deterministic Platform Recovery Evidence

**Files:**
- Modify: `scripts/etcd_recovery_smoke.sh`
- Modify: `scripts/etcd_recovery_smoke_test.sh`
- Verify: `scripts/qualification/platform_recovery_test.sh`
- Modify: `docs/runbooks/production-release.md`

**Interfaces:**
- APISIX-Go replicas connect only to the `etcd` TCP gateway alias.
- Qualification control operations connect directly to `etcd-origin`.
- Evidence proves last-good service during isolation and converged state after DELETE/PUT plus compaction.

- [x] **Step 1: Replace unreliable network-disconnect and log-string assertions**

Use the pinned etcd image's stateless TCP gateway to create a deterministic revision gap. Assert pre-reconnect stale state and post-reconnect current state through both replicas.

- [x] **Step 2: Make gateway retry deterministic**

Set `--retry-delay=1s`; the fixture must fail without that exact flag.

- [x] **Step 3: Run fixture and real Podman qualification**

```bash
bash scripts/etcd_recovery_smoke_test.sh
env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u NO_PROXY \
  SOURCE_COMMIT=faab7791a4910c06ce5749076e7b69435a7281b5 \
  CONTAINER_BIN=podman \
  bash scripts/etcd_recovery_smoke.sh \
  sha256:025732c46282f923cea4fc3d5a57cc94cc00da7dbf4b20c636460d2502d65935
```

The functional arm64 image validates the gate only. Final release evidence must rerun against the new `linux/amd64` image built from the final source commit.

- [x] **Step 4: Remove strict selection from the recovery environment**

Run the recovery replicas with the production config's `compat` selection. Keep explicit HTTPS etcd, certificate verification, proxy clearing, non-root UID/GID, HTTP-only shape, and exact production qualification profile assertions.

- [x] **Step 5: Validate emitted platform records**

```bash
bash scripts/qualification/platform_recovery_test.sh --evidence-dir .cache/release-evidence/etcd-recovery/<run-id>/platform-recovery-v1
```

The final release workflow supplies the actual run directory and immutable image identity.

### Task 3: Codify the External Supervisor Lifecycle Contract

**Files:**
- Modify: `docs/design.md`
- Modify: `docs/production-profile.md`
- Modify: `docs/runbooks/production-release.md`
- Modify: `scripts/container_smoke.sh`
- Modify: `scripts/release_gate_test.sh`

**Interfaces:**
- APISIX-Go owns startup recovery, readiness, graceful TERM handling, and durable journal replay.
- Kubernetes/systemd owns restart on abnormal exit, replica replacement, rollout, and availability during replacement.

- [x] **Step 1: Record the architecture decision**

Replace planned external supervisor/worker wording with the selected single-process boundary. State that internal worker probation, IPC activation, and listener inheritance are not required for the HTTP production claim.

- [x] **Step 2: Strengthen process lifecycle smoke**

Keep non-root UID/GID checks and TERM exit code 0. Add readiness-before-traffic and readiness-after-start assertions against the candidate container; retain the exact 30-second shutdown bound.

- [x] **Step 3: Bind restart recovery to the platform gate**

The etcd recovery smoke must restart one replica, verify readiness returns, verify the committed route and TLS state return, and retain the replica's before/after identity in evidence. The other replica must continue serving throughout.

- [x] **Step 4: Make deployment-owner obligations fail closed in the runbook**

Require at least two replicas, Kubernetes/systemd restart-on-failure, a 30-second-or-greater stop timeout, liveness `/status`, readiness `/status/ready`, immutable image digest, rolling replacement with at least one ready replica, and external ingress logs. These are environment evidence, not claims produced by repository unit tests.

- [x] **Step 5: Verify lifecycle contracts**

```bash
bash scripts/container_smoke_test.sh
bash scripts/etcd_recovery_smoke_test.sh
bash scripts/release_gate_test.sh
```

### Task 4: Make the Linux amd64 Image a Release Artifact

**Files:**
- Modify: `Dockerfile`
- Modify: `scripts/release_gate_test.sh`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `.github/workflows/release-candidate.yml`
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- `container-evidence` builds one `linux/amd64` image, exports it once, and passes that exact archive/config digest to every operational job.
- Only `publish-image` receives registry write/OIDC/attestation permissions.

- [x] **Step 1: Add failing Dockerfile release assertions**

Require BuildKit cache mounts for module and build caches so caches do not become committed builder layers. Keep source copies explicit and the runtime image non-root.

- [x] **Step 2: Fix builder cache layers**

Add Dockerfile frontend syntax and cache mounts for `/go/pkg/mod` and `/root/.cache/go-build`. Do not copy the repository root and do not add dependencies.

- [x] **Step 3: Verify the immutable image chain**

The workflow must prove source commit, local image config digest, archive checksum, SBOM, zero HIGH/CRITICAL Trivy findings, non-root smoke, recovery, and soak all refer to the same exported image before publication.

- [x] **Step 4: Keep RC and final evidence distinct**

RC qualification may publish only when explicitly requested through the protected `production-release` environment. Final release must rerun every gate and may not relabel RC evidence as final evidence.

- [ ] **Step 5: Verify workflow contracts and build on native amd64 CI**

```bash
bash scripts/release_gate_test.sh
git diff --check
```

Local arm64/QEMU builds are diagnostic only. The authoritative image build and operational rerun happen on GitHub's native Linux `amd64` runner.

### Task 5: Add Upgrade and Rollback Qualification

**Files:**
- Create: `scripts/upgrade_rollback_smoke.sh`
- Create: `scripts/upgrade_rollback_smoke_test.sh`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `.github/workflows/release.yml`
- Modify: `scripts/release_gate_test.sh`
- Modify: `docs/runbooks/production-release.md`

**Interfaces:**
- Inputs are a candidate image digest and a distinct previously qualified image digest.
- Output is append-only JSON evidence containing both digests, source identities, probe results, and transition timestamps.

- [x] **Step 1: Write the fail-closed shell fixture**

Reject mutable tags, identical candidate/rollback digests, missing prior qualification metadata, failed readiness, failed route probes, and any transition that leaves no ready replica.

- [x] **Step 2: Implement rolling candidate and rollback transitions**

Start two replicas on the known-good digest, replace them one at a time with the candidate while keeping traffic serviceable, then replace them one at a time back to the known-good digest. Verify readiness, committed route state, and TLS after every replica transition.

- [x] **Step 3: Add the operational workflow job**

Require a distinct digest-qualified rollback image and its qualification metadata. The publication job must depend on successful upgrade/rollback evidence in addition to recovery and soak.

- [x] **Step 4: Verify the fixture and workflow contract**

```bash
bash scripts/upgrade_rollback_smoke_test.sh
bash scripts/release_gate_test.sh
git diff --check
```

### Task 6: Final Review and Qualification Run

**Files:**
- Review every changed source and generated projection.

- [x] **Step 1: Run impact-scoped repository gates**

```bash
source .envrc
export GOFLAGS=-mod=readonly
scripts/go_cache.sh run -- go test ./pkg/capability ./cmd/capability-gen ./pkg/config -count=1
bash scripts/etcd_recovery_smoke_test.sh
bash scripts/container_smoke_test.sh
bash scripts/upgrade_rollback_smoke_test.sh
bash scripts/release_gate_test.sh
make build
git diff --check
```

- [x] **Step 2: Request independent review**

Review the HTTP scope relation, production hardening ownership, recovery isolation, image identity chain, and rollback false-pass resistance. Fix only confirmed findings.

- [ ] **Step 3: Run the native Linux amd64 RC workflow**

Run the protected release-candidate workflow with operational gates enabled and a distinct qualified rollback digest. Do not publish unless separately authorized.

- [ ] **Step 4: State the exact permitted claim**

Only after every record passes for one final digest:

> `http-data-plane-v1` is production qualified on Linux `amd64` for the named Kubernetes/systemd-managed environment, source `<commit>`, image `<repository>@sha256:<digest>`, covering the 110 APISIX 3.17-qualified HTTP-domain plugins, evidence bundle `<URL and SHA256>`.
