#!/usr/bin/env bash
set -euo pipefail

# This gate is deliberately cache-only. The source comparison must be
# reproducible from the pinned module archive and must not silently fetch a
# different upstream revision through a proxy or a mutable network endpoint.
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY FTP_PROXY
unset http_proxy https_proxy all_proxy no_proxy ftp_proxy

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
module_dir="$repo_root/third_party/rocketmq-client-go"
module='github.com/apache/rocketmq-client-go/v2'
version='v2.1.3-0.20231106021916-c9e197c3af45'

die() {
	printf 'rocketmq patch gate: %s\n' "$*" >&2
	exit 1
}

require_file() {
	[[ -f "$1" ]] || die "required file is missing: $1"
}

sha256_file() {
	local file=$1
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$file" | awk '{print $1}'
		return
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$file" | awk '{print $1}'
		return
	fi
	die 'sha256sum or shasum is required'
}

is_allowed_production_difference() {
	case "$1" in
		internal/client.go|\
		internal/mock_namesrv.go|\
		internal/namesrv.go|\
		internal/remote/remote_client.go|\
		internal/remote/tcp_conn.go|\
		internal/route.go|\
		producer/option.go|\
		producer/producer.go)
			return 0
			;;
		*)
			return 1
			;;
	esac
}

is_allowed_test_difference() {
	case "$1" in
		internal/client_lifecycle_test.go|\
		internal/client_options_isolation_test.go|\
		internal/remote/remote_context_test.go|\
		internal/remote/tcp_conn_context_test.go|\
		internal/route_context_test.go|\
		internal/route_test.go|\
		producer/context_test.go)
			return 0
			;;
		*)
			return 1
			;;
	esac
}

require_file "$module_dir/go.mod"
require_file "$module_dir/go.sum"
require_file "$module_dir/LICENSE"
require_file "$module_dir/NOTICE"
require_file "$module_dir/APISIX_GO_PATCHES.md"

command -v go >/dev/null 2>&1 || die 'go is required to locate the module cache'
command -v unzip >/dev/null 2>&1 || die 'unzip is required for the pinned source archive'

gomodcache=$(go env GOMODCACHE 2>/dev/null) || die 'cannot resolve GOMODCACHE'
archive="$gomodcache/cache/download/github.com/apache/rocketmq-client-go/v2/@v/$version.zip"
ziphash="${archive%.zip}.ziphash"
[[ -s "$archive" ]] || die "pinned source archive is not cached: $archive (run go mod download before this gate)"
[[ -s "$ziphash" ]] || die "pinned source archive hash is not cached: $ziphash"

expected_module_sum=$(awk -v module="$module" -v version="$version" \
	'$1 == module && $2 == version { print $3; exit }' "$repo_root/go.sum")
[[ -n "$expected_module_sum" ]] || die "root go.sum lacks the pinned module sum"
cached_module_sum=$(tr -d '[:space:]' <"$ziphash")
[[ "$cached_module_sum" == "$expected_module_sum" ]] || die \
	"cached module hash $cached_module_sum differs from root go.sum $expected_module_sum"

doc="$module_dir/APISIX_GO_PATCHES.md"
grep -Fq "source copy of" "$doc" || die 'patch provenance document lacks source-copy declaration'
grep -Fq "$version" "$doc" || die "patch provenance document lacks pinned version $version"
grep -Fq 'carries six narrowly scoped runtime-safety patches' "$doc" || \
	die 'patch provenance document patch count is not six'

allowed_production=(
	internal/client.go
	internal/mock_namesrv.go
	internal/namesrv.go
	internal/remote/remote_client.go
	internal/remote/tcp_conn.go
	internal/route.go
	producer/option.go
	producer/producer.go
)
allowed_tests=(
	internal/client_lifecycle_test.go
	internal/client_options_isolation_test.go
	internal/remote/remote_context_test.go
	internal/remote/tcp_conn_context_test.go
	internal/route_context_test.go
	internal/route_test.go
	producer/context_test.go
)
for path in "${allowed_production[@]}" "${allowed_tests[@]}"; do
	grep -Fq "\`$path\`" "$doc" || die "patch document does not list allowed path: $path"
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/rocketmq-patch-gate.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT
unzip -q "$archive" -d "$work_dir"
upstream_dir="$work_dir/${module}@${version}"
[[ -d "$upstream_dir" ]] || die "archive root is not the expected module path: $upstream_dir"

for license_file in LICENSE NOTICE; do
	cmp -s "$upstream_dir/$license_file" "$module_dir/$license_file" || \
		die "$license_file differs from the pinned upstream source"
done

path_list="$work_dir/paths"
{
	(cd "$upstream_dir" && find . -type f -print | sed 's#^\./##')
	(cd "$module_dir" && find . -type f -print | sed 's#^\./##')
} | sort -u >"$path_list"

while IFS= read -r path; do
	[[ -n "$path" ]] || continue
	local_path="$module_dir/$path"
	upstream_path="$upstream_dir/$path"
	local_exists=false
	upstream_exists=false
	[[ -f "$local_path" ]] && local_exists=true
	[[ -f "$upstream_path" ]] && upstream_exists=true

	if [[ "$local_exists" != "$upstream_exists" ]]; then
		if [[ "$path" == APISIX_GO_PATCHES.md && "$local_exists" == true && "$upstream_exists" == false ]]; then
			continue
		fi
		if [[ "$local_exists" == true && "$upstream_exists" == false ]] && \
			is_allowed_test_difference "$path"; then
			continue
		fi
		die "file presence differs outside the documented test additions: $path"
	fi
	if cmp -s "$upstream_path" "$local_path"; then
		continue
	fi
	if is_allowed_production_difference "$path" || is_allowed_test_difference "$path"; then
		continue
	fi
	[[ "$path" == APISIX_GO_PATCHES.md ]] && continue
	die "content differs outside documented RocketMQ patches: $path"
done <"$path_list"

printf 'rocketmq patch gate: PASS\n'
printf '  module=%s\n' "$module"
printf '  version=%s\n' "$version"
printf '  archive_sha256=%s\n' "$(sha256_file "$archive")"
printf '  source_hash=%s\n' "$cached_module_sum"
printf '  production_patch_files=%d\n' "${#allowed_production[@]}"
printf '  focused_test_files=%d\n' "${#allowed_tests[@]}"
