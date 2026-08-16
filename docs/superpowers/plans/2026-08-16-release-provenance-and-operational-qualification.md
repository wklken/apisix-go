# Release Provenance and Operational Qualification Implementation Plan

> **For agentic workers:** Use this plan task-by-task in an isolated worktree.
> It does not authorize subagents, commits, pushes, tags, releases, or other
> external actions.

**Goal:** Make release candidates and final releases qualify the exact
`http-data-plane-v1` artifact through the same security and operational gates,
publish an immutable GHCR image with keyless signature and provenance, and
retain executable evidence for etcd loss/recovery, soak, and rollback.

This plan defines an executable mechanism, not a completed qualification. The
profile remains a candidate and the repository remains **not ready for
production** until a post-merge RC passes for its own immutable image and the
final release independently retains complete evidence for the final image.

**Architecture:** Keep `.github/workflows/security-release-gates.yml` as the
reusable artifact owner. A read-only `container-evidence` job builds and loads
one image, runs non-root smoke, SBOM, and Trivy, and uploads the exact archive.
A separate guarded `publish-image` job alone receives publication permissions,
downloads/checks that archive, pushes the exact image, captures the registry
digest, and signs/attests that digest. Add a verified-TLS Docker-based etcd
recovery harness and run the existing bounded proxy soak only for RC/final
qualification, not routine PR validation.

**Tech Stack:** GitHub Actions reusable workflows, Docker Buildx, GHCR,
Sigstore/cosign, GitHub artifact attestations, Trivy, CycloneDX, etcd 3.6.13,
Go 1.26.

## Corrections to the source plan

The following corrections are part of this tracked plan and are required for
an honest, executable release contract:

1. GitHub's reusable-workflow attestation contract requires both the caller
   and called workflow to grant `attestations: write`. Container publication
   also requires `contents: read`, `id-token: write`, and `packages: write` in
   the final publication path. Pull requests, master pushes, and RC calls keep
   read-only permissions.
2. `container-evidence` owns build, smoke, SBOM, Trivy rejection, archive, and
   upload with no registry publication authority. A separate guarded
   `publish-image` job downloads the exact archive and alone has write
   permissions. This prevents PR/master jobs from receiving publication
   authority.
3. `publish-image` references the protected `environment: production-release`.
   Publishing cannot bypass its required reviewers and wait timer.
4. The metadata CLI is
   `release_metadata.sh IMAGE_REFERENCE IMAGE_DIGEST OUTPUT [ARTIFACT ...]`.
   It is Docker-independent, writes schema 2 top-level `image_reference` and
   `image_digest`, validates a lowercase 64-hex SHA256 digest, and in
   `PUBLISH_IMAGE=true` mode requires `REFERENCE@DIGEST` consistency.
5. The soak job writes real JSON evidence with
   `go test -json ... | tee .cache/release-evidence/proxy-soak.json`. The
   optional `.cache/telemetry` directory is reserved and must not be described
   as populated unless a producer is added.
6. The recovery harness uses verified HTTPS etcd because the exact production
   profile rejects plaintext etcd. It generates a temporary CA/server
   certificate, mounts the CA through `SSL_CERT_FILE`, and still runs
   APISIX-Go as `10001:10001`.
7. A periodic etcd reachability probe is a prerequisite product fix. The
   pinned etcd watch can retry recoverable network failures indefinitely and
   does not reliably close on `docker stop`; without the probe `/readyz` may
   never become 503.
8. The first release has no previous immutable digest. The runbook records
   this bootstrap limitation and must not claim rollback qualification until a
   distinct older published digest exists and is exercised.
9. The repository's `production-release` environment currently permits only
   protected branches while the final workflow is tag-triggered. The runbook
   requires an operator to allow the intended `v*` tag policy before release;
   this plan does not mutate repository settings or bypass reviewer/wait
   protections.
10. The selected RC source is resolved once to an immutable commit; final jobs
    use the triggering `github.sha`. Every checkout and publication metadata
    assertion uses that commit, so a moving branch or tag cannot mix evidence.
11. GoReleaser runs with a clean repository. Generated container-reference and
    evidence files live under `runner.temp`, not the checkout. The final
    release attaches the verified evidence bundle, checksum manifest, and
    bundle checksum as durable release assets rather than relying on expiring
    Actions artifacts.
12. RC and final workflows rebuild and therefore produce separate image
    identities. Each runs the complete gates; RC evidence is never relabeled or
    combined with final evidence.
13. Signature and attestation verification bind the digest to the exact release
    tag and source commit. Production qualification additionally remains open
    until the deployment owner supplies environment-specific capacity and
    failure evidence.

## Frozen contracts

- Pull requests and pushes to `master` keep `contents: read` only and never
  push, sign, or attest an image.
- RC tags run all security, container, recovery, and soak gates with
  `publish-image: false`; final `v*` tags run the same gates with
  `publish-image: true`. Each run has its own image identity.
- Final publication uses
  `ghcr.io/${{ github.repository }}:${{ github.ref_name }}` and records a
  canonical digest-qualified
  `ghcr.io/${{ github.repository }}@sha256:<64-hex>` identity. Before push,
  scanning and rollback metadata bind the local tag, image-config digest, and
  archive checksum. Final metadata then binds those artifacts to the registry
  manifest digest used for signing, image attestation, and release evidence.
- The final caller and called image-publication job grant `contents: read`,
  `packages: write`, `id-token: write`, and `attestations: write`. The protected
  release job separately grants `contents: write`, `id-token: write`, and
  `attestations: write` for release assets and their evidence attestation; no
  long-lived registry or signing secret is introduced.
- Cosign uses GitHub OIDC keyless signing. GitHub build provenance attests the
  OCI digest, and a separate protected final-release attestation covers the
  durable evidence bundle. Verification is fail-closed and constrained to the
  exact tag, source commit, repository, and workflow identities in the runbook.
- Operational evidence uses `gcr.io/etcd-development/etcd:v3.6.13`, matching
  the project's etcd client line. During etcd loss `/readyz` becomes 503 while
  the last successfully applied route continues serving; after recovery
  readiness returns and a newer route revision is applied.
- The existing `TestProxyRuntimeSoak` is the canonical 30-minute concurrency
  soak. Do not add a second soak implementation.
- Rollback means redeploying a previous immutable digest and proving health
  plus proxy traffic; retagging a mutable tag is not rollback evidence. The
  first release remains bootstrap-only until an older digest exists.
- No workflow inspection, local test, tag, digest, run ID, or artifact is
  itself a production-qualification claim. The ledger stays open until the
  final evidence is post-merge, durable, and tied to the final image identity.

### Task 1: Make the reusable gate contract explicit and include RCs

**Files:**

- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `.github/workflows/release-candidate.yml`
- Modify: `.github/workflows/release.yml`
- Modify: `scripts/release_gate_test.sh`

- [ ] Add red assertions for optional boolean `workflow_call.inputs.publish-image`
  and `run-operational` (both default false), optional strings `source-ref` and
  `source-commit`
  (default empty), the RC call with `publish-image: false` and
  `run-operational: true`, the selected source ref on every checkout, the RC
  security gate in `package-validation.needs`, and final caller permissions
  including `packages: write`, `id-token: write`, and `attestations: write`.
- [ ] Assert that no PR/master path can evaluate a registry login or push
  condition as true.
- [ ] Resolve manual RC input once, pass its commit through `source-commit`, and
  use `ref: ${{ inputs.source-commit || github.sha }}` for reusable checkouts.
  Final checkouts use `github.sha`. Record and compare `git rev-parse HEAD`
  against the archived evidence commit before publication.
- [ ] Add the RC call with only `contents: read`. Add the final call with
  `publish-image: true`, `run-operational: true`, and all required publication
  permissions. Keep the protected `production-release` environment on the
  called publication job.

Focused red/green check:

```bash
bash scripts/release_gate_test.sh
```

### Task 2: Build, scan, sign, and attest one immutable image

**Files:**

- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `scripts/release_metadata.sh`
- Modify: `scripts/release_metadata_test.sh`
- Modify: `scripts/release_gate_test.sh`

- [ ] Require schema 2 `image_digest` and `image_reference` in rollback
  metadata, and test RC local image ID plus final digest-qualified reference.
- [ ] Assert these exact action pins:

  ```text
  docker/setup-buildx-action@v4.2.0
  docker/login-action@v4.6.0
  docker/metadata-action@v6.2.0
  docker/build-push-action@v7.3.0
  sigstore/cosign-installer@v4.1.2
  actions/attest-build-provenance@v4.2.2
  actions/download-artifact@v8.0.1
  ```

- [ ] Build exactly one `linux/amd64` image with `load: true` and `push: false`
  in `container-evidence`. Run the existing smoke, SBOM, and fail-closed
  Trivy rejection against that loaded image. Do not rebuild, retag, or mutate
  it between scanning and publication.
- [ ] After Trivy rejection, let only guarded `publish-image` log in and push
  the archive-loaded image. Validate the canonical RepoDigest and expose
  separate `reference` and `digest` outputs.
- [ ] Guard cosign and `actions/attest-build-provenance@v4.2.2` with
  `publish-image`; use `cosign sign --yes` and `subject-digest` from the
  captured registry digest, never a mutable tag.
- [ ] Use the corrected metadata CLI:

  ```text
  scripts/release_metadata.sh IMAGE_REFERENCE IMAGE_DIGEST OUTPUT [ARTIFACT ...]
  ```

  It must reject missing artifacts, invalid digests, tag-only publish
  references, and reference/digest mismatches before writing output. In
  publish mode set `PUBLISH_IMAGE=true` and require a digest-qualified
  reference.
- [ ] Upload `qualified-image-${{ github.run_id }}` from the read-only job and
  let recovery/soak jobs download the exact archive. The final release writes
  the reusable output under `runner.temp`, downloads and verifies all final
  evidence, writes a checksum manifest, and attaches `container-image.txt`, the
  evidence bundle, and its checksum without dirtying the GoReleaser checkout or
  changing generated release notes. Attest the bundle from the protected final
  workflow and retain run/source/runner/command context inside it.

Focused checks:

```bash
bash scripts/release_metadata_test.sh
bash scripts/release_gate_test.sh
```

### Task 3: Prove etcd degradation, last-good service, and recovery

**Files:**

- Create: `scripts/etcd_recovery_smoke.sh`
- Create: `scripts/etcd_recovery_smoke_test.sh`
- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `Makefile`

- [ ] Add fail-closed fixtures for missing Docker, malformed/tag-only image
  references, bounded polls, cleanup-on-failure, and the ordered happy-path
  transcript.
- [ ] The real harness accepts exactly one immutable local image ID or
  digest-qualified reference, installs cleanup before the first Docker
  mutation, uses unique names/no host networking, generates a temporary CA
  and DNS `etcd` certificate, and starts etcd over verified HTTPS.
- [ ] Mount the CA with `SSL_CERT_FILE`; use `conf/config-production.yaml`, an
  explicit endpoint override, and UID/GID `10001:10001`. Start route/upstream
  v1, prove live/ready/proxy, stop etcd and prove ready 503 plus last-good v1,
  restart etcd, update the same IDs to v2, and prove ready 200 plus v2.
- [ ] Capture logs, readiness bodies, and the transcript under
  `.cache/release-evidence/etcd-recovery/<run-id>/` before cleanup. Add
  `make release-etcd-recovery`, requiring `APISIX_IMAGE` and rejecting a
  tag-only handoff. The reachability probe must be implemented and tested
  before treating this scenario as a qualification gate.

Focused checks:

```bash
bash scripts/etcd_recovery_smoke_test.sh
bash -n scripts/etcd_recovery_smoke.sh scripts/etcd_recovery_smoke_test.sh
```

### Task 4: Promote the existing soak to an RC/release gate

**Files:**

- Modify: `.github/workflows/security-release-gates.yml`
- Modify: `scripts/release_gate_test.sh`
- Modify: `docs/design.md`

- [ ] Add one `run-operational`-guarded job using the existing test:

  ```bash
  APISIX_GO_RUN_SOAK=1 APISIX_GO_SOAK_DURATION=30m \
    go test -json ./pkg/route -run '^TestProxyRuntimeSoak$' -count=1 -timeout=40m \
    | tee .cache/release-evidence/proxy-soak.json
  ```

- [ ] Set the job timeout to 50 minutes, upload the JSON with `if: always()`,
  and upload `.cache/telemetry` only as optional evidence if a producer
  populated it. Do not run this operational job on ordinary PRs or master
  pushes. Final publication needs this job and every other security and
  operational gate to have succeeded.

### Task 5: Document digest verification, rollout, and rollback

**Files:**

- Create: `docs/runbooks/production-release.md`
- Modify: `README.md`
- Modify: `docs/configuration.md`
- Modify: `docs/design.md`
- Modify: `docs/plugins.md`
- Modify: `docs/production-profile.md`
- Modify: `docs/production-readiness-remediation-2026-08-15.md`
- Modify: `conf/config-default.yaml`

- [ ] Link the runbook from the project documentation and describe the exact
  profile, exclusions, digest identity, clean-host checks, rollout probes,
  evidence inventory, verified-TLS etcd degradation/recovery, and immutable
  rollback.
- [ ] Use the exact fail-closed verification commands in the runbook:

  ```bash
  cosign verify \
    --certificate-identity "https://github.com/wklken/apisix-go/.github/workflows/security-release-gates.yml@refs/tags/$RELEASE_TAG" \
    --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
    "$IMAGE_REFERENCE"

  gh attestation verify \
    "oci://$IMAGE_REFERENCE" \
    --repo wklken/apisix-go \
    --signer-workflow wklken/apisix-go/.github/workflows/security-release-gates.yml \
    --source-ref "refs/tags/$RELEASE_TAG" \
    --source-digest "$SOURCE_COMMIT"
  ```

- [ ] State that `<digest>` is an operator input, that authenticated GHCR read
  access is required, and that deployment commands are operator-supplied. Do
  not fabricate tags, digests, run IDs, Kubernetes/Helm commands, or evidence
  links.
- [ ] Keep all candidate/not-ready wording and keep release/operations ledger
  items open until post-merge RC/final evidence exists. Explicitly document
  the first-release no-previous-digest bootstrap limitation and the current
  `production-release` `v*` tag-policy prerequisite while retaining reviewers
  and wait-timer protection.
- [ ] Verify the evidence-bundle attestation against the exact final tag and
  source commit, then validate its outer checksum, inner manifest, and every
  artifact hash recorded by both metadata files.
- [ ] Require a source-matched clean checkout and all actual tools used by the
  acceptance commands. Document `deployment.etcd.health_check_timeout` as the
  probe interval and `deployment.etcd.timeout` as the request deadline.
- [ ] Keep qualification open until the deployment owner supplies explicit
  environment capacity thresholds/load evidence and relevant failure evidence.
  Verify an older rollback release's durable bundle, manifest, exact tag-bound
  signature/attestation, digest, archive-loaded image ID, and digest-pulled
  image ID before running probes.

### Task 6: Verify and deliver without claiming qualification

- [ ] Run the docs-only review (`git diff --check` plus a cross-file
  terminology/source-of-truth scan). The real Docker recovery and 30-minute
  soak remain required in RC/final workflows and are not replaced by local
  shell fixtures.
- [ ] The infrastructure change may be accepted only with normal PR CI green;
  scoped production qualification remains pending until separately authorized
  post-merge RC/final release evidence exists.
- [ ] Do not create a tag, publish a release/image, sign/attest externally,
  commit, or push as part of this implementation work unit.
