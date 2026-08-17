#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/plugin-status.yml"

require_fixed() {
	local text=$1
	local content=$2
	if ! grep -Fq -- "$text" <<<"$content"; then
		printf 'missing %q in %s\n' "$text" "$workflow" >&2
		exit 1
	fi
}

trigger_block() {
	local trigger=$1
	awk -v trigger="$trigger" '
		$0 == "on:" { in_on = 1; next }
		in_on && $0 !~ /^[[:space:]]/ { exit }
		in_on && $0 == "  " trigger ":" { in_trigger = 1; print; next }
		in_trigger && $0 ~ /^  [A-Za-z0-9_-]+:/ { exit }
		in_trigger { print }
	' "$workflow"
}

paths_from_trigger() {
	awk '
		$0 == "    paths:" { in_paths = 1; next }
		in_paths && $0 ~ /^    [A-Za-z0-9_-]+:/ { exit }
		in_paths && $0 ~ /^      - / {
			sub(/^      - /, "")
			print
		}
	'
}

if [[ ! -f "$workflow" ]]; then
	printf 'missing plugin status workflow: %s\n' "$workflow" >&2
	exit 1
fi

header=$(sed -n '1,/^jobs:$/p' "$workflow")
require_fixed 'name: Plugin Status Contract' "$header"
require_fixed 'workflow_dispatch:' "$header"
require_fixed 'permissions:' "$header"
require_fixed '  contents: read' "$header"

expected_paths=$(printf '%s\n' \
	docs/plugins.md \
	't/plugin/*.yaml' \
	t/plugin/coverage_test.go \
	.github/workflows/plugin-status.yml | sort)

pull_request_block=$(trigger_block pull_request)
if [[ -z "$pull_request_block" ]]; then
	printf 'missing pull_request trigger in %s\n' "$workflow" >&2
	exit 1
fi
if grep -Fq '    paths:' <<<"$pull_request_block"; then
	printf 'pull_request must not use path filters because this workflow is a required check: %s\n' "$workflow" >&2
	exit 1
fi

push_block=$(trigger_block push)
if [[ -z "$push_block" ]]; then
	printf 'missing push trigger in %s\n' "$workflow" >&2
	exit 1
fi
require_fixed '    paths:' "$push_block"
push_paths=$(printf '%s\n' "$push_block" | paths_from_trigger | sort)
if [[ "$push_paths" != "$expected_paths" ]]; then
	printf 'push paths differ from the plugin status contract in %s\n' "$workflow" >&2
	printf 'want:\n%s\ngot:\n%s\n' "$expected_paths" "$push_paths" >&2
	exit 1
fi
require_fixed '    branches:' "$push_block"
require_fixed '      - master' "$push_block"

jobs=$(sed -n '/^jobs:$/,$p' "$workflow")
job_count=$(grep -Ec '^  [A-Za-z0-9_-]+:$' <<<"$jobs")
if [[ "$job_count" -ne 1 ]]; then
	printf 'expected one plugin status job in %s, found %s\n' "$workflow" "$job_count" >&2
	exit 1
fi
require_fixed '    runs-on: ubuntu-latest' "$jobs"
require_fixed 'actions/checkout@v7' "$jobs"
require_fixed 'actions/setup-go@v7' "$jobs"
require_fixed '      go-version-file: go.mod' "$jobs"
require_fixed "      run: go test ./t/plugin -run '^TestSupportedPluginManifestSelection$' -count=1" "$jobs"

if grep -Eq 'TestPluginIntegration|make test-integration' <<<"$jobs"; then
	printf 'real-process plugin integration cases are not allowed in %s\n' "$workflow" >&2
	exit 1
fi

printf 'plugin status workflow contract: PASS\n'
