#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache_script="$repo_root/scripts/go_cache.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/apisix-go-cache-gc-test.XXXXXX")"
background_pids=()

cleanup() {
	local pid
	for pid in "${background_pids[@]}"; do
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	rm -rf -- "$test_root"
}
trap cleanup EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

assert_file() {
	local path="$1"
	[[ -f "$path" ]] || fail "expected file: $path"
}

assert_missing() {
	local path="$1"
	[[ ! -e "$path" ]] || fail "expected missing path: $path"
}

wait_for_file() {
	local path="$1"
	for _ in {1..100}; do
		[[ -f "$path" ]] && return 0
		sleep 0.05
	done
	fail "timed out waiting for $path"
}

fake_bin="$test_root/bin"
shared_cache="$test_root/shared"
go_cache="$shared_cache/go-build"
go_log="$test_root/go.log"
real_du="$(command -v du)"
mkdir -p "$fake_bin" "$go_cache"
: >"$go_log"

# The fake script expands these variables when it runs, not while generated.
# shellcheck disable=SC2016
printf '%s\n' \
	'#!/usr/bin/env bash' \
	'set -euo pipefail' \
	'printf "%s\\n" "$*" >>"$GO_CACHE_TEST_LOG"' \
	'if [[ "$*" == "clean -cache" ]]; then' \
	'  [[ "${GO_CACHE_TEST_CLEAN_FAIL:-0}" != "1" ]] || exit 9' \
	'  find "$GOCACHE" -mindepth 1 -maxdepth 1 -type d -name "[0-9a-f][0-9a-f]" -exec rm -rf -- {} +' \
	'fi' >"$fake_bin/go"
chmod +x "$fake_bin/go"

# The fake du can pause a pressure scan so a second runner can prove the
# coordination gate remains available.
# shellcheck disable=SC2016
printf '%s\n' \
	'#!/usr/bin/env bash' \
	'set -euo pipefail' \
	'if [[ -n "${GO_CACHE_TEST_DU_READY:-}" ]]; then' \
	'  touch "$GO_CACHE_TEST_DU_READY"' \
	'  while [[ ! -f "$GO_CACHE_TEST_DU_STOP" ]]; do sleep 0.05; done' \
	'fi' \
	'exec "$GO_CACHE_TEST_REAL_DU" "$@"' >"$fake_bin/du"
chmod +x "$fake_bin/du"

run_cache() {
	APISIX_GO_SHARED_CACHE="$shared_cache" \
		GOCACHE="$go_cache" \
		GO_CACHE_TEST_LOG="$go_log" \
		GO_CACHE_TEST_REAL_DU="$real_du" \
		PATH="$fake_bin:$PATH" \
		"$cache_script" "$@"
}

make_cache_entry() {
	local name="$1"
	mkdir -p "$go_cache/00"
	printf 'cache data\n' >"$go_cache/00/$name"
}

# An explicit clean must refuse while another participating command is active.
ready="$test_root/lease-ready"
stop="$test_root/lease-stop"
make_cache_entry active
# The child shell receives the ready and stop paths as positional arguments.
# shellcheck disable=SC2016
run_cache run -- bash -c 'touch "$1"; while [[ ! -f "$2" ]]; do sleep 0.05; done' _ "$ready" "$stop" &
lease_runner_pid=$!
background_pids+=("$lease_runner_pid")
wait_for_file "$ready"
if run_cache clean >"$test_root/clean.out" 2>"$test_root/clean.err"; then
	fail 'clean succeeded while a lease was active'
fi
grep -Fq 'active cache users' "$test_root/clean.err" || fail 'clean refusal did not report active users'
assert_file "$go_cache/00/active"
[[ ! -s "$go_log" ]] || fail 'clean invoked go while a lease was active'
touch "$stop"
wait "$lease_runner_pid"

# A wrapper signal leaves the lease active until its child exits.
signal_ready="$test_root/signal-ready"
signal_child_pid="$test_root/signal-child-pid"
make_cache_entry signal
# The child shell owns signal handling and writes its real PID to the fixture.
# shellcheck disable=SC2016
run_cache run -- bash -c 'trap "exit 0" TERM; printf "%s\n" "$$" >"$1"; touch "$2"; sleep 2' _ "$signal_child_pid" "$signal_ready" \
	>"$test_root/signal-runner.out" 2>"$test_root/signal-runner.err" &
signal_runner_pid=$!
background_pids+=("$signal_runner_pid")
wait_for_file "$signal_ready"
kill -TERM "$signal_runner_pid"
set +e
wait "$signal_runner_pid"
signal_status=$?
set -e
[[ "$signal_status" -eq 143 ]] || fail "TERM changed wrapper status to $signal_status"
child_pid="$(cat "$signal_child_pid")"
kill -0 "$child_pid" 2>/dev/null || fail 'signal fixture child did not remain active'
if run_cache clean >"$test_root/signal-clean.out" 2>"$test_root/signal-clean.err"; then
	fail 'clean succeeded while the signaled wrapper child was still active'
fi
grep -Fq 'active cache users' "$test_root/signal-clean.err" ||
	fail 'child-owned lease was not reported after wrapper TERM'
assert_file "$go_cache/00/signal"
for _ in {1..100}; do
	if ! kill -0 "$child_pid" 2>/dev/null; then
		break
	fi
	sleep 0.05
done
kill -0 "$child_pid" 2>/dev/null && fail 'signal fixture child did not exit'
run_cache clean
assert_missing "$go_cache/00/signal"

# Forced clean succeeds after the final lease exits.
run_cache clean
assert_missing "$go_cache/00/active"
grep -Fxq 'clean -cache' "$go_log" || fail 'clean did not invoke go clean -cache'

# A stale lease is pruned instead of blocking cleanup forever.
: >"$go_log"
make_cache_entry stale
mkdir -p "$shared_cache/cache-coordination/active"
printf '999999\n' >"$shared_cache/cache-coordination/active/999999-stale"
run_cache clean
assert_missing "$go_cache/00/stale"
assert_missing "$shared_cache/cache-coordination/active/999999-stale"

# A dead gate owner disables cleanup without blocking the wrapped command.
stale_claim="$shared_cache/cache-coordination/gate.claim.999999.stale"
printf '999999 0 0 %s\n' "$stale_claim" >"$stale_claim"
ln "$stale_claim" "$shared_cache/cache-coordination/gate.lock"
run_cache run -- true 2>"$test_root/stale-gate.err"
grep -Fq 'running command without a cache lease' "$test_root/stale-gate.err" ||
	fail 'stale gate did not report fail-safe uncoordinated execution'
assert_file "$shared_cache/cache-coordination/gate.lock"
assert_file "$stale_claim"
if run_cache clean >"$test_root/stale-clean.out" 2>"$test_root/stale-clean.err"; then
	fail 'clean succeeded through a stale coordination gate'
fi
rm -f "$shared_cache/cache-coordination/gate.lock" "$stale_claim"

# Measuring a large cache must not hold the registration gate.
rm -f "$shared_cache/cache-coordination/last-check" "$shared_cache/cache-coordination/last-clean"
du_ready="$test_root/du-ready"
du_stop="$test_root/du-stop"
second_done="$test_root/second-done"
(
	GO_CACHE_TEST_DU_READY="$du_ready" \
		GO_CACHE_TEST_DU_STOP="$du_stop" \
		CACHE_GC_MAX_GIB=999999 \
		CACHE_GC_MIN_FREE_GIB=0 \
		CACHE_GC_CHECK_INTERVAL=0 \
		run_cache run -- true
) &
scan_runner_pid=$!
background_pids+=("$scan_runner_pid")
wait_for_file "$du_ready"
(
	CACHE_GC_CHECK_INTERVAL=3600 run_cache run -- true
	touch "$second_done"
) &
second_runner_pid=$!
background_pids+=("$second_runner_pid")
wait_for_file "$second_done"
touch "$du_stop"
wait "$scan_runner_pid"
wait "$second_runner_pid"

# The live target installs Air under a short lease, then runs it without a
# process-lifetime shared-cache lease.
live_bin="$test_root/live-bin"
mkdir -p "$live_bin"
live_bin="$(cd "$live_bin" && pwd -P)"
live_dry_run="$(make -s -n -C "$repo_root" live CACHE_BIN="$live_bin" GO_CACHE_RUNNER=runner)"
grep -Fq "runner env GOBIN=\"$live_bin\" go install github.com/cosmtrek/air@v1.51.0" <<<"$live_dry_run" ||
	fail 'live target does not install Air through a bounded lease'
grep -Fq "$live_bin/air" <<<"$live_dry_run" ||
	fail 'live target does not execute the installed Air binary'
if grep -Fq "runner $live_bin/air" <<<"$live_dry_run"; then
	fail 'live target holds a cache lease for the Air process lifetime'
fi

# Automatic GC runs after the last lease when the size threshold is exceeded.
: >"$go_log"
rm -f "$shared_cache/cache-coordination/last-clean" "$shared_cache/cache-coordination/last-check"
make_cache_entry automatic
CACHE_GC_MAX_GIB=0 \
	CACHE_GC_MIN_FREE_GIB=0 \
	CACHE_GC_CHECK_INTERVAL=0 \
	CACHE_GC_COOLDOWN=0 \
	run_cache run -- true
assert_missing "$go_cache/00/automatic"
grep -Fxq 'clean -cache' "$go_log" || fail 'automatic GC did not clean under pressure'

# A recent cleanup suppresses another automatic cleanup during the cooldown.
: >"$go_log"
make_cache_entry cooldown
date +%s >"$shared_cache/cache-coordination/last-clean"
rm -f "$shared_cache/cache-coordination/last-check"
CACHE_GC_MAX_GIB=0 \
	CACHE_GC_MIN_FREE_GIB=0 \
	CACHE_GC_CHECK_INTERVAL=0 \
	CACHE_GC_COOLDOWN=86400 \
	run_cache run -- true
assert_file "$go_cache/00/cooldown"
[[ ! -s "$go_log" ]] || fail 'automatic GC ignored the cleanup cooldown'

# Failed housekeeping must not replace the wrapped command's exit status.
rm -f "$shared_cache/cache-coordination/last-clean" "$shared_cache/cache-coordination/last-check"
set +e
GO_CACHE_TEST_CLEAN_FAIL=1 \
	CACHE_GC_MAX_GIB=0 \
	CACHE_GC_MIN_FREE_GIB=0 \
	CACHE_GC_CHECK_INTERVAL=0 \
	CACHE_GC_COOLDOWN=0 \
	run_cache run -- bash -c 'exit 7' >"$test_root/failing-clean.out" 2>"$test_root/failing-clean.err"
command_status=$?
set -e
[[ "$command_status" -eq 7 ]] || fail "wrapped exit status changed to $command_status"
grep -Fq 'automatic cleanup check failed' "$test_root/failing-clean.err" ||
	fail 'failed automatic cleanup was not reported'
assert_missing "$shared_cache/cache-coordination/last-clean"

printf 'go cache test: PASS\n'
