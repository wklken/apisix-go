#!/usr/bin/env bash
set -euo pipefail

coverage_file=${1:-coverage.out}
minimum=${COVERAGE_MIN:-80.0}

missing=$(go list -f '{{if and (gt (len .GoFiles) 0) (eq (len .TestGoFiles) 0) (eq (len .XTestGoFiles) 0)}}{{.ImportPath}}{{end}}' ./cmd/... ./pkg/... | sed '/^$/d')
if [[ -n "$missing" ]]; then
  printf 'production packages without unit tests:\n%s\n' "$missing" >&2
  exit 1
fi

go test -coverprofile="$coverage_file" ./cmd/... ./pkg/... -count=1
total=$(go tool cover -func="$coverage_file" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')
if ! awk -v total="$total" -v minimum="$minimum" 'BEGIN { exit !(total + 0 >= minimum + 0) }'; then
  printf 'unit coverage %s%% is below required %s%%\n' "$total" "$minimum" >&2
  exit 1
fi
printf 'unit coverage %s%% meets required %s%%\n' "$total" "$minimum"
