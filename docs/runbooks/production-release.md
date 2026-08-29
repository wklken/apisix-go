# Production Release Runbook

This runbook separates three decisions:

1. release-candidate qualification;
2. immutable artifact publication; and
3. environment deployment acceptance.

Passing one decision does not imply the next. The supported product claim is
defined in [HTTP data-plane compatibility](../http-data-plane.md).

## 1. Qualify a release candidate

Run the `Release Candidate` workflow for one immutable ref:

```bash
gh workflow run release-candidate.yml -f ref='<commit-or-tag>'
```

The workflow resolves the ref once and binds every job to that commit. It must
pass:

| Gate | Required evidence |
| --- | --- |
| Source | Lint, build, unit coverage, capability drift, plugin harness, and focused real-process HTTP cases. |
| Plugin behavior | Complete differential evidence bound to the source commit and candidate binary SHA-256. |
| Concurrency | Focused race tests for the configuration, serving, routing, proxy, journal, etcd, and stream owners. |
| Container | Linux amd64 image, non-root UID/GID `10001:10001`, standalone proxy smoke, and graceful TERM with an in-flight request. |
| Recovery | Verified-TLS etcd last-good service, update, compaction recovery, replica restart, and live delete/re-add convergence. |
| Stability | Canonical 30-minute, concurrency-256 proxy soak using the thresholds in [Proxy Runtime Acceptance](../performance/proxy-runtime-acceptance.md). |

An RC is read-only: it does not publish or relabel its image. Missing, skipped,
stale, or identity-mismatched evidence fails the decision.

Useful local contract checks:

```bash
bash scripts/container_smoke_test.sh
bash scripts/release_metadata_test.sh
bash scripts/release_gate_test.sh
```

These checks validate scripts and workflow contracts. They are not substitutes
for the post-merge RC run or real container recovery evidence.

## 2. Publish an immutable release

The final `Release` workflow runs only for a non-RC `v*` tag. Before creating
the tag:

- the same source revision must have a successful RC;
- `APISIX_GO_ROLLBACK_RELEASE_TAG` must name a distinct, previously verified
  final release;
- the `production-release` environment must permit the tag and retain its
  required approval policy; and
- all repository and environment protections must be checked in GitHub rather
  than inferred from this document.

The final workflow reruns the gates. It does not reuse the RC image. Publication
pushes, signs, and attests the exact qualified GHCR digest, then publishes Linux
amd64 and arm64 GoReleaser archives plus a durable evidence bundle.

Required release assets include:

- `container-image.txt`;
- `release-evidence.tar.gz`;
- `release-evidence.tar.gz.sha256`; and
- `checksums.txt` for GoReleaser archives.

Verify the bundle before using the image:

```bash
set -euo pipefail
release_tag='<published-v-tag>'
evidence_dir="$PWD/release-evidence"
mkdir -p "$evidence_dir"

gh release download "$release_tag" --repo wklken/apisix-go \
  --pattern 'container-image.txt' \
  --pattern 'release-evidence.tar.gz' \
  --pattern 'release-evidence.tar.gz.sha256' \
  --dir "$evidence_dir"

(cd "$evidence_dir" && sha256sum -c release-evidence.tar.gz.sha256)
gh attestation verify "$evidence_dir/release-evidence.tar.gz" \
  --repo wklken/apisix-go \
  --signer-workflow wklken/apisix-go/.github/workflows/release.yml \
  --source-ref "refs/tags/$release_tag"

tar -C "$evidence_dir" -xzf "$evidence_dir/release-evidence.tar.gz"
(cd "$evidence_dir/release-evidence" && sha256sum -c MANIFEST.sha256)
image_reference=$(sed -n '1p' "$evidence_dir/container-image.txt")
[[ "$image_reference" =~ ^ghcr\.io/.+@sha256:[0-9a-f]{64}$ ]]
```

Verify the OCI signature and attestation for that digest before pull or deploy.
The repository, workflow, tag, source commit, and digest must all match the
release evidence.

## 3. Accept an environment deployment

Repository qualification does not prove an operator environment. Deploy only
the verified digest and record:

- image digest, source commit, deployment identity, and timestamps;
- effective configuration digest;
- liveness and readiness results;
- representative authenticated success, rejection, and failure probes;
- redacted ingress request-log evidence;
- declared capacity, latency, error-rate, and resource thresholds; and
- material failure and recovery scenarios for the environment.

At minimum, verify:

```bash
curl --fail --silent --show-error 'http://127.0.0.1:7085/status'
curl --fail --silent --show-error 'http://127.0.0.1:7085/status/ready'
curl --fail --silent --show-error \
  -H '<operator-auth-header>' \
  'https://<gateway>/<allowlisted-route>'
```

Run at least two replicas under an external service manager, allow at least 30
seconds for graceful termination, and replace replicas without removing the
last ready instance. Environment owners choose the deployment and load tools;
this repository does not invent those commands or thresholds.

## 4. Roll back

Rollback means deploying a distinct older published digest, never retagging a
mutable tag. Verify the previous release bundle and digest exactly as above,
then replace one replica at a time while checking:

- the surviving replica continues to serve;
- the replaced replica becomes ready;
- representative HTTP and frontend TLS routes remain healthy; and
- the recorded image identity matches the verified previous release.

The final workflow runs `scripts/upgrade_rollback_smoke.sh` against the
configured previous release and includes its append-only result in the release
bundle. A first release without a distinct verified predecessor cannot claim
upgrade/rollback acceptance.

## Decision record

Record each decision as one of:

- **passed**: every required gate passed for the same identity;
- **failed**: a required gate failed;
- **pending**: required infrastructure or evidence was unavailable.

Never combine evidence from different commits, binaries, images, RC runs, or
environments into one passing decision.
