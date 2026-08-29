#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
runner="$repo_root/scripts/upgrade_rollback_smoke.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/apisix-upgrade-rollback-test.XXXXXX")
cleanup_test_root() {
    if [[ ${KEEP_UPGRADE_ROLLBACK_TEST_ROOT:-0} == 1 ]]; then
        printf 'upgrade rollback fixture: preserved %s\n' "$test_root" >&2
        return
    fi
    rm -rf "$test_root"
}
trap cleanup_test_root EXIT

rollback_digest=sha256:$(printf '2%.0s' {1..64})
candidate_id=sha256:$(printf '3%.0s' {1..64})
rollback_id=sha256:$(printf '4%.0s' {1..64})
candidate_commit=$(printf 'a%.0s' {1..40})
rollback_commit=$(printf 'b%.0s' {1..40})
candidate_ref=$candidate_id
rollback_ref="registry.example.test/apisix-go@$rollback_digest"

fail() {
    printf 'upgrade rollback fixture: %s\n' "$*" >&2
    exit 1
}

assert_contains() {
    local needle=$1
    local file=$2
    grep -Fq -- "$needle" "$file" || {
        printf 'missing %q in %s\n' "$needle" "$file" >&2
        sed -n '1,240p' "$file" >&2 || true
        exit 1
    }
}

assert_not_contains() {
    local needle=$1
    local file=$2
    if grep -Fq -- "$needle" "$file"; then
        printf 'unexpected %q in %s\n' "$needle" "$file" >&2
        sed -n '1,240p' "$file" >&2 || true
        exit 1
    fi
}

write_stub() {
    local path=$1
    local body=$2
    printf '%s\n' "$body" >"$path"
    chmod +x "$path"
}

write_fake_docker() {
    local bin=$1
    # shellcheck disable=SC2016
    write_stub "$bin/docker" '#!/usr/bin/env bash
set -euo pipefail
state=${FAKE_STATE_DIR:?}
mkdir -p "$state"
printf "docker %s\n" "$*" >>"$state/docker.log"

if [[ ${1:-} == image && ${2:-} == inspect ]]; then
    format=${4:-}
    ref=${5:-}
    if [[ "$ref" == "${FAKE_CANDIDATE_REF:?}" ]]; then
        image_id=${FAKE_CANDIDATE_ID:?}
        source_commit=${FAKE_CANDIDATE_COMMIT:?}
    elif [[ "$ref" == "${FAKE_ROLLBACK_REF:?}" ]]; then
        image_id=${FAKE_ROLLBACK_ID:?}
        source_commit=${FAKE_ROLLBACK_COMMIT:?}
    else
        exit 1
    fi
    if [[ "$format" == *org.opencontainers.image.revision* ]]; then
        printf "%s\n" "$source_commit"
    else
        printf "%s\n" "$image_id"
    fi
    exit 0
fi

case ${1:-} in
    network)
        exit 0
        ;;
    run)
        name=
        image=
        for ((i = 2; i <= $#; i++)); do
            arg=${!i}
            if [[ "$arg" == --name ]]; then
                next=$((i + 1))
                name=${!next}
            fi
            if [[ "$arg" == "${FAKE_CANDIDATE_REF:?}" || "$arg" == "${FAKE_ROLLBACK_REF:?}" || "$arg" == busybox:* || "$arg" == gcr.io/etcd-development/etcd:* ]]; then
                image=$arg
            fi
        done
        if [[ "$image" == gcr.io/etcd-development/etcd:* || "$image" == busybox:* ]]; then
            printf "fake-container-id\n"
            exit 0
        fi
        if [[ "$image" != busybox:* ]]; then
            [[ -n "$name" && -n "$image" ]]
            printf "%s\n" "$image" >"$state/image-$name"
            : >"$state/ready-$name"
            if [[ "$image" == "${FAKE_CANDIDATE_REF:?}" && ${FAKE_FAIL_SURVIVOR_DURING_START:-0} == 1 ]]; then
                find "$state" -name "ready-*-b-*" -delete
                sleep 2
            fi
        fi
        printf "fake-container-id\n"
        exit 0
        ;;
    port)
        name=${2:?}
        port=${3:?}
        if [[ "$name" == *-a-* ]]; then base=10000; else base=20000; fi
        case "$port" in
            9080/tcp) mapped=$((base + 80)) ;;
            9443/tcp) mapped=$((base + 443)) ;;
            7085/tcp) mapped=$((base + 85)) ;;
            *) exit 1 ;;
        esac
        printf "127.0.0.1:%s\n" "$mapped"
        exit 0
        ;;
    rm)
        for arg in "$@"; do
            [[ "$arg" == *apisix-rollout-* ]] || continue
            rm -f "$state/ready-$arg" "$state/image-$arg"
        done
        exit 0
        ;;
    logs|inspect)
        exit 0
        ;;
esac
exit 0
'
}

write_fake_curl() {
    local bin=$1
    # shellcheck disable=SC2016
    write_stub "$bin/curl" '#!/usr/bin/env bash
set -euo pipefail
state=${FAKE_STATE_DIR:?}
printf "curl %s\n" "$*" >>"$state/curl.log"
url=${!#}
case "$url" in
    *:10085/*) replica=a ;;
    *:20085/*) replica=b ;;
    *:10080/*|*:10443/*) replica=a ;;
    *:20080/*|*:20443/*) replica=b ;;
    *) exit 22 ;;
esac
ready=$(find "$state" -name "ready-*-$replica-*" -print -quit)
[[ -n "$ready" ]] || exit 22
if [[ " $* " == *" --write-out "* ]]; then
    printf "200"
elif [[ "$url" == */status || "$url" == */status/ready ]]; then
    printf "ready"
else
    printf "apisix-upgrade-rollback"
fi
'
}

write_metadata() {
    local path=$1
    local reference=${2:-$rollback_ref}
    local image_id=${3:-$rollback_id}
    local commit=${4:-$rollback_commit}
    local result=${5:-passed}
    jq -n \
        --arg reference "$reference" \
        --arg image_id "$image_id" \
        --arg commit "$commit" \
        --arg result "$result" '
        {
          schema_version: 1,
          image_reference: $reference,
          image_digest: $image_id,
          source: {commit: $commit},
          qualification: {
            profile: "http-data-plane-v1",
            result: $result
          }
        }
    ' >"$path"
}

run_fixture() {
    local case_name=$1
    shift
    local case_dir="$test_root/$case_name"
    local bin="$case_dir/bin"
    local fail_survivor=0
    if [[ "$case_name" == survivor-fails ]]; then fail_survivor=1; fi
    mkdir -p "$bin" "$case_dir/state" "$case_dir/evidence"
    write_fake_docker "$bin"
    write_fake_curl "$bin"
    PATH="$bin:$PATH" \
        CONTAINER_BIN=docker \
        UPGRADE_ROLLBACK_PROBE_MODE=host \
        UPGRADE_ROLLBACK_POLL_INTERVAL_SECONDS=0 \
        UPGRADE_ROLLBACK_GUARD_INTERVAL_SECONDS=1 \
        UPGRADE_ROLLBACK_TIMEOUT_SECONDS=3 \
        RELEASE_EVIDENCE_ROOT="$case_dir/evidence" \
        FAKE_STATE_DIR="$case_dir/state" \
        FAKE_CANDIDATE_REF="$candidate_ref" \
        FAKE_CANDIDATE_ID="$candidate_id" \
        FAKE_CANDIDATE_COMMIT="$candidate_commit" \
        FAKE_ROLLBACK_REF="$rollback_ref" \
        FAKE_ROLLBACK_ID="$rollback_id" \
        FAKE_ROLLBACK_COMMIT="$rollback_commit" \
        FAKE_FAIL_SURVIVOR_DURING_START="$fail_survivor" \
        bash "$runner" "$@"
}

[[ -x "$runner" ]] || fail "runner is missing or not executable: $runner"

mutable_output="$test_root/mutable.out"
if run_fixture mutable apisix-go:candidate "$rollback_ref" "$test_root/missing.json" >"$mutable_output" 2>&1; then
    fail 'mutable candidate reference unexpectedly succeeded'
fi
assert_contains 'digest-qualified reference' "$mutable_output"

same_output="$test_root/same.out"
if run_fixture same "$rollback_ref" "$rollback_ref" "$test_root/missing.json" >"$same_output" 2>&1; then
    fail 'identical candidate and rollback digest unexpectedly succeeded'
fi
assert_contains 'must be distinct' "$same_output"

missing_output="$test_root/missing.out"
if run_fixture missing "$candidate_ref" "$rollback_ref" "$test_root/missing.json" >"$missing_output" 2>&1; then
    fail 'missing rollback qualification metadata unexpectedly succeeded'
fi
assert_contains 'metadata is not a file' "$missing_output"

mismatch_metadata="$test_root/mismatch.json"
write_metadata "$mismatch_metadata" "$candidate_ref"
mismatch_output="$test_root/mismatch.out"
if run_fixture mismatch "$candidate_ref" "$rollback_ref" "$mismatch_metadata" >"$mismatch_output" 2>&1; then
    fail 'mismatched rollback qualification metadata unexpectedly succeeded'
fi
assert_contains 'does not match rollback image' "$mismatch_output"

failed_metadata="$test_root/failed.json"
write_metadata "$failed_metadata" "$rollback_ref" "$rollback_id" "$rollback_commit" failed
failed_output="$test_root/failed.out"
if run_fixture failed "$candidate_ref" "$rollback_ref" "$failed_metadata" >"$failed_output" 2>&1; then
    fail 'failed prior qualification unexpectedly succeeded'
fi
assert_contains 'does not prove a passed http-data-plane-v1 qualification' "$failed_output"

metadata="$test_root/rollback-metadata.json"
write_metadata "$metadata"

survivor_output="$test_root/survivor-fails.out"
if run_fixture survivor-fails "$candidate_ref" "$rollback_ref" "$metadata" >"$survivor_output" 2>&1; then
    fail 'survivor failure during replacement startup unexpectedly succeeded'
fi
assert_contains 'survivor failed during replacement window: upgrade-a' "$survivor_output"

happy_output="$test_root/happy.out"
run_fixture happy "$candidate_ref" "$rollback_ref" "$metadata" >"$happy_output" 2>&1
assert_contains 'upgrade rollback smoke: PASS' "$happy_output"

docker_log="$test_root/happy/state/docker.log"
candidate_image_runs=$(grep -F "docker run" "$docker_log" | grep -F -c -- "$candidate_ref" || true)
rollback_image_runs=$(grep -F "docker run" "$docker_log" | grep -F -c -- "$rollback_ref" || true)
(( candidate_image_runs == 2 )) || fail "wanted two candidate starts, got $candidate_image_runs"
(( rollback_image_runs == 4 )) || fail "wanted four rollback-image starts, got $rollback_image_runs"
assert_contains '--client-cert-auth=true' "$docker_log"
assert_contains '--endpoints=https://etcd:2379' "$docker_log"
assert_contains '--cert=/etc/etcd/tls/etcd-client.crt' "$docker_log"
assert_contains '--key=/etc/etcd/tls/etcd-client.key' "$docker_log"
assert_contains 'put /apisix/routes/upgrade-rollback-route' "$docker_log"
assert_contains 'put /apisix/ssls/upgrade-rollback-tls' "$docker_log"
assert_contains "$repo_root/conf/config-production.yaml:/usr/local/apisix/conf/config-production.yaml:ro" "$docker_log"
assert_contains 'APISIXGO_DEPLOYMENT_ETCD_HOST=https://etcd:2379' "$docker_log"
assert_contains 'APISIXGO_DEPLOYMENT_ETCD_TLS_CERT=/etc/etcd/tls/client.crt' "$docker_log"
assert_contains 'APISIXGO_DEPLOYMENT_ETCD_TLS_KEY=/etc/etcd/tls/client.key' "$docker_log"
assert_contains 'SSL_CERT_FILE=/etc/etcd/tls/ca.crt' "$docker_log"
assert_not_contains '/usr/local/apisix/conf/apisix.yaml' "$docker_log"
assert_not_contains '/usr/local/apisix/conf/config.yaml' "$docker_log"
replica_a_data_mounts=$(grep -F "docker run" "$docker_log" | grep -F -c -- '/replica-a/data:/usr/local/apisix/data' || true)
replica_b_data_mounts=$(grep -F "docker run" "$docker_log" | grep -F -c -- '/replica-b/data:/usr/local/apisix/data' || true)
(( replica_a_data_mounts == 3 )) || fail "wanted replica A persistent data dir reused three times, got $replica_a_data_mounts"
(( replica_b_data_mounts == 3 )) || fail "wanted replica B persistent data dir reused three times, got $replica_b_data_mounts"

evidence=$(find "$test_root/happy/evidence" -name events.jsonl -type f -print -quit)
[[ -n "$evidence" ]] || fail 'missing append-only JSONL evidence'
jq -e -s \
    --arg candidate_ref "$candidate_ref" \
    --arg rollback_ref "$rollback_ref" \
    --arg candidate_commit "$candidate_commit" \
    --arg rollback_commit "$rollback_commit" '
    (.[0].event == "run_started") and
    (.[0].candidate.image_reference == $candidate_ref) and
    (.[0].candidate.source_commit == $candidate_commit) and
    (.[0].rollback.image_reference == $rollback_ref) and
    (.[0].rollback.source_commit == $rollback_commit) and
    ([.[] | select(.event == "transition_completed") | .phase] ==
      ["known-good-a", "known-good-b", "upgrade-a", "upgrade-b", "rollback-a", "rollback-b"]) and
    ([.[] | select(.event == "probe_succeeded" and .probe == "readiness")] | length >= 10) and
    ([.[] | select(.event == "probe_succeeded" and .probe == "http-route")] | length >= 10) and
    ([.[] | select(.event == "probe_succeeded" and .probe == "tls-route")] | length >= 10) and
    (.[-1].event == "run_finished" and .[-1].result == "passed")
    ' "$evidence" >/dev/null || fail 'JSONL evidence does not describe the complete upgrade/rollback sequence'

printf 'upgrade rollback fixture: PASS\n'
