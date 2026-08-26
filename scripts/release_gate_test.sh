#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
makefile="$repo_root/Makefile"
dockerfile="$repo_root/Dockerfile"
goreleaser="$repo_root/.goreleaser.yaml"
workflow="$repo_root/.github/workflows/security-release-gates.yml"
unit_workflow="$repo_root/.github/workflows/unit-test.yml"
capability_status_workflow="$repo_root/.github/workflows/capability-status.yml"
rc_workflow="$repo_root/.github/workflows/release-candidate.yml"
release_workflow="$repo_root/.github/workflows/release.yml"

require_pattern() {
    local pattern=$1
    local file=$2
    if ! grep -Eq -- "$pattern" "$file"; then
        printf 'missing %q in %s\n' "$pattern" "$file" >&2
        return 1
    fi
}

require_fixed() {
    local text=$1
    local file=$2
    if ! grep -Fq -- "$text" "$file"; then
        printf 'missing %q in %s\n' "$text" "$file" >&2
        return 1
    fi
}

require_text() {
    local text=$1
    local content=$2
    if ! grep -Fq -- "$text" <<<"$content"; then
        printf 'missing %q in workflow header\n' "$text" >&2
        return 1
    fi
}

job_block() {
    local file=$1
    local job=$2
    awk -v target="$job" '
        $0 == "jobs:" { in_jobs = 1; next }
        in_jobs && $0 == "  " target ":" { in_job = 1; print; next }
        in_job && $0 ~ /^  [A-Za-z0-9_-]+:/ { exit }
        in_job { print }
    ' "$file"
}

require_job_pattern() {
    local file=$1
    local job=$2
    local pattern=$3
    local block
    block=$(job_block "$file" "$job")
    if [[ -z "$block" ]] || ! grep -Eq -- "$pattern" <<<"$block"; then
        printf 'missing %q in job %s of %s\n' "$pattern" "$job" "$file" >&2
        return 1
    fi
}

require_job_fixed() {
    local file=$1
    local job=$2
    local text=$3
    local block
    block=$(job_block "$file" "$job")
    if [[ -z "$block" ]] || ! grep -Fq -- "$text" <<<"$block"; then
        printf 'missing %q in job %s of %s\n' "$text" "$job" "$file" >&2
        return 1
    fi
}

reject_job_pattern() {
    local file=$1
    local job=$2
    local pattern=$3
    local block
    block=$(job_block "$file" "$job")
    if [[ -n "$block" ]] && grep -Eq -- "$pattern" <<<"$block"; then
        printf 'unexpected %q in job %s of %s\n' "$pattern" "$job" "$file" >&2
        return 1
    fi
}

make_target_body() {
    local target=$1
    awk -v target="$target:" '
        $0 == target { in_target = 1; next }
        in_target && /^\t/ { print; next }
        in_target && /^[[:space:]]*$/ { next }
        in_target { exit }
    ' "$makefile"
}

require_make_target_body() {
    local target=$1
    local expected=$2
    local count actual
    count=$(grep -Ec -- "^${target}::?([[:space:]]|$)" "$makefile" || true)
    if [[ "$count" -ne 1 ]]; then
        printf 'expected exactly one %s target in %s, found %s\n' "$target" "$makefile" "$count" >&2
        return 1
    fi
    actual=$(make_target_body "$target")
    if [[ "$actual" != "$expected" ]]; then
        printf 'target %s body differs from the release contract in %s\n' "$target" "$makefile" >&2
        printf 'want:\n%s\ngot:\n%s\n' "$expected" "$actual" >&2
        return 1
    fi
}

test -f "$workflow"
test -f "$rc_workflow"
test -f "$release_workflow"
test -f "$makefile"
test -f "$unit_workflow"
test -f "$capability_status_workflow"
test -f "$dockerfile"
test -f "$goreleaser"

# Every local and packaged binary must use the same path/symbol stripping
# contract while retaining the four runtime build metadata values.
require_pattern 'go build[[:space:]]+-trimpath[[:space:]]+-ldflags[[:space:]]+"-s[[:space:]]+-w[[:space:]]+' "$makefile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.Version=\$\(VERSION\)' "$makefile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.Commit=\$\(COMMIT\)' "$makefile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.BuildTime=\$\(BUILD_TIME\)' "$makefile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.GoVersion=\$\(GO_VERSION\)' "$makefile"
require_fixed 'GO_CACHE_RUNNER ?= bash scripts/go_cache.sh run --' "$makefile"
require_fixed 'APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 $(GO_CACHE_RUNNER) go test ./t/plugin -count=1' "$makefile"
require_fixed '.PHONY: generate-capabilities' "$makefile"
require_fixed '.PHONY: check-capability-drift' "$makefile"
require_fixed '.PHONY: test-capability-status' "$makefile"
require_make_target_body generate-capabilities \
    $'\t$(GO_CACHE_RUNNER) go run ./cmd/capability-gen -repo-root . -write'
require_make_target_body check-capability-drift \
    $'\t$(GO_CACHE_RUNNER) go run ./cmd/capability-gen -repo-root . -check'
status_target_body=$(printf '\t%s\n\t%s' \
    "\$(GO_CACHE_RUNNER) go test ./pkg/capability ./pkg/config ./pkg/plugin -run '^(TestLoadedManifest|TestManifest|TestProfileSelection|TestCapabilityManifest|TestCapabilityRegistry)' -count=1" \
    "APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 \$(GO_CACHE_RUNNER) go test ./t/plugin -run '^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestUpstreamCorpusAccountingWithoutSourceCheckout|TestCorpusEvidenceMatchesCompatibilityTarget)\$\$' -count=1")
require_make_target_body test-capability-status "$status_target_body"
require_fixed '.PHONY: test-plugin-smoke' "$makefile"
require_fixed 'APISIX_GO_PLUGIN_SMOKE_CASE="$(PLUGIN_SMOKE_CASE)" $(GO_CACHE_RUNNER) go test ./t/plugin -run '\''^TestPluginIntegration$$'\'' -count=1 -v' "$makefile"
require_fixed '.PHONY: cache-gc-test' "$makefile"
require_fixed '.PHONY: cache-gc' "$makefile"
require_fixed '.PHONY: cache-clean-shared' "$makefile"
require_job_fixed "$unit_workflow" build-and-unit 'run: make test-plugin-harness'
require_job_fixed "$unit_workflow" build-and-unit 'run: bash scripts/release_gate_test.sh'
require_job_fixed "$capability_status_workflow" capability-status 'run: bash scripts/capability_status_gate_test.sh'
require_job_fixed "$capability_status_workflow" capability-status 'run: bash -lc '\''source .envrc && make check-capability-drift'\'''
require_job_fixed "$capability_status_workflow" capability-status 'run: bash -lc '\''source .envrc && make test-capability-status'\'''
require_job_fixed "$unit_workflow" integration-smoke 'run: make test-plugin-smoke PLUGIN_SMOKE_CASE='\''${{ matrix.case.selector }}'\'''
reject_job_pattern "$unit_workflow" integration-smoke 'go test[[:space:]]+\./t/plugin[[:space:]]+-run'
require_job_fixed "$release_workflow" build-and-unit 'run: make test-plugin-harness'
require_job_fixed "$rc_workflow" build-and-unit 'run: make test-plugin-harness'
require_job_fixed "$release_workflow" integration-smoke 'run: make test-plugin-smoke PLUGIN_SMOKE_CASE='\''${{ matrix.case.pattern }}'\'''
require_job_fixed "$rc_workflow" integration-smoke 'run: make test-plugin-smoke PLUGIN_SMOKE_CASE='\''${{ matrix.case.pattern }}'\'''
require_pattern 'go build[[:space:]]+-trimpath[[:space:]]+-ldflags[[:space:]]+"-s[[:space:]]+-w[[:space:]]+' "$dockerfile"
require_pattern '^    flags:[[:space:]]*\[[[:space:]]*-trimpath[[:space:]]*\][[:space:]]*$' "$goreleaser"
require_pattern '^    ldflags:[[:space:]]*$' "$goreleaser"
require_pattern '-s[[:space:]]+-w' "$goreleaser"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.Version=\{\{[[:space:]]*\.Version[[:space:]]*\}\}' "$goreleaser"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.Commit=\{\{[[:space:]]*\.Commit[[:space:]]*\}\}' "$goreleaser"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.BuildTime=\{\{[[:space:]]*\.Date[[:space:]]*\}\}' "$goreleaser"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.GoVersion=\{\{[[:space:]]*\.Env\.GOVERSION[[:space:]]*\}\}' "$goreleaser"
require_job_pattern "$release_workflow" release 'GOVERSION=\$\(go version\).*GITHUB_ENV'
require_job_pattern "$rc_workflow" package-validation 'GOVERSION=\$\(go version\).*GITHUB_ENV'

# Reusable interface and permissions are checked in the workflow header, not
# merely anywhere in the file.
header=$(sed -n '1,/^jobs:$/p' "$workflow")
require_text 'workflow_call:' "$header"
require_text 'publish-image:' "$header"
require_text 'run-operational:' "$header"
require_text 'source-ref:' "$header"
require_text 'source-commit:' "$header"
require_text 'image-reference:' "$header"
require_text 'workflow_dispatch:' "$header"
permissions_block=$(awk '/^permissions:/{in_permissions=1; next} in_permissions && /^concurrency:/{exit} in_permissions{print}' "$workflow")
require_text 'contents: read' "$permissions_block"
permission_lines=$(sed '/^[[:space:]]*$/d' <<<"$permissions_block")
if [[ "$permission_lines" != '  contents: read' ]]; then
    printf 'top-level permissions must be contents: read only in %s\n' "$workflow" >&2
    exit 1
fi
dispatch_block=$(awk '/^  workflow_dispatch:/{in_dispatch=1; print; next} in_dispatch && /^permissions:/{exit} in_dispatch{print}' "$workflow")
if grep -Eq '^    inputs:|^      (publish-image|run-operational|source-ref|source-commit):' <<<"$dispatch_block"; then
    printf 'direct workflow_dispatch must not expose release inputs\n' >&2
    exit 1
fi

require_job_fixed "$workflow" validate-inputs 'PUBLISH_IMAGE: ${{ inputs.publish-image }}'
require_job_fixed "$workflow" validate-inputs 'RUN_OPERATIONAL: ${{ inputs.run-operational }}'
require_job_fixed "$workflow" validate-inputs 'SOURCE_COMMIT: ${{ inputs.source-commit || github.sha }}'
require_job_fixed "$workflow" validate-inputs 'publish-image requires run-operational=true'
require_job_fixed "$workflow" validate-inputs 'exit 1'
require_job_fixed "$workflow" validate-inputs 'contents: read'

require_job_fixed "$workflow" race-and-vulnerability 'needs: validate-inputs'
require_job_fixed "$workflow" race-and-vulnerability 'ref: ${{ inputs.source-commit || github.sha }}'
require_job_fixed "$workflow" race-and-vulnerability 'git rev-parse HEAD'
require_job_fixed "$workflow" race-and-vulnerability 'go test -race ./pkg/config ./cmd ./pkg/server ./pkg/route ./pkg/proxy ./pkg/store ./pkg/etcd ./pkg/stream -count=1'
require_job_fixed "$workflow" race-and-vulnerability 'govulncheck@v1.7.0'
require_job_fixed "$workflow" race-and-vulnerability 'contents: read'
for job in race-and-vulnerability container-evidence etcd-recovery proxy-soak publish-image; do
    require_job_fixed "$workflow" "$job" 'ref: ${{ inputs.source-commit || github.sha }}'
done

require_job_fixed "$workflow" container-evidence 'needs: validate-inputs'
require_job_fixed "$workflow" container-evidence 'Record checked-out commit and build metadata'
require_job_fixed "$workflow" container-evidence 'EXPECTED_SOURCE_COMMIT: ${{ inputs.source-commit || github.sha }}'
require_job_fixed "$workflow" container-evidence '[[ "$commit" == "$EXPECTED_SOURCE_COMMIT" ]]'
require_job_fixed "$workflow" container-evidence 'mkdir -p "$EVIDENCE_DIR"'
require_job_fixed "$workflow" container-evidence 'selected_ref=${SELECTED_SOURCE_REF:-${GITHUB_REF:-security-release-gate}}'
require_job_fixed "$workflow" container-evidence 'version=${selected_ref##*/}'
require_job_fixed "$workflow" container-evidence 'go_version=$(go version)'
require_job_fixed "$workflow" container-evidence 'id: build-image'
require_job_fixed "$workflow" container-evidence 'docker/setup-buildx-action@v4.2.0'
require_job_fixed "$workflow" container-evidence 'docker/metadata-action@v6.2.0'
require_job_fixed "$workflow" container-evidence 'docker/build-push-action@v7.3.0'
require_job_fixed "$workflow" container-evidence 'platforms: linux/amd64'
require_job_fixed "$workflow" container-evidence 'load: true'
require_job_fixed "$workflow" container-evidence 'push: false'
require_job_fixed "$workflow" container-evidence 'VERSION=${{ steps.source.outputs.version }}'
require_job_fixed "$workflow" container-evidence 'COMMIT=${{ steps.source.outputs.commit }}'
require_job_fixed "$workflow" container-evidence 'BUILD_TIME=security-release-gate'
require_job_fixed "$workflow" container-evidence 'GO_VERSION=${{ steps.source.outputs.go_version }}'
require_job_fixed "$workflow" container-evidence 'org.opencontainers.image.revision=${{ steps.source.outputs.commit }}'
require_job_fixed "$workflow" container-evidence 'org.opencontainers.image.version=${{ steps.source.outputs.version }}'
require_job_fixed "$workflow" container-evidence 'APISIX_SKIP_BUILD: "1"'
require_job_fixed "$workflow" container-evidence 'bash scripts/container_smoke.sh'
require_job_fixed "$workflow" container-evidence 'anchore/sbom-action@v0.24.0'
require_job_fixed "$workflow" container-evidence 'aquasecurity/trivy-action@v0.36.0'
require_job_fixed "$workflow" container-evidence 'ignore-unfixed: false'
require_job_fixed "$workflow" container-evidence 'exit-code: "0"'
require_job_fixed "$workflow" container-evidence 'jq -e'
require_job_fixed "$workflow" container-evidence 'docker save "$APISIX_IMAGE" | gzip --best'
require_job_fixed "$workflow" container-evidence 'sha256sum apisix-image.tar.gz >apisix-image.tar.gz.sha256'
require_job_fixed "$workflow" container-evidence 'bash scripts/release_metadata.sh'
require_job_fixed "$workflow" container-evidence 'image_digest'
require_job_fixed "$workflow" container-evidence 'qualified-image-${{ github.run_id }}'
require_job_fixed "$workflow" container-evidence 'if: always()'
reject_job_pattern "$workflow" container-evidence '(packages|id-token|attestations): write'
reject_job_pattern "$workflow" container-evidence 'contents: write'

for job in etcd-recovery proxy-soak; do
    require_job_fixed "$workflow" "$job" 'if: ${{ inputs.run-operational }}'
    require_job_fixed "$workflow" "$job" 'race-and-vulnerability'
    require_job_fixed "$workflow" "$job" 'container-evidence'
    require_job_fixed "$workflow" "$job" 'contents: read'
done
require_job_fixed "$workflow" etcd-recovery 'actions/download-artifact@v8.0.1'
require_job_fixed "$workflow" etcd-recovery 'sha256sum -c apisix-image.tar.gz.sha256'
require_job_fixed "$workflow" etcd-recovery 'docker load --input'
require_job_fixed "$workflow" etcd-recovery 'bash scripts/etcd_recovery_smoke.sh "$APISIX_IMAGE"'
require_job_fixed "$workflow" etcd-recovery 'image_id=$(docker image inspect --format'
require_job_fixed "$workflow" proxy-soak 'APISIX_GO_SOAK_DURATION=30m'
require_job_fixed "$workflow" proxy-soak 'go test -json ./pkg/route -run '\''^TestProxyRuntimeSoak$'\'' -count=1 -timeout=40m'
require_job_fixed "$workflow" proxy-soak 'tee "$EVIDENCE_DIR/proxy-soak.json"'
require_job_fixed "$workflow" proxy-soak 'timeout-minutes: 50'

require_job_fixed "$workflow" publish-image 'inputs.publish-image && inputs.run-operational'
for gate in container-evidence race-and-vulnerability etcd-recovery proxy-soak; do
    require_job_fixed "$workflow" publish-image "$gate"
done
require_job_fixed "$workflow" publish-image 'environment: production-release'
require_job_fixed "$workflow" publish-image 'packages: write'
require_job_fixed "$workflow" publish-image 'id-token: write'
require_job_fixed "$workflow" publish-image 'attestations: write'
require_job_fixed "$workflow" publish-image 'actions/download-artifact@v8.0.1'
require_job_fixed "$workflow" publish-image 'sha256sum -c apisix-image.tar.gz.sha256'
require_job_fixed "$workflow" publish-image 'docker load --input'
require_job_fixed "$workflow" publish-image 'EXPECTED_SOURCE_COMMIT: ${{ inputs.source-commit || github.sha }}'
require_job_fixed "$workflow" publish-image '[[ "$(<"$EVIDENCE_DIR/source-commit.txt")" == "$EXPECTED_SOURCE_COMMIT" ]]'
require_job_fixed "$workflow" publish-image '[[ "$(<"$EVIDENCE_DIR/publication-source-commit.txt")" == "$EXPECTED_SOURCE_COMMIT" ]]'
require_job_fixed "$workflow" publish-image 'docker/login-action@v4.6.0'
require_job_fixed "$workflow" publish-image 'docker push "$IMAGE_REFERENCE"'
require_job_fixed "$workflow" publish-image 'RepoDigests'
require_job_fixed "$workflow" publish-image 'sigstore/cosign-installer@v4.1.2'
require_job_fixed "$workflow" publish-image 'cosign sign --yes'
require_job_fixed "$workflow" publish-image 'actions/attest-build-provenance@v4.2.2'
require_job_fixed "$workflow" publish-image 'subject-digest: ${{ steps.push.outputs.digest }}'
reject_job_pattern "$workflow" publish-image 'subject-digest: \$\{\{ env\.IMAGE_DIGEST \}\}'
require_job_fixed "$workflow" publish-image 'PUBLISH_IMAGE=true'
require_job_fixed "$workflow" publish-image 'scripts/release_metadata.sh'
require_job_fixed "$workflow" publish-image 'image-reference=%s'
require_job_fixed "$workflow" publish-image 'PUBLISHED_IMAGE_REFERENCE'
require_job_fixed "$workflow" publish-image 'release-publication-${{ github.run_id }}'

# Callers must pass the fixed interface, and RC packaging must wait for the
# reusable qualification result.
require_job_fixed "$rc_workflow" resolve-source 'ref: ${{ inputs.ref || github.sha }}'
require_job_fixed "$rc_workflow" resolve-source 'commit=$(git rev-parse HEAD)'
require_job_fixed "$rc_workflow" resolve-source 'commit=%s'
for job in lint build-and-unit integration-smoke package-validation; do
    require_job_fixed "$rc_workflow" "$job" 'resolve-source'
    require_job_fixed "$rc_workflow" "$job" 'ref: ${{ needs.resolve-source.outputs.commit }}'
done
require_job_fixed "$rc_workflow" security-release-gates 'uses: ./.github/workflows/security-release-gates.yml'
require_job_fixed "$rc_workflow" security-release-gates 'resolve-source'
require_job_fixed "$rc_workflow" security-release-gates 'publish-image: false'
require_job_fixed "$rc_workflow" security-release-gates 'run-operational: true'
require_job_fixed "$rc_workflow" security-release-gates 'source-ref: ${{ inputs.ref || github.ref }}'
require_job_fixed "$rc_workflow" security-release-gates 'source-commit: ${{ needs.resolve-source.outputs.commit }}'
require_job_fixed "$rc_workflow" package-validation 'security-release-gates'
require_fixed 'group: release-candidate-${{ inputs.ref || github.ref }}' "$rc_workflow"

require_job_fixed "$release_workflow" security-release-gates 'uses: ./.github/workflows/security-release-gates.yml'
require_job_fixed "$release_workflow" security-release-gates 'publish-image: true'
require_job_fixed "$release_workflow" security-release-gates 'run-operational: true'
require_job_fixed "$release_workflow" security-release-gates 'source-ref: ${{ github.ref }}'
require_job_fixed "$release_workflow" security-release-gates 'source-commit: ${{ github.sha }}'
for permission in 'contents: read' 'packages: write' 'id-token: write' 'attestations: write'; do
    require_job_fixed "$release_workflow" security-release-gates "$permission"
done
require_job_fixed "$release_workflow" release 'security-release-gates'
require_job_fixed "$release_workflow" release 'needs.security-release-gates.outputs.image-reference'
require_job_fixed "$release_workflow" release 'id-token: write'
require_job_fixed "$release_workflow" release 'attestations: write'
require_job_fixed "$release_workflow" release 'actions/download-artifact@v8.0.1'
for artifact in qualified-image etcd-recovery proxy-soak release-publication; do
    require_job_fixed "$release_workflow" release "${artifact}-\${{ github.run_id }}"
done
require_job_fixed "$release_workflow" release '$RUNNER_TEMP/container-image.txt'
require_job_fixed "$release_workflow" release '$RUNNER_TEMP/release-evidence.tar.gz'
require_job_fixed "$release_workflow" release '$RUNNER_TEMP/release-evidence.tar.gz.sha256'
require_job_fixed "$release_workflow" release 'MANIFEST.sha256'
require_job_fixed "$release_workflow" release 'sha256sum -c apisix-image.tar.gz.sha256'
require_job_fixed "$release_workflow" release '.source.commit == $commit'
require_job_fixed "$release_workflow" release '.image_digest == $digest'
require_job_fixed "$release_workflow" release 'publication-source-commit.txt'
require_job_fixed "$release_workflow" release 'qualification-context.json'
require_job_fixed "$release_workflow" release '"make test-plugin-harness"'
require_job_fixed "$release_workflow" release 'make test-plugin-smoke PLUGIN_SMOKE_CASE=key-auth/valid-consumer-schema'
require_job_fixed "$release_workflow" release 'make test-plugin-smoke PLUGIN_SMOKE_CASE=proxy-rewrite/rewrite-host'
require_job_fixed "$release_workflow" release 'make test-plugin-smoke PLUGIN_SMOKE_CASE=proxy-control/request-buffering-disabled'
require_job_fixed "$release_workflow" release 'make test-plugin-smoke PLUGIN_SMOKE_CASE=uri-blocker/one-rule-blocks-query'
require_job_fixed "$release_workflow" release 'GITHUB_RUN_ID'
require_job_fixed "$release_workflow" release 'RUNNER_OS'
require_job_fixed "$release_workflow" release 'ImageVersion'
require_job_fixed "$release_workflow" release 'go test -json ./pkg/route'
require_job_fixed "$release_workflow" release 'actions/attest-build-provenance@v4.2.2'
require_job_fixed "$release_workflow" release 'subject-path: ${{ runner.temp }}/release-evidence.tar.gz'
reject_job_pattern "$release_workflow" release '> container-image.txt'
reject_job_pattern "$release_workflow" release '--clobber'
require_job_fixed "$release_workflow" release 'GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}'
require_job_fixed "$release_workflow" release 'gh release upload "$GITHUB_REF_NAME"'

for job in lint build-and-unit integration-smoke release; do
    require_job_fixed "$release_workflow" "$job" 'ref: ${{ github.sha }}'
done

# Keep all newly introduced actions immutable and ensure each checkout records
# the selected ref's actual commit in its own job block.
for file in "$workflow" "$rc_workflow" "$release_workflow"; do
    if grep -Eq 'uses: [^#[:space:]]+@(main|master|latest)([[:space:]]|$)' "$file"; then
        printf 'floating action reference in %s\n' "$file" >&2
        exit 1
    fi
done
for file in "$workflow" "$rc_workflow" "$release_workflow"; do
    while IFS= read -r job; do
        [[ -n "$job" ]] || continue
        if ! job_block "$file" "$job" | grep -Fq 'actions/checkout@'; then
            continue
        fi
        require_job_fixed "$file" "$job" 'git rev-parse HEAD'
        require_job_fixed "$file" "$job" 'ref:'
    done < <(sed -n '/^jobs:$/,/^[^ ]/p' "$file" | awk '/^  [A-Za-z0-9_-]+:$/ { sub(/^  /, ""); sub(/:$/, ""); print }')
done

printf 'security release gate contract: PASS\n'
