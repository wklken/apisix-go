#!/usr/bin/env bash

set -euo pipefail

# A validation run must not inherit a developer or CI proxy. This applies
# to resolver, candidate, fixture, and oracle processes.
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY FTP_PROXY
unset http_proxy https_proxy all_proxy no_proxy ftp_proxy

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
container_bin=${CONTAINER_BIN:-${DOCKER_BIN:-podman}}
oracle_file=${APISIX_GO_ORACLE_FILE:-$repo_root/validation/oracle.yaml}
catalog_file=${APISIX_GO_DIFFERENTIAL_CATALOG:-$repo_root/validation/differential-cases.yaml}
candidate_bin=${APISIX_GO_CANDIDATE_BIN:-$repo_root/.cache/out/apisix}
artifact=${APISIX_GO_DIFFERENTIAL_ARTIFACT:-$repo_root/.cache/validation/differential/attempt-1.json}
if [[ "$artifact" != /* ]]; then
    artifact=$repo_root/$artifact
fi
resolver=$repo_root/scripts/validation/resolve_oracle.sh
go_cache_runner=${APISIX_GO_CACHE_RUNNER:-$repo_root/scripts/go_cache.sh}

selection_environment=()
if [[ -v APISIX_GO_DIFFERENTIAL_PLUGINS ]]; then
    selection_environment+=("APISIX_GO_DIFFERENTIAL_PLUGINS=$APISIX_GO_DIFFERENTIAL_PLUGINS")
fi
if [[ -v APISIX_GO_DIFFERENTIAL_CASES ]]; then
    selection_environment+=("APISIX_GO_DIFFERENTIAL_CASES=$APISIX_GO_DIFFERENTIAL_CASES")
fi
if [[ -v APISIX_GO_DIFFERENTIAL_SHARD_INDEX ]]; then
    selection_environment+=("APISIX_GO_DIFFERENTIAL_SHARD_INDEX=$APISIX_GO_DIFFERENTIAL_SHARD_INDEX")
fi
if [[ -v APISIX_GO_DIFFERENTIAL_SHARD_COUNT ]]; then
    selection_environment+=("APISIX_GO_DIFFERENTIAL_SHARD_COUNT=$APISIX_GO_DIFFERENTIAL_SHARD_COUNT")
fi

die() {
    printf 'plugin differential: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

validate_ipv4() {
    local value=$1
    local octet
    [[ "$value" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    IFS=. read -r -a octets <<<"$value"
    for octet in "${octets[@]}"; do
        (( 10#$octet <= 255 )) || return 1
    done
}

resolve_differential_host_gateway() {
    local configured=${APISIX_GO_DIFFERENTIAL_HOST_GATEWAY:-}
    local host_lookup timeout_bin
    local timeout_seconds=${APISIX_GO_CONTAINER_TIMEOUT_SECONDS:-5}
    if [[ -n "$configured" ]]; then
        validate_ipv4 "$configured" || die \
            "APISIX_GO_DIFFERENTIAL_HOST_GATEWAY must be an IPv4 address: $configured"
        printf '%s\n' "$configured"
        return
    fi
    if [[ "$(basename "$container_bin")" != *podman* ]]; then
        return
    fi
    [[ "$timeout_seconds" =~ ^[1-9][0-9]*$ ]] || die \
        "APISIX_GO_CONTAINER_TIMEOUT_SECONDS must be a positive integer: $timeout_seconds"
    timeout_bin=$(command -v gtimeout || command -v timeout || true)
    [[ -z "$timeout_bin" ]] && return
    host_lookup=$("$timeout_bin" "${timeout_seconds}s" \
        "$container_bin" machine ssh -- getent ahostsv4 host.containers.internal 2>/dev/null || true)
    configured=$(awk 'NR == 1 {print $1}' <<<"$host_lookup")
    [[ -z "$configured" ]] && return
    validate_ipv4 "$configured" || die "Podman machine returned an invalid default gateway: $configured"
    printf '%s\n' "$configured"
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

require_command git
require_command jq
require_command go
require_command "$container_bin"
[[ -f "$oracle_file" ]] || die "oracle file does not exist: $oracle_file"
[[ -f "$catalog_file" ]] || die "differential catalog does not exist: $catalog_file"
[[ -x "$resolver" ]] || die "oracle resolver is not executable: $resolver"
[[ -x "$go_cache_runner" ]] || die "Go cache runner is not executable: $go_cache_runner"

if [[ ! -x "$candidate_bin" ]]; then
    [[ "${APISIX_GO_SKIP_BUILD:-0}" == 1 ]] && die "candidate binary is missing: $candidate_bin"
    (
        cd "$repo_root"
        source .envrc
        make build
    )
    [[ -x "$candidate_bin" ]] || die "candidate build did not produce an executable: $candidate_bin"
fi

candidate_source_commit=${APISIX_GO_CANDIDATE_SOURCE_COMMIT:-}
if [[ -z "$candidate_source_commit" ]]; then
    candidate_source_commit=$(git -C "$repo_root" rev-parse HEAD) || die 'resolve candidate source commit'
fi
[[ "$candidate_source_commit" =~ ^[0-9a-f]{40}$ ]] || die \
    "candidate source commit must be a 40-character commit: $candidate_source_commit"
candidate_binary_sha256=$(sha256_file "$candidate_bin") || die 'hash candidate binary'
catalog_sha256=sha256:$(sha256_file "$catalog_file") || die 'hash differential catalog'
configured_candidate_sha256=${APISIX_GO_CANDIDATE_BINARY_SHA256:-$candidate_binary_sha256}
[[ "$configured_candidate_sha256" =~ ^[0-9a-f]{64}$ ]] || die \
    "candidate binary sha256 must be 64 lowercase hexadecimal characters: $configured_candidate_sha256"
[[ "$configured_candidate_sha256" == "$candidate_binary_sha256" ]] || die \
    "candidate binary sha256 mismatch: expected $configured_candidate_sha256, got $candidate_binary_sha256"

mkdir -p "$(dirname "$artifact")"
[[ ! -e "$artifact" ]] || die "differential artifact already exists; use a new attempt path: $artifact"

preflight_status=0
preflight_output=$(
    cd "$repo_root"
    source .envrc
    preflight_environment=(
        "GOFLAGS=-mod=readonly"
        "APISIX_GO_DIFFERENTIAL_PREFLIGHT=1"
        "APISIX_GO_REPO_ROOT=$repo_root"
        "APISIX_GO_DIFFERENTIAL_CATALOG=$catalog_file"
    )
    preflight_environment+=("${selection_environment[@]}")
    env "${preflight_environment[@]}" \
        "$go_cache_runner" run -- go test ./t/plugin -run '^TestDifferentialSelectionPreflight$' -count=1 -v
) || preflight_status=$?
(( preflight_status == 0 )) || die "differential selection preflight failed with status $preflight_status"
selection_marker_count=$(printf '%s\n' "$preflight_output" | \
    awk '/^DIFFERENTIAL_SELECTION_JSON=/{count++} END{print count+0}')
[[ "$selection_marker_count" == 1 ]] || die \
    "differential selection preflight emitted $selection_marker_count markers; expected exactly one"
expected_selection_json=$(printf '%s\n' "$preflight_output" | \
    sed -n 's/^DIFFERENTIAL_SELECTION_JSON=//p')
jq -e '
    type == "object" and
    (.plugins | type == "array") and all(.plugins[]; type == "string" and length > 0) and
    (.cases | type == "array") and all(.cases[]; type == "string" and length > 0) and
    (.shard_index | type == "number") and
    (.shard_count | type == "number") and
    (.selected_case_count | type == "number" and . > 0) and
    (.full_catalog_run | type == "boolean")
' <<<"$expected_selection_json" >/dev/null || die \
    'differential selection preflight emitted invalid JSON'
expected_plugins_json=$(jq -c '.plugins' <<<"$expected_selection_json")
expected_cases_json=$(jq -c '.cases' <<<"$expected_selection_json")
expected_shard_index_json=$(jq -c '.shard_index' <<<"$expected_selection_json")
expected_shard_count_json=$(jq -c '.shard_count' <<<"$expected_selection_json")
expected_selected_case_count_json=$(jq -c '.selected_case_count' <<<"$expected_selection_json")
expected_full_run_json=$(jq -c '.full_catalog_run' <<<"$expected_selection_json")

differential_host_gateway=$(resolve_differential_host_gateway)

CONTAINER_BIN="$container_bin" "$resolver" "$oracle_file"

run_status=0
(
    cd "$repo_root"
    source .envrc
    runner_environment=(
        "GOFLAGS=-mod=readonly"
        "APISIX_GO_RUN_DIFFERENTIAL=1"
        "APISIX_GO_REPO_ROOT=$repo_root"
        "APISIX_GO_ORACLE_FILE=$oracle_file"
        "APISIX_GO_DIFFERENTIAL_CATALOG=$catalog_file"
        "APISIX_GO_NORMALIZATION_FILE=$repo_root/t/plugin/testdata/normalization.yaml"
        "APISIX_GO_CANDIDATE_BIN=$candidate_bin"
        "APISIX_GO_CANDIDATE_BINARY_SHA256=$candidate_binary_sha256"
        "APISIX_GO_CONTAINER_BIN=$container_bin"
        "APISIX_GO_CANDIDATE_SOURCE_COMMIT=$candidate_source_commit"
        "APISIX_GO_DIFFERENTIAL_ARTIFACT=$artifact"
    )
    if [[ -n "$differential_host_gateway" ]]; then
        runner_environment+=("APISIX_GO_DIFFERENTIAL_HOST_GATEWAY=$differential_host_gateway")
    fi
    runner_environment+=("${selection_environment[@]}")
    env "${runner_environment[@]}" \
        "$go_cache_runner" run -- go test ./t/plugin -run '^TestPluginDifferential$' -count=1 -v
) || run_status=$?

[[ -s "$artifact" ]] || die "differential test did not write first-attempt artifact: $artifact"
jq -e \
    --arg candidate "$candidate_source_commit" \
    --arg candidate_sha "$candidate_binary_sha256" \
    --arg catalog_sha "$catalog_sha256" \
    --arg source "$(awk -F ': ' '$1 == "source_commit" {print $2}' "$oracle_file")" \
    --arg digest "$(awk -F ': ' '$1 == "image_linux_amd64_digest" {print $2}' "$oracle_file")" \
    --argjson expected_plugins "$expected_plugins_json" \
    --argjson expected_cases "$expected_cases_json" \
    --argjson expected_shard_index "$expected_shard_index_json" \
    --argjson expected_shard_count "$expected_shard_count_json" \
    --argjson expected_selected_case_count "$expected_selected_case_count_json" \
    --argjson expected_full_run "$expected_full_run_json" \
    '
        . as $artifact |
        .schema_version == 3 and
        .suite == "apisix-3.17-plugin-differential-v1" and
        .catalog_sha256 == $catalog_sha and
        .attempt == 1 and
        .first_attempt == true and
        .candidate.source_commit == $candidate and
        .candidate.binary_sha256 == $candidate_sha and
        .oracle.source_commit == $source and
        .oracle.image_linux_amd64_digest == $digest and
        (.selection | type == "object") and
        (.selection.plugins == $expected_plugins) and
        (.selection.cases == $expected_cases) and
        (.selection.shard_index == $expected_shard_index) and
        (.selection.shard_count == $expected_shard_count) and
        (.selection.selected_case_count == $expected_selected_case_count) and
        (.selection.full_catalog_run == $expected_full_run) and
        (.selection.selected_case_count == (.cases | length)) and
        (.selection.selected_case_count > 0) and
        (.cases | length) > 0 and
        (all(.cases[]; (.first_attempt == true and (.name | type == "string" and length > 0) and (.plugin | type == "string" and length > 0) and (.obligation | type == "string" and length > 0) and (.passed | type == "boolean") and ((.passed == true) or (.error | type == "string" and length > 0))))) and
        ([.cases[] | select(.passed == true)] | length) == .passed and
        ([.cases[] | select(.passed == false)] | length) == .failed and
        (.passed + .failed == (.cases | length)) and
        .coverage.required_count == 111 and
        .coverage.covered_count > 0 and
        .coverage.covered_count <= .coverage.required_count and
        (.coverage.required_plugins | length) == 111 and
        (.coverage.required_plugins | unique | length) == 111 and
        (.coverage.covered_plugins | length) == .coverage.covered_count and
        (.coverage.covered_plugins | unique | length) == .coverage.covered_count and
        ([.coverage.covered_plugins[] as $plugin | .coverage.required_plugins | index($plugin)] | all(. != null)) and
        (.plugins | length) == .coverage.covered_count and
        ([.plugins[].obligations] | add) == (.cases | length) and
        (all(.plugins[]; (.obligations == (.obligation_names | length) and .obligations == (.case_names | length) and .obligations == (.passed + .failed) and (.first_attempt | type == "boolean") and (.result == (if .failed == 0 and .first_attempt == true then "pass" else "fail" end))))) and
        ([.plugins[].passed] | add) == .passed and
        ([.plugins[].failed] | add) == .failed and
        ([.plugins[].plugin] == .coverage.covered_plugins) and
        (.result == (if .failed == 0 and .first_attempt == true then "pass" else "fail" end)) and
        (
            ($artifact.cases | sort_by(.plugin) | group_by(.plugin) | map(
                . as $rows |
                ($rows | map(select(.passed == true)) | length) as $passed |
                ($rows | map(select(.passed == false)) | length) as $failed |
                {
                    plugin: $rows[0].plugin,
                    obligation_names: ($rows | map(.obligation) | sort),
                    case_names: ($rows | map(.name) | sort),
                    obligations: ($rows | length),
                    first_attempt: all($rows[]; .first_attempt == true),
                    passed: $passed,
                    failed: $failed,
                    result: (if $failed == 0 and all($rows[]; .first_attempt == true) then "pass" else "fail" end)
                }
            )) as $expected_plugins |
            ($artifact.plugins | map({
                plugin,
                obligation_names: (.obligation_names | sort),
                case_names: (.case_names | sort),
                obligations,
                first_attempt,
                passed,
                failed,
                result
            })) == $expected_plugins
        )
    ' "$artifact" >/dev/null || die "artifact identity or first-attempt contract is invalid: $artifact"

cat "$artifact"
if (( run_status != 0 )); then
    exit "$run_status"
fi
[[ "$(jq -r .result "$artifact")" == pass ]] || die "differential result is not pass"
printf 'plugin differential smoke: PASS (%s/111 plugins, %s)\n' "$(jq -r .coverage.covered_count "$artifact")" "$artifact"
