#!/usr/bin/env bash
set -euo pipefail

# Reproducible benchmark runner for apisix-go.
#
#   benchmark.sh run <label>
#   benchmark.sh compare <baseline-label> <current-label>
#   benchmark.sh profile-cpu <package> <anchored-benchmark-regex>
#   benchmark.sh profile-mem <package> <anchored-benchmark-regex>
#
# Results and metadata live under $BENCH_DIR (default .cache/bench) and are
# never committed. A baseline run is immutable: rerunning the same label is
# rejected. compare refuses to compare runs whose host, toolchain, corpus, or
# command settings differ. See AGENTS.md "Performance Benchmarking" for the
# acceptance contract.
#
# Environment: BENCH_DIR BENCH_PACKAGES BENCH_CORPUS_FILES BENCH_REGEX
# BENCH_TIME BENCH_COUNT BENCH_CPU BENCH_P BENCHMARK_VERSION BENCHSTAT
# BENCHSTAT_VERSION GO_BIN HOST_ID

: "${BENCH_DIR:=.cache/bench}"
: "${BENCH_PACKAGES:=./pkg/json ./pkg/plugin/base ./pkg/proxy ./pkg/route}"
: "${BENCH_CORPUS_FILES:=pkg/json/benchmark_test.go pkg/plugin/base/logging_benchmark_test.go pkg/proxy/benchmark_test.go pkg/route/benchmark_test.go}"
: "${BENCH_REGEX:=.}"
: "${BENCH_TIME:=1s}"
: "${BENCH_COUNT:=10}"
: "${BENCH_CPU:=1,4}"
: "${BENCH_P:=1}"
: "${BENCHMARK_VERSION:=1}"
: "${BENCHSTAT:=.cache/bin/benchstat}"
: "${BENCHSTAT_VERSION:=v0.0.0-20260709024250-82a0b07e230d}"
: "${GO_BIN:=go}"

COMPARABLE_KEYS=(
    BENCHMARK_VERSION
    BENCH_HARNESS_SHA
    BENCH_CORPUS_SHA
    GO_VERSION
    GOVERSION
    GOOS
    GOARCH
    GOARM64
    CGO_ENABLED
    GOEXPERIMENT
    GOFLAGS
    HOST_ID
    KERNEL
    CPU_MODEL
    BENCH_PACKAGES
    BENCH_REGEX
    BENCH_TIME
    BENCH_COUNT
    BENCH_CPU
    BENCH_P
    BENCHSTAT_VERSION
)

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
    echo "benchmark.sh: not inside a git repository" >&2
    exit 1
}
cd "$ROOT"

fail() {
    echo "benchmark.sh: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
usage: benchmark.sh run <label>
       benchmark.sh compare <baseline-label> <current-label>
       benchmark.sh profile-cpu <package> <anchored-benchmark-regex>
       benchmark.sh profile-mem <package> <anchored-benchmark-regex>
EOF
}

# corpus_sha hashes the content of every file listed in BENCH_CORPUS_FILES so
# fixture or benchmark-name drift invalidates baseline comparisons. Missing
# corpus files are fatal: a partial corpus would poison the baseline.
corpus_sha() {
    local file input=""
    for file in "$@"; do
        [[ -f "$file" ]] || {
            echo "benchmark.sh: corpus file not found: $file" >&2
            return 1
        }
        input+="$(printf '%s\t%s\n' "$file" "$(git hash-object "$file")")"
        input+=$'\n'
    done
    printf '%s' "$input" | git hash-object --stdin
}

harness_sha() {
    git hash-object scripts/benchmark.sh
}

meta_value() {
    local key="$1" value="$2"
    if [[ "$value" == *$'\t'* || "$value" == *$'\n'* ]]; then
        echo "benchmark.sh: metadata value for $key contains a tab or newline" >&2
        exit 1
    fi
    printf '%s\t%s\n' "$key" "$value"
}

write_metadata() {
    local file="$1" corpus_hash="$2" harness_hash="$3"
    local go_version goversion goos goarch goarm64 cgo_enabled goexperiment goflags
    local env_output host_id kernel cpu_model head_sha worktree_sha run_at

    go_version="$("$GO_BIN" version 2>/dev/null || true)"
    env_output="$("$GO_BIN" env GOVERSION GOOS GOARCH GOARM64 CGO_ENABLED GOEXPERIMENT GOFLAGS 2>/dev/null || true)"
    goversion=unknown
    goos=unknown
    goarch=unknown
    goarm64=unknown
    cgo_enabled=unknown
    goexperiment=unknown
    goflags=unknown
    IFS=$'\n' read -r goversion goos goarch goarm64 cgo_enabled goexperiment goflags <<< "$env_output" || true

    host_id="${HOST_ID:-$(hostname)}"
    if [[ "$(uname -s)" == "Darwin" ]]; then
        cpu_model="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
    else
        cpu_model="unknown"
    fi
    kernel="$(uname -sr)"
    head_sha="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
    worktree_sha="$(git status --porcelain 2>/dev/null | git hash-object --stdin || echo unknown)"
    run_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo unknown)"

    {
        meta_value "BENCHMARK_VERSION" "$BENCHMARK_VERSION"
        meta_value "BENCH_HARNESS_SHA" "$harness_hash"
        meta_value "BENCH_CORPUS_SHA" "$corpus_hash"
        meta_value "GO_VERSION" "$go_version"
        meta_value "GOVERSION" "$goversion"
        meta_value "GOOS" "$goos"
        meta_value "GOARCH" "$goarch"
        meta_value "GOARM64" "$goarm64"
        meta_value "CGO_ENABLED" "$cgo_enabled"
        meta_value "GOEXPERIMENT" "$goexperiment"
        meta_value "GOFLAGS" "$goflags"
        meta_value "HOST_ID" "$host_id"
        meta_value "KERNEL" "$kernel"
        meta_value "CPU_MODEL" "$cpu_model"
        meta_value "BENCH_PACKAGES" "$BENCH_PACKAGES"
        meta_value "BENCH_REGEX" "$BENCH_REGEX"
        meta_value "BENCH_TIME" "$BENCH_TIME"
        meta_value "BENCH_COUNT" "$BENCH_COUNT"
        meta_value "BENCH_CPU" "$BENCH_CPU"
        meta_value "BENCH_P" "$BENCH_P"
        meta_value "BENCHSTAT_VERSION" "$BENCHSTAT_VERSION"
        meta_value "HEAD_SHA" "$head_sha"
        meta_value "WORKTREE_STATUS_SHA" "$worktree_sha"
        meta_value "RUN_AT_UTC" "$run_at"
    } > "$file"
}

run_benchmark() {
    local label="$1" dir result meta tmp_result tmp_meta cleanup
    local packages corpus_files
    read -r -a packages <<< "$BENCH_PACKAGES"
    read -r -a corpus_files <<< "$BENCH_CORPUS_FILES"
    if (( ${#packages[@]} == 0 )); then
        fail "BENCH_PACKAGES is empty"
    fi
    if (( ${#corpus_files[@]} == 0 )); then
        fail "BENCH_CORPUS_FILES is empty"
    fi

    [[ "$label" =~ ^[A-Za-z0-9._-]+$ ]] || fail "invalid label '$label' (allowed: [A-Za-z0-9._-])"
    dir="$BENCH_DIR"
    result="$dir/$label.txt"
    meta="$dir/$label.meta"
    if [[ -e "$result" || -e "$meta" ]]; then
        fail "run '$label' already exists ($result); baseline is immutable"
    fi
    mkdir -p "$dir"

    local corpus_hash harness_hash
    corpus_hash="$(corpus_sha "${corpus_files[@]}")" || fail "corpus hashing failed"
    harness_hash="$(harness_sha)" || fail "harness hashing failed"

    tmp_result="$(mktemp "$dir/.$label.XXXXXX")"
    tmp_meta="$(mktemp "$dir/.$label.XXXXXX")"
    cleanup=run
    trap 'rm -f "${tmp_result:-}" "${tmp_meta:-}"; if [[ "${cleanup:-}" == "publish" ]]; then rm -f "${result:-}" "${meta:-}"; fi' EXIT

    if ! "$GO_BIN" test -p="$BENCH_P" "${packages[@]}" \
        -run '^$' -bench "$BENCH_REGEX" -benchmem \
        -benchtime="$BENCH_TIME" -count="$BENCH_COUNT" -cpu="$BENCH_CPU" \
        >"$tmp_result" 2>&1; then
        cat "$tmp_result" >&2
        fail "go test failed; no results published"
    fi
    if ! grep -Eq '^Benchmark[^[:space:]]*[[:space:]].*ns/op[[:space:]].*B/op[[:space:]].*allocs/op' "$tmp_result"; then
        echo "benchmark.sh: no benchmark rows (ns/op, B/op, allocs/op) in output; refusing to publish" >&2
        exit 1
    fi

    write_metadata "$tmp_meta" "$corpus_hash" "$harness_hash"

    cleanup=publish
    if ! mv "$tmp_result" "$result"; then
        echo "benchmark.sh: failed to publish $result" >&2
        exit 1
    fi
    if ! mv "$tmp_meta" "$meta"; then
        echo "benchmark.sh: failed to publish $meta" >&2
        exit 1
    fi
    cleanup=done
    echo "benchmark.sh: published $label (result + metadata)"
}

# meta_get prints the value for key; the last occurrence wins so callers can
# append overrides. Exits 1 when the key is absent.
meta_get() {
    local file="$1" key="$2" line value result=""
    local found=0
    while IFS=$'\t' read -r line value; do
        if [[ "$line" == "$key" ]]; then
            result="$value"
            found=1
        fi
    done < "$file"
    if [[ "$found" == 1 ]]; then
        printf '%s' "$result"
        return 0
    fi
    return 1
}

compare_results() {
    local baseline="$1" current="$2" dir="$BENCH_DIR"
    local baseline_meta="$dir/$baseline.meta" current_meta="$dir/$current.meta"
    local baseline_txt="$dir/$baseline.txt" current_txt="$dir/$current.txt"
    if [[ ! -f "$baseline_meta" || ! -f "$current_meta" || ! -f "$baseline_txt" || ! -f "$current_txt" ]]; then
        fail "missing run artifacts for '$baseline' or '$current'"
    fi

    local key baseline_value current_value
    for key in "${COMPARABLE_KEYS[@]}"; do
        if ! baseline_value="$(meta_get "$baseline_meta" "$key")"; then
            fail "missing $key in $baseline_meta"
        fi
        if ! current_value="$(meta_get "$current_meta" "$key")"; then
            fail "missing $key in $current_meta"
        fi
        if [[ "$baseline_value" != "$current_value" ]]; then
            echo "benchmark.sh: metadata mismatch for $key:" >&2
            echo "  baseline: $baseline_value" >&2
            echo "  current:  $current_value" >&2
            return 1
        fi
    done

    echo "benchmark.sh: metadata match; running benchstat" >&2
    "$BENCHSTAT" "$baseline_txt" "$current_txt" | tee "$dir/compare.txt"
}

profile_benchmark() {
    local kind="$1" package="$2" regex="$3" out extra
    [[ -n "$package" ]] || fail "profile requires a package"
    [[ "$regex" == ^Benchmark* && "$regex" == *$ ]] || fail "profile requires an anchored regex: ^Benchmark...$"
    mkdir -p "$BENCH_DIR"
    if [[ "$kind" == "cpu" ]]; then
        out="$BENCH_DIR/profile.cpu.pprof"
        extra="-cpuprofile=$out"
    else
        out="$BENCH_DIR/profile.mem.pprof"
        extra="-memprofile=$out"
    fi
    "$GO_BIN" test -p=1 "$package" -run '^$' -bench "$regex" \
        -count=1 -cpu=1 -benchtime=5s "$extra"
    echo "benchmark.sh: profile written to $out"
}

main() {
    case "${1:-}" in
        run) run_benchmark "${2:?missing label}" ;;
        compare) compare_results "${2:?missing baseline label}" "${3:?missing current label}" ;;
        profile-cpu) profile_benchmark cpu "${2:?missing package}" "${3:?missing benchmark regex}" ;;
        profile-mem) profile_benchmark mem "${2:?missing package}" "${3:?missing benchmark regex}" ;;
        *) usage >&2; return 2 ;;
    esac
}

main "$@"
