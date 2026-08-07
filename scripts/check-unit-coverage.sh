#!/usr/bin/env bash
set -euo pipefail

coverage_counts() {
  awk 'NR > 1 { total += $2; if ($3 > 0) covered += $2 } END { print covered + 0, total + 0 }' "$1"
}

coverage_meets_minimum() {
  awk -v covered="$1" -v total="$2" -v minimum="$3" \
    'BEGIN { exit !(total > 0 && covered * 100 >= minimum * total) }'
}

coverage_percentage() {
  awk -v covered="$1" -v total="$2" 'BEGIN { printf "%.6f", covered * 100 / total }'
}

main() {
  local coverage_file=${1:-coverage.out}
  local minimum=${COVERAGE_MIN:-80.0}
  local missing
  missing=$(go list -f '{{if and (gt (len .GoFiles) 0) (eq (len .TestGoFiles) 0) (eq (len .XTestGoFiles) 0)}}{{.ImportPath}}{{end}}' ./cmd/... ./pkg/... | sed '/^$/d')
  if [[ -n "$missing" ]]; then
    printf 'production packages without unit tests:\n%s\n' "$missing" >&2
    exit 1
  fi

  go test -coverprofile="$coverage_file" ./cmd/... ./pkg/... -count=1
  local covered total percentage
  read -r covered total < <(coverage_counts "$coverage_file")
  percentage=$(coverage_percentage "$covered" "$total")
  if ! coverage_meets_minimum "$covered" "$total" "$minimum"; then
    printf 'unit coverage %s%% (%s/%s statements) is below required %s%%\n' \
      "$percentage" "$covered" "$total" "$minimum" >&2
    exit 1
  fi
  printf 'unit coverage %s%% (%s/%s statements) meets required %s%%\n' \
    "$percentage" "$covered" "$total" "$minimum"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
