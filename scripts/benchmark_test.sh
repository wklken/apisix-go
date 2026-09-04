#!/usr/bin/env bash
set -euo pipefail

# Regression tests for scripts/benchmark.sh. Run from the repository root:
#
#   bash scripts/benchmark_test.sh
#
# The runner is exercised with fake GO_BIN and BENCHSTAT binaries so no real
# benchmark or toolchain download is required.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
RUNNER="$SCRIPT_DIR/benchmark.sh"

TMP_BASE="$(mktemp -d "${TMPDIR:-/tmp}/apisix-go-benchmark-test.XXXXXX")"
trap 'rm -rf "${TMP_BASE:-}"' EXIT

fail_test() {
    echo "FAIL: $*" >&2
    exit 1
}

pass_test() {
    echo "PASS: $*"
}

if [[ ! -f "$RUNNER" ]]; then
    fail_test "$RUNNER does not exist"
fi

fake_go="$TMP_BASE/fake-go"
fake_benchstat="$TMP_BASE/fake-benchstat"
fake_output="$TMP_BASE/benchstat-output.txt"

cat > "$fake_go" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
    version)
        echo "${FAKE_GO_VERSION:-go version go1.26.5 darwin/arm64}"
        ;;
    env)
        case "${2:-}" in
            GOVERSION) echo go1.26.5 ;;
            GOOS) echo darwin ;;
            GOARCH) echo arm64 ;;
            GOARM64) echo v8.0 ;;
            CGO_ENABLED) echo 1 ;;
            GOEXPERIMENT) echo rangefunc ;;
            GOFLAGS) printf '%s\n' '-mod=readonly' ;;
        esac
        ;;
    test)
        if [[ "${FAKE_GO_FAIL:-0}" == 1 ]]; then
            echo "FAIL: simulated go test failure" >&2
            exit 1
        fi
        if [[ "${FAKE_GO_NO_ROWS:-0}" == 1 ]]; then
            echo "PASS"
            echo "ok  github.com/wklken/apisix-go/pkg/fake 1.0s"
            exit 0
        fi
        echo "BenchmarkFake 1000 1234 ns/op 100 B/op 10 allocs/op"
        echo "PASS"
        echo "ok  github.com/wklken/apisix-go/pkg/fake 1.0s"
        ;;
esac
EOF
chmod +x "$fake_go"

printf 'name\told ns/op\tnew ns/op\tdelta\nBenchmarkFake\t1234\t1200\t-2.76%%\n' > "$fake_output"

cat > "$fake_benchstat" <<'EOF'
#!/usr/bin/env bash
cat "$FAKE_BENCHSTAT_OUTPUT"
EOF
chmod +x "$fake_benchstat"

setup_env() {
    local dir="$1"
    export BENCH_DIR="$dir"
    export BENCH_PACKAGES="./pkg/json"
    export BENCH_CORPUS_FILES="pkg/json/benchmark_test.go"
    export BENCH_REGEX="."
    export BENCH_TIME=1s
    export BENCH_COUNT=10
    export BENCH_CPU=1,4
    export BENCH_P=1
    export BENCHMARK_VERSION=1
    export BENCHSTAT="$fake_benchstat"
    export BENCHSTAT_VERSION=v0.0.0-fake
    export GO_BIN="$fake_go"
    export HOST_ID=fake-host
    export FAKE_BENCHSTAT_OUTPUT="$fake_output"
    unset FAKE_GO_FAIL FAKE_GO_NO_ROWS FAKE_GO_VERSION
}

run_test_1() {
    local dir="$TMP_BASE/t1"
    setup_env "$dir"
    export FAKE_GO_FAIL=1
    if bash "$RUNNER" run baseline >/dev/null 2>&1; then
        fail_test "test 1: failing go test should abort run"
    fi
    if [[ -e "$dir/baseline.txt" || -e "$dir/baseline.meta" ]]; then
        fail_test "test 1: failed run published artifacts"
    fi
    if [[ -n "$(ls -A "$dir" 2>/dev/null)" ]]; then
        fail_test "test 1: failed run left files in BENCH_DIR"
    fi
    pass_test "test 1: failed go test publishes nothing"
}

run_test_2() {
    local dir="$TMP_BASE/t2"
    setup_env "$dir"
    export FAKE_GO_NO_ROWS=1
    if bash "$RUNNER" run baseline >/dev/null 2>&1; then
        fail_test "test 2: output without benchmark rows should be rejected"
    fi
    if [[ -e "$dir/baseline.txt" || -e "$dir/baseline.meta" ]]; then
        fail_test "test 2: row-less run published artifacts"
    fi
    pass_test "test 2: output without benchmark rows is rejected"
}

run_test_3() {
    local dir="$TMP_BASE/t3"
    setup_env "$dir"
    bash "$RUNNER" run baseline >/dev/null 2>&1
    local before
    before="$(git hash-object "$dir/baseline.txt")"
    if bash "$RUNNER" run baseline >/dev/null 2>&1; then
        fail_test "test 3: second run of the same label should fail"
    fi
    local after
    after="$(git hash-object "$dir/baseline.txt")"
    if [[ "$before" != "$after" ]]; then
        fail_test "test 3: baseline.txt changed on rejected rerun"
    fi
    pass_test "test 3: baseline rerun is rejected without modifying files"
}

run_test_4() {
    local dir="$TMP_BASE/t4"
    setup_env "$dir"
    bash "$RUNNER" run baseline >/dev/null 2>&1
    bash "$RUNNER" run current >/dev/null 2>&1
    printf 'HEAD_SHA\tother-head\n' >> "$dir/current.meta"
    printf 'WORKTREE_STATUS_SHA\tother-worktree\n' >> "$dir/current.meta"
    local output
    output="$(bash "$RUNNER" compare baseline current)" || fail_test "test 4: compare failed"
    if [[ "$output" != *"BenchmarkFake"* ]]; then
        fail_test "test 4: compare output missing benchstat rows"
    fi
    if [[ ! -f "$dir/compare.txt" ]]; then
        fail_test "test 4: compare.txt not written"
    fi
    pass_test "test 4: audit-only metadata differences compare successfully"
}

run_test_5() {
    local dir="$TMP_BASE/t5"
    setup_env "$dir"
    bash "$RUNNER" run baseline >/dev/null 2>&1
    bash "$RUNNER" run current >/dev/null 2>&1
    grep -v '^BENCH_P\t' "$dir/current.meta" > "$dir/current.meta.tmp"
    mv "$dir/current.meta.tmp" "$dir/current.meta"
    if bash "$RUNNER" compare baseline current >/dev/null 2>&1; then
        fail_test "test 5: compare with missing BENCH_P should fail"
    fi
    local output
    output="$(bash "$RUNNER" compare baseline current 2>&1 || true)"
    if [[ "$output" != *"BENCH_P"* ]]; then
        fail_test "test 5: missing key failure did not print BENCH_P"
    fi
    pass_test "test 5: missing required metadata key fails and names the key"
}

run_test_6() {
    local dir="$TMP_BASE/t6"
    local corpus_a="$TMP_BASE/corpus-a.txt"
    local corpus_b="$TMP_BASE/corpus-b.txt"
    printf 'corpus-a\n' > "$corpus_a"
    printf 'corpus-b\n' > "$corpus_b"

    local key
    while read -r key; do
        local subdir="$dir/$key"
        setup_env "$subdir"
        bash "$RUNNER" run baseline >/dev/null 2>&1
        case "$key" in
            BENCH_CPU) export BENCH_CPU=1 ;;
            BENCH_P) export BENCH_P=2 ;;
            GO_VERSION) export FAKE_GO_VERSION="go version go1.25.0 darwin/arm64" ;;
            HOST_ID) export HOST_ID=other-host ;;
            BENCH_CORPUS_SHA) export BENCH_CORPUS_FILES="$corpus_b" ;;
        esac
        bash "$RUNNER" run current >/dev/null 2>&1
        if bash "$RUNNER" compare baseline current >/dev/null 2>&1; then
            fail_test "test 6: $key: mismatched metadata compared successfully"
        fi
        local output
        output="$(bash "$RUNNER" compare baseline current 2>&1 || true)"
        if [[ "$output" != *"$key"* ]]; then
            fail_test "test 6: $key: mismatch failure did not name the key"
        fi
    done <<'EOF'
BENCH_CPU
BENCH_P
GO_VERSION
HOST_ID
BENCH_CORPUS_SHA
EOF
    pass_test "test 6: every metadata change fails compare and names the key"
}

run_test_7() {
    local dir="$TMP_BASE/t7"
    setup_env "$dir"
    bash "$RUNNER" run baseline >/dev/null 2>&1
    bash "$RUNNER" run current >/dev/null 2>&1
    local output
    output="$(bash "$RUNNER" compare baseline current)" || fail_test "test 7: compare failed"
    if [[ "$output" != *"BenchmarkFake"* ]]; then
        fail_test "test 7: stdout missing benchstat output"
    fi
    if ! diff -q "$fake_output" "$dir/compare.txt" >/dev/null; then
        fail_test "test 7: compare.txt does not match benchstat output"
    fi
    pass_test "test 7: compare writes benchstat output to compare.txt and stdout"
}

run_test_8() {
    local dir="$TMP_BASE/t8"
    setup_env "$dir"
    export BENCH_CORPUS_FILES="$TMP_BASE/does-not-exist.txt"
    if bash "$RUNNER" run baseline >/dev/null 2>&1; then
        fail_test "test 8: missing corpus file should abort run"
    fi
    if [[ -e "$dir/baseline.txt" || -e "$dir/baseline.meta" ]]; then
        fail_test "test 8: missing-corpus run published artifacts"
    fi
    if [[ -n "$(ls -A "$dir" 2>/dev/null)" ]]; then
        fail_test "test 8: missing-corpus run left files in BENCH_DIR"
    fi
    pass_test "test 8: missing corpus file aborts run without publishing"
}

run_test_9() {
    local dir="$TMP_BASE/t9"
    setup_env "$dir"
    bash "$RUNNER" run baseline >/dev/null 2>&1
    for expected in \
        $'GOVERSION\tgo1.26.5' \
        $'GOOS\tdarwin' \
        $'GOARCH\tarm64' \
        $'GOARM64\tv8.0' \
        $'CGO_ENABLED\t1' \
        $'GOEXPERIMENT\trangefunc' \
        $'GOFLAGS\t-mod=readonly'; do
        if ! grep -Fxq "$expected" "$dir/baseline.meta"; then
            fail_test "test 9: missing exact go env metadata $expected"
        fi
    done
    pass_test "test 9: every requested go env value is recorded"
}

run_test_1
run_test_2
run_test_3
run_test_4
run_test_5
run_test_6
run_test_7
run_test_8
run_test_9

echo "all benchmark runner regression tests passed"
