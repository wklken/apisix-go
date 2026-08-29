# Production release runbook

This runbook is the operator contract for the `http-data-plane-v1` release
candidate. It describes the release and operations evidence that must be
collected; it is not evidence by itself. The profile remains a candidate and
the repository remains **not ready for production** until a post-merge release
candidate has passed against its own immutable image and the final release has
independently passed with durable evidence for the final image.

The runbook deliberately does not invent Kubernetes, Helm, or environment-
specific deployment commands. The deployment owner supplies that step and
must keep the image reference digest-qualified.

## Scope and prerequisites

The qualification scope is exactly [`http-data-plane-v1`](../production-profile.md):
an HTTP-only data plane using the six-plugin allowlist
`request-id`, `cors`, `key-auth`, `jwt-auth`, `basic-auth`, and `prometheus`.
The selected configuration must use the data-plane etcd provider, verified
HTTPS etcd endpoints, no stream listeners or stream plugins, no Admin API, and
no process access-log claim. Lua/OpenResty behavior, external plugins, WASM,
XRPC, QUIC/HTTP/3, stream TLS/mTLS, Kafka PubSub, stateful session/cache
families, and unsupported discovery remain outside this contract.

Before an operator starts, obtain:

- a clean Bash host with Git, Docker or Podman, `curl`, `jq`, `openssl`, `make`, `tar`,
  `sha256sum`, and network access to GHCR and the release evidence;
- authenticated GHCR read access for pulling the image and verifying its OCI
  attestation, plus `cosign` and `gh` with access to the repository;
- the post-merge RC/final workflow result, the actual checked-out commit, and
  the evidence bundle. Check out this repository at that recorded commit before
  running repository scripts. Do not substitute a branch head, mutable tag,
  drifted harness checkout, or unrecorded local build for that identity;
- an environment-specific deployment procedure and an authenticated probe
  credential for one route in the six-plugin allowlist;
- an external ingress request-log evidence owner and a redacted evidence bundle
  plan covering representative successful, rejected, and failed requests.

The final workflow must use the protected `production-release` environment.
The repository currently permits only protected branches in that environment,
while final publication is tag-triggered. Before the first release, an
operator must explicitly allow the intended `v*` tag policy in that environment
without removing its required reviewers or wait timer. This runbook and the
implementation do not mutate repository settings or bypass those protections.

Before RC or final qualification, the release owner must verify that `master`
remains protected, the required CI, security, and independent `Capability
Status Contract` checks are enforced, and no self-approval is permitted. The
capability check validates manifest, generated-output, profile, corpus, and
accepted-ADR drift. The
`production-release` environment must retain its required reviewers and wait
timer while allowing only the explicitly approved `v*` tag policy. These are
repository and environment prerequisites to verify externally; this runbook
does not change them.

## Gate and evidence contract

The post-merge RC and the independent final must qualify the same recorded
source revision. Each RC/final run resolves its selected ref once and every job
uses the same immutable commit. RC and final runs build different artifacts and
must each pass the full gates; RC evidence qualifies only the RC image and is
never relabeled as final evidence. A read-only `container-evidence` job builds and loads one
`linux/amd64` image, runs the non-root container smoke, creates the SBOM,
rejects HIGH/CRITICAL Trivy findings, and archives the exact image. A separate
guarded `publish-image` job alone has registry write, OIDC, and attestation
permissions; it downloads and checks that archive, pushes that exact image,
captures the registry digest, and then signs and attests that digest. The
publish job cannot start until `container-evidence`, the security/race and
vulnerability gates, `etcd-recovery`, and `proxy-soak` all succeed. Publication
is never an opt-out from operational qualification.

The operational gates are:

1. focused race and vulnerability checks;
2. the exact container smoke, SBOM, and fail-closed Trivy result;
3. the release-grade verified-TLS etcd recovery scenario described below:
   readiness and liveness remain 200 while the committed last-good route set
   continues to work during an etcd outage, then the same route ID switches to
   the second upstream resource after recovery;
4. the canonical 30-minute proxy soak, with real JSON evidence produced by:

   ```bash
   APISIX_GO_RUN_SOAK=1 APISIX_GO_SOAK_DURATION=30m \
     go test -json ./pkg/route -run '^TestProxyRuntimeSoak$' -count=1 -timeout=40m \
     | tee .cache/release-evidence/proxy-soak.json
   ```
5. the external ingress request-log evidence bundle described below.

`.cache/telemetry` is optional and reserved for a producer; do not describe it
as populated unless a workflow step actually creates it. The final workflow
verifies the SBOM, Trivy JSON, image archive/checksum, rollback metadata,
recovery transcript and logs, soak JSON, source ref/commit, and publication
record, writes `MANIFEST.sha256`, and attaches `release-evidence.tar.gz` plus
its checksum to the GitHub release. The protected final workflow separately
attests that bundle and includes `qualification-context.json` with the run URL,
source identity, timestamp, runner identity, and exact gate command/action
identities. Actions artifacts are transient transfer objects, not the durable
qualification record.

## Discover and verify the immutable image

The final workflow attaches `container-image.txt`, `release-evidence.tar.gz`,
and `release-evidence.tar.gz.sha256` to the GitHub release. Supply the published
tag as an operator input; do not copy a tag or digest from this document:

```bash
set -euo pipefail
export RELEASE_TAG='<published-v-tag>'
export EVIDENCE_DIR="$PWD/release-evidence"
mkdir -p "$EVIDENCE_DIR"
gh release download "$RELEASE_TAG" --repo wklken/apisix-go \
  --pattern 'container-image.txt' \
  --pattern 'release-evidence.tar.gz' \
  --pattern 'release-evidence.tar.gz.sha256' \
  --dir "$EVIDENCE_DIR"
gh attestation verify "$EVIDENCE_DIR/release-evidence.tar.gz" \
  --repo wklken/apisix-go \
  --signer-workflow wklken/apisix-go/.github/workflows/release.yml \
  --source-ref "refs/tags/$RELEASE_TAG"
(cd "$EVIDENCE_DIR" && sha256sum -c release-evidence.tar.gz.sha256)
tar -C "$EVIDENCE_DIR" -xzf "$EVIDENCE_DIR/release-evidence.tar.gz"
(cd "$EVIDENCE_DIR/release-evidence" && sha256sum -c MANIFEST.sha256)
export IMAGE_REFERENCE="$(sed -n '1p' "$EVIDENCE_DIR/container-image.txt")"
case "$IMAGE_REFERENCE" in
  ghcr.io/wklken/apisix-go@sha256:[0-9a-fA-F]*) ;;
  *) echo "container-image.txt is not a digest-qualified GHCR reference" >&2; exit 1 ;;
esac
export IMAGE_DIGEST="${IMAGE_REFERENCE##*@}"
[[ "$IMAGE_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "image digest is not a lowercase 64-hex SHA256" >&2
  exit 1
}
export SOURCE_COMMIT="$(jq -r '.source.commit' \
  "$EVIDENCE_DIR/release-evidence/publication/release-metadata.json")"
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || {
  echo "release metadata source commit is invalid" >&2
  exit 1
}
jq -e --arg ref "$IMAGE_REFERENCE" --arg commit "$SOURCE_COMMIT" '
  .image_reference == $ref and .source.commit == $commit
' "$EVIDENCE_DIR/release-evidence/publication/release-metadata.json" >/dev/null
gh attestation verify "$EVIDENCE_DIR/release-evidence.tar.gz" \
  --repo wklken/apisix-go \
  --signer-workflow wklken/apisix-go/.github/workflows/release.yml \
  --source-ref "refs/tags/$RELEASE_TAG" \
  --source-digest "$SOURCE_COMMIT"
jq -e \
  --arg ref "$IMAGE_REFERENCE" \
  --arg commit "$SOURCE_COMMIT" \
  --arg source_ref "refs/tags/$RELEASE_TAG" '
  .source.ref == $source_ref and
  .source.commit == $commit and
  .image_reference == $ref and
  (.workflow.url | startswith("https://github.com/wklken/apisix-go/actions/runs/"))
' "$EVIDENCE_DIR/release-evidence/qualification-context.json" >/dev/null

verify_recorded_artifacts() {
  local root=$1
  local metadata=$2
  local expected name match candidate actual count records
  records=$(jq -e -r '
    .artifacts
    | if type == "array" and length > 0 then
        .[] | [.sha256, .name] | @tsv
      else
        error("metadata artifacts must be a non-empty array")
      end
  ' "$metadata") || return 1
  while IFS=$'\t' read -r expected name; do
    [[ "$expected" =~ ^[0-9a-f]{64}$ && "$name" =~ ^[A-Za-z0-9._-]+$ ]] || return 1
    match=''
    count=0
    while IFS= read -r candidate; do
      match=$candidate
      count=$((count + 1))
    done < <(find "$root" -type f -name "$name" -print)
    [[ $count -eq 1 ]] || return 1
    actual=$(sha256sum "$match" | awk '{print $1}')
    [[ "$actual" == "$expected" ]] || return 1
  done <<< "$records"
}
verify_recorded_artifacts \
  "$EVIDENCE_DIR/release-evidence" \
  "$EVIDENCE_DIR/release-evidence/qualified-image/rollback-metadata.json"
verify_recorded_artifacts \
  "$EVIDENCE_DIR/release-evidence" \
  "$EVIDENCE_DIR/release-evidence/publication/release-metadata.json"
```

The signature and provenance checks are fail-closed and must be run against
that digest-qualified reference. GHCR authentication is required before the
OCI attestation lookup:

```bash
set -euo pipefail
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

The digest is operator-supplied through the selected release asset, not a value
asserted by this document. The verification identity, source ref, and source
commit must match that exact release tag and its
`security-release-gates.yml` invocation; a signature or attestation for another
repository, workflow, tag, commit, or digest does not qualify the release.

## Clean-host acceptance

Use a host with no cached copy of the candidate image. Pull the exact digest,
confirm the image user, and run the existing smoke without rebuilding:

```bash
set -euo pipefail
export CONTAINER_BIN=${CONTAINER_BIN:-docker}
[[ "$(git rev-parse HEAD)" == "$SOURCE_COMMIT" ]] || {
  echo "repository checkout does not match the qualified source commit" >&2
  exit 1
}
"$CONTAINER_BIN" pull "$IMAGE_REFERENCE"
export EXPECTED_IMAGE_ID="$(sed -n '1p' \
  "$EVIDENCE_DIR/release-evidence/qualified-image/image-config-digest.txt")"
[[ "$EXPECTED_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]]
[[ "$("$CONTAINER_BIN" image inspect --format '{{.Id}}' "$IMAGE_REFERENCE")" == "$EXPECTED_IMAGE_ID" ]]
test "$("$CONTAINER_BIN" image inspect --format '{{.Config.User}}' "$IMAGE_REFERENCE")" = '10001:10001'
APISIX_IMAGE="$IMAGE_REFERENCE" APISIX_SKIP_BUILD=1 bash scripts/container_smoke.sh
```

The real etcd recovery gate uses the generated CA/server certificate and
`SSL_CERT_FILE` against exactly one TLS etcd 3.6.13 member from
`gcr.io/etcd-development/etcd:v3.6.13`. It must run as UID/GID `10001:10001`,
use the exact `conf/config-production.yaml` profile, and reject a tag-only
handoff. Pass the immutable local image ID produced by the selected runtime:

```bash
set -euo pipefail
export CONTAINER_BIN=${CONTAINER_BIN:-docker}
export APISIX_IMAGE="$("$CONTAINER_BIN" image inspect --format '{{.Id}}' "$IMAGE_REFERENCE")"
[[ "$APISIX_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "loaded image ID is not immutable" >&2
  exit 1
}
SOURCE_COMMIT="$SOURCE_COMMIT" CONTAINER_BIN="$CONTAINER_BIN" \
  make release-etcd-recovery APISIX_IMAGE="$APISIX_IMAGE"
```

The recovery command must leave the evidence transcript and captured logs in
`.cache/release-evidence/etcd-recovery/<run-id>/` and the platform-owned
`journal` and `generation` records in its `platform-recovery-v1/` child before
cleanup. Validate those records with `scripts/qualification/platform_recovery_test.sh
--evidence-dir <dir>`. A host without a compatible Docker or Podman runtime
cannot claim recovery or clean-host qualification; record the gate as pending.

## Rollout probes and operator deployment

Run the environment-specific deployment command supplied by the deployment
owner, passing only `$IMAGE_REFERENCE` as the image identity. Preserve the
command and its output in the release evidence; this runbook intentionally
does not choose a platform or invent a deployment API.

After each rollout step, set the endpoint and authenticated route inputs and
run the probes:

```bash
set -euo pipefail
export GATEWAY_URL='https://<operator-endpoint>'
export STATUS_URL='http://127.0.0.1:7085'
export ALLOWLISTED_ROUTE='/<operator-allowlisted-route>'
export PROBE_AUTH_HEADER='<operator-auth-header>'

curl --fail --silent --show-error "$STATUS_URL/status"
curl --fail --silent --show-error "$STATUS_URL/status/ready"
curl --fail --silent --show-error \
  -H "$PROBE_AUTH_HEADER" "$GATEWAY_URL$ALLOWLISTED_ROUTE"
```

The expected rollout state is status liveness 200, readiness 200 with a
serviceable committed configuration, and a successful response from the
allowlisted authenticated route. A deployment may use a TCP socket probe on
the intended listener for process liveness instead. Record the image digest,
replica or deployment identity, timestamps, response status, and evidence links
for each step. Do not infer qualification from workflow YAML inspection alone.

## External ingress request-log evidence

The exact six-plugin profile has no in-process request logger. Before RC/final
qualification, the external ingress owner must export a redacted evidence
bundle for representative successful, rejected, and failed requests. Each
sample or its correlated record must demonstrate:

- a redacted request ID and trace correlation;
- the HTTP method and normalized path with query-string secrets removed;
- response status and latency;
- the selected upstream identity; and
- the retention owner, retention period, and evidence location.

The bundle must identify the ingress configuration and collection time, and
the release evidence must retain the redaction check and the query/export
procedure used to produce it. This is external ingress evidence, not a claim
that the Go runtime emits these fields; container logs or process access-log
settings alone cannot satisfy this gate.

## Environment capacity and failure qualification

Repository gates do not establish production capacity for an operator's
topology. Before qualification, the deployment owner must declare the exact
replica count, upstream shape, traffic mix, concurrency and duration, latency
and error-rate thresholds, resource ceilings, and pass/fail criteria. Run that
environment-specific load exercise against `$IMAGE_REFERENCE`, retain the raw
results and monitoring evidence, and record the digest, source commit,
configuration fingerprint, runner/load-generator details, and timestamps.

The owner must also declare and exercise the environment failures that remain
material beyond the canonical etcd outage, such as ingress/upstream loss or a
replica termination, with explicit health, proxy-traffic, recovery-time, and
data-consistency expectations. This runbook does not invent a load tool,
capacity threshold, or platform failure command. Without that operator-supplied
evidence, the profile remains a candidate even if every repository workflow
gate passes.

## etcd degradation and recovery

The periodic etcd reachability probe remains operational evidence, separate
from readiness. The production probe uses
`deployment.etcd.health_check_timeout` as its interval in seconds, defaulting to
10 when omitted or non-positive. Each probe is bounded separately by the
existing `deployment.etcd.timeout` request deadline.

The verified-TLS recovery harness is the canonical acceptance scenario. During
etcd loss, expect `/status` and `/status/ready` to remain 200 while the last
successfully applied route continues serving. After etcd restarts, update the
same route ID to the second upstream resource and prove the newer response.
Capture the harness transcript, readiness bodies, container logs, and image
identity. A manual request that does not exercise this order is not recovery
evidence.

The harness is a real-etcd lifecycle gate, not a YAML or fake-watcher check. It
starts exactly one TLS etcd 3.6.13 member, two upstream fixtures, and two
independent APISIX-Go gateway containers. Every readiness, liveness, resource,
and proxy assertion is made against both gateway replicas. The initial etcd
PUTs cover routes, upstreams, a service, and a frontend SSL resource. The gate
observes service-selected upstream responses and a fresh TLS SNI handshake. It
updates the direct route and service upstream, while an invalid route
generation must remain uncommitted and the exact predecessor continues to
serve. Plugin behavior is qualified separately by plugin-owned unit,
integration, corpus, and differential tests; this harness does not emit
per-plugin recovery claims.

The gate exercises real deletes as well as puts. During the compaction gap it
deletes a route and changes the service back to the first upstream; after
recovery both replicas must return 404 for the deleted route and serve the
first upstream. It then deletes and re-adds the SSL resource; both replicas
must fail a fresh TLS handshake while it is deleted and serve it again only
after the re-add is committed.

For compaction recovery, the harness keeps both gateways on a control network
and disconnects only their data/etcd network until both still-reachable
processes report readiness 200 from their committed last-good state. It mutates and deletes etcd state, compacts at
the current revision, reconnects the data network, and requires each gateway
log to contain the stable etcd compacted-revision error. It then proves that
both replicas publish the same recovered snapshot, including the current
service, SSL, and route state. All
created gateway, fixture, etcd, and network resources are cleaned up on every
exit. The gate also restarts one replica from its committed journal, then
writes identity-bound platform evidence after the SSL delete/re-add cycle.
A host without a compatible container runtime may run the Docker-free contract
test, but cannot claim that this real-etcd gate passed; record it as pending.

## Immutable rollback

Rollback means redeploying an older published digest and repeating the health
and authenticated proxy probes. Retagging a mutable release tag is not
rollback evidence. Verify the rollback bundle checksums and loaded image ID
before asking the deployment owner to run the environment-specific rollback:

```bash
set -euo pipefail
export PREVIOUS_RELEASE_TAG='<older-published-v-tag>'
export PREVIOUS_EVIDENCE_DIR="$PWD/previous-release-evidence"
mkdir -p "$PREVIOUS_EVIDENCE_DIR"
gh release download "$PREVIOUS_RELEASE_TAG" --repo wklken/apisix-go \
  --pattern 'container-image.txt' \
  --pattern 'release-evidence.tar.gz' \
  --pattern 'release-evidence.tar.gz.sha256' \
  --dir "$PREVIOUS_EVIDENCE_DIR"
gh attestation verify "$PREVIOUS_EVIDENCE_DIR/release-evidence.tar.gz" \
  --repo wklken/apisix-go \
  --signer-workflow wklken/apisix-go/.github/workflows/release.yml \
  --source-ref "refs/tags/$PREVIOUS_RELEASE_TAG"
(cd "$PREVIOUS_EVIDENCE_DIR" && sha256sum -c release-evidence.tar.gz.sha256)
tar -C "$PREVIOUS_EVIDENCE_DIR" -xzf "$PREVIOUS_EVIDENCE_DIR/release-evidence.tar.gz"
(cd "$PREVIOUS_EVIDENCE_DIR/release-evidence" && sha256sum -c MANIFEST.sha256)

export PREVIOUS_IMAGE_REFERENCE="$(sed -n '1p' \
  "$PREVIOUS_EVIDENCE_DIR/container-image.txt")"
[[ "$PREVIOUS_IMAGE_REFERENCE" =~ ^ghcr.io/wklken/apisix-go@sha256:[0-9a-f]{64}$ ]] || {
  echo "previous image must be a published digest-qualified reference" >&2
  exit 1
}
[[ "$PREVIOUS_IMAGE_REFERENCE" != "$IMAGE_REFERENCE" ]] || {
  echo "rollback digest must differ from the current digest" >&2
  exit 1
}
export PREVIOUS_SOURCE_COMMIT="$(jq -r '.source.commit' \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence/publication/release-metadata.json")"
[[ "$PREVIOUS_SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || {
  echo "previous release source commit is invalid" >&2
  exit 1
}
gh attestation verify "$PREVIOUS_EVIDENCE_DIR/release-evidence.tar.gz" \
  --repo wklken/apisix-go \
  --signer-workflow wklken/apisix-go/.github/workflows/release.yml \
  --source-ref "refs/tags/$PREVIOUS_RELEASE_TAG" \
  --source-digest "$PREVIOUS_SOURCE_COMMIT"
jq -e --arg ref "$PREVIOUS_IMAGE_REFERENCE" --arg commit "$PREVIOUS_SOURCE_COMMIT" '
  .image_reference == $ref and .source.commit == $commit
' "$PREVIOUS_EVIDENCE_DIR/release-evidence/publication/release-metadata.json" >/dev/null
declare -F verify_recorded_artifacts >/dev/null
verify_recorded_artifacts \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence" \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence/qualified-image/rollback-metadata.json"
verify_recorded_artifacts \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence" \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence/publication/release-metadata.json"

cosign verify \
  --certificate-identity "https://github.com/wklken/apisix-go/.github/workflows/security-release-gates.yml@refs/tags/$PREVIOUS_RELEASE_TAG" \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  "$PREVIOUS_IMAGE_REFERENCE"
gh attestation verify "oci://$PREVIOUS_IMAGE_REFERENCE" \
  --repo wklken/apisix-go \
  --signer-workflow wklken/apisix-go/.github/workflows/security-release-gates.yml \
  --source-ref "refs/tags/$PREVIOUS_RELEASE_TAG" \
  --source-digest "$PREVIOUS_SOURCE_COMMIT"

export PREVIOUS_ARCHIVE_REFERENCE="$(sed -n '1p' \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence/qualified-image/image-reference.txt")"
export EXPECTED_PREVIOUS_IMAGE_ID="$(sed -n '1p' \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence/qualified-image/image-config-digest.txt")"
[[ "$EXPECTED_PREVIOUS_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]]
docker load --input \
  "$PREVIOUS_EVIDENCE_DIR/release-evidence/qualified-image/apisix-image.tar.gz"
export LOADED_PREVIOUS_IMAGE_ID="$(docker image inspect --format '{{.Id}}' \
  "$PREVIOUS_ARCHIVE_REFERENCE")"
[[ "$LOADED_PREVIOUS_IMAGE_ID" == "$EXPECTED_PREVIOUS_IMAGE_ID" ]]
docker pull "$PREVIOUS_IMAGE_REFERENCE"
docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
  "$PREVIOUS_IMAGE_REFERENCE" | grep -Fx "$PREVIOUS_IMAGE_REFERENCE"
export PREVIOUS_IMAGE_ID="$(docker image inspect --format '{{.Id}}' \
  "$PREVIOUS_IMAGE_REFERENCE")"
[[ "$PREVIOUS_IMAGE_ID" == "$EXPECTED_PREVIOUS_IMAGE_ID" ]] || {
  echo "pulled image ID does not match the qualified archive" >&2
  exit 1
}
```

The first release has no previous immutable digest. Until a distinct older
published digest exists and is exercised, rollback qualification must remain
open; a local image, a tag-only reference, or the current digest cannot satisfy
that requirement. Once the verified older digest exists, pass only
`$PREVIOUS_IMAGE_REFERENCE` to the operator-supplied rollback command and rerun
the `/status`, `/status/ready`, and allowlisted authenticated-route probes.
Record the command, results, recovery time, timestamp, runner/environment
details, and durable release-asset links.

## Qualification decision

Keep the profile and ledger wording as candidate/not-ready until a post-merge
RC has passed for its own digest and all final-release gates, clean-host
acceptance, deployment probes, environment-specific capacity/failure evidence,
external ingress request-log evidence, and verified rollback evidence exist for
the final digest. The protected `master` policy, required CI/security/
`Capability Status Contract` checks, protected `production-release`
reviewers/wait timer, approved
`v*` tag policy, and no-self-approval rule must also remain in force. Never
combine RC and final evidence as though independently built images had one
identity. A successful local shell test, workflow definition, or attached
artifact is not by itself a production qualification claim.
