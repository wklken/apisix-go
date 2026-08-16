#!/usr/bin/env bash
set -euo pipefail

if (( $# < 3 )); then
    printf 'usage: %s IMAGE_REFERENCE IMAGE_DIGEST OUTPUT [ARTIFACT ...]\n' "$0" >&2
    exit 2
fi

image_reference=$1
image_digest=$2
output=$3
shift 3

if ! command -v jq >/dev/null 2>&1; then
    printf 'required command is unavailable: jq\n' >&2
    exit 1
fi

if [[ -z "$image_reference" ]]; then
    printf 'image reference must not be empty\n' >&2
    exit 1
fi
if [[ ! "$image_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    printf 'image digest must be a lowercase sha256 digest: %s\n' "$image_digest" >&2
    exit 1
fi
if [[ ${PUBLISH_IMAGE:-false} == true ]]; then
    if [[ ! "$image_reference" =~ @sha256:[0-9a-f]{64}$ ]]; then
        printf 'published image reference must be digest-qualified: %s\n' "$image_reference" >&2
        exit 1
    fi
    reference_digest=${image_reference##*@}
    reference_name=${image_reference%@*}
    reference_last_component=${reference_name##*/}
    if [[ -z "$reference_name" || "$reference_name" == *"@"* || -z "$reference_last_component" || "$reference_last_component" == *:* ]]; then
        printf 'published image reference must not contain a tag: %s\n' "$image_reference" >&2
        exit 1
    fi
    if [[ "$reference_digest" != "$image_digest" ]]; then
        printf 'published image reference digest does not match image digest\n' >&2
        exit 1
    fi
fi

source_ref=${SOURCE_REF:-${GITHUB_REF:-}}
source_commit=${SOURCE_COMMIT:-${GITHUB_SHA:-}}
if [[ -z "$source_ref" ]]; then
    source_ref=$(git symbolic-ref -q HEAD || git describe --tags --always)
fi
if [[ -z "$source_commit" ]]; then
    source_commit=$(git rev-parse HEAD)
fi
sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        printf 'no SHA256 utility is available\n' >&2
        return 1
    fi
}

artifact_lines=$(mktemp "${TMPDIR:-/tmp}/apisix-release-artifacts.XXXXXX")
trap 'rm -f "$artifact_lines"' EXIT
for artifact in "$@"; do
    if [[ ! -f "$artifact" ]]; then
        printf 'release artifact is not a file: %s\n' "$artifact" >&2
        exit 1
    fi
    if ! artifact_digest=$(sha256_file "$artifact"); then
        printf 'failed to hash release artifact: %s\n' "$artifact" >&2
        exit 1
    fi
    if [[ ! "$artifact_digest" =~ ^[0-9a-f]{64}$ ]]; then
        printf 'release artifact hash is not a lowercase SHA256: %s\n' "$artifact" >&2
        exit 1
    fi
    printf '%s\t%s\n' "$artifact_digest" "$(basename "$artifact")" >>"$artifact_lines"
done

mkdir -p "$(dirname "$output")"
jq --null-input \
    --arg source_ref "$source_ref" \
    --arg source_commit "$source_commit" \
    --arg image_reference "$image_reference" \
    --arg image_digest "$image_digest" \
    --rawfile artifacts "$artifact_lines" '
    {
      schema_version: 2,
      source: {ref: $source_ref, commit: $source_commit},
      image_reference: $image_reference,
      image_digest: $image_digest,
      artifacts: (
        $artifacts
        | split("\n")
        | map(select(length > 0) | split("\t") | {sha256: .[0], name: .[1]})
        | sort_by(.name)
      )
    }
' >"$output"

printf 'release metadata written to %s\n' "$output"
