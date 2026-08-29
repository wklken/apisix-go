#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
dockerfile="$repo_root/Dockerfile"
makefile="$repo_root/Makefile"
gate="$repo_root/scripts/qualification/rocketmq_patch_gate.sh"
patch_doc="$repo_root/third_party/rocketmq-client-go/APISIX_GO_PATCHES.md"

require_fixed() {
	local text=$1 file=$2
	grep -Fq -- "$text" "$file" || {
		printf 'rocketmq contract: missing %q in %s\n' "$text" "$file" >&2
		exit 1
	}
}

line_number() {
	local pattern=$1 file=$2 line
	line=$(grep -nF -- "$pattern" "$file" | head -n 1 | cut -d: -f1 || true)
	[[ -n "$line" ]] || {
		printf 'rocketmq contract: missing %q in %s\n' "$pattern" "$file" >&2
		exit 1
	}
	printf '%s\n' "$line"
}

test -x "$gate"
bash -n "$gate"
require_fixed 'This gate is deliberately cache-only.' "$gate"
require_fixed 'LICENSE' "$gate"
require_fixed 'NOTICE' "$gate"
require_fixed 'carries six narrowly scoped runtime-safety patches' "$patch_doc"

nested_mod=$(line_number 'COPY third_party/rocketmq-client-go/go.mod /app/third_party/rocketmq-client-go/go.mod' "$dockerfile")
nested_sum=$(line_number 'COPY third_party/rocketmq-client-go/go.sum /app/third_party/rocketmq-client-go/go.sum' "$dockerfile")
download=$(line_number 'RUN go mod download' "$dockerfile")
full_copy=$(line_number 'COPY third_party/rocketmq-client-go /app/third_party/rocketmq-client-go' "$dockerfile")
build=$(line_number 'RUN go build -trimpath' "$dockerfile")
(( nested_mod < download && nested_sum < download && download < full_copy && full_copy < build )) || {
	printf 'rocketmq contract: Dockerfile nested-module copy order is invalid\n' >&2
	exit 1
}

require_fixed '.PHONY: test-rocketmq-patch' "$makefile"
require_fixed '.PHONY: test-rocketmq-client-patch' "$makefile"
require_fixed '.PHONY: test-rocketmq-nested' "$makefile"
require_fixed '$(MAKE) test-rocketmq-client-patch' "$makefile"
require_fixed '$(GO_CACHE_RUNNER) go mod download github.com/apache/rocketmq-client-go/v2@v2.1.3-0.20231106021916-c9e197c3af45' "$makefile"
require_fixed 'test-rocketmq-client-patch: test-rocketmq-patch test-rocketmq-nested' "$makefile"
require_fixed 'cd third_party/rocketmq-client-go && ../../scripts/go_cache.sh run -- go test ./internal/remote' "$makefile"
require_fixed 'TestDoRequestWaitsForCancellationDeadlineCallbackBeforeReusingConnection' "$makefile"
require_fixed 'cd third_party/rocketmq-client-go && ../../scripts/go_cache.sh run -- go test ./internal' "$makefile"
require_fixed 'cd third_party/rocketmq-client-go && ../../scripts/go_cache.sh run -- go test ./producer' "$makefile"

"$gate"
printf 'rocketmq patch/container contract: PASS\n'
