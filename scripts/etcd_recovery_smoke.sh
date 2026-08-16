#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
etcd_image=gcr.io/etcd-development/etcd:v3.6.13

die() {
    printf 'etcd recovery smoke: %s\n' "$*" >&2
    return 1
}

require_command() {
    if ! command -v "$1" >/dev/null 2>&1; then
        die "required command is unavailable: $1"
    fi
}

if (( $# != 1 )); then
    printf 'usage: %s IMMUTABLE_IMAGE_ID_OR_REFERENCE\n' "$0" >&2
    exit 2
fi

image=$1
for command_name in docker curl openssl; do
    require_command "$command_name"
done

if [[ ! "$image" =~ ^sha256:[0-9a-f]{64}$ &&
    ! "$image" =~ ^([a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?/)*[a-z0-9]+([._-][a-z0-9]+)*@sha256:[0-9a-f]{64}$ ]]; then
    die 'image must be an immutable image ID or digest-qualified reference'
fi

if ! image_id=$(docker image inspect --format '{{.Id}}' "$image"); then
    die "immutable image is not available locally: $image"
fi
if [[ -z "$image_id" ]]; then
    die "Docker returned an empty image ID for: $image"
fi

timeout_seconds=${ETCD_RECOVERY_TIMEOUT_SECONDS:-90}
poll_interval=${ETCD_RECOVERY_POLL_INTERVAL_SECONDS:-1}
curl_timeout=${ETCD_RECOVERY_CURL_TIMEOUT_SECONDS:-5}
etcdctl_timeout=${ETCD_RECOVERY_ETCDCTL_TIMEOUT_SECONDS:-5}
for setting in timeout_seconds curl_timeout etcdctl_timeout; do
    value=${!setting}
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || die "$setting must be a positive integer"
done
[[ "$poll_interval" =~ ^[0-9]+$ ]] || die 'poll_interval must be a non-negative integer'

run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}"
evidence_root=${RELEASE_EVIDENCE_ROOT:-"$repo_root/.cache/release-evidence/etcd-recovery"}
run_dir="$evidence_root/$run_id"
mkdir -p "$run_dir"
transcript="$run_dir/steps.log"
: >"$transcript"

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/apisix-etcd-recovery.XXXXXX")
tls_dir="$temp_dir/tls"
mkdir -p "$tls_dir"

network="apisix-release-etcd-$run_id"
etcd_container="apisix-release-etcd-$run_id"
upstream_v1_container="apisix-release-upstream-v1-$run_id"
upstream_v2_container="apisix-release-upstream-v2-$run_id"
gateway_container="apisix-release-gateway-$run_id"
upstream_v1_alias="release-upstream-v1-$run_id"
upstream_v2_alias="release-upstream-v2-$run_id"
route_id=release-route
upstream_id=release-upstream
etcd_prefix=/apisix
gateway_port=

record_step() {
    printf '%s\n' "$*" >>"$transcript"
}

cleanup() {
    local status=$?
    set +e
    if [[ -n ${run_dir:-} ]]; then
        printf 'cleanup exit=%s\n' "$status" >>"$transcript"
        for container in "$gateway_container" "$upstream_v2_container" "$upstream_v1_container" "$etcd_container"; do
            docker logs "$container" >"$run_dir/${container}.logs" 2>&1 || true
            docker inspect "$container" >"$run_dir/${container}.inspect" 2>&1 || true
        done
        printf 'cleanup: evidence preserved at %s\n' "$run_dir" >&2
    fi
    docker rm -f "$gateway_container" "$upstream_v2_container" "$upstream_v1_container" "$etcd_container" \
        >"$run_dir/cleanup.log" 2>&1 || true
    docker network rm "$network" >>"$run_dir/cleanup.log" 2>&1 || true
    rm -rf "$temp_dir"
    trap - EXIT
    exit "$status"
}

# The trap is installed before the first Docker mutation below.
trap cleanup EXIT

record_step 'generate temporary CA and DNS etcd certificate'
cat >"$temp_dir/server.ext" <<'EOF'
subjectAltName = DNS:etcd
extendedKeyUsage = serverAuth
EOF
openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$tls_dir/ca.key" -out "$tls_dir/ca.crt" -days 1 \
    -subj '/CN=apisix-go-etcd-recovery-ca'
openssl req -newkey rsa:2048 -nodes \
    -keyout "$tls_dir/server.key" -out "$temp_dir/server.csr" \
    -subj '/CN=etcd'
openssl x509 -req -in "$temp_dir/server.csr" \
    -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" -CAcreateserial \
    -out "$tls_dir/server.crt" -days 1 -extfile "$temp_dir/server.ext"
chmod 0644 "$tls_dir/ca.crt" "$tls_dir/server.crt"

sleep_until_poll() {
    local deadline=$1
    local remaining=$((deadline - SECONDS))
    local delay=$poll_interval
    if (( remaining <= 0 )); then
        return
    fi
    if (( delay > remaining )); then
        delay=$remaining
    fi
    sleep "$delay"
}

wait_http_status() {
    local label=$1
    local url=$2
    local expected=$3
    local body_file="$run_dir/$label.body"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout
    local remaining
    local status=
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then
            request_timeout=$remaining
        fi
        status=$(curl --silent --show-error \
            --connect-timeout "$request_timeout" --max-time "$request_timeout" \
            --output "$body_file" --write-out '%{http_code}' "$url" \
            2>>"$transcript") || status=
        if [[ "$status" == "$expected" ]]; then
            return 0
        fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted HTTP $expected)"
}

wait_http_body() {
    local label=$1
    local url=$2
    local expected=$3
    local body_file="$run_dir/$label.body"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout
    local remaining
    local status=
    local body=
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then
            request_timeout=$remaining
        fi
        status=$(curl --silent --show-error \
            --connect-timeout "$request_timeout" --max-time "$request_timeout" \
            --output "$body_file" --write-out '%{http_code}' "$url" \
            2>>"$transcript") || status=
        if [[ "$status" == 200 && -f "$body_file" ]]; then
            body=$(tr -d '\r\n' <"$body_file")
            if [[ "$body" == "$expected" ]]; then
                return 0
            fi
        fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted body $expected)"
}

etcdctl_with_timeout() {
    local command_timeout=$1
    shift
    docker run --rm --network "$network" \
        --entrypoint /usr/local/bin/etcdctl \
        --volume "$tls_dir:/etc/etcd/tls:ro" \
        --env ETCDCTL_API=3 "$etcd_image" \
        --endpoints=https://etcd:2379 \
        --cacert=/etc/etcd/tls/ca.crt \
        --dial-timeout="${command_timeout}s" \
        --command-timeout="${command_timeout}s" "$@"
}

etcdctl() {
    etcdctl_with_timeout "$etcdctl_timeout" "$@"
}

wait_etcd() {
    local label=$1
    local deadline=$((SECONDS + timeout_seconds))
    local command_timeout
    local remaining
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        command_timeout=$etcdctl_timeout
        if (( command_timeout > remaining )); then
            command_timeout=$remaining
        fi
        if etcdctl_with_timeout "$command_timeout" endpoint health \
            >>"$run_dir/etcdctl.log" 2>&1; then
            return 0
        fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for etcd $label"
}

start_upstream() {
    local name=$1
    local alias=$2
    local body=$3
    local path=$4
    docker run --detach --name "$name" --network "$network" \
        --network-alias "$alias" busybox:1.37.0 sh -c \
        "mkdir -p /www && printf '%s' '$body' >/www/$path && exec httpd -f -p 8081 -h /www" \
        >/dev/null
}

etcdctl_put() {
    local key=$1
    local value=$2
    etcdctl put "$key" "$value" >>"$run_dir/etcdctl.log" 2>&1
}

route_v1=$(printf '{"id":"%s","status":1,"uri":"/release-v1","upstream_id":"%s"}' "$route_id" "$upstream_id")
route_v2=$(printf '{"id":"%s","status":1,"uri":"/release-v2","upstream_id":"%s"}' "$route_id" "$upstream_id")
upstream_v1_config=$(printf '{"nodes":{"%s:8081":1},"type":"roundrobin"}' "$upstream_v1_alias")
upstream_v2_config=$(printf '{"nodes":{"%s:8081":1},"type":"roundrobin"}' "$upstream_v2_alias")

record_step 'network create'
docker network create "$network" >/dev/null

record_step 'start etcd 3.6.13'
docker run --detach --name "$etcd_container" --network "$network" --network-alias etcd \
    --volume "$tls_dir:/etc/etcd/tls:ro" "$etcd_image" \
    /usr/local/bin/etcd \
    --name=etcd0 --data-dir=/etcd-data \
    --listen-client-urls=https://0.0.0.0:2379 \
    --advertise-client-urls=https://etcd:2379 \
    --listen-peer-urls=http://0.0.0.0:2380 \
    --initial-advertise-peer-urls=http://etcd:2380 \
    --initial-cluster=etcd0=http://etcd:2380 \
    --initial-cluster-state=new \
    --cert-file=/etc/etcd/tls/server.crt \
    --key-file=/etc/etcd/tls/server.key \
    >/dev/null
wait_etcd 'startup'

record_step 'start upstream v1'
start_upstream "$upstream_v1_container" "$upstream_v1_alias" v1 release-v1

record_step 'write route/upstream v1'
etcdctl_put "$etcd_prefix/upstreams/$upstream_id" "$upstream_v1_config"
etcdctl_put "$etcd_prefix/routes/$route_id" "$route_v1"

record_step 'start APISIX-Go as 10001:10001'
docker run --detach --name "$gateway_container" --network "$network" \
    --publish 127.0.0.1::9080 --user 10001:10001 \
    --env SSL_CERT_FILE=/etc/ssl/certs/etcd-ca.crt \
    --env APISIXGO_DEPLOYMENT_ETCD_HOST=https://etcd:2379 \
    --volume "$repo_root/conf/config-production.yaml:/usr/local/apisix/conf/config-production.yaml:ro" \
    --volume "$tls_dir/ca.crt:/etc/ssl/certs/etcd-ca.crt:ro" \
    "$image" -c /usr/local/apisix/conf/config-production.yaml \
    >/dev/null
published=$(docker port "$gateway_container" 9080/tcp)
gateway_port=${published##*:}
gateway_url="http://127.0.0.1:$gateway_port"

wait_http_status 'livez' "$gateway_url/livez" 200
record_step 'livez 200'
wait_http_status 'readyz' "$gateway_url/readyz" 200
record_step 'readyz 200'
wait_http_body 'release-v1' "$gateway_url/release-v1" v1
record_step 'proxy /release-v1 -> v1'

record_step 'stop etcd'
docker stop "$etcd_container" >/dev/null
wait_http_status 'readyz-degraded' "$gateway_url/readyz" 503
record_step 'readyz 503'
wait_http_status 'livez-degraded' "$gateway_url/livez" 200
record_step 'livez 200 during etcd outage'
wait_http_body 'last-good-release-v1' "$gateway_url/release-v1" v1
record_step 'last-good /release-v1 -> v1'

record_step 'restart etcd'
docker start "$etcd_container" >/dev/null
wait_etcd 'recovery'
wait_http_status 'readyz-after-recovery' "$gateway_url/readyz" 200
record_step 'readyz 200 after recovery'

record_step 'start upstream v2'
docker rm -f "$upstream_v1_container" >/dev/null 2>&1 || true
start_upstream "$upstream_v2_container" "$upstream_v2_alias" v2 release-v2

record_step 'update same route/upstream IDs to v2'
etcdctl_put "$etcd_prefix/upstreams/$upstream_id" "$upstream_v2_config"
etcdctl_put "$etcd_prefix/routes/$route_id" "$route_v2"
wait_http_body 'release-v2' "$gateway_url/release-v2" v2
record_step 'proxy /release-v2 -> v2'

printf 'etcd recovery smoke: PASS\n'
