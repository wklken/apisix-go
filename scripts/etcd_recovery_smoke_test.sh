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
source_commit=$(printf 'a%.0s' {1..40})

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

assert_count() {
    local needle=$1
    local file=$2
    local expected=$3
    local count
    count=$(grep -F -c -- "$needle" "$file" || true)
    (( count == expected )) || fail "wanted $expected occurrences of $needle in $file, got $count"
}

assert_at_least() {
    local needle=$1
    local file=$2
    local minimum=$3
    local count
    count=$(grep -F -c -- "$needle" "$file" || true)
    (( count >= minimum )) || fail "wanted at least $minimum occurrences of $needle in $file, got $count"
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
    # The generated stub expands these variables when it runs.
    # shellcheck disable=SC2016
    write_stub "$bin/openssl" '#!/usr/bin/env bash
set -euo pipefail
state_dir=${FAKE_STATE_DIR:-${TMPDIR:-/tmp}/apisix-etcd-recovery-fake}
mkdir -p "$state_dir"
printf "openssl %s\n" "$*" >>"${FAKE_LOG:-/dev/null}"
if [[ ${1:-} == base64 ]]; then
    /usr/bin/base64 | tr -d "\n"
    exit 0
fi
if [[ ${1:-} == dgst ]]; then
    if [[ " $* " == *" -binary "* ]]; then
        printf "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    else
        printf "SHA2-256(stdin)= %064d\n" 2
    fi
    exit 0
fi
if [[ ${1:-} == s_client ]]; then
    if [[ -e "$state_dir/ssl-deleted" ]]; then
        printf "SSL handshake failed for release.example.test\n" >&2
        exit 1
    fi
    printf "CONNECTED(00000003)\nsubject=CN = release.example.test\nVerify return code: 0 (ok)\n"
    exit 0
fi
for ((i = 1; i <= $#; i++)); do
    arg=${!i}
    case "$arg" in
        -keyout|-out)
            next=$((i + 1))
            target=${!next}
            mkdir -p "$(dirname "$target")"
            : >"$target"
            ;;
    esac
done
'
}

write_fake_docker() {
    local bin=$1
    # The generated stub expands these variables when it runs.
    # shellcheck disable=SC2016
    write_stub "$bin/docker" '#!/usr/bin/env bash
set -euo pipefail
state_dir=${FAKE_STATE_DIR:-${TMPDIR:-/tmp}/apisix-etcd-recovery-fake}
mkdir -p "$state_dir"
printf "docker %s\n" "$*" >>"${FAKE_LOG:-/dev/null}"

container_name=
for ((i = 1; i <= $#; i++)); do
    argument=${!i}
    case "$argument" in
        --name)
            next=$((i + 1))
            container_name=${!next}
            ;;
        --name=*)
            container_name=${argument#--name=}
            ;;
    esac
done

record_etcd_put() {
    local key=$1
    local value=$2
    printf "%s %s\n" "$key" "$value" >>"$state_dir/etcd-puts.log"
    case "$key" in
        */routes/release-route)
            if [[ "$value" == *"/release-v2"* ]]; then
                : >"$state_dir/route-v2"
                rm -f "$state_dir/route-v1"
            else
                : >"$state_dir/route-v1"
                rm -f "$state_dir/route-v2"
            fi
            ;;
        */routes/release-managed-route)
            : >"$state_dir/managed-route"
            ;;
        */routes/release-stale-route)
            : >"$state_dir/stale-route"
            ;;
        */services/release-service)
            if [[ "$value" == *release-upstream-v2* ]]; then
                printf "v2\n" >"$state_dir/service-version"
            else
                printf "v1\n" >"$state_dir/service-version"
            fi
            ;;
        */ssls/release-ssl)
            : >"$state_dir/ssl"
            rm -f "$state_dir/ssl-deleted"
            ;;
    esac
}

record_etcd_delete() {
    local key=$1
    printf "%s\n" "$key" >>"$state_dir/etcd-deletes.log"
    case "$key" in
        */routes/release-stale-route)
            rm -f "$state_dir/stale-route"
            ;;
        */routes/release-invalid-route)
            rm -f "$state_dir/invalid-route"
            ;;
        */ssls/release-ssl)
            rm -f "$state_dir/ssl"
            : >"$state_dir/ssl-deleted"
            ;;
    esac
}

if [[ ${1:-} == image && ${2:-} == inspect ]]; then
    printf "%s\n" "${FAKE_IMAGE_ID:?}"
    exit 0
fi

if [[ ${1:-} == network ]]; then
    case "${2:-}" in
        create)
            network_name=${!#}
            printf "%s\n" "$network_name" >>"$state_dir/networks-created"
            if [[ "$network_name" == *data* || ! -e "$state_dir/data-network" ]]; then
                printf "%s\n" "$network_name" >"$state_dir/data-network"
            else
                printf "%s\n" "$network_name" >"$state_dir/control-network"
            fi
            printf "fake-network-id\n"
            exit 0
            ;;
        disconnect|connect)
            network_name=${3:-}
            container=${4:-}
            if [[ "${2:-}" != disconnect && "${2:-}" != connect ]]; then
                network_name=${2:-}
                container=${3:-}
            fi
            control_network=$(cat "$state_dir/control-network" 2>/dev/null || true)
            if [[ "${2:-}" == disconnect && "$network_name" == "$control_network" ]]; then
                printf "fake contract: control network cannot be disconnected\n" >&2
                exit 95
            fi
            if [[ "${2:-}" == disconnect ]]; then
                : >"$state_dir/disconnected-$container"
            else
                if [[ -e "$state_dir/disconnected-$container" && -e "$state_dir/compaction" ]]; then
                    : >"$state_dir/reconnected-after-compaction-$container"
                fi
                rm -f "$state_dir/disconnected-$container"
            fi
            exit 0
            ;;
        rm)
            exit 0
            ;;
    esac
fi

if [[ ${1:-} == run ]]; then
    if [[ "$*" == *"--entrypoint /usr/local/bin/etcdctl"* ]]; then
        if [[ ${FAKE_ETCDCTL_FAIL:-0} == 1 ]]; then
            exit 1
        fi
        subcommand=
        subcommand_index=0
        for ((i = 1; i <= $#; i++)); do
            argument=${!i}
            case "$argument" in
                endpoint|get|put|del|compact)
                    subcommand=$argument
                    subcommand_index=$i
                    break
                    ;;
            esac
        done
        case "$subcommand" in
            endpoint)
                if [[ -e ${FAKE_ETCD_STOPPED:?} ]]; then
                    exit 1
                fi
                printf "https://etcd:2379 is healthy\n"
                ;;
            get)
                printf "{\"header\":{\"revision\":%s},\"kvs\":[]}\n" "${FAKE_ETCD_REVISION:-42}"
                ;;
            put)
                key_index=$((subcommand_index + 1))
                value_index=$((subcommand_index + 2))
                record_etcd_put "${!key_index}" "${!value_index}"
                ;;
            del)
                key_index=$((subcommand_index + 1))
                record_etcd_delete "${!key_index}"
                ;;
            compact)
                revision_index=$((subcommand_index + 1))
                printf "%s\n" "${!revision_index}" >"$state_dir/compacted-revision"
                : >"$state_dir/compaction"
                printf "compacted revision %s\n" "${!revision_index}"
                ;;
            *)
                printf "fake contract: unknown etcdctl command\n" >&2
                exit 94
                ;;
        esac
        exit 0
    fi
    if [[ "$*" == *"--name=etcd0"* ]]; then
        if [[ "$*" != *"gcr.io/etcd-development/etcd:v3.6.13 /usr/local/bin/etcd --name=etcd0"* ]]; then
            printf "fake contract: etcd executable is missing\n" >&2
            exit 98
        fi
        if [[ "$*" == *"--trusted-ca-file="* ]]; then
            printf "fake contract: one-way TLS unexpectedly requires client certificates\n" >&2
            exit 99
        fi
        : >"$state_dir/etcd-started"
        printf "fake-etcd-container-id\n"
        exit 0
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
        if [[ ${FAKE_FAIL_GATEWAY:-0} == 1 ]]; then
            exit 42
        fi
        gateway_count=$(cat "$state_dir/gateway-count" 2>/dev/null || printf "0")
        gateway_count=$((gateway_count + 1))
        printf "%s\n" "$gateway_count" >"$state_dir/gateway-count"
        http_port=$((18079 + gateway_count))
        https_port=$((18442 + gateway_count))
        status_port=$((18784 + gateway_count))
        printf "%s\n" "$http_port" >"$state_dir/gateway-$container_name-http"
        printf "%s\n" "$https_port" >"$state_dir/gateway-$container_name-https"
        printf "%s\n" "$status_port" >"$state_dir/gateway-$container_name-status"
        : >"$state_dir/gateway-$container_name"
        printf "%s\n" "$container_name" >>"$state_dir/gateway-names"
        printf "fake-gateway-%s\n" "$gateway_count"
        exit 0
    fi
    if [[ "$*" == *"busybox:1.37.0"* ]]; then
        : >"$state_dir/upstream-$container_name"
        printf "fake-upstream-%s\n" "$container_name"
        exit 0
    fi
    printf "fake-container-id\n"
    exit 0
fi

case "${1:-}" in
    port)
        container=${2:-}
        requested_port=${3:-}
        case "$requested_port" in
            9080|9080/tcp) cat "$state_dir/gateway-$container-http" ;;
            9443|9443/tcp) cat "$state_dir/gateway-$container-https" ;;
            7085|7085/tcp) cat "$state_dir/gateway-$container-status" ;;
            *) exit 2 ;;
        esac
        printf "\n"
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
    restart)
        exit 0
        ;;
    exec)
        printf "apisix_http_status{code=\"200\",route=\"release-managed-route\"} 1\n"
        exit 0
        ;;
    logs)
        container=${!#}
        if [[ "$container" == *gateway* && -e "$state_dir/compaction" && \
            -e "$state_dir/reconnected-after-compaction-$container" ]]; then
            count=$(cat "$state_dir/gateway-log-count" 2>/dev/null || printf "0")
            printf "%s\n" "$((count + 1))" >"$state_dir/gateway-log-count"
            : >"$state_dir/compacted-log-$container"
            printf "etcdserver: mvcc: required revision has been compacted\n"
        else
            printf "fake logs output\n"
        fi
        exit 0
        ;;
    inspect)
        printf "fake inspect output\n"
        exit 0
        ;;
    rm|kill)
        exit 0
        ;;
esac
exit 0
'
}

write_fake_curl() {
    local bin=$1
    # The generated stub expands these variables when it runs.
    # shellcheck disable=SC2016
    write_stub "$bin/curl" '#!/usr/bin/env bash
set -euo pipefail
state_dir=${FAKE_STATE_DIR:-${TMPDIR:-/tmp}/apisix-etcd-recovery-fake}
mkdir -p "$state_dir"
printf "curl %s\n" "$*" >>"${FAKE_LOG:-/dev/null}"
if [[ ${FAKE_CURL_MODE:-happy} == timeout ]]; then
    if [[ ! -e "$state_dir/timeout-wait-started" ]]; then
        date +%s >"$state_dir/timeout-wait-started"
    fi
    exit 28
fi
body_file=
write_out=
url=
for ((i = 1; i <= $#; i++)); do
    argument=${!i}
    case "$argument" in
        --output|-o)
            next=$((i + 1))
            body_file=${!next}
            ;;
        --write-out|-w)
            next=$((i + 1))
            write_out=${!next}
            ;;
    esac
    case "$argument" in
        http://*|https://*)
            url=$argument
            ;;
    esac
done
[[ -n "$url" ]] || exit 2
url_hostport=${url#*://}
url_hostport=${url_hostport%%/*}
url_path=/${url#*://*/}
path=${url_path%%\?*}
port=${url_hostport##*:}
gateway=
protocol=http
if [[ "$url" == https://* ]]; then
    protocol=https
fi
for marker in "$state_dir"/gateway-*-$protocol; do
    [[ -f "$marker" ]] || continue
    if [[ $(cat "$marker") == "$port" ]]; then
        marker_name=${marker##*/}
        gateway=${marker_name#gateway-}
        gateway=${gateway%-$protocol}
        break
    fi
done
if [[ -z "$gateway" && "$protocol" == http ]]; then
    for marker in "$state_dir"/gateway-*-status; do
        [[ -f "$marker" ]] || continue
        if [[ $(cat "$marker") == "$port" ]]; then
            marker_name=${marker##*/}
            gateway=${marker_name#gateway-}
            gateway=${gateway%-status}
            protocol=status
            break
        fi
    done
fi
if [[ -z "$gateway" ]]; then
    printf "fake contract: unknown gateway port %s\n" "$port" >&2
    exit 7
fi
printf "%s %s %s\n" "$gateway" "$protocol" "$path" >>"$state_dir/gateway-probes"
if [[ -e "$state_dir/compaction" && -e "$state_dir/reconnected-after-compaction-$gateway" ]]; then
    printf "%s %s %s\n" "$gateway" "$protocol" "$path" >>"$state_dir/post-compaction-probes"
fi
if [[ "$url" == https://* && ! -e "$state_dir/ssl" ]]; then
    printf "\\n" >>"$state_dir/ssl-failure"
    printf "\\n" >>"$state_dir/ssl-failure-$gateway"
    printf "SSL handshake failed for release.example.test\n" >&2
    exit 35
fi
status=200
body=
case "$path" in
    /status/ready)
        body=ready
        if [[ -n "$gateway" && -e "$state_dir/disconnected-$gateway" ]]; then
            printf "\\n" >>"$state_dir/disconnected-ready"
            printf "\\n" >>"$state_dir/disconnected-ready-$gateway"
        fi
        ;;
    /status)
        body=live
        ;;
    /release-v1)
        if [[ ! -e "$state_dir/route-v1" ]]; then
            status=404
            body=not-found
        else
            body=v1
        fi
        ;;
    /release-v2)
        if [[ ! -e "$state_dir/route-v2" ]]; then
            status=404
            body=not-found
        else
            body=v2
        fi
        ;;
    /managed)
        if [[ ! -e "$state_dir/managed-route" ]]; then
            status=404
            body=not-found
        else
            body=$(cat "$state_dir/service-version" 2>/dev/null || printf "v1")
        fi
        ;;
    /invalid)
        status=404
        body=not-found
        ;;
    /stale)
        if [[ ! -e "$state_dir/stale-route" ]]; then
            status=404
            body=not-found
        else
            body=$(cat "$state_dir/service-version" 2>/dev/null || printf "v1")
        fi
        ;;
    *)
        status=404
        body=not-found
        ;;
esac
if [[ -n "$body_file" ]]; then
    printf "%s" "$body" >"$body_file"
else
    printf "%s" "$body"
fi
if [[ "$write_out" == *"%{http_code}"* ]]; then
    printf "%s" "$status"
fi
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
            FAKE_STATE_DIR="$test_root/mutable-state" \
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
    run_expect_failure "$output" env PATH="$bin:$original_path" \
        FAKE_LOG="$log" FAKE_IMAGE_ID="$image_id" \
        FAKE_STATE_DIR="$test_root/timeout-state" \
        FAKE_ETCD_STOPPED="$test_root/timeout-stopped" \
        FAKE_ETCD_RESTARTED="$test_root/timeout-restarted" \
        SOURCE_COMMIT="$source_commit" \
        FAKE_CURL_MODE=timeout RELEASE_EVIDENCE_ROOT="$evidence" \
        ETCD_RECOVERY_TIMEOUT_SECONDS=1 ETCD_RECOVERY_POLL_INTERVAL_SECONDS=4 \
        "$test_shell" "$runner" "$image_id"
    local wait_started elapsed
    if [[ ! -s "$test_root/timeout-state/timeout-wait-started" ]]; then
        sed -n '1,240p' "$output" >&2 || true
        fail 'timeout scenario exited before the bounded HTTP wait started'
    fi
    wait_started=$(cat "$test_root/timeout-state/timeout-wait-started")
    elapsed=$(($(date +%s) - wait_started))
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
        FAKE_STATE_DIR="$test_root/cleanup-state" \
        FAKE_ETCD_STOPPED="$test_root/cleanup-stopped" \
        FAKE_ETCD_RESTARTED="$test_root/cleanup-restarted" \
        SOURCE_COMMIT="$source_commit" \
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
    if ! env PATH="$bin:$original_path" \
        FAKE_LOG="$log" FAKE_IMAGE_ID="$image_id" \
        FAKE_STATE_DIR="$test_root/happy-state" \
        FAKE_ETCD_STOPPED="$test_root/happy-stopped" \
        FAKE_ETCD_RESTARTED="$test_root/happy-restarted" \
        SOURCE_COMMIT="$source_commit" \
        RELEASE_EVIDENCE_ROOT="$evidence" ETCD_RECOVERY_TIMEOUT_SECONDS=2 \
        "$test_shell" "$runner" "$image_id" >"$output" 2>&1; then
        sed -n '1,320p' "$output" >&2 || true
        fail 'happy-path runner failed'
    fi
    assert_contains 'etcd recovery smoke: PASS' "$output"
    local transcript
    transcript=$(find "$evidence" -type f -name steps.log -print -quit)
    [[ -n "$transcript" ]] || fail 'happy-path transcript was not preserved'
    assert_order "$transcript" \
        'generate temporary CA and etcd/gateway certificates' \
        'network create' \
        'start etcd 3.6.13' \
        'start upstream v1 and v2' \
        'write route/upstream v1' \
        'start APISIX-Go replicas as 10001:10001' \
        'replicas ready and proxy v1' \
        'write service and SSL resources' \
        'replicas converge on managed resource graph' \
        'reject invalid route generation and retain last-good' \
        'update service to upstream v2' \
        'replicas serve dynamic SSL' \
        'stop etcd' \
        'replicas retain last-good during etcd outage' \
        'restart etcd' \
        'replicas ready after recovery' \
        'update route to v2' \
        'replicas proxy route v2' \
        'seed route before compaction gap' \
        'disconnect replicas from etcd' \
        'mutate and compact etcd while replicas disconnected' \
        'reconnect replicas' \
        'replicas recover compacted snapshot consistently' \
        'restart one replica and recover committed journal state' \
        'delete SSL resource' \
        'replicas converge on live SSL delete' \
        're-add SSL resource' \
        'replicas converge on re-added SSL resource'
    assert_contains 'gcr.io/etcd-development/etcd:v3.6.13' "$log"
    assert_count 'docker network create' "$log" 2
    assert_count '--user 10001:10001' "$log" 2
    assert_count 'busybox:1.37.0' "$log" 2
    assert_count 'docker run --detach --name apisix-release-etcd-' "$log" 1
    assert_at_least 'docker port ' "$log" 6
    assert_contains 'SSL_CERT_FILE' "$log"
    assert_contains 'https://etcd:2379' "$log"
    assert_contains 'APISIXGO_DEPLOYMENT_ETCD_TLS_VERIFY=true' "$log"
    assert_contains 'APISIXGO_SECURITY_PROFILE=strict' "$log"
    assert_contains 'HOME=/usr/local/apisix' "$log"
    assert_contains 'APISIXGO_RUNTIME_PATHS_DATA_DIR=/usr/local/apisix/data' "$log"
    assert_contains 'APISIXGO_RUNTIME_PATHS_RUNTIME_DIR=/usr/local/apisix/run' "$log"
    assert_contains 'APISIXGO_RUNTIME_PATHS_LOG_DIR=/usr/local/apisix/logs' "$log"
    assert_contains 'APISIXGO_RUNTIME_PATHS_TEMP_DIR=/usr/local/apisix/tmp' "$log"
    assert_contains 'HTTP_PROXY=' "$log"
    assert_contains 'HTTPS_PROXY=' "$log"
    assert_contains 'NO_PROXY=' "$log"
    assert_contains 'APISIXGO_APISIX_STATUS_IP=0.0.0.0' "$log"
    for resource in \
        upstreams/release-upstream-v1 \
        upstreams/release-upstream-v2 \
        services/release-service \
        ssls/release-ssl \
        routes/release-route \
        routes/release-managed-route \
        routes/release-invalid-route \
        routes/release-stale-route; do
        assert_contains "put /apisix/$resource" "$log"
    done
    assert_at_least 'put /apisix/services/release-service' "$log" 2
    assert_at_least 'put /apisix/routes/release-route' "$log" 2
    assert_contains 'put /apisix/routes/release-invalid-route' "$log"
    for resource in \
        routes/release-stale-route \
        routes/release-invalid-route \
        ssls/release-ssl; do
        assert_contains "del /apisix/$resource" "$log"
    done
    assert_contains 'get /apisix --prefix --write-out=json' "$log"
    assert_contains 'compact 42' "$log"
    assert_contains 'docker restart apisix-release-gateway-a-' "$log"
    data_network=$(cat "$test_root/happy-state/data-network")
    control_network=$(cat "$test_root/happy-state/control-network")
    assert_count "docker network connect $control_network " "$log" 2
    assert_count "docker network disconnect $data_network " "$log" 2
    assert_count "docker network connect $data_network " "$log" 2
    if grep -Fq -- "docker network disconnect $control_network " "$log"; then
        fail 'compaction gap disconnected the control network'
    fi
    assert_at_least '--resolve release.example.test:' "$log" 2
    assert_at_least 'https://release.example.test:' "$log" 2
    disconnected_ready=$(wc -l <"$test_root/happy-state/disconnected-ready" 2>/dev/null || printf '0')
    (( disconnected_ready >= 2 )) || fail "wanted last-good readiness on both disconnected replicas, got $disconnected_ready"
    ssl_failures=$(wc -l <"$test_root/happy-state/ssl-failure" 2>/dev/null || printf '0')
    (( ssl_failures >= 2 )) || fail "wanted fresh TLS failure on both deleted SSL probes, got $ssl_failures"
    gateway_log_count=$(cat "$test_root/happy-state/gateway-log-count" 2>/dev/null || printf '0')
    (( gateway_log_count >= 4 )) || fail "wanted compaction logs from both replicas in addition to cleanup, got $gateway_log_count"
    gateway_name_count=$(sort -u "$test_root/happy-state/gateway-names" | wc -l | tr -d ' ')
    (( gateway_name_count == 2 )) || fail "wanted two distinct gateway names, got $gateway_name_count"
    while IFS= read -r gateway; do
        [[ -n "$gateway" ]] || continue
        [[ -s "$test_root/happy-state/disconnected-ready-$gateway" ]] || \
            fail "missing disconnected readiness probe for $gateway"
        [[ -e "$test_root/happy-state/reconnected-after-compaction-$gateway" ]] || \
            fail "missing post-compaction reconnect for $gateway"
        [[ -e "$test_root/happy-state/compacted-log-$gateway" ]] || \
            fail "missing compacted-revision log for $gateway"
        [[ -s "$test_root/happy-state/ssl-failure-$gateway" ]] || \
            fail "missing deleted-SSL failure probe for $gateway"
        assert_contains "$gateway status /status/ready" "$test_root/happy-state/post-compaction-probes"
        assert_contains "$gateway http /managed" "$test_root/happy-state/post-compaction-probes"
        assert_contains "$gateway http /release-v2" "$test_root/happy-state/post-compaction-probes"
        assert_at_least "$gateway https /release-v2" "$test_root/happy-state/post-compaction-probes" 2
    done < <(sort -u "$test_root/happy-state/gateway-names")
    local platform_evidence record_name record
    platform_evidence=$(dirname "$transcript")/platform-recovery-v1
    for record_name in journal generation; do
        record="$platform_evidence/$record_name.json"
        [[ -s "$record" ]] || fail "missing platform recovery evidence for $record_name"
        assert_contains '"scope":"platform-recovery-v1"' "$record"
        assert_contains '"config_profile":"http-data-plane-v1"' "$record"
        assert_contains "\"record\":\"$record_name\"" "$record"
        assert_contains "\"source_commit\":\"$source_commit\"" "$record"
        assert_contains "\"image_id\":\"$image_id\"" "$record"
        assert_contains '"config_sha256":"' "$record"
        assert_contains '"before_generation":"' "$record"
        assert_contains '"after_generation":"' "$record"
        assert_contains '"probe_result":"pass"' "$record"
        assert_contains '"output_sha256":"' "$record"
    done
    assert_order "$log" \
        'docker network create apisix-release-etcd-' \
        'docker run --detach --name apisix-release-etcd-' \
        'docker run --detach --name apisix-release-upstream-v1-' \
        'docker run --detach --name apisix-release-upstream-v2-' \
        'put /apisix/routes/release-route' \
        'docker run --detach --name apisix-release-gateway-' \
        'docker stop apisix-release-etcd-' \
        'docker start apisix-release-etcd-' \
        'docker network disconnect ' \
        'del /apisix/routes/release-stale-route' \
        'get /apisix --prefix --write-out=json' \
        'compact 42' \
        "docker network connect $data_network " \
        'docker restart apisix-release-gateway-a-' \
        'del /apisix/ssls/release-ssl' \
        'docker rm -f ' \
        'docker network rm '
    assert_contains "docker network rm $data_network" "$log"
    assert_contains "$control_network" "$log"
}

test_missing_tools
test_rejects_mutable_image
test_bounded_timeout
test_cleanup_on_failure
test_ordered_happy_path

printf 'etcd recovery fixture: PASS\n'
