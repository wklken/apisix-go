#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/security-release-gates.yml"
release_workflow="$repo_root/.github/workflows/release.yml"
design="$repo_root/docs/design.md"

require_pattern() {
    local pattern=$1
    local file=$2
    if ! grep -Eq "$pattern" "$file"; then
        printf 'missing %q in %s\n' "$pattern" "$file" >&2
        return 1
    fi
}

test -f "$workflow"
require_pattern 'actions/checkout@v7\.0\.1' "$workflow"
require_pattern 'actions/setup-go@v7\.0\.0' "$workflow"
require_pattern 'anchore/sbom-action@v0\.24\.0' "$workflow"
require_pattern 'aquasecurity/trivy-action@v0\.36\.0' "$workflow"
require_pattern 'actions/upload-artifact@v7\.0\.1' "$workflow"
require_pattern '^  workflow_call:$' "$workflow"
require_pattern 'govulncheck@v1\.7\.0' "$workflow"
require_pattern 'go test -race ./pkg/config ./cmd ./pkg/server ./pkg/route ./pkg/proxy ./pkg/store ./pkg/etcd' "$workflow"
require_pattern 'APISIX_SKIP_BUILD: "1"' "$workflow"
require_pattern 'bash scripts/container_smoke\.sh' "$workflow"
require_pattern 'bash scripts/release_metadata\.sh' "$workflow"
require_pattern 'rollback-metadata\.json' "$workflow"
require_pattern 'sbom\.cdx\.json' "$workflow"
require_pattern 'trivy\.json' "$workflow"
require_pattern 'ignore-unfixed: false' "$workflow"
require_pattern 'docker save.*APISIX_IMAGE' "$workflow"
require_pattern 'apisix-image\.tar\.gz' "$workflow"
if grep -Eq 'uses: [^#[:space:]]+@(main|master|latest)([[:space:]]|$)' "$workflow"; then
    printf 'floating action reference in %s\n' "$workflow" >&2
    exit 1
fi
require_pattern 'uses: \./\.github/workflows/security-release-gates\.yml' "$release_workflow"
require_pattern 'security-release-gates' "$release_workflow"

require_pattern 'default -> selected override -> APISIXGO_\*' "$design"
require_pattern 'rollback-metadata\.json' "$design"
require_pattern 'docker load' "$design"
require_pattern 'scripts/container_smoke\.sh' "$design"
require_pattern 'UID/GID 10001' "$design"

printf 'security release gate contract: PASS\n'
