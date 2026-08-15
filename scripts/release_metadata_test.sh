#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/apisix-release-metadata-test.XXXXXX")
trap 'rm -rf "$temp_dir"' EXIT

mkdir -p "$temp_dir/bin"
cat >"$temp_dir/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == image && $2 == inspect ]]; then
    printf 'sha256:image-id\n'
    exit 0
fi
exit 2
SH
chmod +x "$temp_dir/bin/docker"
printf 'sbom-content' >"$temp_dir/sbom.cdx.json"
printf 'scan-content' >"$temp_dir/trivy.json"

PATH="$temp_dir/bin:$PATH" \
SOURCE_REF=refs/tags/v1.2.3 \
SOURCE_COMMIT=0123456789abcdef \
    bash "$repo_root/scripts/release_metadata.sh" \
        apisix-go:v1.2.3 "$temp_dir/rollback-metadata.json" \
        "$temp_dir/sbom.cdx.json" "$temp_dir/trivy.json"

jq -e '
    .schema_version == 1 and
    .source.ref == "refs/tags/v1.2.3" and
    .source.commit == "0123456789abcdef" and
    .image.ref == "apisix-go:v1.2.3" and
    .image.id == "sha256:image-id" and
    (.artifacts | length) == 2 and
    ([.artifacts[].name] | sort) == ["sbom.cdx.json", "trivy.json"] and
    (all(.artifacts[]; (.sha256 | length) == 64))
' "$temp_dir/rollback-metadata.json" >/dev/null

printf 'release metadata: PASS\n'
