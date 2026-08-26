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

data_network="apisix-release-etcd-data-$run_id"
control_network="apisix-release-etcd-control-$run_id"
etcd_container="apisix-release-etcd-$run_id"
upstream_v1_container="apisix-release-upstream-v1-$run_id"
upstream_v2_container="apisix-release-upstream-v2-$run_id"
gateway_a_container="apisix-release-gateway-a-$run_id"
gateway_b_container="apisix-release-gateway-b-$run_id"
gateway_containers=("$gateway_a_container" "$gateway_b_container")
upstream_v1_alias="release-upstream-v1-$run_id"
upstream_v2_alias="release-upstream-v2-$run_id"
route_id=release-route
managed_route_id=release-managed-route
stale_route_id=release-stale-route
upstream_v1_id=release-upstream-v1
upstream_v2_id=release-upstream-v2
service_id=release-service
consumer_id=release-consumer
global_rule_id=release-global-rule
plugin_config_id=release-plugin-config
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
    if [[ -n ${run_dir:-} ]]; then
        printf 'cleanup exit=%s\n' "$status" >>"$transcript"
        for container in "${gateway_containers[@]}" "$upstream_v2_container" "$upstream_v1_container" "$etcd_container"; do
            docker logs "$container" >"$run_dir/${container}.logs" 2>&1 || true
            docker inspect "$container" >"$run_dir/${container}.inspect" 2>&1 || true
        done
        printf 'cleanup: evidence preserved at %s\n' "$run_dir" >&2
    fi
    docker rm -f "${gateway_containers[@]}" "$upstream_v2_container" "$upstream_v1_container" "$etcd_container" \
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
subjectAltName = DNS:etcd
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

wait_http_header() {
    local label=$1
    local url=$2
    local expected_status=$3
    local header_name=$4
    local expected_value=$5
    shift 5
    local body_file="$run_dir/$label.body"
    local header_file="$run_dir/$label.headers"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout remaining status got
    local curl_args=("$@")
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        status=$(curl --silent --show-error --connect-timeout "$request_timeout" \
            --max-time "$request_timeout" "${curl_args[@]}" --dump-header "$header_file" \
            --output "$body_file" --write-out '%{http_code}' "$url" 2>>"$transcript") || status=
        got=
        if [[ -f "$header_file" ]]; then
            got=$(tr -d '\r' <"$header_file" | awk -F': ' -v name="$header_name" \
                'tolower($1) == tolower(name) { print $2; exit }')
        fi
        if [[ "$status" == "$expected_status" ]] && \
            { [[ "$expected_value" == '__nonempty__' && -n "$got" ]] || [[ "$got" == "$expected_value" ]]; }; then
            return 0
        fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted HTTP $expected_status and $header_name=$expected_value)"
}

wait_http_header_absent() {
    local label=$1
    local url=$2
    local expected_status=$3
    local header_name=$4
    shift 4
    local body_file="$run_dir/$label.body"
    local header_file="$run_dir/$label.headers"
    local deadline=$((SECONDS + timeout_seconds))
    local request_timeout remaining status got
    local curl_args=("$@")
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        status=$(curl --silent --show-error --connect-timeout "$request_timeout" \
            --max-time "$request_timeout" "${curl_args[@]}" --dump-header "$header_file" \
            --output "$body_file" --write-out '%{http_code}' "$url" 2>>"$transcript") || status=
        got=
        if [[ -f "$header_file" ]]; then
            got=$(tr -d '\r' <"$header_file" | awk -F': ' -v name="$header_name" \
                'tolower($1) == tolower(name) { print $2; exit }')
        fi
        if [[ "$status" == "$expected_status" && -z "$got" ]]; then return 0; fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted HTTP $expected_status without $header_name)"
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

wait_gateway_header() {
    local label=$1 path=$2 expected_status=$3 header_name=$4 expected_value=$5
    shift 5
    local curl_args=("$@") index
    for index in "${!gateway_containers[@]}"; do
        wait_http_header "$label-replica-$((index + 1))" "${gateway_urls[index]}$path" \
            "$expected_status" "$header_name" "$expected_value" "${curl_args[@]}"
    done
}

wait_gateway_header_absent() {
    local label=$1 path=$2 expected_status=$3 header_name=$4
    shift 4
    local curl_args=("$@") index
    for index in "${!gateway_containers[@]}"; do
        wait_http_header_absent "$label-replica-$((index + 1))" "${gateway_urls[index]}$path" \
            "$expected_status" "$header_name" "${curl_args[@]}"
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

wait_gateway_log() {
    local label=$1
    local marker=$2
    local deadline=$((SECONDS + timeout_seconds))
    local index logs
    while (( SECONDS < deadline )); do
        for index in "${!gateway_containers[@]}"; do
            logs=$(docker logs "${gateway_containers[index]}" 2>&1 || true)
            printf '%s\n' "$logs" >"$run_dir/${label}-replica-$((index + 1)).logs"
            if ! grep -Fq -- "$marker" <<<"$logs"; then
                break
            fi
            if (( index == ${#gateway_containers[@]} - 1 )); then
                return 0
            fi
        done
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for gateway logs to contain $marker"
}

etcdctl_with_timeout() {
    local command_timeout=$1
    shift
    docker run --rm --network "$data_network" \
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

json_string_from_file() {
    awk 'BEGIN { printf "\"" } { printf "%s\\n", $0 } END { printf "\"" }' "$1"
}

route_v1=$(printf '{"id":"%s","status":1,"uri":"/release-v1","upstream_id":"%s"}' \
    "$route_id" "$upstream_v1_id")
route_v2=$(printf '{"id":"%s","status":1,"uri":"/release-v2","upstream_id":"%s"}' \
    "$route_id" "$upstream_v2_id")
managed_route=$(printf '{"id":"%s","status":1,"uri":"/managed","service_id":"%s","plugin_config_id":"%s","plugins":{"key-auth":{}}}' \
    "$managed_route_id" "$service_id" "$plugin_config_id")
stale_route=$(printf '{"id":"%s","status":1,"uri":"/stale","service_id":"%s"}' \
    "$stale_route_id" "$service_id")
upstream_v1_config=$(printf '{"nodes":{"%s:8081":1},"type":"roundrobin"}' "$upstream_v1_alias")
upstream_v2_config=$(printf '{"nodes":{"%s:8081":1},"type":"roundrobin"}' "$upstream_v2_alias")
service_v1=$(printf '{"id":"%s","upstream_id":"%s"}' "$service_id" "$upstream_v1_id")
service_v2=$(printf '{"id":"%s","upstream_id":"%s"}' "$service_id" "$upstream_v2_id")
consumer_v1=$(printf '{"username":"%s","plugins":{"key-auth":{"key":"release-key-v1"}}}' "$consumer_id")
consumer_v2=$(printf '{"username":"%s","plugins":{"key-auth":{"key":"release-key-v2"}}}' "$consumer_id")
global_rule_v1=$(printf '{"id":"%s","plugins":{"cors":{"allow_origins":"https://global-v1.example"}}}' "$global_rule_id")
global_rule_v2=$(printf '{"id":"%s","plugins":{"cors":{"allow_origins":"https://global-v2.example"}}}' "$global_rule_id")
plugin_config_v1=$(printf '{"id":"%s","plugins":{"request-id":{"header_name":"X-Plugin-Config-V1","include_in_response":true}}}' \
    "$plugin_config_id")
plugin_config_v2=$(printf '{"id":"%s","plugins":{"request-id":{"header_name":"X-Plugin-Config-V2","include_in_response":true}}}' \
    "$plugin_config_id")
frontend_cert_json=$(json_string_from_file "$tls_dir/frontend.crt")
frontend_key_json=$(json_string_from_file "$tls_dir/frontend.key")
ssl_config=$(printf '{"id":"%s","snis":["release.example.test"],"cert":%s,"key":%s,"status":1}' \
    "$ssl_id" "$frontend_cert_json" "$frontend_key_json")

record_step 'network create'
docker network create "$data_network" >/dev/null
docker network create "$control_network" >/dev/null

record_step 'start etcd 3.6.13'
docker run --detach --name "$etcd_container" --network "$data_network" --network-alias etcd \
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
        --env APISIXGO_APISIX_STATUS_IP=0.0.0.0 \
        --volume "$repo_root/conf/config-production.yaml:/usr/local/apisix/conf/config-production.yaml:ro" \
        --volume "$tls_dir/ca.crt:/etc/ssl/certs/etcd-ca.crt:ro" \
        "$image" -c /usr/local/apisix/conf/config-production.yaml \
        >/dev/null
    docker network connect "$control_network" "$gateway_container"
done
for index in "${!gateway_containers[@]}"; do
    published=$(docker port "${gateway_containers[index]}" 9080/tcp)
    gateway_http_ports[index]=${published##*:}
    published=$(docker port "${gateway_containers[index]}" 9443/tcp)
    gateway_tls_ports[index]=${published##*:}
    published=$(docker port "${gateway_containers[index]}" 7085/tcp)
    gateway_status_ports[index]=${published##*:}
    gateway_urls[index]="http://127.0.0.1:${gateway_http_ports[index]}"
    gateway_tls_urls[index]="https://release.example.test:${gateway_tls_ports[index]}"
    gateway_status_urls[index]="http://127.0.0.1:${gateway_status_ports[index]}"
done

record_step 'replicas ready and proxy v1'
wait_gateway_probe 'replicas-status' /status 200
wait_gateway_probe 'replicas-ready' /status/ready 200
wait_gateway_body 'replicas-release-v1' /release-v1 v1

record_step 'write consumer/service/global rule/plugin config/SSL resources'
etcdctl_put "$etcd_prefix/upstreams/$upstream_v2_id" "$upstream_v2_config"
etcdctl_put "$etcd_prefix/services/$service_id" "$service_v1"
etcdctl_put "$etcd_prefix/consumers/$consumer_id" "$consumer_v1"
etcdctl_put "$etcd_prefix/global_rules/$global_rule_id" "$global_rule_v1"
etcdctl_put "$etcd_prefix/plugin_configs/$plugin_config_id" "$plugin_config_v1"
etcdctl_put "$etcd_prefix/ssls/$ssl_id" "$ssl_config"
etcdctl_put "$etcd_prefix/routes/$managed_route_id" "$managed_route"

record_step 'replicas converge on managed resource graph'
wait_gateway_status 'managed-no-key' /managed 401
wait_gateway_body 'managed-v1' /managed v1 --header 'apikey: release-key-v1'
wait_gateway_header 'managed-request-id-v1' /managed 200 X-Plugin-Config-V1 __nonempty__ \
    --header 'apikey: release-key-v1'
wait_gateway_header 'global-cors-v1' /release-v1 200 Access-Control-Allow-Origin \
    https://global-v1.example --header 'Origin: https://global-v1.example'

record_step 'update consumer credential'
etcdctl_put "$etcd_prefix/consumers/$consumer_id" "$consumer_v2"
wait_gateway_status 'managed-old-key-rejected' /managed 401 --header 'apikey: release-key-v1'
wait_gateway_body 'managed-v1-new-key' /managed v1 --header 'apikey: release-key-v2'

record_step 'update plugin config and global rule'
etcdctl_put "$etcd_prefix/plugin_configs/$plugin_config_id" "$plugin_config_v2"
etcdctl_put "$etcd_prefix/global_rules/$global_rule_id" "$global_rule_v2"
wait_gateway_header 'managed-request-id-v2' /managed 200 X-Plugin-Config-V2 __nonempty__ \
    --header 'apikey: release-key-v2'
wait_gateway_header_absent 'managed-request-id-v1-removed' /managed 200 X-Plugin-Config-V1 \
    --header 'apikey: release-key-v2'
wait_gateway_header 'global-cors-v2' /release-v1 200 Access-Control-Allow-Origin \
    https://global-v2.example --header 'Origin: https://global-v2.example'

record_step 'update service to upstream v2'
etcdctl_put "$etcd_prefix/services/$service_id" "$service_v2"
wait_gateway_body 'managed-v2' /managed v2 --header 'apikey: release-key-v2'

record_step 'replicas serve dynamic SSL'
wait_gateway_tls_body 'dynamic-ssl' /release-v1 v1

record_step 'stop etcd'
docker stop "$etcd_container" >/dev/null

record_step 'replicas retain last-good during etcd outage'
wait_gateway_probe 'outage-ready' /status/ready 200
wait_gateway_probe 'outage-status' /status 200
wait_gateway_body 'outage-last-good-release-v1' /release-v1 v1
wait_gateway_body 'outage-last-good-managed-v2' /managed v2 --header 'apikey: release-key-v2'

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

record_step 'disconnect replicas from etcd'
for gateway_container in "${gateway_containers[@]}"; do
    docker network disconnect "$data_network" "$gateway_container"
done
wait_gateway_probe 'compaction-gap-ready' /status/ready 200

record_step 'mutate and compact etcd while replicas disconnected'
etcdctl_delete "$etcd_prefix/routes/$stale_route_id"
etcdctl_put "$etcd_prefix/services/$service_id" "$service_v1"
snapshot_json=$(etcdctl get "$etcd_prefix" --prefix --write-out=json 2>>"$run_dir/etcdctl.log")
printf '%s\n' "$snapshot_json" >"$run_dir/compaction-snapshot.json"
revision=$(printf '%s\n' "$snapshot_json" | sed -n 's/.*"revision":[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n 1)
[[ "$revision" =~ ^[1-9][0-9]*$ ]] || die 'etcd snapshot did not contain a current revision'
etcdctl compact "$revision" >>"$run_dir/etcdctl.log" 2>&1

record_step 'reconnect replicas'
for gateway_container in "${gateway_containers[@]}"; do
    docker network connect "$data_network" "$gateway_container"
done

record_step 'replicas recover compacted snapshot consistently'
wait_gateway_log 'compaction-recovery' 'required revision has been compacted'
wait_gateway_probe 'compaction-recovery-ready' /status/ready 200
wait_gateway_status 'compaction-stale-route-removed' /stale 404
wait_gateway_body 'compaction-managed-v1' /managed v1 --header 'apikey: release-key-v2'
wait_gateway_header 'compaction-request-id-v2' /managed 200 X-Plugin-Config-V2 __nonempty__ \
    --header 'apikey: release-key-v2'
wait_gateway_header_absent 'compaction-request-id-v1-removed' /managed 200 X-Plugin-Config-V1 \
    --header 'apikey: release-key-v2'
wait_gateway_header 'compaction-global-cors-v2' /release-v2 200 Access-Control-Allow-Origin \
    https://global-v2.example --header 'Origin: https://global-v2.example'
wait_gateway_tls_body 'compaction-dynamic-ssl' /release-v2 v2
wait_gateway_body 'compaction-route-v2' /release-v2 v2

record_step 'delete consumer/global rule/SSL resources'
etcdctl_delete "$etcd_prefix/consumers/$consumer_id"
etcdctl_delete "$etcd_prefix/global_rules/$global_rule_id"
etcdctl_delete "$etcd_prefix/ssls/$ssl_id"

record_step 'replicas converge on live deletes'
wait_gateway_status 'deleted-consumer-key-v2' /managed 401 --header 'apikey: release-key-v2'
wait_gateway_status 'deleted-consumer-old-key' /managed 401 --header 'apikey: release-key-v1'
wait_gateway_header_absent 'deleted-global-cors' /release-v2 200 Access-Control-Allow-Origin \
    --header 'Origin: https://global-v2.example'
wait_gateway_tls_failure 'deleted-ssl' /release-v2

printf 'etcd recovery smoke: PASS\n'
