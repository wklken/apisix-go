#!/usr/bin/env bash

set -euo pipefail

cache_max_gib="${CACHE_GC_MAX_GIB:-50}"
min_free_gib="${CACHE_GC_MIN_FREE_GIB:-40}"
check_interval="${CACHE_GC_CHECK_INTERVAL:-3600}"
cleanup_cooldown="${CACHE_GC_COOLDOWN:-43200}"
gate_timeout="${CACHE_GC_GATE_TIMEOUT:-60}"

coord_dir=""
active_dir=""
gate_lock=""
gate_claim=""
gate_token=""
gate_held=0
lease_file=""
child_pid=""
cache_kib=0
free_kib=0

usage() {
	cat <<'EOF'
usage: scripts/go_cache.sh run -- <command> [args...]
       scripts/go_cache.sh gc
       scripts/go_cache.sh clean
       scripts/go_cache.sh status

run     Execute a command while holding an active shared-cache lease.
gc      Clean under configured pressure thresholds when no lease is active.
clean   Clean regardless of thresholds when no lease is active.
status  Print cache pressure, coordination, and cleanup status.
EOF
}

fail() {
	printf 'go cache: %s\n' "$*" >&2
	exit 1
}

validate_uint() {
	local name="$1"
	local value="$2"
	case "$value" in
	'' | *[!0-9]*) fail "$name must be a non-negative integer, got $value" ;;
	esac
}

validate_settings() {
	validate_uint CACHE_GC_MAX_GIB "$cache_max_gib"
	validate_uint CACHE_GC_MIN_FREE_GIB "$min_free_gib"
	validate_uint CACHE_GC_CHECK_INTERVAL "$check_interval"
	validate_uint CACHE_GC_COOLDOWN "$cleanup_cooldown"
	validate_uint CACHE_GC_GATE_TIMEOUT "$gate_timeout"
}

initialize_coordination() {
	[[ -n "${APISIX_GO_SHARED_CACHE:-}" ]] || fail 'source .envrc before cache coordination'
	[[ -n "${GOCACHE:-}" ]] || fail 'GOCACHE is not set'
	case "$APISIX_GO_SHARED_CACHE" in
	/ | "$HOME" | '') fail "unsafe APISIX_GO_SHARED_CACHE: $APISIX_GO_SHARED_CACHE" ;;
	esac
	[[ "$GOCACHE" == "$APISIX_GO_SHARED_CACHE/go-build" ]] ||
		fail "GOCACHE must be $APISIX_GO_SHARED_CACHE/go-build, got $GOCACHE"

	coord_dir="$APISIX_GO_SHARED_CACHE/cache-coordination"
	active_dir="$coord_dir/active"
	gate_lock="$coord_dir/gate.lock"
	mkdir -p "$active_dir" "$GOCACHE"
}

read_epoch() {
	local path="$1"
	local value=""
	if [[ -f "$path" ]]; then
		IFS= read -r value <"$path" || true
	fi
	case "$value" in
	'' | *[!0-9]*) printf '0\n' ;;
	*) printf '%s\n' "$value" ;;
	esac
}

write_epoch() {
	local path="$1"
	local value="$2"
	local temporary="$path.tmp.$$.$RANDOM"
	printf '%s\n' "$value" >"$temporary"
	mv -f -- "$temporary" "$path"
}

acquire_gate() {
	local started now owner_pid
	started="$(date +%s)"
	gate_claim="$coord_dir/gate.claim.$$.$RANDOM"
	gate_token="$$ $started $RANDOM $gate_claim"
	printf '%s\n' "$gate_token" >"$gate_claim"

	while ! ln "$gate_claim" "$gate_lock" 2>/dev/null; do
		owner_pid=""
		if [[ -f "$gate_lock" ]]; then
			read -r owner_pid _ <"$gate_lock" || true
		fi
		if [[ "$owner_pid" =~ ^[0-9]+$ ]] && ! kill -0 "$owner_pid" 2>/dev/null; then
			rm -f -- "$gate_claim"
			gate_claim=""
			printf 'go cache: stale coordination gate owner %s\n' "$owner_pid" >&2
			return 2
		fi
		now="$(date +%s)"
		if ((now - started >= gate_timeout)); then
			rm -f -- "$gate_claim"
			gate_claim=""
			printf 'go cache: timed out waiting for cache coordination gate after %ss\n' "$gate_timeout" >&2
			return 1
		fi
		sleep 0.05
	done
	gate_held=1
}

release_gate() {
	if ((gate_held == 0)); then
		return
	fi
	local current_token=""
	IFS= read -r current_token <"$gate_lock" || true
	if [[ "$current_token" == "$gate_token" ]]; then
		rm -f -- "$gate_lock"
	fi
	rm -f -- "$gate_claim"
	gate_held=0
}

prune_stale_leases() {
	local lease owner_pid child_owner_pid owner_alive
	shopt -s nullglob
	for lease in "$active_dir"/*; do
		owner_pid=""
		child_owner_pid=""
		owner_alive=0
		read -r owner_pid child_owner_pid _ <"$lease" || true
		if [[ "$owner_pid" =~ ^[0-9]+$ ]] && kill -0 "$owner_pid" 2>/dev/null; then
			owner_alive=1
		fi
		if [[ "$child_owner_pid" =~ ^[0-9]+$ ]] && kill -0 "$child_owner_pid" 2>/dev/null; then
			owner_alive=1
		fi
		if ((owner_alive == 0)); then
			rm -f -- "$lease"
		fi
	done
	shopt -u nullglob
}

active_lease_count() {
	local leases
	shopt -s nullglob
	leases=("$active_dir"/*)
	shopt -u nullglob
	printf '%s\n' "${#leases[@]}"
}

clean_cache_gate_held() {
	local now
	printf 'go cache: cleaning %s\n' "$GOCACHE" >&2
	if ! go clean -cache; then
		return 1
	fi
	now="$(date +%s)"
	write_epoch "$coord_dir/last-clean" "$now"
	write_epoch "$coord_dir/last-check" "$now"
}

measure_pressure() {
	local max_kib min_free_kib
	cache_kib="$(du -sk "$GOCACHE" | awk 'NR == 1 { print $1 }')"
	free_kib="$(df -Pk "$APISIX_GO_SHARED_CACHE" | awk 'NR == 2 { print $4 }')"
	if [[ ! "$cache_kib" =~ ^[0-9]+$ ]]; then
		printf 'go cache: could not measure GOCACHE size\n' >&2
		return 2
	fi
	if [[ ! "$free_kib" =~ ^[0-9]+$ ]]; then
		printf 'go cache: could not measure free disk space\n' >&2
		return 2
	fi
	max_kib=$((cache_max_gib * 1024 * 1024))
	min_free_kib=$((min_free_gib * 1024 * 1024))
	if ((cache_kib < max_kib && free_kib > min_free_kib)); then
		return 1
	fi
	return 0
}

cleanup_if_pressure() {
	local force_check="$1"
	local report_active="$2"
	local gate_status=0 active_count now last_check last_clean pressure_status=0

	acquire_gate || gate_status=$?
	((gate_status == 0)) || return "$gate_status"
	prune_stale_leases
	active_count="$(active_lease_count)"
	if [[ "$active_count" != '0' ]]; then
		release_gate
		if [[ "$report_active" == '1' ]]; then
			printf 'go cache: refusing cleanup; %s active cache users\n' "$active_count" >&2
			return 1
		fi
		return 0
	fi
	now="$(date +%s)"
	last_check="$(read_epoch "$coord_dir/last-check")"
	if [[ "$force_check" != '1' ]] && ((now - last_check < check_interval)); then
		release_gate
		return 0
	fi
	write_epoch "$coord_dir/last-check" "$now"
	release_gate

	measure_pressure || pressure_status=$?
	if ((pressure_status == 1)); then
		return 0
	fi
	((pressure_status == 0)) || return "$pressure_status"

	gate_status=0
	acquire_gate || gate_status=$?
	((gate_status == 0)) || return "$gate_status"
	prune_stale_leases
	active_count="$(active_lease_count)"
	if [[ "$active_count" != '0' ]]; then
		release_gate
		if [[ "$report_active" == '1' ]]; then
			printf 'go cache: refusing cleanup; %s active cache users\n' "$active_count" >&2
			return 1
		fi
		return 0
	fi

	last_clean="$(read_epoch "$coord_dir/last-clean")"
	if ((now - last_clean < cleanup_cooldown)); then
		printf 'go cache: pressure detected; cleanup cooldown remains active\n' >&2
		release_gate
		return 0
	fi

	local clean_status=0
	clean_cache_gate_held || clean_status=$?
	release_gate
	return "$clean_status"
}

remove_lease_on_exit() {
	[[ -n "$lease_file" ]] || return 0
	if [[ -n "$child_pid" ]] && kill -0 "$child_pid" 2>/dev/null; then
		return 0
	fi
	if ((gate_held == 0)); then
		if ! acquire_gate; then
			printf 'go cache: leaving lease for stale-process recovery\n' >&2
			return 0
		fi
	fi
	rm -f -- "$lease_file"
	lease_file=""
	release_gate
}

run_with_lease() {
	if [[ "${1:-}" == '--' ]]; then
		shift
	fi
	(($# > 0)) || fail 'run requires a command'

	# Preserve the documented non-.envrc fallback: run normally without touching
	# the user's global Go cache when this repository's shared cache is inactive.
	if [[ -z "${APISIX_GO_SHARED_CACHE:-}" ]]; then
		"$@"
		return
	fi

	initialize_coordination
	local gate_status=0
	acquire_gate || gate_status=$?
	if ((gate_status != 0)); then
		if ((gate_status == 2)); then
			printf 'go cache: running command without a cache lease; coordinated cleanup remains disabled\n' >&2
			"$@"
			return
		fi
		return "$gate_status"
	fi
	prune_stale_leases
	lease_file="$active_dir/$$-$(date +%s)-$RANDOM"
	printf '%s\t0\t%s\n' "$$" "$*" >"$lease_file"
	release_gate
	trap remove_lease_on_exit EXIT

	local command_status
	set +e
	"$@" <&0 &
	child_pid=$!
	printf '%s\t%s\t%s\n' "$$" "$child_pid" "$*" >"$lease_file.tmp.$$"
	mv -f -- "$lease_file.tmp.$$" "$lease_file"
	wait "$child_pid"
	command_status=$?
	child_pid=""
	set -e

	remove_lease_on_exit
	if [[ -z "$lease_file" ]]; then
		cleanup_if_pressure 0 0 ||
			printf 'go cache: automatic cleanup check failed; command result is unchanged\n' >&2
	fi
	trap - EXIT HUP INT TERM
	return "$command_status"
}

run_gc() {
	local force_clean="$1"
	initialize_coordination
	if [[ "$force_clean" != '1' ]]; then
		cleanup_if_pressure 1 1
		return
	fi
	acquire_gate
	prune_stale_leases
	local active_count clean_status=0
	active_count="$(active_lease_count)"
	if [[ "$active_count" != '0' ]]; then
		release_gate
		printf 'go cache: refusing cleanup; %s active cache users\n' "$active_count" >&2
		return 1
	fi
	clean_cache_gate_held || clean_status=$?
	release_gate
	return "$clean_status"
}

print_status() {
	initialize_coordination
	local pressure_status=0 gate_status=0 active_count last_check last_clean
	measure_pressure || pressure_status=$?
	((pressure_status < 2)) || return "$pressure_status"
	acquire_gate || gate_status=$?
	((gate_status == 0)) || return "$gate_status"
	prune_stale_leases
	active_count="$(active_lease_count)"
	last_check="$(read_epoch "$coord_dir/last-check")"
	last_clean="$(read_epoch "$coord_dir/last-clean")"
	release_gate
	printf 'GOCACHE:              %s\n' "$GOCACHE"
	printf 'cache size:           %s KiB\n' "$cache_kib"
	printf 'filesystem free:      %s KiB\n' "$free_kib"
	printf 'active cache users:   %s\n' "$active_count"
	printf 'maximum cache:        %s GiB\n' "$cache_max_gib"
	printf 'minimum free space:   %s GiB\n' "$min_free_gib"
	printf 'check interval:       %s seconds\n' "$check_interval"
	printf 'cleanup cooldown:     %s seconds\n' "$cleanup_cooldown"
	printf 'last pressure check:  %s\n' "$last_check"
	printf 'last cleanup:         %s\n' "$last_clean"
}

validate_settings

case "${1:-}" in
run)
	shift
	run_with_lease "$@"
	;;
gc)
	shift
	(($# == 0)) || fail 'gc does not accept arguments'
	run_gc 0
	;;
clean)
	shift
	(($# == 0)) || fail 'clean does not accept arguments'
	run_gc 1
	;;
status)
	shift
	(($# == 0)) || fail 'status does not accept arguments'
	print_status
	;;
-h | --help | help)
	usage
	;;
*)
	usage >&2
	exit 2
	;;
esac
