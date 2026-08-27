#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
resolver="$repo_root/scripts/qualification/resolve_oracle.sh"
task_dir=$(mktemp -d)
trap 'rm -rf "$task_dir"' EXIT

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
        return
    fi
    shasum -a 256 "$1" | awk '{print $1}'
}

index_json="$task_dir/index.json"
cat >"$index_json" <<'JSON'
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","platform":{"architecture":"amd64","os":"linux"}}]}
JSON
index_digest="sha256:$(sha256_file "$index_json")"

fake_docker="$task_dir/docker"
cat >"$fake_docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "buildx imagetools inspect --raw apache/apisix:3.17.0-debian" ]]; then
    cat "$FAKE_INDEX_JSON"
    exit 0
fi
if [[ "$*" == "run --rm --platform linux/amd64 --entrypoint apisix docker.io/apache/apisix@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb version" ]]; then
    printf 'Apache APISIX version %s\n' "${FAKE_APISIX_VERSION:-3.17.0}"
    exit 0
fi
printf 'unexpected docker arguments: %s\n' "$*" >&2
exit 2
SH
chmod +x "$fake_docker"

oracle="$task_dir/oracle.yaml"
cat >"$oracle" <<EOF
schema_version: 1
image_tag: apache/apisix:3.17.0-debian
image_repository: docker.io/apache/apisix
image_index_digest: $index_digest
image_linux_amd64_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
source_repository: https://github.com/apache/apisix
source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
expected_version: 3.17.0
EOF

output=$(FAKE_INDEX_JSON="$index_json" DOCKER_BIN="$fake_docker" "$resolver" "$oracle")
grep -Fq "index_digest=$index_digest" <<<"$output"
grep -Fq 'platform_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' <<<"$output"
grep -Fq 'version=3.17.0' <<<"$output"

if FAKE_INDEX_JSON="$index_json" FAKE_APISIX_VERSION=3.16.0 DOCKER_BIN="$fake_docker" \
    "$resolver" "$oracle" >"$task_dir/wrong-version.out" 2>&1; then
    printf 'resolver accepted the wrong runtime version\n' >&2
    exit 1
fi
grep -Fq 'runtime version' "$task_dir/wrong-version.out"

sed "s/^image_index_digest:.*/image_index_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/" \
    "$oracle" >"$task_dir/wrong-digest.yaml"
if FAKE_INDEX_JSON="$index_json" DOCKER_BIN="$fake_docker" \
    "$resolver" "$task_dir/wrong-digest.yaml" >"$task_dir/wrong-digest.out" 2>&1; then
    printf 'resolver accepted the wrong index digest\n' >&2
    exit 1
fi
grep -Fq 'index digest' "$task_dir/wrong-digest.out"

printf 'resolve_oracle tests passed\n'
