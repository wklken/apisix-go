#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
etcd_image=gcr.io/etcd-development/etcd:v3.6.13
production_config="$repo_root/conf/config-production.yaml"

die() {
    printf 'upgrade rollback smoke: %s\n' "$*" >&2
    return 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

if (( $# != 3 )); then
    printf 'usage: %s CANDIDATE_DIGEST_REFERENCE ROLLBACK_DIGEST_REFERENCE ROLLBACK_QUALIFICATION_METADATA\n' "$0" >&2
    exit 2
fi

candidate_reference=$1
rollback_reference=$2
rollback_metadata=$3
container_bin=${CONTAINER_BIN:-docker}

for command_name in "$container_bin" curl jq openssl; do
    require_command "$command_name"
done

docker() {
    command "$container_bin" "$@"
}

is_digest_reference() {
    [[ "$1" =~ ^([a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?/)*[a-z0-9]+([._-][a-z0-9]+)*@sha256:[0-9a-f]{64}$ ]]
}

if [[ ! "$candidate_reference" =~ ^sha256:[0-9a-f]{64}$ ]] && \
    ! is_digest_reference "$candidate_reference"; then
    die 'candidate image must be an immutable image ID or digest-qualified reference'
fi
is_digest_reference "$rollback_reference" || \
    die 'rollback image must be a digest-qualified reference without a tag'

candidate_manifest_digest=
if [[ "$candidate_reference" == *@* ]]; then
    candidate_manifest_digest=${candidate_reference##*@}
fi
rollback_manifest_digest=${rollback_reference##*@}
[[ -z "$candidate_manifest_digest" || "$candidate_manifest_digest" != "$rollback_manifest_digest" ]] || \
    die 'candidate and rollback image digests must be distinct'

[[ -f "$rollback_metadata" ]] || \
    die "rollback qualification metadata is not a file: $rollback_metadata"

inspect_image() {
    local reference=$1
    local image_id
    if ! image_id=$(docker image inspect --format '{{.Id}}' "$reference"); then
        die "immutable image is not available locally: $reference"
        return
    fi
    if [[ "$image_id" =~ ^[0-9a-f]{64}$ ]]; then
        image_id=sha256:$image_id
    fi
    [[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || \
        die "container runtime returned an invalid image ID for: $reference"
    printf '%s\n' "$image_id"
}

inspect_source_commit() {
    local reference=$1
    local source_commit
    if ! source_commit=$(docker image inspect \
        --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$reference"); then
        die "cannot read source revision from image: $reference"
        return
    fi
    [[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] || \
        die "image source revision label is not an exact 40-character commit: $reference"
    printf '%s\n' "$source_commit"
}

candidate_image_id=$(inspect_image "$candidate_reference")
rollback_image_id=$(inspect_image "$rollback_reference")
[[ "$candidate_image_id" != "$rollback_image_id" ]] || \
    die 'candidate and rollback references resolve to the same local image ID'
candidate_source_commit=$(inspect_source_commit "$candidate_reference")
rollback_source_commit=$(inspect_source_commit "$rollback_reference")

if ! jq -e \
    --arg reference "$rollback_reference" \
    --arg image_id "$rollback_image_id" \
    --arg source_commit "$rollback_source_commit" '
      .schema_version == 1 and
      .image_reference == $reference and
      .image_digest == $image_id and
      .source.commit == $source_commit and
      .qualification.profile == "http-data-plane-v1" and
      .qualification.result == "passed"
    ' "$rollback_metadata" >/dev/null; then
    if jq -e '
        .schema_version == 1 and
        .qualification.profile == "http-data-plane-v1" and
        .qualification.result == "passed"
      ' "$rollback_metadata" >/dev/null 2>&1; then
        die 'rollback qualification metadata identity does not match rollback image'
    fi
    die 'rollback metadata does not prove a passed http-data-plane-v1 qualification'
fi

probe_mode=${UPGRADE_ROLLBACK_PROBE_MODE:-auto}
if [[ "$probe_mode" == auto ]]; then
    if [[ $(basename "$container_bin") == podman ]] && docker machine ssh true >/dev/null 2>&1; then
        probe_mode=podman-machine
    else
        probe_mode=host
    fi
fi
[[ "$probe_mode" == host || "$probe_mode" == podman-machine ]] || \
    die 'UPGRADE_ROLLBACK_PROBE_MODE must be auto, host, or podman-machine'

curl() {
    if [[ "$probe_mode" == podman-machine ]]; then
        docker machine ssh -- curl "$@"
        return
    fi
    command curl "$@"
}

timeout_seconds=${UPGRADE_ROLLBACK_TIMEOUT_SECONDS:-90}
poll_interval=${UPGRADE_ROLLBACK_POLL_INTERVAL_SECONDS:-1}
curl_timeout=${UPGRADE_ROLLBACK_CURL_TIMEOUT_SECONDS:-5}
etcdctl_timeout=${UPGRADE_ROLLBACK_ETCDCTL_TIMEOUT_SECONDS:-5}
guard_interval=${UPGRADE_ROLLBACK_GUARD_INTERVAL_SECONDS:-1}
for setting in timeout_seconds curl_timeout etcdctl_timeout; do
    value=${!setting}
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || die "$setting must be a positive integer"
done
[[ "$poll_interval" =~ ^[0-9]+$ ]] || die 'poll_interval must be a non-negative integer'
[[ "$guard_interval" =~ ^[0-9]+$ ]] || die 'guard_interval must be a non-negative integer'

run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}"
evidence_root=${RELEASE_EVIDENCE_ROOT:-"$repo_root/.cache/release-evidence/upgrade-rollback"}
run_dir="$evidence_root/$run_id"
mkdir -p "$run_dir"
events="$run_dir/events.jsonl"
transcript="$run_dir/steps.log"
touch "$events" "$transcript"

if [[ "$probe_mode" == podman-machine ]]; then
    mkdir -p "$repo_root/.cache/tmp"
    temp_dir=$(mktemp -d "$repo_root/.cache/tmp/apisix-upgrade-rollback.XXXXXX")
else
    temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/apisix-upgrade-rollback.XXXXXX")
fi
tls_dir="$temp_dir/tls"
mkdir -p "$tls_dir"

network="apisix-rollout-$run_id"
etcd="apisix-rollout-etcd-$run_id"
upstream="apisix-rollout-upstream-$run_id"
upstream_alias="apisix-rollout-upstream-$run_id"
gateway_a="apisix-rollout-a-$run_id"
gateway_b="apisix-rollout-b-$run_id"
replica_a_root="$temp_dir/replica-a"
replica_b_root="$temp_dir/replica-b"
guard_pid=
guard_stop_file=
run_finished=false

timestamp() {
    date -u +%Y-%m-%dT%H:%M:%SZ
}

append_run_started() {
    jq -cn \
        --arg timestamp "$(timestamp)" \
        --arg candidate_reference "$candidate_reference" \
        --arg candidate_manifest_digest "$candidate_manifest_digest" \
        --arg candidate_image_id "$candidate_image_id" \
        --arg candidate_source_commit "$candidate_source_commit" \
        --arg rollback_reference "$rollback_reference" \
        --arg rollback_manifest_digest "$rollback_manifest_digest" \
        --arg rollback_image_id "$rollback_image_id" \
        --arg rollback_source_commit "$rollback_source_commit" '
        {
          timestamp: $timestamp,
          event: "run_started",
          candidate: {
            image_reference: $candidate_reference,
            manifest_digest: $candidate_manifest_digest,
            image_id: $candidate_image_id,
            source_commit: $candidate_source_commit
          },
          rollback: {
            image_reference: $rollback_reference,
            manifest_digest: $rollback_manifest_digest,
            image_id: $rollback_image_id,
            source_commit: $rollback_source_commit,
            qualification_profile: "http-data-plane-v1"
          }
        }
    ' >>"$events"
}

append_transition() {
    local event=$1
    local phase=$2
    local replica=$3
    local reference=$4
    local image_id=$5
    local source_commit=$6
    jq -cn \
        --arg timestamp "$(timestamp)" \
        --arg event "$event" \
        --arg phase "$phase" \
        --arg replica "$replica" \
        --arg reference "$reference" \
        --arg image_id "$image_id" \
        --arg source_commit "$source_commit" '
        {
          timestamp: $timestamp,
          event: $event,
          phase: $phase,
          replica: $replica,
          image_reference: $reference,
          image_id: $image_id,
          source_commit: $source_commit
        }
    ' >>"$events"
}

append_probe() {
    local phase=$1
    local replica=$2
    local probe=$3
    jq -cn \
        --arg timestamp "$(timestamp)" \
        --arg phase "$phase" \
        --arg replica "$replica" \
        --arg probe "$probe" '
        {
          timestamp: $timestamp,
          event: "probe_succeeded",
          phase: $phase,
          replica: $replica,
          probe: $probe
        }
    ' >>"$events"
}

append_result() {
    local result=$1
    jq -cn \
        --arg timestamp "$(timestamp)" \
        --arg result "$result" '
        {timestamp: $timestamp, event: "run_finished", result: $result}
    ' >>"$events"
}

cleanup() {
    local status=$?
    trap - EXIT
    set +e
    if [[ -n "$guard_stop_file" ]]; then touch "$guard_stop_file"; fi
    if [[ -n "$guard_pid" ]]; then
        kill "$guard_pid" >/dev/null 2>&1 || true
        wait "$guard_pid" >/dev/null 2>&1 || true
    fi
    if [[ "$run_finished" != true ]]; then
        if (( status == 0 )); then
            append_result passed
        else
            append_result failed
        fi
        run_finished=true
    fi
    for container in "$gateway_a" "$gateway_b" "$upstream" "$etcd"; do
        docker logs "$container" >"$run_dir/${container}.logs" 2>&1 || true
        docker inspect "$container" >"$run_dir/${container}.inspect" 2>&1 || true
    done
    docker rm -f "$gateway_a" "$gateway_b" "$upstream" "$etcd" \
        >"$run_dir/cleanup.log" 2>&1 || true
    docker network rm "$network" >>"$run_dir/cleanup.log" 2>&1 || true
    rm -rf "$temp_dir"
    printf 'upgrade rollback smoke: evidence preserved at %s\n' "$run_dir" >&2
    exit "$status"
}

trap cleanup EXIT
append_run_started

cat >"$temp_dir/frontend.ext" <<'EOF'
subjectAltName = DNS:rollout.example.test
extendedKeyUsage = serverAuth
EOF
cat >"$temp_dir/etcd-server.ext" <<'EOF'
subjectAltName = DNS:etcd
extendedKeyUsage = serverAuth
EOF
cat >"$temp_dir/etcd-client.ext" <<'EOF'
extendedKeyUsage = clientAuth
EOF
{
    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout "$tls_dir/ca.key" -out "$tls_dir/ca.crt" -days 1 \
        -subj '/CN=apisix-go-upgrade-rollback-ca'
    openssl req -newkey rsa:2048 -nodes \
        -keyout "$tls_dir/etcd-server.key" -out "$temp_dir/etcd-server.csr" \
        -subj '/CN=etcd'
    openssl x509 -req -in "$temp_dir/etcd-server.csr" \
        -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" \
        -CAcreateserial -CAserial "$temp_dir/ca.srl" \
        -out "$tls_dir/etcd-server.crt" -days 1 \
        -extfile "$temp_dir/etcd-server.ext"
    openssl req -newkey rsa:2048 -nodes \
        -keyout "$tls_dir/etcd-client.key" -out "$temp_dir/etcd-client.csr" \
        -subj '/CN=apisix-go-qualification-client'
    openssl x509 -req -in "$temp_dir/etcd-client.csr" \
        -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" \
        -CAserial "$temp_dir/ca.srl" \
        -out "$tls_dir/etcd-client.crt" -days 1 \
        -extfile "$temp_dir/etcd-client.ext"
    openssl req -newkey rsa:2048 -nodes \
        -keyout "$tls_dir/frontend.key" -out "$temp_dir/frontend.csr" \
        -subj '/CN=rollout.example.test'
    openssl x509 -req -in "$temp_dir/frontend.csr" \
        -CA "$tls_dir/ca.crt" -CAkey "$tls_dir/ca.key" \
        -CAcreateserial -CAserial "$temp_dir/ca.srl" \
        -out "$tls_dir/frontend.crt" -days 1 \
        -extfile "$temp_dir/frontend.ext"
} >>"$transcript" 2>&1
chmod 0644 "$tls_dir/ca.crt" "$tls_dir/etcd-server.crt" \
    "$tls_dir/etcd-client.crt" "$tls_dir/etcd-client.key" "$tls_dir/frontend.crt"

for replica_root in "$replica_a_root" "$replica_b_root"; do
    mkdir -p "$replica_root/data" "$replica_root/run" "$replica_root/logs" "$replica_root/tmp"
    chmod 0777 "$replica_root/data" "$replica_root/run" "$replica_root/logs" "$replica_root/tmp"
done

# Every mutation is intentionally appended to the single chronological transcript.
# shellcheck disable=SC2129
docker network create "$network" >>"$transcript"
docker run --detach --name "$etcd" --network "$network" --network-alias etcd \
    --volume "$tls_dir:/etc/etcd/tls:ro" "$etcd_image" \
    /usr/local/bin/etcd \
    --name=etcd0 --data-dir=/etcd-data \
    --listen-client-urls=https://0.0.0.0:2379 \
    --advertise-client-urls=https://etcd:2379 \
    --listen-peer-urls=http://0.0.0.0:2380 \
    --initial-advertise-peer-urls=http://etcd:2380 \
    --initial-cluster=etcd0=http://etcd:2380 \
    --initial-cluster-state=new \
    --cert-file=/etc/etcd/tls/etcd-server.crt \
    --key-file=/etc/etcd/tls/etcd-server.key \
    --trusted-ca-file=/etc/etcd/tls/ca.crt \
    --client-cert-auth=true >>"$transcript"
docker run --detach --name "$upstream" --network "$network" \
    --network-alias "$upstream_alias" busybox:1.37.0 sh -c \
    'mkdir -p /www && printf "%s" "apisix-upgrade-rollback" >/www/qualification && exec httpd -f -p 8081 -h /www' \
    >>"$transcript"

sleep_until_poll() {
    local deadline=$1
    local remaining=$((deadline - SECONDS))
    local delay=$poll_interval
    (( remaining > 0 )) || return
    if (( delay > remaining )); then delay=$remaining; fi
    sleep "$delay"
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
        --cert=/etc/etcd/tls/etcd-client.crt \
        --key=/etc/etcd/tls/etcd-client.key \
        --dial-timeout="${command_timeout}s" \
        --command-timeout="${command_timeout}s" "$@"
}

wait_etcd() {
    local deadline=$((SECONDS + timeout_seconds))
    local command_timeout remaining
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        command_timeout=$etcdctl_timeout
        if (( command_timeout > remaining )); then command_timeout=$remaining; fi
        if etcdctl_with_timeout "$command_timeout" endpoint health >>"$transcript" 2>&1; then
            return 0
        fi
        sleep_until_poll "$deadline"
    done
    die 'timed out waiting for TLS etcd startup'
}

etcdctl_put() {
    etcdctl_with_timeout "$etcdctl_timeout" put "$1" "$2" >>"$transcript" 2>&1
}

json_string_from_file() {
    awk 'BEGIN { printf "\"" } { printf "%s\\n", $0 } END { printf "\"" }' "$1"
}

wait_etcd
route_config=$(printf '{"id":"upgrade-rollback-route","status":1,"uri":"/qualification","upstream_id":"upgrade-rollback-upstream","plugins":{"request-id":{}}}')
upstream_config=$(printf '{"nodes":{"%s:8081":1},"type":"roundrobin"}' "$upstream_alias")
frontend_cert_json=$(json_string_from_file "$tls_dir/frontend.crt")
frontend_key_json=$(json_string_from_file "$tls_dir/frontend.key")
ssl_config=$(printf '{"id":"upgrade-rollback-tls","snis":["rollout.example.test"],"cert":%s,"key":%s,"status":1}' \
    "$frontend_cert_json" "$frontend_key_json")
etcdctl_put /apisix/upstreams/upgrade-rollback-upstream "$upstream_config"
etcdctl_put /apisix/routes/upgrade-rollback-route "$route_config"
etcdctl_put /apisix/ssls/upgrade-rollback-tls "$ssl_config"

container_for_replica() {
    case "$1" in
        a) printf '%s\n' "$gateway_a" ;;
        b) printf '%s\n' "$gateway_b" ;;
        *) die "unknown replica: $1" ;;
    esac
}

root_for_replica() {
    case "$1" in
        a) printf '%s\n' "$replica_a_root" ;;
        b) printf '%s\n' "$replica_b_root" ;;
        *) die "unknown replica: $1" ;;
    esac
}

start_replica() {
    local phase=$1
    local replica=$2
    local reference=$3
    local image_id=$4
    local source_commit=$5
    local container replica_root
    container=$(container_for_replica "$replica")
    replica_root=$(root_for_replica "$replica")
    append_transition transition_started "$phase" "$replica" "$reference" "$image_id" "$source_commit"
    docker run --detach --name "$container" --network "$network" \
        --publish 127.0.0.1::9080 \
        --publish 127.0.0.1::9443 \
        --publish 127.0.0.1::7085 \
        --user 10001:10001 \
        --env SSL_CERT_FILE=/etc/etcd/tls/ca.crt \
        --env APISIXGO_DEPLOYMENT_ETCD_HOST=https://etcd:2379 \
        --env APISIXGO_DEPLOYMENT_ETCD_TLS_CERT=/etc/etcd/tls/client.crt \
        --env APISIXGO_DEPLOYMENT_ETCD_TLS_KEY=/etc/etcd/tls/client.key \
        --env APISIXGO_DEPLOYMENT_ETCD_TLS_VERIFY=true \
        --env APISIXGO_APISIX_STATUS_IP=0.0.0.0 \
        --env APISIXGO_RUNTIME_PATHS_DATA_DIR=/usr/local/apisix/data \
        --env APISIXGO_RUNTIME_PATHS_RUNTIME_DIR=/usr/local/apisix/run \
        --env APISIXGO_RUNTIME_PATHS_LOG_DIR=/usr/local/apisix/logs \
        --env APISIXGO_RUNTIME_PATHS_TEMP_DIR=/usr/local/apisix/tmp \
        --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY= \
        --env http_proxy= --env https_proxy= --env all_proxy= --env no_proxy= \
        --volume "$production_config:/usr/local/apisix/conf/config-production.yaml:ro" \
        --volume "$tls_dir/ca.crt:/etc/etcd/tls/ca.crt:ro" \
        --volume "$tls_dir/etcd-client.crt:/etc/etcd/tls/client.crt:ro" \
        --volume "$tls_dir/etcd-client.key:/etc/etcd/tls/client.key:ro" \
        --volume "$replica_root/data:/usr/local/apisix/data" \
        --volume "$replica_root/run:/usr/local/apisix/run" \
        --volume "$replica_root/logs:/usr/local/apisix/logs" \
        --volume "$replica_root/tmp:/usr/local/apisix/tmp" \
        "$reference" -c /usr/local/apisix/conf/config-production.yaml >>"$transcript"
}

published_endpoint() {
    local replica=$1
    local container_port=$2
    local container
    container=$(container_for_replica "$replica")
    docker port "$container" "$container_port/tcp" | head -n 1
}

wait_body() {
    local label=$1
    local url=$2
    local expected=$3
    shift 3
    local deadline=$((SECONDS + timeout_seconds))
    local remaining request_timeout body=
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        body=$(curl --fail --silent --show-error --noproxy '*' \
            --connect-timeout "$request_timeout" --max-time "$request_timeout" \
            "$@" "$url" 2>>"$transcript") || body=
        if [[ "$body" == "$expected" ]]; then return 0; fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url"
}

wait_status() {
    local label=$1
    local url=$2
    local expected=$3
    local deadline=$((SECONDS + timeout_seconds))
    local remaining request_timeout status=
    local body_file="$run_dir/$label.body"
    while (( SECONDS < deadline )); do
        remaining=$((deadline - SECONDS))
        request_timeout=$curl_timeout
        if (( request_timeout > remaining )); then request_timeout=$remaining; fi
        status=$(curl --silent --show-error --noproxy '*' \
            --connect-timeout "$request_timeout" --max-time "$request_timeout" \
            --output "$body_file" --write-out '%{http_code}' "$url" \
            2>>"$transcript") || status=
        if [[ "$status" == "$expected" ]]; then return 0; fi
        sleep_until_poll "$deadline"
    done
    die "timed out waiting for $label at $url (wanted HTTP $expected)"
}

probe_replica() {
    local phase=$1
    local replica=$2
    local status_endpoint http_endpoint tls_endpoint tls_port
    status_endpoint=$(published_endpoint "$replica" 7085)
    http_endpoint=$(published_endpoint "$replica" 9080)
    tls_endpoint=$(published_endpoint "$replica" 9443)
    tls_port=${tls_endpoint##*:}

    wait_status "$phase-$replica-readiness" \
        "http://$status_endpoint/status/ready" 200
    append_probe "$phase" "$replica" readiness
    wait_body "$phase-$replica-http-route" \
        "http://$http_endpoint/qualification" apisix-upgrade-rollback
    append_probe "$phase" "$replica" http-route
    wait_body "$phase-$replica-tls-route" \
        "https://rollout.example.test:$tls_port/qualification" apisix-upgrade-rollback \
        --cacert "$tls_dir/ca.crt" \
        --resolve "rollout.example.test:$tls_port:127.0.0.1"
    append_probe "$phase" "$replica" tls-route
}

probe_replica_once() {
    local phase=$1
    local replica=$2
    local status_endpoint http_endpoint tls_endpoint tls_port status body
    status_endpoint=$(published_endpoint "$replica" 7085)
    http_endpoint=$(published_endpoint "$replica" 9080)
    tls_endpoint=$(published_endpoint "$replica" 9443)
    tls_port=${tls_endpoint##*:}

    status=$(curl --silent --show-error --noproxy '*' \
        --connect-timeout "$curl_timeout" --max-time "$curl_timeout" \
        --output "$run_dir/$phase-$replica-readiness.body" \
        --write-out '%{http_code}' "http://$status_endpoint/status/ready" \
        2>>"$transcript") || return 1
    [[ "$status" == 200 ]] || return 1
    append_probe "$phase" "$replica" readiness

    body=$(curl --fail --silent --show-error --noproxy '*' \
        --connect-timeout "$curl_timeout" --max-time "$curl_timeout" \
        "http://$http_endpoint/qualification" 2>>"$transcript") || return 1
    [[ "$body" == apisix-upgrade-rollback ]] || return 1
    append_probe "$phase" "$replica" http-route

    body=$(curl --fail --silent --show-error --noproxy '*' \
        --connect-timeout "$curl_timeout" --max-time "$curl_timeout" \
        --cacert "$tls_dir/ca.crt" \
        --resolve "rollout.example.test:$tls_port:127.0.0.1" \
        "https://rollout.example.test:$tls_port/qualification" \
        2>>"$transcript") || return 1
    [[ "$body" == apisix-upgrade-rollback ]] || return 1
    append_probe "$phase" "$replica" tls-route
}

survivor_guard() {
    local phase=$1
    local survivor=$2
    local stop_file=$3
    local started_file=$4
    local iteration=0
    while [[ ! -e "$stop_file" ]]; do
        iteration=$((iteration + 1))
        if ! probe_replica_once "$phase-guard-$iteration" "$survivor"; then
            return 1
        fi
        if (( iteration == 1 )); then touch "$started_file"; fi
        sleep "$guard_interval"
    done
}

start_survivor_guard() {
    local phase=$1
    local survivor=$2
    local started_file="$run_dir/$phase.guard.started"
    local deadline=$((SECONDS + 10))
    guard_stop_file="$run_dir/$phase.guard.stop"
    rm -f "$guard_stop_file" "$started_file"
    survivor_guard "$phase" "$survivor" "$guard_stop_file" "$started_file" &
    guard_pid=$!
    while [[ ! -e "$started_file" ]]; do
        if ! kill -0 "$guard_pid" >/dev/null 2>&1; then
            wait "$guard_pid" || true
            guard_pid=
            guard_stop_file=
            die "survivor guard failed before transition: $phase"
            return
        fi
        if (( SECONDS >= deadline )); then
            kill "$guard_pid" >/dev/null 2>&1 || true
            wait "$guard_pid" >/dev/null 2>&1 || true
            guard_pid=
            guard_stop_file=
            die "survivor guard did not start: $phase"
            return
        fi
        sleep 1
    done
}

stop_survivor_guard() {
    local phase=$1
    local status=0
    touch "$guard_stop_file"
    wait "$guard_pid" || status=$?
    guard_pid=
    guard_stop_file=
    if (( status != 0 )); then
        die "survivor failed during replacement window: $phase"
        return
    fi
}

finish_transition() {
    local phase=$1
    local replica=$2
    local reference=$3
    local image_id=$4
    local source_commit=$5
    probe_replica "$phase" "$replica"
    append_transition transition_completed "$phase" "$replica" "$reference" "$image_id" "$source_commit"
}

replace_replica() {
    local phase=$1
    local replica=$2
    local survivor=$3
    local reference=$4
    local image_id=$5
    local source_commit=$6
    local container
    container=$(container_for_replica "$replica")

    probe_replica "$phase-survivor-before" "$survivor"
    start_survivor_guard "$phase" "$survivor"
    docker rm -f "$container" >>"$transcript"
    start_replica "$phase" "$replica" "$reference" "$image_id" "$source_commit"
    probe_replica "$phase" "$replica"
    stop_survivor_guard "$phase"
    probe_replica "$phase-survivor-after" "$survivor"
    append_transition transition_completed "$phase" "$replica" "$reference" "$image_id" "$source_commit"
}

start_replica known-good-a a "$rollback_reference" "$rollback_image_id" "$rollback_source_commit"
finish_transition known-good-a a "$rollback_reference" "$rollback_image_id" "$rollback_source_commit"
start_replica known-good-b b "$rollback_reference" "$rollback_image_id" "$rollback_source_commit"
finish_transition known-good-b b "$rollback_reference" "$rollback_image_id" "$rollback_source_commit"

replace_replica upgrade-a a b "$candidate_reference" "$candidate_image_id" "$candidate_source_commit"
replace_replica upgrade-b b a "$candidate_reference" "$candidate_image_id" "$candidate_source_commit"
replace_replica rollback-a a b "$rollback_reference" "$rollback_image_id" "$rollback_source_commit"
replace_replica rollback-b b a "$rollback_reference" "$rollback_image_id" "$rollback_source_commit"

append_result passed
run_finished=true
printf 'upgrade rollback smoke: PASS (evidence: %s)\n' "$run_dir"
