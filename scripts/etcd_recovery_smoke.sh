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
container_bin=${CONTAINER_BIN:-docker}
for command_name in "$container_bin" curl openssl; do
    require_command "$command_name"
done

docker() {
    command "$container_bin" "$@"
}

probe_mode=${ETCD_RECOVERY_PROBE_MODE:-auto}
if [[ "$probe_mode" == auto ]]; then
    if [[ $(basename "$container_bin") == podman ]] && docker machine ssh true >/dev/null 2>&1; then
        probe_mode=podman-machine
    else
        probe_mode=host
    fi
fi
[[ "$probe_mode" == host || "$probe_mode" == podman-machine ]] || \
    die 'ETCD_RECOVERY_PROBE_MODE must be auto, host, or podman-machine'

curl() {
    if [[ "$probe_mode" == podman-machine ]]; then
        docker machine ssh -- curl "$@"
        return
    fi
    command curl "$@"
}

if [[ ! "$image" =~ ^sha256:[0-9a-f]{64}$ &&
    ! "$image" =~ ^([a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?/)*[a-z0-9]+([._-][a-z0-9]+)*@sha256:[0-9a-f]{64}$ ]]; then
    die 'image must be an immutable image ID or digest-qualified reference'
fi

if ! image_id=$(docker image inspect --format '{{.Id}}' "$image"); then
    die "immutable image is not available locally: $image"
fi
if [[ -z "$image_id" ]]; then
    die "container runtime returned an empty image ID for: $image"
fi
if [[ "$image_id" =~ ^[0-9a-f]{64}$ ]]; then
    image_id="sha256:$image_id"
fi
[[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || die "container runtime returned an invalid image ID for: $image"

source_commit=${SOURCE_COMMIT:-}
[[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] || die 'SOURCE_COMMIT must be the exact 40-character candidate source commit'
production_config="$repo_root/conf/config-production.yaml"

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
evidence_root=${CANDIDATE_EVIDENCE_ROOT:-"$repo_root/.cache/candidate-evidence/etcd-recovery"}
run_dir="$evidence_root/$run_id"
mkdir -p "$run_dir"
transcript="$run_dir/steps.log"
: >"$transcript"
survivor_monitor_pid=
survivor_monitor_stop="$run_dir/survivor-monitor.stop"
survivor_monitor_failure="$run_dir/survivor-monitor.failed"
survivor_probe_count_file="$run_dir/survivor-monitor.count"
survivor_monitor_started="$run_dir/survivor-monitor.started"
survivor_monitor_window="$run_dir/survivor-monitor.window"
survivor_window_probe_count_file="$run_dir/survivor-monitor.window.count"
restart_before_identity=
restart_after_identity=
restart_survivor_probe_count=0
restart_survivor_window_probe_count=0

if [[ "$probe_mode" == podman-machine ]]; then
    mkdir -p "$repo_root/.cache/tmp"
    temp_dir=$(mktemp -d "$repo_root/.cache/tmp/apisix-etcd-recovery.XXXXXX")
else
    temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/apisix-etcd-recovery.XXXXXX")
fi
tls_dir="$temp_dir/tls"
mkdir -p "$tls_dir"
candidate_config="$production_config"
config_sha256=$(openssl dgst -sha256 "$candidate_config" | awk '{print $NF}')
[[ "$config_sha256" =~ ^[0-9a-f]{64}$ ]] || die 'failed to fingerprint validation candidate configuration'

data_network="apisix-release-etcd-data-$run_id"
control_network="apisix-release-etcd-control-$run_id"
etcd_container="apisix-release-etcd-$run_id"
etcd_proxy_container="apisix-release-etcd-proxy-$run_id"
upstream_v1_container="apisix-release-upstream-v1-$run_id"
upstream_v2_container="apisix-release-upstream-v2-$run_id"
gateway_a_container="apisix-release-gateway-a-$run_id"
gateway_b_container="apisix-release-gateway-b-$run_id"
gateway_containers=("$gateway_a_container" "$gateway_b_container")
upstream_v1_alias="release-upstream-v1-$run_id"
upstream_v2_alias="release-upstream-v2-$run_id"
route_id=release-route
managed_route_id=release-managed-route
invalid_route_id=release-invalid-route
stale_route_id=release-stale-route
upstream_v1_id=release-upstream-v1
upstream_v2_id=release-upstream-v2
service_id=release-service
ssl_id=release-ssl
etcd_prefix=/apisix
gateway_http_ports=()
gateway_tls_ports=()
gateway_status_ports=()
gateway_urls=()
gateway_tls_urls=()
gateway_status_urls=()

record_step() {
    printf '%s\n' "$*" >>"$transcript"
}

cleanup() {
    local status=$?
    set +e
    if [[ -n "$survivor_monitor_pid" ]]; then
        : >"$survivor_monitor_stop"
        wait "$survivor_monitor_pid" >/dev/null 2>&1 || true
    fi
    if [[ -n ${run_dir:-} ]]; then
        printf 'cleanup exit=%s\n' "$status" >>"$transcript"
        for container in "${gateway_containers[@]}" "$upstream_v2_container" "$upstream_v1_container" \
            "$etcd_proxy_container" "$etcd_container"; do
            docker logs "$container" >"$run_dir/${container}.logs" 2>&1 || true
            docker inspect "$container" >"$run_dir/${container}.inspect" 2>&1 || true
        done
        printf 'cleanup: evidence preserved at %s\n' "$run_dir" >&2
    fi
    docker rm -f "${gateway_containers[@]}" "$upstream_v2_container" "$upstream_v1_container" \
        "$etcd_proxy_container" "$etcd_container" \
        >"$run_dir/cleanup.log" 2>&1 || true
    docker network rm "$data_network" "$control_network" >>"$run_dir/cleanup.log" 2>&1 || true
    rm -rf "$temp_dir"
    trap - EXIT
    exit "$status"
}

# The trap is installed before the first Docker mutation below.
trap cleanup EXIT

record_step 'generate temporary CA and etcd/gateway certificates'
cat >"$temp_dir/etcd-server.ext" <<'EOF'
subjectAltName = DNS:etcd,DNS:etcd-origin
extendedKeyUsage = serverAuth
EOF
cat >"$temp_dir/frontend.ext" <<'EOF'
subjectAltName = DNS:release.example.test
extendedKeyUsage = serverAuth
EOF
openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$tls_dir/ca.key" -out "$tls_dir/ca.crt" -days 1 \
    -subj '/CN=apisix-go-etcd-recovery-ca'
openssl req -newkey rsa:2048 -nodes \
    -keyout "$tls_dir/server.key" -out "$temp_dir/server.csr" \
    -subj '/CN=etcd'
openssl x509 -req -in "$temp_dir/server.csr" \
    -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" \
    -CAserial "$temp_dir/ca.srl" -CAcreateserial \
    -out "$tls_dir/server.crt" -days 1 -extfile "$temp_dir/etcd-server.ext"
openssl req -newkey rsa:2048 -nodes \
    -keyout "$tls_dir/frontend.key" -out "$temp_dir/frontend.csr" \
    -subj '/CN=release.example.test'
openssl x509 -req -in "$temp_dir/frontend.csr" \
    -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" \
    -CAserial "$temp_dir/ca.srl" \
    -out "$tls_dir/frontend.crt" -days 1 -extfile "$temp_dir/frontend.ext"
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
    shift 3
    local body_file="$run_dir/$label.body"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout remaining status=
    local curl_args=("$@")
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        status=$(curl --silent --show-error --connect-timeout "$request_timeout" \
            --max-time "$request_timeout" "${curl_args[@]}" --output "$body_file" \
            --write-out '%{http_code}' "$url" 2>>"$transcript") || status=
        if [[ "$status" == "$expected" ]]; then return 0; fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted HTTP $expected)"
}

wait_http_body() {
    local label=$1
    local url=$2
    local expected=$3
    shift 3
    local body_file="$run_dir/$label.body"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout remaining status body
    local curl_args=("$@")
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        status=$(curl --silent --show-error --connect-timeout "$request_timeout" \
            --max-time "$request_timeout" "${curl_args[@]}" --output "$body_file" \
            --write-out '%{http_code}' "$url" 2>>"$transcript") || status=
        if [[ "$status" == 200 && -f "$body_file" ]]; then
            body=$(tr -d '\r\n' <"$body_file")
            if [[ "$body" == "$expected" ]]; then return 0; fi
        fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted body $expected)"
}

probe_survivor_once() {
    local status_url=$1 route_url=$2
    local status body
    status=$(curl --silent --show-error --connect-timeout "$curl_timeout" \
        --max-time "$curl_timeout" --output /dev/null --write-out '%{http_code}' \
        "$status_url/status/ready" 2>>"$transcript") || return 1
    [[ "$status" == 200 ]] || return 1
    body=$(curl --fail --silent --show-error --connect-timeout "$curl_timeout" \
        --max-time "$curl_timeout" "$route_url/managed" 2>>"$transcript") || return 1
    [[ "$(printf '%s' "$body" | tr -d '\r\n')" == v1 ]]
}

start_survivor_monitor() {
    local survivor_index=$1
    rm -f "$survivor_monitor_stop" "$survivor_monitor_failure" \
        "$survivor_monitor_started" "$survivor_monitor_window"
    : >"$survivor_probe_count_file"
    printf '0\n' >"$survivor_window_probe_count_file"
    if ! probe_survivor_once \
        "${gateway_status_urls[survivor_index]}" "${gateway_urls[survivor_index]}"; then
        die 'surviving replica was unavailable before restart'
    fi
    printf '1\n' >"$survivor_probe_count_file"
    (
        local count=1 window_count=0
        while [[ ! -e "$survivor_monitor_stop" ]]; do
            if ! probe_survivor_once \
                "${gateway_status_urls[survivor_index]}" "${gateway_urls[survivor_index]}"; then
                : >"$survivor_monitor_failure"
                exit 1
            fi
            count=$((count + 1))
            printf '%s\n' "$count" >"$survivor_probe_count_file"
            if [[ -e "$survivor_monitor_window" ]]; then
                window_count=$((window_count + 1))
                printf '%s\n' "$window_count" >"$survivor_window_probe_count_file"
            fi
            printf '1\n' >"$survivor_monitor_started"
            sleep "$poll_interval"
        done
    ) &
    survivor_monitor_pid=$!

    local deadline=$((SECONDS + timeout_seconds))
    while [[ ! -e "$survivor_monitor_started" ]]; do
        [[ ! -e "$survivor_monitor_failure" ]] || die 'surviving replica failed while starting restart monitor'
        kill -0 "$survivor_monitor_pid" 2>/dev/null || die 'surviving replica restart monitor exited before startup handshake'
        (( SECONDS < deadline )) || die 'timed out waiting for surviving replica restart monitor startup'
        sleep 0.01
    done
}

wait_for_survivor_window_probe() {
    local deadline=$((SECONDS + timeout_seconds)) count
    while true; do
        count=$(<"$survivor_window_probe_count_file")
        if [[ "$count" =~ ^[1-9][0-9]*$ ]]; then
            return
        fi
        [[ ! -e "$survivor_monitor_failure" ]] || die 'surviving replica failed during restart'
        kill -0 "$survivor_monitor_pid" 2>/dev/null || die 'surviving replica restart monitor exited during restart'
        (( SECONDS < deadline )) || die 'timed out waiting for surviving replica probe during restart'
        sleep 0.01
    done
}

stop_survivor_monitor() {
    : >"$survivor_monitor_stop"
    if ! wait "$survivor_monitor_pid"; then
        survivor_monitor_pid=
        die 'surviving replica failed during restart'
        return
    fi
    survivor_monitor_pid=
    [[ ! -e "$survivor_monitor_failure" ]] || die 'surviving replica failed during restart'
    restart_survivor_probe_count=$(<"$survivor_probe_count_file")
    restart_survivor_window_probe_count=$(<"$survivor_window_probe_count_file")
    if [[ ! "$restart_survivor_probe_count" =~ ^[0-9]+$ ]] || \
        (( restart_survivor_probe_count < 2 )); then
        die 'survivor restart probe count is invalid'
    fi
    [[ "$restart_survivor_window_probe_count" =~ ^[1-9][0-9]*$ ]] || \
        die 'survivor restart window probe count is invalid'
    rm -f "$survivor_monitor_window"
}

wait_https_body() {
    local label=$1
    local url=$2
    local expected=$3
    local ca_file=$4
    local resolve=$5
    local body_file="$run_dir/$label.body"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout remaining status body
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        status=$(curl --silent --show-error --noproxy '*' --cacert "$ca_file" --resolve "$resolve" \
            --connect-timeout "$request_timeout" --max-time "$request_timeout" \
            --output "$body_file" --write-out '%{http_code}' "$url" 2>>"$transcript") || status=
        if [[ "$status" == 200 && -f "$body_file" ]]; then
            body=$(tr -d '\r\n' <"$body_file")
            if [[ "$body" == "$expected" ]]; then return 0; fi
        fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted HTTPS body $expected)"
}

wait_https_failure() {
    local label=$1
    local url=$2
    local ca_file=$3
    local resolve=$4
    local body_file="$run_dir/$label.body"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout remaining status
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        if curl --silent --show-error --noproxy '*' --cacert "$ca_file" --resolve "$resolve" \
            --connect-timeout "$request_timeout" --max-time "$request_timeout" \
            --output "$body_file" "$url" 2>>"$transcript"; then
            status=0
        else
            status=$?
        fi
        if (( status == 35 )); then return 0; fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted HTTPS handshake failure)"
}

wait_gateway_status() {
    local label=$1 path=$2 expected=$3
    shift 3
    local curl_args=("$@") index
    for index in "${!gateway_containers[@]}"; do
        wait_http_status "$label-replica-$((index + 1))" "${gateway_urls[index]}$path" "$expected" \
            "${curl_args[@]}"
    done
}

wait_gateway_probe() {
    local label=$1 path=$2 expected=$3
    local index
    for index in "${!gateway_containers[@]}"; do
        wait_http_status "$label-replica-$((index + 1))" "${gateway_status_urls[index]}$path" "$expected"
    done
}

wait_gateway_body() {
    local label=$1 path=$2 expected=$3
    shift 3
    local curl_args=("$@") index
    for index in "${!gateway_containers[@]}"; do
        wait_http_body "$label-replica-$((index + 1))" "${gateway_urls[index]}$path" "$expected" \
            "${curl_args[@]}"
    done
}

wait_gateway_tls_body() {
    local label=$1 path=$2 expected=$3 index
    for index in "${!gateway_containers[@]}"; do
        wait_https_body "$label-replica-$((index + 1))" "${gateway_tls_urls[index]}$path" "$expected" \
            "$tls_dir/ca.crt" "release.example.test:${gateway_tls_ports[index]}:127.0.0.1"
    done
}

wait_gateway_tls_failure() {
    local label=$1 path=$2 index
    for index in "${!gateway_containers[@]}"; do
        wait_https_failure "$label-replica-$((index + 1))" "${gateway_tls_urls[index]}$path" \
            "$tls_dir/ca.crt" "release.example.test:${gateway_tls_ports[index]}:127.0.0.1"
    done
}

etcdctl_with_timeout() {
    local command_timeout=$1
    shift
    docker run --rm --network "$data_network" \
        --entrypoint /usr/local/bin/etcdctl \
        --volume "$tls_dir:/etc/etcd/tls:ro" \
        --env ETCDCTL_API=3 "$etcd_image" \
        --endpoints=https://etcd-origin:2379 \
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
    local command_timeout remaining
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        command_timeout=$etcdctl_timeout
        if (( command_timeout > remaining )); then command_timeout=$remaining; fi
        if etcdctl_with_timeout "$command_timeout" endpoint health >>"$run_dir/etcdctl.log" 2>&1; then
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
    docker run --detach --name "$name" --network "$data_network" \
        --network-alias "$alias" busybox:1.37.0 sh -c \
        "mkdir -p /www && printf '%s' '$body' >/www/managed && printf '%s' '$body' >/www/release-v1 && printf '%s' '$body' >/www/release-v2 && printf '%s' '$body' >/www/stale && exec httpd -f -p 8081 -h /www" \
        >/dev/null
}

etcdctl_put() {
    local key=$1
    local value=$2
    etcdctl put "$key" "$value" >>"$run_dir/etcdctl.log" 2>&1
}

etcdctl_delete() {
    local key=$1
    etcdctl del "$key" >>"$run_dir/etcdctl.log" 2>&1
}

current_etcd_revision() {
    local snapshot current
    snapshot=$(etcdctl get "$etcd_prefix" --prefix --write-out=json 2>>"$run_dir/etcdctl.log")
    current=$(printf '%s\n' "$snapshot" | sed -n 's/.*"revision":[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n 1)
    [[ "$current" =~ ^[1-9][0-9]*$ ]] || die 'etcd snapshot did not contain a current revision'
    printf '%s' "$current"
}

write_recovery_evidence() {
    local before_generation=$1 after_generation=$2
    local evidence_dir="$run_dir/platform-recovery-v1" output_sha256 ca_sha256 server_cert_sha256 record_name
    mkdir -p "$evidence_dir"
    output_sha256=$(openssl dgst -sha256 "$transcript" | awk '{print $NF}')
    ca_sha256=$(openssl dgst -sha256 "$tls_dir/ca.crt" | awk '{print $NF}')
    server_cert_sha256=$(openssl dgst -sha256 "$tls_dir/server.crt" | awk '{print $NF}')
    for record_name in journal generation; do
        printf '{"scope":"platform-recovery-v1","record":"%s","source_commit":"%s","image_id":"%s","config_sha256":"%s","before_generation":"%s","after_generation":"%s","probe_result":"pass","etcd_tls_peer":"etcd:2379","etcd_ca_sha256":"%s","etcd_server_cert_sha256":"%s","replica_before_identity":"%s","replica_after_identity":"%s","survivor_probe_count":"%s","survivor_window_probe_count":"%s","command":"scripts/etcd_recovery_smoke.sh <immutable-image>","attempt":"%s","output_sha256":"%s"}\n' \
            "$record_name" "$source_commit" "$image_id" "$config_sha256" "$before_generation" "$after_generation" \
            "$ca_sha256" "$server_cert_sha256" "$restart_before_identity" "$restart_after_identity" \
            "$restart_survivor_probe_count" "$restart_survivor_window_probe_count" \
            "$run_id" "$output_sha256" >"$evidence_dir/$record_name.json"
    done
}

json_string_from_file() {
    awk 'BEGIN { printf "\"" } { printf "%s\\n", $0 } END { printf "\"" }' "$1"
}

route_v1=$(printf '{"id":"%s","status":1,"uri":"/release-v1","upstream_id":"%s"}' \
    "$route_id" "$upstream_v1_id")
route_v2=$(printf '{"id":"%s","status":1,"uri":"/release-v2","upstream_id":"%s"}' \
    "$route_id" "$upstream_v2_id")
managed_route=$(printf '{"id":"%s","status":1,"uri":"/managed","service_id":"%s"}' \
    "$managed_route_id" "$service_id")
invalid_route=$(printf '{"id":"%s","status":1,"uri":"/invalid","methods":["BOGUS"]}' \
    "$invalid_route_id")
stale_route=$(printf '{"id":"%s","status":1,"uri":"/stale","service_id":"%s"}' \
    "$stale_route_id" "$service_id")
upstream_v1_config=$(printf '{"nodes":{"%s:8081":1},"type":"roundrobin"}' "$upstream_v1_alias")
upstream_v2_config=$(printf '{"nodes":{"%s:8081":1},"type":"roundrobin"}' "$upstream_v2_alias")
service_v1=$(printf '{"id":"%s","upstream_id":"%s"}' "$service_id" "$upstream_v1_id")
service_v2=$(printf '{"id":"%s","upstream_id":"%s"}' "$service_id" "$upstream_v2_id")
frontend_cert_json=$(json_string_from_file "$tls_dir/frontend.crt")
frontend_key_json=$(json_string_from_file "$tls_dir/frontend.key")
ssl_config=$(printf '{"id":"%s","snis":["release.example.test"],"cert":%s,"key":%s,"status":1}' \
    "$ssl_id" "$frontend_cert_json" "$frontend_key_json")

record_step 'network create'
docker network create "$data_network" >/dev/null
docker network create "$control_network" >/dev/null

record_step 'start etcd 3.6.13'
docker run --detach --name "$etcd_container" --network "$data_network" --network-alias etcd-origin \
    --volume "$tls_dir:/etc/etcd/tls:ro" "$etcd_image" \
    /usr/local/bin/etcd \
    --name=etcd0 --data-dir=/etcd-data \
    --listen-client-urls=https://0.0.0.0:2379 \
    --advertise-client-urls=https://etcd-origin:2379 \
    --listen-peer-urls=http://0.0.0.0:2380 \
    --initial-advertise-peer-urls=http://etcd:2380 \
    --initial-cluster=etcd0=http://etcd:2380 \
    --initial-cluster-state=new \
    --cert-file=/etc/etcd/tls/server.crt \
    --key-file=/etc/etcd/tls/server.key \
    >/dev/null
wait_etcd 'startup'

record_step 'start etcd TCP gateway'
	docker run --detach --name "$etcd_proxy_container" --network "$data_network" --network-alias etcd \
		--entrypoint /usr/local/bin/etcd "$etcd_image" \
		gateway start --listen-addr=0.0.0.0:2379 --endpoints=etcd-origin:2379 --retry-delay=1s \
		>/dev/null

record_step 'start upstream v1 and v2'
start_upstream "$upstream_v1_container" "$upstream_v1_alias" v1
start_upstream "$upstream_v2_container" "$upstream_v2_alias" v2

record_step 'write route/upstream v1'
etcdctl_put "$etcd_prefix/upstreams/$upstream_v1_id" "$upstream_v1_config"
etcdctl_put "$etcd_prefix/routes/$route_id" "$route_v1"

record_step 'start APISIX-Go replicas as 10001:10001'
for gateway_container in "${gateway_containers[@]}"; do
    docker run --detach --name "$gateway_container" --network "$data_network" \
        --publish 127.0.0.1::9080 --publish 127.0.0.1::9443 --publish 127.0.0.1::7085 \
        --user 10001:10001 \
        --env SSL_CERT_FILE=/etc/ssl/certs/etcd-ca.crt \
        --env APISIXGO_DEPLOYMENT_ETCD_HOST=https://etcd:2379 \
        --env APISIXGO_DEPLOYMENT_ETCD_TLS_VERIFY=true \
        --env APISIXGO_APISIX_STATUS_IP=0.0.0.0 \
        --env HOME=/usr/local/apisix \
        --env APISIXGO_RUNTIME_PATHS_DATA_DIR=/usr/local/apisix/data \
        --env APISIXGO_RUNTIME_PATHS_RUNTIME_DIR=/usr/local/apisix/run \
        --env APISIXGO_RUNTIME_PATHS_LOG_DIR=/usr/local/apisix/logs \
        --env APISIXGO_RUNTIME_PATHS_TEMP_DIR=/usr/local/apisix/tmp \
        --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY= \
        --env http_proxy= --env https_proxy= --env all_proxy= --env no_proxy= \
        --volume "$candidate_config:/usr/local/apisix/conf/config-production.yaml:ro" \
        --volume "$tls_dir/ca.crt:/etc/ssl/certs/etcd-ca.crt:ro" \
        "$image" -c /usr/local/apisix/conf/config-production.yaml \
        >/dev/null
    docker network connect "$control_network" "$gateway_container"
done
for index in "${!gateway_containers[@]}"; do
    published=$(docker port "${gateway_containers[index]}" 9080/tcp)
    gateway_http_ports[index]=${published##*:}
    [[ "${gateway_http_ports[index]}" =~ ^[1-9][0-9]*$ ]] || die "invalid HTTP port mapping for ${gateway_containers[index]}"
    published=$(docker port "${gateway_containers[index]}" 9443/tcp)
    gateway_tls_ports[index]=${published##*:}
    [[ "${gateway_tls_ports[index]}" =~ ^[1-9][0-9]*$ ]] || die "invalid TLS port mapping for ${gateway_containers[index]}"
    published=$(docker port "${gateway_containers[index]}" 7085/tcp)
    gateway_status_ports[index]=${published##*:}
    [[ "${gateway_status_ports[index]}" =~ ^[1-9][0-9]*$ ]] || die "invalid status port mapping for ${gateway_containers[index]}"
    gateway_urls[index]="http://127.0.0.1:${gateway_http_ports[index]}"
    gateway_tls_urls[index]="https://release.example.test:${gateway_tls_ports[index]}"
    gateway_status_urls[index]="http://127.0.0.1:${gateway_status_ports[index]}"
done

record_step 'replicas ready and proxy v1'
wait_gateway_probe 'replicas-status' /status 200
wait_gateway_probe 'replicas-ready' /status/ready 200
wait_gateway_body 'replicas-release-v1' /release-v1 v1

record_step 'write service and SSL resources'
etcdctl_put "$etcd_prefix/upstreams/$upstream_v2_id" "$upstream_v2_config"
etcdctl_put "$etcd_prefix/services/$service_id" "$service_v1"
etcdctl_put "$etcd_prefix/ssls/$ssl_id" "$ssl_config"
etcdctl_put "$etcd_prefix/routes/$managed_route_id" "$managed_route"

record_step 'replicas converge on managed resource graph'
wait_gateway_body 'managed-v1' /managed v1

record_step 'reject invalid route generation and retain last-good'
etcdctl_put "$etcd_prefix/routes/$invalid_route_id" "$invalid_route"
wait_gateway_body 'invalid-generation-last-good' /managed v1
wait_gateway_status 'invalid-generation-not-activated' /invalid 404
etcdctl_delete "$etcd_prefix/routes/$invalid_route_id"

record_step 'update service to upstream v2'
etcdctl_put "$etcd_prefix/services/$service_id" "$service_v2"
wait_gateway_body 'managed-v2' /managed v2

record_step 'replicas serve dynamic SSL'
wait_gateway_tls_body 'dynamic-ssl' /release-v1 v1

record_step 'stop etcd'
docker stop "$etcd_container" >/dev/null

record_step 'replicas retain last-good during etcd outage'
wait_gateway_probe 'outage-ready' /status/ready 200
wait_gateway_probe 'outage-status' /status 200
wait_gateway_body 'outage-last-good-release-v1' /release-v1 v1
wait_gateway_body 'outage-last-good-managed-v2' /managed v2

record_step 'restart etcd'
docker start "$etcd_container" >/dev/null
wait_etcd 'recovery'

record_step 'replicas ready after recovery'
wait_gateway_probe 'recovery-ready' /status/ready 200

record_step 'update route to v2'
etcdctl_put "$etcd_prefix/routes/$route_id" "$route_v2"

record_step 'replicas proxy route v2'
wait_gateway_status 'route-v1-removed' /release-v1 404
wait_gateway_body 'route-v2' /release-v2 v2

record_step 'seed route before compaction gap'
etcdctl_put "$etcd_prefix/routes/$stale_route_id" "$stale_route"
wait_gateway_body 'stale-route-seeded' /stale v2

record_step 'stop etcd TCP gateway to create revision gap'
docker stop "$etcd_proxy_container" >/dev/null
wait_gateway_probe 'compaction-gap-ready' /status/ready 200

record_step 'mutate and compact etcd behind stopped gateway'
etcdctl_delete "$etcd_prefix/routes/$stale_route_id"
etcdctl_put "$etcd_prefix/services/$service_id" "$service_v1"
snapshot_json=$(etcdctl get "$etcd_prefix" --prefix --write-out=json 2>>"$run_dir/etcdctl.log")
printf '%s\n' "$snapshot_json" >"$run_dir/compaction-snapshot.json"
revision=$(printf '%s\n' "$snapshot_json" | sed -n 's/.*"revision":[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n 1)
[[ "$revision" =~ ^[1-9][0-9]*$ ]] || die 'etcd snapshot did not contain a current revision'
before_generation=$revision
etcdctl compact "$revision" >>"$run_dir/etcdctl.log" 2>&1

record_step 'replicas retain pre-gap state while disconnected'
wait_gateway_body 'compaction-gap-stale-route' /stale v2
wait_gateway_body 'compaction-gap-managed-v2' /managed v2

record_step 'restart etcd TCP gateway after compaction'
docker start "$etcd_proxy_container" >/dev/null

record_step 'replicas recover compacted snapshot consistently'
wait_gateway_probe 'compaction-recovery-ready' /status/ready 200
wait_gateway_status 'compaction-stale-route-removed' /stale 404
wait_gateway_body 'compaction-managed-v1' /managed v1
wait_gateway_tls_body 'compaction-dynamic-ssl' /release-v2 v2
wait_gateway_body 'compaction-route-v2' /release-v2 v2

record_step 'restart one replica and recover committed journal state'
restart_before_identity=$(docker inspect --format '{{.Id}} {{.State.StartedAt}}' "$gateway_a_container")
start_survivor_monitor 1
: >"$survivor_monitor_window"
docker restart "$gateway_a_container" >/dev/null
wait_for_survivor_window_probe
wait_http_status 'replica-restart-ready' "${gateway_status_urls[0]}/status/ready" 200
wait_http_body 'replica-restart-managed' "${gateway_urls[0]}/managed" v1
stop_survivor_monitor
probe_survivor_once "${gateway_status_urls[1]}" "${gateway_urls[1]}" || \
    die 'surviving replica failed after restart'
restart_after_identity=$(docker inspect --format '{{.Id}} {{.State.StartedAt}}' "$gateway_a_container")
[[ "$restart_before_identity" != "$restart_after_identity" ]] || \
    die 'restarted replica identity did not change'
printf '{"replica":"%s","before":"%s","after":"%s","survivor":"%s","survivor_probe_count":%s,"survivor_window_probe_count":%s}\n' \
    "$gateway_a_container" "$restart_before_identity" "$restart_after_identity" \
    "$gateway_b_container" "$restart_survivor_probe_count" \
    "$restart_survivor_window_probe_count" >"$run_dir/replica-restart-identity.json"

record_step 'delete SSL resource'
etcdctl_delete "$etcd_prefix/ssls/$ssl_id"

record_step 'replicas converge on live SSL delete'
wait_gateway_tls_failure 'deleted-ssl' /release-v2

record_step 're-add SSL resource'
etcdctl_put "$etcd_prefix/ssls/$ssl_id" "$ssl_config"

record_step 'replicas converge on re-added SSL resource'
wait_gateway_tls_body 'readded-ssl' /release-v2 v2
after_generation=$(current_etcd_revision)
write_recovery_evidence "$before_generation" "$after_generation"

printf 'etcd recovery smoke: PASS\n'
