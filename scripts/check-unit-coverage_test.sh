#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/apisix-go-coverage-test.XXXXXX")
trap 'rm -r -- "$test_root"' EXIT

mkdir -p "$test_root/bin"
cat >"$test_root/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
list)
  exit 0
  ;;
test)
  for argument in "$@"; do
    case "$argument" in
    -coverprofile=*)
      cp "$COVERAGE_FIXTURE" "${argument#-coverprofile=}"
      exit 0
      ;;
    esac
  done
  exit 1
  ;;
tool)
  printf 'total:\t(statements)\t80.0%%\n'
  exit 0
  ;;
esac
exit 1
EOF
chmod +x "$test_root/bin/go"

run_gate() {
  local fixture=$1
  local output=$2
  PATH="$test_root/bin:$PATH" COVERAGE_FIXTURE="$fixture" COVERAGE_MIN=80.0 \
    "$repo_root/scripts/check-unit-coverage.sh" "$output"
}

cat >"$test_root/below.out" <<'EOF'
mode: set
example.go:1.1,1.2 7995 1
example.go:2.1,2.2 2005 0
EOF
if run_gate "$test_root/below.out" "$test_root/below-result.out"; then
  printf '79.95%% coverage passed the 80.0%% gate\n' >&2
  exit 1
fi

cat >"$test_root/exact.out" <<'EOF'
mode: set
example.go:1.1,1.2 80 1
example.go:2.1,2.2 20 0
EOF
run_gate "$test_root/exact.out" "$test_root/exact-result.out"

printf 'unit coverage gate regression tests passed\n'
