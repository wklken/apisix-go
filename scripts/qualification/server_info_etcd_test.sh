#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
container_bin=${CONTAINER_BIN:-podman}
candidate_image=${1:-}
etcd_image=${2:-}

die() {
    printf 'server-info etcd test: %s\n' "$*" >&2
    exit 1
}

for command_name in "$container_bin" python3; do
    command -v "$command_name" >/dev/null 2>&1 || die "required command is unavailable: $command_name"
done
for image in "$candidate_image" "$etcd_image"; do
    [[ "$image" =~ ^sha256:[0-9a-f]{64}$ || "$image" =~ @sha256:[0-9a-f]{64}$ ]] || \
        die 'pass immutable candidate and etcd image IDs or digest-qualified references'
    "$container_bin" image inspect "$image" >/dev/null 2>&1 || die "image is unavailable: $image"
done

container() {
    command "$container_bin" "$@"
}

probe_mode=host
if [[ $(basename "$container_bin") == podman ]] && container machine ssh true >/dev/null 2>&1; then
    probe_mode=podman-machine
fi

probe_curl() {
    if [[ "$probe_mode" == podman-machine ]]; then
        container machine ssh -- curl "$@"
        return
    fi
    command curl "$@"
}

if [[ "$probe_mode" == host ]]; then
    command -v curl >/dev/null 2>&1 || die 'required command is unavailable: curl'
fi

mkdir -p "$repo_root/.cache/tmp"
work_dir=$(mktemp -d "$repo_root/.cache/tmp/server-info-etcd.XXXXXX")
run_id="$$-${RANDOM}"
network_name="apisix-go-server-info-$run_id"
etcd_name="apisix-go-server-info-etcd-$run_id"
gateway_name="apisix-go-server-info-gateway-$run_id"

cleanup() {
    status=$?
    set +e
    container logs "$gateway_name" >"$work_dir/gateway.log" 2>&1 || true
    container logs "$etcd_name" >"$work_dir/etcd.log" 2>&1 || true
    container rm -f "$gateway_name" "$etcd_name" >/dev/null 2>&1 || true
    container network rm "$network_name" >/dev/null 2>&1 || true
    if (( status == 0 )); then
        rm -rf "$work_dir"
    else
        printf 'server-info etcd test: evidence retained at %s\n' "$work_dir" >&2
    fi
    trap - EXIT
    exit "$status"
}
trap cleanup EXIT

container network create "$network_name" >"$work_dir/network.id"
container run -d --name "$etcd_name" --network "$network_name" --network-alias etcd "$etcd_image" \
    /usr/local/bin/etcd \
    --name=etcd0 \
    --data-dir=/etcd-data \
    --listen-client-urls=http://0.0.0.0:2379 \
    --advertise-client-urls=http://etcd:2379 >"$work_dir/etcd-container.id"

deadline=$((SECONDS + 30))
until container exec "$etcd_name" /usr/local/bin/etcdctl \
    --endpoints=http://127.0.0.1:2379 endpoint health >"$work_dir/etcd-health.log" 2>&1; do
    (( SECONDS < deadline )) || die 'etcd did not become healthy'
    sleep 1
done

config_path="$work_dir/config.yaml"
cat >"$config_path" <<'EOF'
qualification_profile: ""
apisix:
  id: "123456"
  node_listen:
    - ip: 0.0.0.0
      port: 9080
  status:
    ip: 0.0.0.0
    port: 7085
plugins:
  - server-info
plugin_attr:
  server-info:
    report_ttl: 3
deployment:
  role: traditional
  role_traditional:
    config_provider: etcd
  etcd:
    host:
      - http://etcd:2379
    prefix: /apisix
    timeout: 3
apisix_go:
  runtime_paths:
    data_dir: /tmp/apisix-go/data
    log_dir: /tmp/apisix-go/log
    runtime_dir: /tmp/apisix-go/run
    temp_dir: /tmp/apisix-go/tmp
EOF

container create --name "$gateway_name" --network "$network_name" \
    --publish 127.0.0.1::9080 "$candidate_image" >"$work_dir/gateway-container.id"
container cp "$config_path" "$gateway_name:/usr/local/apisix/conf/config-production.yaml"
container start "$gateway_name" >/dev/null

published=$(container port "$gateway_name" 9080/tcp | tail -n 1)
gateway_port=${published##*:}
[[ "$gateway_port" =~ ^[0-9]+$ ]] || die "cannot resolve mapped gateway port from: $published"
gateway_url="http://127.0.0.1:$gateway_port"
printf '%s\n' "$gateway_url" >"$work_dir/gateway-url"

control_body="$work_dir/control.json"
deadline=$((SECONDS + 45))
while true; do
    if probe_curl --silent --fail --max-time 2 "$gateway_url/v1/server_info" \
        >"$control_body" 2>>"$work_dir/curl.log" && \
        python3 - "$control_body" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    value = json.load(source)
raise SystemExit(0 if value.get("etcd_version") == "3.6.13" else 1)
PY
    then
        break
    fi
    [[ $(container inspect --format '{{.State.Running}}' "$gateway_name" 2>/dev/null) == true ]] || \
        die 'candidate gateway exited before server-info became ready'
    (( SECONDS < deadline )) || die 'server-info control API did not report etcd 3.6.13'
    sleep 1
done

record_body="$work_dir/record.json"
deadline=$((SECONDS + 15))
while true; do
    if container exec "$etcd_name" /usr/local/bin/etcdctl \
        --endpoints=http://127.0.0.1:2379 \
        get /apisix/data_plane/server_info/123456 --print-value-only >"$record_body" 2>>"$work_dir/etcdctl.log" && \
        [[ -s "$record_body" ]]; then
        break
    fi
    (( SECONDS < deadline )) || die 'server-info record was not published to etcd'
    sleep 1
done

python3 - "$control_body" "$record_body" <<'PY'
import json, re, sys

with open(sys.argv[1], encoding="utf-8") as source:
    control = json.load(source)
with open(sys.argv[2], encoding="utf-8") as source:
    record = json.load(source)

if control != record:
    raise SystemExit(f"control API and etcd record differ: {control!r} != {record!r}")
if control.get("etcd_version") != "3.6.13":
    raise SystemExit(f"unexpected etcd version: {control.get('etcd_version')!r}")
if control.get("id") != "123456":
    raise SystemExit(f"unexpected node id: {control.get('id')!r}")
if not isinstance(control.get("boot_time"), int) or control["boot_time"] <= 0:
    raise SystemExit(f"invalid boot_time: {control.get('boot_time')!r}")
if not re.fullmatch(r"[0-9A-Za-z._+-]+", control.get("version", "")):
    raise SystemExit(f"invalid gateway version: {control.get('version')!r}")
if not control.get("hostname"):
    raise SystemExit("hostname is empty")
PY

printf 'server-info etcd test: PASS (version=3.6.13 id=123456)\n'
