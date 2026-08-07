#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/apisix-go-cache-layout-test.XXXXXX")"

cleanup() {
	chmod -R u+w "$test_root" 2>/dev/null || true
	rm -rf -- "$test_root"
}
trap cleanup EXIT

fixture_repo="$test_root/repo"
fixture_worktree="$test_root/worktree"

git init -q "$fixture_repo"
fixture_repo="$(cd "$fixture_repo" && pwd -P)"
cp "$repo_root/.envrc" "$fixture_repo/.envrc"
printf 'module example.com/cache-layout\n\ngo 1.26.0\n\ntoolchain go1.26.0\n' >"$fixture_repo/go.mod"
git -C "$fixture_repo" add .envrc go.mod
git -C "$fixture_repo" \
	-c user.name=cache-layout-test \
	-c user.email=cache-layout-test@example.invalid \
	commit -qm initial
git -C "$fixture_repo" worktree add -q -b linked "$fixture_worktree"
fixture_worktree="$(cd "$fixture_worktree" && pwd -P)"

capture_layout() {
	local checkout="$1"
	(
		cd "$checkout"
		unset APISIX_GO_ROOT APISIX_GO_SHARED_CACHE
		unset GOPATH GOBIN GOCACHE GOMODCACHE GOLANGCI_LINT_CACHE
		unset GOTMPDIR TMPDIR TEST_TELEMETRY_DIR
		# shellcheck disable=SC1091
		source .envrc
		printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
			"$APISIX_GO_ROOT" \
			"${APISIX_GO_SHARED_CACHE:-}" \
			"$GOPATH" \
			"$GOBIN" \
			"$GOCACHE" \
			"$GOMODCACHE" \
			"${GOLANGCI_LINT_CACHE:-}" \
			"$GOTMPDIR" \
			"$TEST_TELEMETRY_DIR"
	)
}

IFS=$'\t' read -r main_root main_shared main_gopath main_gobin main_gocache main_gomodcache main_lint_cache main_tmp main_telemetry < <(capture_layout "$fixture_repo")
IFS=$'\t' read -r linked_root linked_shared linked_gopath linked_gobin linked_gocache linked_gomodcache linked_lint_cache linked_tmp linked_telemetry < <(capture_layout "$fixture_worktree")

assert_equal() {
	local label="$1"
	local left="$2"
	local right="$3"
	if [[ "$left" != "$right" ]]; then
		printf '%s differs: %q != %q\n' "$label" "$left" "$right" >&2
		exit 1
	fi
}

assert_different() {
	local label="$1"
	local left="$2"
	local right="$3"
	if [[ "$left" == "$right" ]]; then
		printf '%s unexpectedly shared: %q\n' "$label" "$left" >&2
		exit 1
	fi
}

if [[ -z "$main_shared" ]]; then
	printf 'APISIX_GO_SHARED_CACHE is not set\n' >&2
	exit 1
fi

assert_different APISIX_GO_ROOT "$main_root" "$linked_root"
assert_equal APISIX_GO_SHARED_CACHE "$main_shared" "$linked_shared"
assert_equal GOPATH "$main_gopath" "$linked_gopath"
assert_equal GOBIN "$main_gobin" "$linked_gobin"
assert_equal GOCACHE "$main_gocache" "$linked_gocache"
assert_equal GOMODCACHE "$main_gomodcache" "$linked_gomodcache"
assert_equal GOLANGCI_LINT_CACHE "$main_lint_cache" "$linked_lint_cache"
assert_different GOTMPDIR "$main_tmp" "$linked_tmp"
assert_different TEST_TELEMETRY_DIR "$main_telemetry" "$linked_telemetry"

case "$main_shared" in
"$fixture_repo"/.cache/shared) ;;
*)
	printf 'shared cache is outside the main checkout: %s\n' "$main_shared" >&2
	exit 1
	;;
esac

case "$main_tmp" in
"$fixture_repo"/.cache/*) ;;
*)
	printf 'main temporary directory is outside its checkout: %s\n' "$main_tmp" >&2
	exit 1
	;;
esac

case "$linked_tmp" in
"$fixture_worktree"/.cache/*) ;;
*)
	printf 'linked temporary directory is outside its checkout: %s\n' "$linked_tmp" >&2
	exit 1
	;;
esac

cleanup_fixture="$test_root/cleanup"
mkdir -p \
	"$cleanup_fixture/.cache/go-mod/example.com/module@v1.0.0" \
	"$cleanup_fixture/.cache/tmp" \
	"$cleanup_fixture/.cache/out" \
	"$cleanup_fixture/.cache/bench" \
	"$cleanup_fixture/.cache/coverage" \
	"$cleanup_fixture/.cache/shared"
printf 'module cache\n' >"$cleanup_fixture/.cache/go-mod/example.com/module@v1.0.0/go.mod"
printf 'keep\n' >"$cleanup_fixture/.cache/bench/keep"
printf 'keep\n' >"$cleanup_fixture/.cache/coverage/keep"
printf 'keep\n' >"$cleanup_fixture/.cache/shared/keep"
chmod -R a-w "$cleanup_fixture/.cache/go-mod"

make -s -C "$cleanup_fixture" -f "$repo_root/Makefile" cache-clean-local

for removed in .cache/go-mod .cache/tmp .cache/out; do
	if [[ -e "$cleanup_fixture/$removed" ]]; then
		printf 'cache-clean-local did not remove %s\n' "$removed" >&2
		exit 1
	fi
done
for preserved in .cache/bench/keep .cache/coverage/keep .cache/shared/keep; do
	if [[ ! -f "$cleanup_fixture/$preserved" ]]; then
		printf 'cache-clean-local removed %s\n' "$preserved" >&2
		exit 1
	fi
done

printf 'cache layout test: PASS\n'
