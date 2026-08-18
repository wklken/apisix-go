#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
image=${APISIX_IMAGE:-apisix-go:container-smoke}
network="apisix-smoke-${$}"
gateway="apisix-smoke-gateway-${$}"
upstream="apisix-smoke-upstream-${$}"
lock_dir="${TMPDIR:-/tmp}/apisix-go-container-smoke.lock"
temp_dir=""

require_command() {
    if ! command -v "$1" >/dev/null 2>&1; then
        printf 'required command is unavailable: %s\n' "$1" >&2
        exit 1
    fi
}

cleanup() {
    docker rm -f "$gateway" "$upstream" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
    if [[ -n "$temp_dir" ]]; then
        rm -rf "$temp_dir"
    fi
    rmdir "$lock_dir" >/dev/null 2>&1 || true
}

require_command docker
require_command curl
if ! mkdir "$lock_dir" 2>/dev/null; then
    printf 'another container smoke owns %s\n' "$lock_dir" >&2
    exit 1
fi
trap cleanup EXIT INT TERM

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/apisix-container-smoke.XXXXXX")
cat >"$temp_dir/config.yaml" <<'YAML'
apisix:
  node_listen:
    - ip: 0.0.0.0
      port: 9080
plugins: [request-id]
deployment:
  role: data_plane
  role_data_plane:
    config_provider: yaml
YAML

if [[ ${APISIX_SKIP_BUILD:-0} != 1 ]]; then
    docker build \
        --build-arg VERSION=container-smoke \
        --build-arg COMMIT="$(git -C "$repo_root" rev-parse --short HEAD)" \
        --build-arg BUILD_TIME=container-smoke \
        --build-arg GO_VERSION="$(go version)" \
        --tag "$image" "$repo_root"
fi

docker network create "$network" >/dev/null
docker run --detach --name "$upstream" --network "$network" --network-alias apisix-smoke-upstream \
    busybox:1.37.0 sh -c \
    'mkdir -p /www && printf "%s" "apisix-container-smoke" >/www/smoke && exec httpd -f -p 8081 -h /www' \
    >/dev/null
upstream_deadline=$((SECONDS + 30))
upstream_ip=""
while (( SECONDS < upstream_deadline )); do
    upstream_ip=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$upstream")
    if [[ -n "$upstream_ip" ]]; then
        break
    fi
    sleep 1
done
if [[ -z "$upstream_ip" ]]; then
    docker inspect --format '{{json .State}} {{json .NetworkSettings.Networks}}' "$upstream" >&2
    docker logs "$upstream" >&2
    printf 'upstream container has no network address\n' >&2
    exit 1
fi
cat >"$temp_dir/apisix.yaml" <<YAML
routes:
  - id: container-smoke
    uri: /smoke
    plugins:
      request-id: {}
    upstream:
      type: roundrobin
      nodes:
        "${upstream_ip}:8081": 1
#END
YAML
docker run --detach --name "$gateway" --network "$network" --publish 127.0.0.1::9080 \
    --volume "$temp_dir/config.yaml:/usr/local/apisix/conf/config.yaml:ro" \
    --volume "$temp_dir/apisix.yaml:/usr/local/apisix/conf/apisix.yaml:ro" \
    "$image" -c /usr/local/apisix/conf/config.yaml >/dev/null

published=$(docker port "$gateway" 9080/tcp | head -n 1)
proxy_deadline=$((SECONDS + 90))
response=""
until response=$(curl --fail --silent --show-error "http://${published}/smoke"); do
    if (( SECONDS >= proxy_deadline )); then
        docker logs "$gateway" >&2
        printf 'proxied request did not succeed before timeout\n' >&2
        exit 1
    fi
    sleep 1
done
if [[ "$response" != apisix-container-smoke ]]; then
    printf 'proxied response = %q, want apisix-container-smoke\n' "$response" >&2
    exit 1
fi
if [[ $(docker exec "$gateway" id -u) != 10001 ]]; then
    printf 'gateway is not running as UID 10001\n' >&2
    exit 1
fi
if [[ $(docker exec "$gateway" id -g) != 10001 ]]; then
    printf 'gateway is not running as GID 10001\n' >&2
    exit 1
fi

docker kill --signal TERM "$gateway" >/dev/null
shutdown_deadline=$((SECONDS + 30))
while [[ $(docker inspect --format '{{.State.Running}}' "$gateway") == true ]]; do
    if (( SECONDS >= shutdown_deadline )); then
        docker logs "$gateway" >&2
        printf 'gateway did not exit before shutdown timeout\n' >&2
        exit 1
    fi
    sleep 1
done
exit_code=$(docker wait "$gateway")
if [[ "$exit_code" != 0 ]]; then
    docker logs "$gateway" >&2
    printf 'gateway exit code = %s, want 0 after TERM\n' "$exit_code" >&2
    exit 1
fi

printf 'container smoke: PASS\n'
