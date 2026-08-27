#!/usr/bin/env bash

set -euo pipefail

die() {
    printf '%s\n' "$*" >&2
    exit 1
}

read_scalar() {
    local key=$1
    local file=$2
    awk -F ': ' -v key="$key" '
        $1 == key {
            sub(/^[^:]+:[[:space:]]*/, "")
            print
            found = 1
        }
        END { if (!found) exit 1 }
    ' "$file"
}

sha256_file() {
    local file=$1
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
        return
    fi
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
        return
    fi
    die 'sha256sum or shasum is required'
}

oracle=${1:-qualification/oracle.yaml}
[[ -f "$oracle" ]] || die "oracle file does not exist: $oracle"

schema_version=$(read_scalar schema_version "$oracle") || die 'schema_version is required'
image_tag=$(read_scalar image_tag "$oracle") || die 'image_tag is required'
image_repository=$(read_scalar image_repository "$oracle") || die 'image_repository is required'
image_index_digest=$(read_scalar image_index_digest "$oracle") || die 'image_index_digest is required'
image_linux_amd64_digest=$(read_scalar image_linux_amd64_digest "$oracle") || die 'image_linux_amd64_digest is required'
source_repository=$(read_scalar source_repository "$oracle") || die 'source_repository is required'
source_commit=$(read_scalar source_commit "$oracle") || die 'source_commit is required'
expected_version=$(read_scalar expected_version "$oracle") || die 'expected_version is required'

[[ "$schema_version" == 1 ]] || die "unsupported oracle schema_version: $schema_version"
[[ "$image_tag" == apache/apisix:3.17.0-debian ]] || die "unexpected oracle image tag: $image_tag"
[[ "$image_repository" == docker.io/apache/apisix ]] || die "unexpected oracle image repository: $image_repository"
[[ "$source_repository" == https://github.com/apache/apisix ]] || die "unexpected oracle source repository: $source_repository"
[[ "$source_commit" == 9ef2ecab67f652d38365049613610ef649bb4ad0 ]] || die "unexpected oracle source commit: $source_commit"
[[ "$expected_version" == 3.17.0 ]] || die "unexpected oracle runtime version: $expected_version"
[[ "$image_index_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "invalid image index digest: $image_index_digest"
[[ "$image_linux_amd64_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die \
    "invalid linux/amd64 image digest: $image_linux_amd64_digest"

docker_bin=${DOCKER_BIN:-docker}
command -v "$docker_bin" >/dev/null 2>&1 || die "Docker CLI is required: $docker_bin"
command -v jq >/dev/null 2>&1 || die 'jq is required'

task_dir=$(mktemp -d)
trap 'rm -rf "$task_dir"' EXIT
index_file="$task_dir/index.json"
"$docker_bin" buildx imagetools inspect --raw "$image_tag" >"$index_file" || \
    die "cannot resolve oracle image index: $image_tag"

resolved_index_digest="sha256:$(sha256_file "$index_file")"
[[ "$resolved_index_digest" == "$image_index_digest" ]] || die \
    "oracle image index digest mismatch: resolved $resolved_index_digest, locked $image_index_digest"

resolved_platform_digest=$(jq -er '
    [.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")] |
    if length == 1 then .[0].digest else error("want exactly one linux/amd64 manifest") end
' "$index_file") || die 'cannot resolve exactly one linux/amd64 manifest from oracle index'
[[ "$resolved_platform_digest" == "$image_linux_amd64_digest" ]] || die \
    "oracle linux/amd64 digest mismatch: resolved $resolved_platform_digest, locked $image_linux_amd64_digest"

image_reference="$image_repository@$image_linux_amd64_digest"
if ! version_output=$("$docker_bin" run --rm --platform linux/amd64 --entrypoint apisix \
    "$image_reference" version 2>&1); then
    die "cannot run oracle image $image_reference: $version_output"
fi
runtime_version=$(grep -Eo '([0-9]+\.){2}[0-9]+' <<<"$version_output" | head -1 || true)
[[ "$runtime_version" == "$expected_version" ]] || die \
    "oracle runtime version mismatch: resolved ${runtime_version:-unknown}, expected $expected_version"

printf 'verified image=%s index_digest=%s platform_digest=%s version=%s source_commit=%s\n' \
    "$image_tag" \
    "$resolved_index_digest" \
    "$resolved_platform_digest" \
    "$runtime_version" \
    "$source_commit"
