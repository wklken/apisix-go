#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
runner="$repo_root/scripts/etcd_recovery_smoke.sh"
original_path=$PATH
test_shell=$(command -v bash)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/apisix-etcd-recovery-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT

image_id=sha256:$(printf '%064d' 1)
image_hex=${image_id#sha256:}

fail() {
    printf 'etcd recovery fixture: %s\n' "$1" >&2
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

assert_order() {
    local file=$1
    shift
    local previous=0
    local item line
    for item in "$@"; do
        line=$(grep -nF -- "$item" "$file" | head -n 1 | cut -d: -f1 || true)
        [[ -n "$line" ]] || fail "missing transcript step: $item"
        (( line > previous )) || fail "transcript step out of order: $item"
        previous=$line
    done
}

write_stub() {
    local path=$1
    local body=$2
    printf '%s\n' "$body" >"$path"
    chmod +x "$path"
}

write_fake_openssl() {
    local bin=$1
    write_stub "$bin/openssl" '#!/usr/bin/env bash
set -euo pipefail
printf "openssl %s\n" "$*" >>"${FAKE_LOG:?}"
for ((i = 1; i <= $#; i++)); do
    arg=${!i}
    case "$arg" in
        -keyout|-out)
            next=$((i + 1))
            : >"${!next}"
            ;;
    esac
done
'
}

write_fake_docker() {
    local bin=$1
    write_stub "$bin/docker" '#!/usr/bin/env bash
set -euo pipefail
printf "docker %s\n" "$*" >>"${FAKE_LOG:?}"
case "${1:-}" in
    image)
        if [[ ${2:-} == inspect ]]; then
            printf "%s\n" "${FAKE_IMAGE_ID:?}"
            exit 0
        fi
        ;;
    network)
        exit 0
        ;;
    run)
        if [[ "$*" == *"--entrypoint /usr/local/bin/etcdctl"* && "$*" == *"gcr.io/etcd-development/etcd:v3.6.13 etcdctl --endpoints="* ]]; then
            printf "fake contract: duplicate etcdctl invocation\n" >&2
            exit 97
        fi
        if [[ "$*" == *"--name=etcd0"* && "$*" != *"gcr.io/etcd-development/etcd:v3.6.13 /usr/local/bin/etcd --name=etcd0"* ]]; then
            printf "fake contract: etcd executable is missing\n" >&2
            exit 98
        fi
        if [[ "$*" == *"--name=etcd0"* && "$*" == *"--trusted-ca-file="* ]]; then
            printf "fake contract: one-way TLS unexpectedly requires client certificates\n" >&2
            exit 99
        fi
        if [[ "$*" == *"--user 10001:10001"* ]]; then
            for argument in "$@"; do
                case "$argument" in
                    *:/etc/ssl/certs/etcd-ca.crt:ro)
                        ca_path=${argument%%:*}
                        if [[ -z $(find "$ca_path" -prune -perm 0644 -print -quit) ]]; then
                            printf "fake contract: CA is not mode 0644\n" >&2
                            exit 96
                        fi
                        ;;
                esac
            done
        fi
        if [[ ${FAKE_FAIL_GATEWAY:-0} == 1 && "$*" == *"--user 10001:10001"* ]]; then
            exit 42
        fi
        if [[ "$*" == *"etcdctl"* && ${FAKE_ETCDCTL_FAIL:-0} == 1 ]]; then
            exit 1
        fi
        printf "fake-container-id\n"
        exit 0
        ;;
    port)
        printf "127.0.0.1:18080\n"
        exit 0
        ;;
    stop)
        : >"${FAKE_ETCD_STOPPED:?}"
        exit 0
        ;;
    start)
        rm -f "${FAKE_ETCD_STOPPED:?}"
        : >"${FAKE_ETCD_RESTARTED:?}"
        exit 0
        ;;
    logs|inspect|rm|kill)
        printf "fake %s output\n" "$1"
        exit 0
        ;;
esac
exit 0
'
}

write_fake_curl() {
    local bin=$1
    write_stub "$bin/curl" '#!/usr/bin/env bash
set -euo pipefail
printf "curl %s\n" "$*" >>"${FAKE_LOG:?}"
if [[ ${FAKE_CURL_MODE:-happy} == timeout ]]; then
    exit 28
fi
body_file=
for ((i = 1; i <= $#; i++)); do
    if [[ ${!i} == --output ]]; then
        next=$((i + 1))
        body_file=${!next}
    fi
done
url=${!#}
status=200
body=
case "$url" in
    */readyz)
        if [[ -e ${FAKE_ETCD_STOPPED:?} ]]; then
            status=503
            body=etcd-unreachable
        else
            body=ready
        fi
        ;;
    */livez)
        body=live
        ;;
    */release-v1)
        body=v1
        ;;
    */release-v2)
        body=v2
        ;;
    *)
        status=404
        body=not-found
        ;;
esac
printf "%s" "$body" >"$body_file"
printf "%s" "$status"
'
}

run_expect_failure() {
    local output=$1
    shift
    if "$@" >"$output" 2>&1; then
        fail "command unexpectedly succeeded: $*"
    fi
}

test_missing_tools() {
    local tool bin output
    for tool in docker curl openssl; do
        bin="$test_root/missing-$tool/bin"
        mkdir -p "$bin"
        ln -s "$(command -v dirname)" "$bin/dirname"
        case "$tool" in
            curl)
                write_stub "$bin/docker" $'#!/usr/bin/env bash\nexit 0'
                ;;
            openssl)
                write_stub "$bin/docker" $'#!/usr/bin/env bash\nexit 0'
                write_stub "$bin/curl" $'#!/usr/bin/env bash\nexit 0'
                ;;
        esac
        output="$test_root/missing-$tool.out"
        run_expect_failure "$output" env PATH="$bin" "$test_shell" "$runner" "$image_id"
        assert_contains "required command is unavailable: $tool" "$output"
    done
}

test_rejects_mutable_image() {
    local bin="$test_root/mutable/bin"
    mkdir -p "$bin"
    write_fake_docker "$bin"
    write_stub "$bin/curl" $'#!/usr/bin/env bash\nexit 0'
    write_fake_openssl "$bin"
    local candidate output
    local uppercase_hex tagged_digest
    uppercase_hex=$(printf 'A%.0s' {1..64})
    tagged_digest="ghcr.io/acme/apisix-go:v1@sha256:$image_hex"
    for candidate in \
        apisix-go:latest \
        ghcr.io/acme/apisix-go@sha256:not-a-digest \
        "sha256:$uppercase_hex" \
        "$tagged_digest"; do
        output="$test_root/mutable-${#candidate}.out"
        run_expect_failure "$output" env PATH="$bin:$original_path" FAKE_LOG="$test_root/mutable.log" \
            "$test_shell" "$runner" "$candidate"
        assert_contains 'immutable image ID or digest-qualified reference' "$output"
    done
}

test_bounded_timeout() {
    local bin="$test_root/timeout/bin"
    local output="$test_root/timeout.out"
    local log="$test_root/timeout.log"
    local evidence="$test_root/timeout-evidence"
    mkdir -p "$bin"
    write_fake_docker "$bin"
    write_fake_curl "$bin"
    write_fake_openssl "$bin"
    : >"$log"
    local started=$SECONDS
    run_expect_failure "$output" env PATH="$bin:$original_path" \
        FAKE_LOG="$log" FAKE_IMAGE_ID="$image_id" \
        FAKE_ETCD_STOPPED="$test_root/timeout-stopped" \
        FAKE_ETCD_RESTARTED="$test_root/timeout-restarted" \
        FAKE_CURL_MODE=timeout RELEASE_EVIDENCE_ROOT="$evidence" \
        ETCD_RECOVERY_TIMEOUT_SECONDS=1 ETCD_RECOVERY_POLL_INTERVAL_SECONDS=4 \
        "$test_shell" "$runner" "$image_id"
    local elapsed=$((SECONDS - started))
    (( elapsed < 4 )) || fail "timeout fixture exceeded bound (${elapsed}s)"
    assert_contains 'timed out waiting for' "$output"
}

test_cleanup_on_failure() {
    local bin="$test_root/cleanup/bin"
    local output="$test_root/cleanup.out"
    local log="$test_root/cleanup.log"
    local evidence="$test_root/cleanup-evidence"
    mkdir -p "$bin"
    write_fake_docker "$bin"
    write_fake_curl "$bin"
    write_fake_openssl "$bin"
    : >"$log"
    run_expect_failure "$output" env PATH="$bin:$original_path" \
        FAKE_LOG="$log" FAKE_IMAGE_ID="$image_id" \
        FAKE_ETCD_STOPPED="$test_root/cleanup-stopped" \
        FAKE_ETCD_RESTARTED="$test_root/cleanup-restarted" \
        FAKE_FAIL_GATEWAY=1 RELEASE_EVIDENCE_ROOT="$evidence" \
        ETCD_RECOVERY_TIMEOUT_SECONDS=1 "$test_shell" "$runner" "$image_id"
    assert_contains 'docker rm -f' "$log"
    assert_contains 'docker network rm' "$log"
    assert_contains 'cleanup' "$output"
}

test_ordered_happy_path() {
    local bin="$test_root/happy/bin"
    local output="$test_root/happy.out"
    local log="$test_root/happy.log"
    local evidence="$test_root/happy-evidence"
    mkdir -p "$bin"
    write_fake_docker "$bin"
    write_fake_curl "$bin"
    write_fake_openssl "$bin"
    : >"$log"
    env PATH="$bin:$original_path" \
        FAKE_LOG="$log" FAKE_IMAGE_ID="$image_id" \
        FAKE_ETCD_STOPPED="$test_root/happy-stopped" \
        FAKE_ETCD_RESTARTED="$test_root/happy-restarted" \
        RELEASE_EVIDENCE_ROOT="$evidence" ETCD_RECOVERY_TIMEOUT_SECONDS=2 \
        "$test_shell" "$runner" "$image_id" >"$output" 2>&1
    assert_contains 'etcd recovery smoke: PASS' "$output"
    local transcript
    transcript=$(find "$evidence" -type f -name steps.log -print -quit)
    [[ -n "$transcript" ]] || fail 'happy-path transcript was not preserved'
    assert_order "$transcript" \
        'network create' \
        'start etcd 3.6.13' \
        'start upstream v1' \
        'write route/upstream v1' \
        'start APISIX-Go as 10001:10001' \
        'livez 200' \
        'readyz 200' \
        'proxy /release-v1 -> v1' \
        'stop etcd' \
        'readyz 503' \
        'last-good /release-v1 -> v1' \
        'restart etcd' \
        'readyz 200 after recovery' \
        'update same route/upstream IDs to v2' \
        'proxy /release-v2 -> v2'
    assert_contains 'gcr.io/etcd-development/etcd:v3.6.13' "$log"
    assert_contains '--user 10001:10001' "$log"
    assert_contains 'SSL_CERT_FILE' "$log"
    assert_contains 'https://etcd:2379' "$log"
    assert_contains 'put /apisix/upstreams/release-upstream {"nodes":{"release-upstream-v1-' "$log"
    assert_contains 'put /apisix/routes/release-route {"id":"release-route","status":1,"uri":"/release-v1","upstream_id":"release-upstream"}' "$log"
    assert_contains 'put /apisix/upstreams/release-upstream {"nodes":{"release-upstream-v2-' "$log"
    assert_contains 'put /apisix/routes/release-route {"id":"release-route","status":1,"uri":"/release-v2","upstream_id":"release-upstream"}' "$log"
    assert_order "$log" \
        'docker network create apisix-release-etcd-' \
        'docker run --detach --name apisix-release-etcd-' \
        'docker run --detach --name apisix-release-upstream-v1-' \
        'put /apisix/upstreams/release-upstream {"nodes":{"release-upstream-v1-' \
        'put /apisix/routes/release-route {"id":"release-route","status":1,"uri":"/release-v1"' \
        'docker run --detach --name apisix-release-gateway-' \
        'docker stop apisix-release-etcd-' \
        'docker start apisix-release-etcd-' \
        'docker rm -f apisix-release-upstream-v1-' \
        'docker run --detach --name apisix-release-upstream-v2-' \
        'put /apisix/upstreams/release-upstream {"nodes":{"release-upstream-v2-' \
        'put /apisix/routes/release-route {"id":"release-route","status":1,"uri":"/release-v2"' \
        'docker rm -f apisix-release-gateway-' \
        'docker network rm apisix-release-etcd-'
}

test_missing_tools
test_rejects_mutable_image
test_bounded_timeout
test_cleanup_on_failure
test_ordered_happy_path

printf 'etcd recovery fixture: PASS\n'
