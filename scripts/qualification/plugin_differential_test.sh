#!/usr/bin/env bash

set -euo pipefail

# Keep the fake resolver/runner contract identical to a real qualification
# invocation: no developer or CI proxy may affect image or fixture traffic.
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY FTP_PROXY
unset http_proxy https_proxy all_proxy no_proxy ftp_proxy

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
script=$repo_root/scripts/qualification/plugin_differential.sh
test_root=$(mktemp -d "${TMPDIR:-/tmp}/plugin-differential-test.XXXXXX")
relative_artifact_abs=
cleanup() {
    rm -rf "$test_root"
    if [[ -n "$relative_artifact_abs" ]]; then
        rm -f "$relative_artifact_abs"
    fi
}
trap cleanup EXIT

fail() {
    printf 'plugin differential test: %s\n' "$*" >&2
    exit 1
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

index_json=$test_root/index.json
cat >"$index_json" <<'JSON'
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:5a8d7dfd8382aebfc0cab7bf9d24edf8dd73a6f0eed0b789d25578a373e86f64","platform":{"architecture":"amd64","os":"linux"}}]}
JSON
index_digest=sha256:$(sha256_file "$index_json")

fake_container=$test_root/podman
cat >"$fake_container" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n ${FAKE_CONTAINER_LOG:-} ]]; then
    printf '%s\n' "$*" >>"$FAKE_CONTAINER_LOG"
fi
if [[ ${FAKE_CONTAINER_HANG:-0} == 1 ]]; then
    while :; do
        sleep 1
    done
fi
if [[ ${1:-} == manifest && ${2:-} == inspect ]]; then
    cat "$FAKE_INDEX_JSON"
    exit 0
fi
if [[ ${1:-} == machine && ${2:-} == ssh && ${3:-} == -- && ${4:-} == getent && ${5:-} == ahostsv4 && ${6:-} == host.containers.internal && -n ${FAKE_MACHINE_HOST_GATEWAY:-} ]]; then
    printf '%s STREAM host.containers.internal\n' "$FAKE_MACHINE_HOST_GATEWAY"
    exit 0
fi
if [[ ${1:-} == run && ${2:-} == --rm && ${3:-} == --platform && ${4:-} == linux/amd64 && ${5:-} == --entrypoint && ${6:-} == apisix ]]; then
    printf 'Apache APISIX version 3.17.0\n'
    exit 0
fi
printf 'unexpected fake container command: %s\n' "$*" >&2
exit 2
SH
chmod +x "$fake_container"

fake_runner=$test_root/go-cache-runner
cat >"$fake_runner" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${APISIX_GO_DIFFERENTIAL_PREFLIGHT:-0} == 1 ]]; then
    printf '%s\n' "$*" >"$FAKE_PREFLIGHT_ARGS"
    env | sort >"$FAKE_PREFLIGHT_ENV"
    if [[ ${FAKE_PREFLIGHT_STATUS:-0} != 0 ]]; then
        printf 'deliberate selection preflight failure\n' >&2
        exit "$FAKE_PREFLIGHT_STATUS"
    fi
    printf 'DIFFERENTIAL_SELECTION_JSON=%s\n' "$FAKE_SELECTION_JSON"
    exit 0
fi
printf '%s\n' "$*" >"$FAKE_RUNNER_ARGS"
env | sort >"$FAKE_RUNNER_ENV"
cat >"$APISIX_GO_DIFFERENTIAL_ARTIFACT" <<JSON
{
  "schema_version": 2,
  "profile": "apisix-3.17-differential-smoke-v1",
  "target_qualification_profile": "apisix-3.17-all-plugins-v1",
  "catalog_sha256": "$FAKE_CATALOG_SHA",
  "attempt": 1,
  "first_attempt": true,
  "candidate": {"source_commit": "$APISIX_GO_CANDIDATE_SOURCE_COMMIT", "binary_sha256": "$APISIX_GO_CANDIDATE_BINARY_SHA256", "security_profile": "compat"},
  "oracle": {
    "schema_version": 1,
    "image_tag": "apache/apisix:3.17.0-debian",
    "image_repository": "docker.io/apache/apisix",
    "image_index_digest": "$FAKE_INDEX_DIGEST",
    "image_linux_amd64_digest": "sha256:5a8d7dfd8382aebfc0cab7bf9d24edf8dd73a6f0eed0b789d25578a373e86f64",
    "source_repository": "https://github.com/apache/apisix",
    "source_commit": "9ef2ecab67f652d38365049613610ef649bb4ad0",
    "expected_version": "3.17.0"
  },
  "selection": {
    "plugins": ["cors", "key-auth", "proxy-rewrite", "request-validation", "response-rewrite"],
    "cases": ["five", "four", "one", "six", "three", "two"],
    "shard_index": 0,
    "shard_count": 1,
    "selected_case_count": 6,
    "full_catalog_run": false
  },
  "cases": [
    {"name":"one","plugin":"proxy-rewrite","obligation":"rewrite-uri-and-host","first_attempt":true,"passed":true},
    {"name":"two","plugin":"cors","obligation":"default-response-headers","first_attempt":true,"passed":true},
    {"name":"three","plugin":"key-auth","obligation":"allow-valid-api-key","first_attempt":true,"passed":true},
    {"name":"four","plugin":"key-auth","obligation":"deny-missing-api-key","first_attempt":true,"passed":true},
    {"name":"five","plugin":"request-validation","obligation":"reject-missing-required-header","first_attempt":true,"passed":true},
    {"name":"six","plugin":"response-rewrite","obligation":"rewrite-body-and-headers","first_attempt":true,"passed":true}
  ],
  "coverage": {
    "required_plugins": ["cors","key-auth","proxy-rewrite","request-validation",$(printf '"required-%03d",' {1..106})"response-rewrite"],
    "covered_plugins": ["cors","key-auth","proxy-rewrite","request-validation","response-rewrite"],
    "required_count": 111,
    "covered_count": 5
  },
  "plugins": [
    {"plugin":"cors","obligation_names":["default-response-headers"],"case_names":["two"],"obligations":1,"first_attempt":true,"passed":1,"failed":0,"result":"pass"},
    {"plugin":"key-auth","obligation_names":["allow-valid-api-key","deny-missing-api-key"],"case_names":["three","four"],"obligations":2,"first_attempt":true,"passed":2,"failed":0,"result":"pass"},
    {"plugin":"proxy-rewrite","obligation_names":["rewrite-uri-and-host"],"case_names":["one"],"obligations":1,"first_attempt":true,"passed":1,"failed":0,"result":"pass"},
    {"plugin":"request-validation","obligation_names":["reject-missing-required-header"],"case_names":["five"],"obligations":1,"first_attempt":true,"passed":1,"failed":0,"result":"pass"},
    {"plugin":"response-rewrite","obligation_names":["rewrite-body-and-headers"],"case_names":["six"],"obligations":1,"first_attempt":true,"passed":1,"failed":0,"result":"pass"}
  ],
  "passed": 6,
  "failed": 0,
  "result": "pass"
}
JSON
if [[ ${FAKE_ARTIFACT_MODE:-pass} == failed ]]; then
    jq '
        .cases[0].passed = false |
        .cases[0].error = "deliberate failure" |
        .passed = 5 |
        .failed = 1 |
        .result = "fail" |
        (.plugins[] | select(.plugin == "proxy-rewrite") | .passed) = 0 |
        (.plugins[] | select(.plugin == "proxy-rewrite") | .failed) = 1 |
        (.plugins[] | select(.plugin == "proxy-rewrite") | .result) = "fail"
    ' "$APISIX_GO_DIFFERENTIAL_ARTIFACT" >"$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp"
    mv "$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp" "$APISIX_GO_DIFFERENTIAL_ARTIFACT"
    exit "${FAKE_RUN_STATUS:-9}"
fi
if [[ ${FAKE_ARTIFACT_MODE:-pass} == aggregate-corrupt ]]; then
    jq '
        .cases[0].passed = false |
        .cases[0].error = "deliberate failure" |
        .passed = 5 |
        .failed = 1 |
        .result = "fail" |
        (.plugins[] | select(.plugin == "key-auth") | .passed) = 1 |
        (.plugins[] | select(.plugin == "key-auth") | .failed) = 1 |
        (.plugins[] | select(.plugin == "key-auth") | .result) = "fail"
    ' "$APISIX_GO_DIFFERENTIAL_ARTIFACT" >"$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp"
    mv "$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp" "$APISIX_GO_DIFFERENTIAL_ARTIFACT"
fi
if [[ ${FAKE_ARTIFACT_MODE:-pass} == result-corrupt ]]; then
    jq '.result = "fail"' "$APISIX_GO_DIFFERENTIAL_ARTIFACT" >"$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp"
    mv "$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp" "$APISIX_GO_DIFFERENTIAL_ARTIFACT"
fi
if [[ ${FAKE_ARTIFACT_MODE:-pass} == names-corrupt ]]; then
    jq '
        (.plugins[] | select(.plugin == "proxy-rewrite") | .case_names) = ["two"] |
        (.plugins[] | select(.plugin == "proxy-rewrite") | .obligation_names) = ["default-response-headers"]
    ' "$APISIX_GO_DIFFERENTIAL_ARTIFACT" >"$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp"
    mv "$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp" "$APISIX_GO_DIFFERENTIAL_ARTIFACT"
fi
if [[ ${FAKE_ARTIFACT_MODE:-pass} == first-attempt-corrupt ]]; then
    jq '
        (.plugins[] | select(.plugin == "proxy-rewrite") | .first_attempt) = false |
        (.plugins[] | select(.plugin == "proxy-rewrite") | .result) = "fail"
    ' "$APISIX_GO_DIFFERENTIAL_ARTIFACT" >"$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp"
    mv "$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp" "$APISIX_GO_DIFFERENTIAL_ARTIFACT"
fi
if [[ ${FAKE_ARTIFACT_MODE:-pass} == malformed ]]; then
    jq '.selection.selected_case_count = 999' "$APISIX_GO_DIFFERENTIAL_ARTIFACT" >"$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp"
    mv "$APISIX_GO_DIFFERENTIAL_ARTIFACT.tmp" "$APISIX_GO_DIFFERENTIAL_ARTIFACT"
fi
exit "${FAKE_RUN_STATUS:-0}"
SH
chmod +x "$fake_runner"

oracle_file=$test_root/oracle.yaml
cat >"$oracle_file" <<EOF
schema_version: 1
image_tag: apache/apisix:3.17.0-debian
image_repository: docker.io/apache/apisix
image_index_digest: $index_digest
image_linux_amd64_digest: sha256:5a8d7dfd8382aebfc0cab7bf9d24edf8dd73a6f0eed0b789d25578a373e86f64
source_repository: https://github.com/apache/apisix
source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
expected_version: 3.17.0
EOF

candidate=$test_root/apisix
: >"$candidate"
chmod +x "$candidate"
artifact=$test_root/evidence/differential.json
args_log=$test_root/runner.args
env_log=$test_root/runner.env
preflight_args_log=$test_root/preflight.args
preflight_env_log=$test_root/preflight.env
container_log=$test_root/container.log
candidate_commit=$(printf 'a%.0s' {1..40})
candidate_sha256=$(sha256_file "$candidate")
catalog_sha256=sha256:$(sha256_file "$repo_root/qualification/differential-cases.yaml")
selection_json='{"plugins":["cors","key-auth","proxy-rewrite","request-validation","response-rewrite"],"cases":["five","four","one","six","three","two"],"shard_index":0,"shard_count":1,"selected_case_count":6,"full_catalog_run":false}'
export FAKE_SELECTION_JSON="$selection_json"
export FAKE_PREFLIGHT_ARGS="$preflight_args_log"
export FAKE_PREFLIGHT_ENV="$preflight_env_log"
export FAKE_CONTAINER_LOG="$container_log"

output=$(FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_HOST_GATEWAY=192.0.2.10 APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 APISIX_GO_SKIP_BUILD=1 "$script")
grep -Fq '"result": "pass"' <<<"$output" || fail 'script did not print the passing artifact'
grep -Fq '"covered_count": 5' <<<"$output" || fail 'script did not report five-plugin smoke coverage'
grep -Fq 'go test ./t/plugin -run ^TestPluginDifferential$ -count=1 -v' "$args_log" || fail 'script did not invoke the focused differential test'
grep -Fq "go test ./t/plugin -run ^TestDifferentialSelectionPreflight$ -count=1 -v" "$preflight_args_log" || fail \
    'script did not invoke the shared Go selection preflight'
grep -Fq 'APISIX_GO_DIFFERENTIAL_PLUGINS=proxy-rewrite,cors,key-auth,request-validation,response-rewrite' "$env_log" || fail \
    'script did not forward differential plugin selectors unchanged'
grep -Fq 'APISIX_GO_DIFFERENTIAL_CASES=six,one,three,two,four,five' "$env_log" || fail \
    'script did not forward differential case selectors unchanged'
grep -Fq 'APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0' "$env_log" || fail \
    'script did not forward differential shard index unchanged'
grep -Fq 'APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1' "$env_log" || fail \
    'script did not forward differential shard count unchanged'
grep -Fq 'APISIX_GO_DIFFERENTIAL_PREFLIGHT=1' "$preflight_env_log" || fail \
    'script did not identify the selection preflight invocation'

for variable in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY FTP_PROXY http_proxy https_proxy all_proxy no_proxy ftp_proxy; do
    if grep -q "^$variable=" "$env_log"; then
        fail "proxy variable survived into the differential runner: $variable"
    fi
done

grep -Fq "APISIX_GO_CANDIDATE_BINARY_SHA256=$candidate_sha256" "$env_log" || fail \
    'candidate binary sha256 was not passed to the differential test'
grep -Fq 'APISIX_GO_DIFFERENTIAL_HOST_GATEWAY=192.0.2.10' "$env_log" || fail \
    'explicit differential host gateway was not passed to the runner'

invalid_gateway_artifact=$test_root/evidence/invalid-host-gateway.json
if FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$invalid_gateway_artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_HOST_GATEWAY=not-an-ip APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 APISIX_GO_SKIP_BUILD=1 "$script" >/dev/null 2>&1; then
    fail 'script accepted an invalid differential host gateway'
fi
[[ ! -e "$invalid_gateway_artifact" ]] || fail 'invalid host gateway launched the differential runner'

auto_gateway_artifact=$test_root/evidence/auto-host-gateway.json
FAKE_MACHINE_HOST_GATEWAY=192.168.127.254 \
    FAKE_INDEX_JSON="$index_json" \
    FAKE_INDEX_DIGEST="$index_digest" \
    FAKE_CATALOG_SHA="$catalog_sha256" \
    FAKE_RUNNER_ARGS="$args_log" \
    FAKE_RUNNER_ENV="$env_log" \
    CONTAINER_BIN="$fake_container" \
    APISIX_GO_ORACLE_FILE="$oracle_file" \
    APISIX_GO_CANDIDATE_BIN="$candidate" \
    APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" \
    APISIX_GO_DIFFERENTIAL_ARTIFACT="$auto_gateway_artifact" \
    APISIX_GO_CACHE_RUNNER="$fake_runner" \
    APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" \
    APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" \
    APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 \
    APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 \
    APISIX_GO_SKIP_BUILD=1 \
    "$script" >/dev/null
grep -Fq 'APISIX_GO_DIFFERENTIAL_HOST_GATEWAY=192.168.127.254' "$env_log" || fail \
    'Podman machine host alias was not forwarded as the differential host gateway'

relative_artifact=.cache/qualification/differential/plugin-differential-relative-$$.json
relative_artifact_abs=$repo_root/$relative_artifact
relative_output=$(
    cd "$test_root"
    FAKE_INDEX_JSON="$index_json" \
        FAKE_INDEX_DIGEST="$index_digest" \
        FAKE_CATALOG_SHA="$catalog_sha256" \
        FAKE_RUNNER_ARGS="$args_log" \
        FAKE_RUNNER_ENV="$env_log" \
        CONTAINER_BIN="$fake_container" \
        APISIX_GO_ORACLE_FILE="$oracle_file" \
        APISIX_GO_CANDIDATE_BIN="$candidate" \
        APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" \
        APISIX_GO_DIFFERENTIAL_ARTIFACT="$relative_artifact" \
        APISIX_GO_CACHE_RUNNER="$fake_runner" \
        APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" \
        APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" \
        APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 \
        APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 \
        APISIX_GO_SKIP_BUILD=1 \
        "$script"
)
grep -Fq 'plugin differential smoke: PASS' <<<"$relative_output" || fail \
    'script did not resolve a relative artifact path from the repository root'
[[ -f "$relative_artifact_abs" ]] || fail \
    'relative artifact was not written beneath the repository root'

if CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 APISIX_GO_SKIP_BUILD=1 FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" "$script" >/dev/null 2>&1; then
    fail 'script accepted an existing artifact instead of requiring a new attempt path'
fi

failed_artifact=$test_root/evidence/failed.json
if failed_output=$(FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$failed_artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 FAKE_ARTIFACT_MODE=failed FAKE_RUN_STATUS=9 APISIX_GO_SKIP_BUILD=1 "$script"); then
    fail 'script accepted a failing differential run as passing'
else
    failed_status=$?
fi
[[ "$failed_status" -eq 9 ]] || fail "script changed the original failing status: $failed_status"
grep -Fq '"result": "fail"' <<<"$failed_output" || fail 'script did not print a valid failed artifact'
grep -Fq '"failed": 1' <<<"$failed_output" || fail 'script did not expose failed case evidence'

aggregate_corrupt_artifact=$test_root/evidence/aggregate-corrupt.json
if FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$aggregate_corrupt_artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 FAKE_ARTIFACT_MODE=aggregate-corrupt APISIX_GO_SKIP_BUILD=1 "$script" >/dev/null 2>&1; then
    fail 'script accepted plugin aggregates that contradict their case rows'
fi

result_corrupt_artifact=$test_root/evidence/result-corrupt.json
if FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$result_corrupt_artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 FAKE_ARTIFACT_MODE=result-corrupt APISIX_GO_SKIP_BUILD=1 "$script" >/dev/null 2>&1; then
    fail 'script accepted a top-level result that contradicts the case totals'
fi

names_corrupt_artifact=$test_root/evidence/names-corrupt.json
if FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$names_corrupt_artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 FAKE_ARTIFACT_MODE=names-corrupt APISIX_GO_SKIP_BUILD=1 "$script" >/dev/null 2>&1; then
    fail 'script accepted case and obligation names that contradict their plugin rows'
fi

first_attempt_corrupt_artifact=$test_root/evidence/first-attempt-corrupt.json
if FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$first_attempt_corrupt_artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 FAKE_ARTIFACT_MODE=first-attempt-corrupt APISIX_GO_SKIP_BUILD=1 "$script" >/dev/null 2>&1; then
    fail 'script accepted a plugin first-attempt fact that contradicts its cases'
fi

malformed_artifact=$test_root/evidence/malformed.json
if FAKE_INDEX_JSON="$index_json" FAKE_INDEX_DIGEST="$index_digest" FAKE_CATALOG_SHA="$catalog_sha256" FAKE_RUNNER_ARGS="$args_log" FAKE_RUNNER_ENV="$env_log" CONTAINER_BIN="$fake_container" APISIX_GO_ORACLE_FILE="$oracle_file" APISIX_GO_CANDIDATE_BIN="$candidate" APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" APISIX_GO_DIFFERENTIAL_ARTIFACT="$malformed_artifact" APISIX_GO_CACHE_RUNNER="$fake_runner" APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 FAKE_ARTIFACT_MODE=malformed APISIX_GO_SKIP_BUILD=1 "$script" >/dev/null 2>&1; then
    fail 'script accepted malformed selection or aggregate facts'
fi

expect_preflight_rejection() {
    local label=$1
    shift
    local rejected_artifact=$test_root/evidence/rejected-$label.json
    : >"$container_log"
    if env \
        FAKE_PREFLIGHT_STATUS=7 \
        FAKE_INDEX_JSON="$index_json" \
        FAKE_INDEX_DIGEST="$index_digest" \
        FAKE_CATALOG_SHA="$catalog_sha256" \
        FAKE_RUNNER_ARGS="$args_log" \
        FAKE_RUNNER_ENV="$env_log" \
        CONTAINER_BIN="$fake_container" \
        APISIX_GO_ORACLE_FILE="$oracle_file" \
        APISIX_GO_CANDIDATE_BIN="$candidate" \
        APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" \
        APISIX_GO_DIFFERENTIAL_ARTIFACT="$rejected_artifact" \
        APISIX_GO_CACHE_RUNNER="$fake_runner" \
        APISIX_GO_SKIP_BUILD=1 \
        "$@" \
        "$script" >"$test_root/rejected-$label.out" 2>&1; then
        fail "script accepted invalid preflight selection: $label"
    fi
    [[ ! -s "$container_log" ]] || fail "invalid selection launched the Oracle resolver: $label"
    [[ ! -e "$rejected_artifact" ]] || fail "invalid selection launched the differential runner: $label"
}

expect_preflight_rejection explicit-empty APISIX_GO_DIFFERENTIAL_PLUGINS=
expect_preflight_rejection duplicate APISIX_GO_DIFFERENTIAL_PLUGINS=cors,cors
expect_preflight_rejection unknown APISIX_GO_DIFFERENTIAL_PLUGINS=does-not-exist
expect_preflight_rejection invalid-shard APISIX_GO_DIFFERENTIAL_SHARD_INDEX=1 APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1
expect_preflight_rejection empty-selection APISIX_GO_DIFFERENTIAL_PLUGINS=cors APISIX_GO_DIFFERENTIAL_CASES=one

timeout_artifact=$test_root/evidence/resolver-timeout.json
started_at=$SECONDS
if timeout_output=$(
    FAKE_CONTAINER_HANG=1 \
        APISIX_GO_CONTAINER_TIMEOUT_SECONDS=1 \
        FAKE_INDEX_JSON="$index_json" \
        FAKE_INDEX_DIGEST="$index_digest" \
        FAKE_CATALOG_SHA="$catalog_sha256" \
        FAKE_RUNNER_ARGS="$args_log" \
        FAKE_RUNNER_ENV="$env_log" \
        CONTAINER_BIN="$fake_container" \
        APISIX_GO_ORACLE_FILE="$oracle_file" \
        APISIX_GO_CANDIDATE_BIN="$candidate" \
        APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_commit" \
        APISIX_GO_DIFFERENTIAL_ARTIFACT="$timeout_artifact" \
        APISIX_GO_CACHE_RUNNER="$fake_runner" \
        APISIX_GO_DIFFERENTIAL_PLUGINS="proxy-rewrite,cors,key-auth,request-validation,response-rewrite" \
        APISIX_GO_DIFFERENTIAL_CASES="six,one,three,two,four,five" \
        APISIX_GO_DIFFERENTIAL_SHARD_INDEX=0 \
        APISIX_GO_DIFFERENTIAL_SHARD_COUNT=1 \
        APISIX_GO_SKIP_BUILD=1 \
        "$script" 2>&1
); then
    fail 'plugin wrapper accepted a hung Oracle resolver'
fi
elapsed=$((SECONDS - started_at))
if (( elapsed > 4 )); then
    fail "plugin wrapper did not inherit the resolver timeout: ${elapsed}s"
fi
grep -Fqi timeout <<<"$timeout_output" || fail 'plugin wrapper hid the resolver timeout reason'
[[ ! -e "$timeout_artifact" ]] || fail 'plugin wrapper launched the differential runner after resolver timeout'

printf 'plugin differential script tests passed\n'
