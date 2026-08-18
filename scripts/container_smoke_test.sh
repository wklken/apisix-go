#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
dockerfile="$repo_root/Dockerfile"
smoke="$repo_root/scripts/container_smoke.sh"

require_pattern() {
    local pattern=$1
    local file=$2
    if ! grep -Eq -- "$pattern" "$file"; then
        printf 'missing %q in %s\n' "$pattern" "$file" >&2
        return 1
    fi
}

require_pattern '^FROM golang:1\.26\.6-alpine3\.24 AS builder$' "$dockerfile"
require_pattern '^FROM alpine:3\.24\.1$' "$dockerfile"
require_pattern 'go build[[:space:]]+-trimpath[[:space:]]+-ldflags[[:space:]]+"-s[[:space:]]+-w[[:space:]]+' "$dockerfile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.Version=\$\{VERSION\}' "$dockerfile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.Commit=\$\{COMMIT\}' "$dockerfile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.BuildTime=\$\{BUILD_TIME\}' "$dockerfile"
require_pattern '-X[[:space:]]+[[:punct:]]?github\.com/wklken/apisix-go/pkg/version\.GoVersion=\$\{GO_VERSION\}' "$dockerfile"
require_pattern 'apk add --no-cache.*ca-certificates.*curl' "$dockerfile"
require_pattern 'addgroup .*10001' "$dockerfile"
require_pattern 'adduser .*10001' "$dockerfile"
require_pattern '^USER 10001:10001$' "$dockerfile"
require_pattern '^HEALTHCHECK ' "$dockerfile"

test -x "$smoke"
bash -n "$smoke"
require_pattern '^[[:space:]]*"\$image" -c /usr/local/apisix/conf/config.yaml' "$smoke"
require_pattern 'flock|mkdir .*lock' "$smoke"
require_pattern 'docker network create' "$smoke"
require_pattern 'busybox:1\.37\.0' "$smoke"
require_pattern '/www/smoke' "$smoke"
require_pattern 'upstream_ip=.*docker inspect.*IPAddress' "$smoke"
require_pattern 'upstream_deadline=' "$smoke"
require_pattern '\$\{upstream_ip\}:8081' "$smoke"
require_pattern '^#END$' "$smoke"
require_pattern 'docker inspect.*Health' "$smoke"
require_pattern 'proxy_deadline=' "$smoke"
require_pattern 'docker exec.*id -u' "$smoke"
require_pattern 'docker exec.*id -g' "$smoke"
require_pattern 'docker kill.*TERM' "$smoke"
require_pattern 'shutdown_deadline=' "$smoke"
require_pattern 'docker wait' "$smoke"

test_bin=$(mktemp -d "${TMPDIR:-/tmp}/apisix-container-contract-bin.XXXXXX")
failure_output=$(mktemp "${TMPDIR:-/tmp}/apisix-container-contract-output.XXXXXX")
trap 'rm -rf "$test_bin" "$failure_output"' EXIT
ln -s "$(command -v dirname)" "$test_bin/dirname"
if PATH="$test_bin" "$(command -v bash)" "$smoke" >"$failure_output" 2>&1; then
    printf 'container smoke accepted a host without Docker\n' >&2
    exit 1
fi
if ! grep -Fq 'required command is unavailable: docker' "$failure_output"; then
    printf 'container smoke missing-prerequisite error was not deterministic\n' >&2
    cat "$failure_output" >&2
    exit 1
fi

printf 'container release contract: PASS\n'
