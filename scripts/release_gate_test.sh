#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
makefile="$repo_root/Makefile"
dockerfile="$repo_root/Dockerfile"
workflow="$repo_root/.github/workflows/security-release-gates.yml"
unit_workflow="$repo_root/.github/workflows/unit-test.yml"
candidate_workflow="$repo_root/.github/workflows/release-candidate.yml"

require_fixed() {
    local text=$1
    local file=$2
    if ! grep -Fq -- "$text" "$file"; then
        printf 'missing %q in %s\n' "$text" "$file" >&2
        return 1
    fi
}

reject_pattern() {
    local pattern=$1
    local file=$2
    if grep -Eq -- "$pattern" "$file"; then
        printf 'unexpected %q in %s\n' "$pattern" "$file" >&2
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

test -f "$workflow"
test -f "$candidate_workflow"
test -f "$unit_workflow"
test -f "$makefile"
test -f "$dockerfile"

for file in "$unit_workflow" "$candidate_workflow" "$makefile"; do
    reject_pattern 'COVERAGE_MIN|make test-cover|check-unit-coverage' "$file"
done

for file in "$workflow" "$candidate_workflow"; do
    reject_pattern 'publish-image|run-upgrade-rollback|rollback-release-tag|upgrade-rollback|production-release|cosign|docker push|attest-build-provenance|packages: write|attestations: write' "$file"
done

# Ordinary CI continues to enforce the candidate contract.
require_job_fixed "$unit_workflow" build-and-unit 'run: bash scripts/release_gate_test.sh'
require_job_fixed "$unit_workflow" build-and-unit 'run: make test'
require_job_fixed "$unit_workflow" build-and-unit 'run: make test-plugin-harness'
require_job_fixed "$unit_workflow" build-and-unit 'run: make test-integration'

# Local, container, and candidate builds must retain immutable build metadata.
require_fixed 'GO_CACHE_RUNNER ?= bash scripts/go_cache.sh run --' "$makefile"
require_fixed '# syntax=docker/dockerfile:1.7' "$dockerfile"
require_fixed 'ARG COMMIT=none' "$dockerfile"
require_fixed 'github.com/wklken/apisix-go/pkg/version.Commit=${COMMIT}' "$dockerfile"

header=$(sed -n '1,/^jobs:$/p' "$workflow")
require_fixed 'workflow_call:' <(printf '%s\n' "$header")
require_fixed 'run-operational:' <(printf '%s\n' "$header")
require_fixed 'source-ref:' <(printf '%s\n' "$header")
require_fixed 'source-commit:' <(printf '%s\n' "$header")
require_fixed 'workflow_dispatch:' <(printf '%s\n' "$header")
reject_pattern '^  (pull_request|push):' <(printf '%s\n' "$header")

# Exactly these jobs own reusable candidate evidence. A separate packaging or
# publication job would be a scope regression.
actual_jobs=$(sed -n '/^jobs:$/,$p' "$workflow" | awk '/^  [A-Za-z0-9_-]+:$/ { sub(/^  /, ""); sub(/:$/, ""); print }')
expected_jobs=$'validate-inputs\nrace-and-vulnerability\ncontainer-evidence\nproxy-soak'
if [[ "$actual_jobs" != "$expected_jobs" ]]; then
    printf 'candidate gate jobs differ\nwant:\n%s\ngot:\n%s\n' "$expected_jobs" "$actual_jobs" >&2
    exit 1
fi

require_job_fixed "$workflow" validate-inputs 'SOURCE_COMMIT: ${{ inputs.source-commit || github.sha }}'
require_job_fixed "$workflow" validate-inputs 'source commit must be an immutable 40-hex SHA'
require_job_fixed "$workflow" race-and-vulnerability 'go test -race ./pkg/config ./cmd ./pkg/server ./pkg/route ./pkg/proxy ./pkg/etcd ./pkg/stream -count=1'
require_job_fixed "$workflow" race-and-vulnerability 'govulncheck@v1.7.0 . ./cmd/... ./pkg/...'

require_job_fixed "$workflow" container-evidence 'EXPECTED_SOURCE_COMMIT: ${{ inputs.source-commit || github.sha }}'
require_job_fixed "$workflow" container-evidence '[[ "$commit" == "$EXPECTED_SOURCE_COMMIT" ]]'
require_job_fixed "$workflow" container-evidence 'docker/build-push-action@v7.3.0'
require_job_fixed "$workflow" container-evidence 'platforms: linux/amd64'
require_job_fixed "$workflow" container-evidence 'push: false'
require_job_fixed "$workflow" container-evidence 'bash scripts/container_smoke.sh'
require_job_fixed "$workflow" container-evidence 'anchore/sbom-action@v0.24.0'
require_job_fixed "$workflow" container-evidence 'aquasecurity/trivy-action@v0.36.0'
require_job_fixed "$workflow" container-evidence 'docker save "$APISIX_IMAGE" | gzip --best'
require_job_fixed "$workflow" container-evidence 'candidate-image-${{ github.run_id }}'
reject_job_pattern "$workflow" container-evidence 'scripts/release_metadata.sh'

for job in proxy-soak; do
    require_job_fixed "$workflow" "$job" 'if: ${{ inputs.run-operational }}'
    require_job_fixed "$workflow" "$job" 'race-and-vulnerability'
    require_job_fixed "$workflow" "$job" 'container-evidence'
done
require_job_fixed "$workflow" proxy-soak 'APISIX_GO_SOAK_DURATION=30m'
require_job_fixed "$workflow" proxy-soak "go test -json ./pkg/route -run '^TestProxyRuntimeSoak$' -count=1 -timeout=40m"

# The candidate workflow binds every gate to one resolved commit and includes
# functional smoke and soak evidence.
require_job_fixed "$candidate_workflow" resolve-source 'commit=$(git rev-parse HEAD)'
for job in lint build-and-unit integration-smoke integration-full; do
    require_job_fixed "$candidate_workflow" "$job" 'resolve-source'
    require_job_fixed "$candidate_workflow" "$job" 'ref: ${{ needs.resolve-source.outputs.commit }}'
done
require_job_fixed "$candidate_workflow" build-and-unit 'run: make test'
require_job_fixed "$candidate_workflow" integration-full 'run: make test-integration'
require_job_fixed "$candidate_workflow" security-release-gates 'integration-full'
require_job_fixed "$candidate_workflow" security-release-gates 'uses: ./.github/workflows/security-release-gates.yml'
require_job_fixed "$candidate_workflow" security-release-gates 'run-operational: true'
require_job_fixed "$candidate_workflow" security-release-gates 'source-commit: ${{ needs.resolve-source.outputs.commit }}'

# Actions stay pinned to versioned refs, and every checkout verifies the source
# selected by its caller.
for file in "$workflow" "$candidate_workflow"; do
    reject_pattern 'uses: [^#[:space:]]+@(main|master|latest)([[:space:]]|$)' "$file"
    while IFS= read -r job; do
        [[ -n "$job" ]] || continue
        if ! job_block "$file" "$job" | grep -Fq 'actions/checkout@'; then
            continue
        fi
        require_job_fixed "$file" "$job" 'git rev-parse HEAD'
        require_job_fixed "$file" "$job" 'ref:'
    done < <(sed -n '/^jobs:$/,$p' "$file" | awk '/^  [A-Za-z0-9_-]+:$/ { sub(/^  /, ""); sub(/:$/, ""); print }')
done

printf 'HTTP candidate gate contract: PASS\n'
