#!/usr/bin/env bash
set -euo pipefail

if (( $# < 2 )); then
    printf 'usage: %s IMAGE_REF OUTPUT [ARTIFACT ...]\n' "$0" >&2
    exit 2
fi

image_ref=$1
output=$2
shift 2

for command_name in docker jq; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        printf 'required command is unavailable: %s\n' "$command_name" >&2
        exit 1
    fi
done

source_ref=${SOURCE_REF:-${GITHUB_REF:-}}
source_commit=${SOURCE_COMMIT:-${GITHUB_SHA:-}}
if [[ -z "$source_ref" ]]; then
    source_ref=$(git symbolic-ref -q HEAD || git describe --tags --always)
fi
if [[ -z "$source_commit" ]]; then
    source_commit=$(git rev-parse HEAD)
fi
image_id=$(docker image inspect --format '{{.Id}}' "$image_ref")

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

artifact_lines=$(mktemp "${TMPDIR:-/tmp}/apisix-release-artifacts.XXXXXX")
trap 'rm -f "$artifact_lines"' EXIT
for artifact in "$@"; do
    if [[ ! -f "$artifact" ]]; then
        printf 'release artifact is not a file: %s\n' "$artifact" >&2
        exit 1
    fi
    printf '%s\t%s\n' "$(sha256_file "$artifact")" "$(basename "$artifact")" >>"$artifact_lines"
done

mkdir -p "$(dirname "$output")"
jq --null-input \
    --arg source_ref "$source_ref" \
    --arg source_commit "$source_commit" \
    --arg image_ref "$image_ref" \
    --arg image_id "$image_id" \
    --rawfile artifacts "$artifact_lines" '
    {
      schema_version: 1,
      source: {ref: $source_ref, commit: $source_commit},
      image: {ref: $image_ref, id: $image_id},
      artifacts: (
        $artifacts
        | split("\n")
        | map(select(length > 0) | split("\t") | {sha256: .[0], name: .[1]})
        | sort_by(.name)
      )
    }
' >"$output"

printf 'release metadata written to %s\n' "$output"
