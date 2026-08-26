#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
workflow="$repo_root/.github/workflows/capability-status.yml"
makefile="$repo_root/Makefile"

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

make_target_body() {
	local target=$1
	awk -v target="$target:" '
		$0 == target { in_target = 1; next }
		in_target && /^\t/ { print; next }
		in_target && /^[[:space:]]*$/ { next }
		in_target { exit }
	' "$makefile"
}

require_make_target_body() {
	local target=$1
	local expected=$2
	local count actual
	count=$(grep -Ec -- "^${target}::?([[:space:]]|$)" "$makefile" || true)
	if [[ "$count" -ne 1 ]]; then
		printf 'expected exactly one %s target in %s, found %s\n' "$target" "$makefile" "$count" >&2
		exit 1
	fi
	actual=$(make_target_body "$target")
	if [[ "$actual" != "$expected" ]]; then
		printf 'target %s body differs from the capability status contract in %s\n' "$target" "$makefile" >&2
		printf 'want:\n%s\ngot:\n%s\n' "$expected" "$actual" >&2
		exit 1
	fi
}

if [[ ! -f "$workflow" ]]; then
	printf 'missing capability status workflow: %s\n' "$workflow" >&2
	exit 1
fi
if [[ ! -f "$makefile" ]]; then
	printf 'missing Makefile: %s\n' "$makefile" >&2
	exit 1
fi

header=$(sed -n '1,/^jobs:$/p' "$workflow")
workflow_names=$(grep -Fxc 'name: Capability Status Contract' <<<"$header")
if [[ "$workflow_names" -ne 1 ]]; then
	printf 'workflow name must be exactly Capability Status Contract in %s\n' "$workflow" >&2
	exit 1
fi

expected_paths=$(printf '%s\n' \
	Makefile \
	'pkg/capability/**' \
	pkg/config/profiles.go \
	pkg/plugin/registry_gen.go \
	'cmd/capability-gen/**' \
	docs/plugins.md \
	'docs/architecture/**' \
	't/plugin/*.yaml' \
	t/plugin/corpus_scope.yaml \
	t/plugin/coverage_test.go \
	t/plugin/corpus_test.go \
	scripts/capability_status_gate_test.sh \
	.github/CODEOWNERS \
	.github/workflows/capability-status.yml \
	.github/workflows/unit-test.yml | sort)

pull_request_block=$(trigger_block pull_request)
if [[ -z "$pull_request_block" ]]; then
	printf 'missing pull_request trigger in %s\n' "$workflow" >&2
	exit 1
fi
if [[ "$pull_request_block" != '  pull_request:' ]]; then
	printf 'pull_request must be unconfigured because this workflow is a required check: %s\n' "$workflow" >&2
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
	printf 'push paths differ from the capability status contract in %s\n' "$workflow" >&2
	printf 'want:\n%s\ngot:\n%s\n' "$expected_paths" "$push_paths" >&2
	exit 1
fi
require_fixed '    branches:' "$push_block"
branches=$(awk '
	$0 == "    branches:" { in_branches = 1; next }
	in_branches && $0 ~ /^    [A-Za-z0-9_-]+:/ { exit }
	in_branches && $0 ~ /^      - / {
		sub(/^      - /, "")
		print
	}
' <<<"$push_block")
if [[ "$branches" != master ]]; then
	printf 'push branches must contain only master in %s\n' "$workflow" >&2
	exit 1
fi

permissions_block=$(awk '
	$0 == "permissions:" { in_permissions = 1; next }
	in_permissions && $0 !~ /^[[:space:]]/ { exit }
	in_permissions { print }
' "$workflow" | sed '/^[[:space:]]*$/d')
if [[ "$permissions_block" != '  contents: read' ]]; then
	printf 'top-level permissions must be contents: read only in %s\n' "$workflow" >&2
	exit 1
fi

jobs=$(sed -n '/^jobs:$/,$p' "$workflow")
job_count=$(grep -Ec '^  [A-Za-z0-9_-]+:$' <<<"$jobs")
if [[ "$job_count" -ne 1 ]] || ! grep -Fxq '  capability-status:' <<<"$jobs"; then
	printf 'expected exactly one capability-status job in %s\n' "$workflow" >&2
	exit 1
fi
job_names=$(grep -Fxc '    name: Capability Status Contract' <<<"$jobs")
if [[ "$job_names" -ne 1 ]]; then
	printf 'job name must be exactly Capability Status Contract in %s\n' "$workflow" >&2
	exit 1
fi
require_fixed '    runs-on: ubuntu-latest' "$jobs"
require_fixed 'uses: actions/checkout@v7' "$jobs"
require_fixed 'uses: actions/setup-go@v7' "$jobs"
require_fixed '          go-version-file: go.mod' "$jobs"
require_fixed '        run: bash scripts/capability_status_gate_test.sh' "$jobs"
require_fixed "        run: bash -lc 'source .envrc && make check-capability-drift'" "$jobs"
require_fixed "        run: bash -lc 'source .envrc && make test-capability-status'" "$jobs"

step_count=$(grep -Ec '^      - name:' <<<"$jobs")
if [[ "$step_count" -ne 5 ]]; then
	printf 'expected exactly five capability status steps in %s, found %s\n' "$workflow" "$step_count" >&2
	exit 1
fi
expected_uses=$(printf '%s\n' 'actions/checkout@v7' 'actions/setup-go@v7' | sort)
actual_uses=$(sed -n 's/^[[:space:]]*uses: //p' <<<"$jobs" | sort)
if [[ "$actual_uses" != "$expected_uses" ]]; then
	printf 'capability status actions differ from the contract in %s\n' "$workflow" >&2
	exit 1
fi
expected_runs=$(printf '%s\n' \
	'bash scripts/capability_status_gate_test.sh' \
	"bash -lc 'source .envrc && make check-capability-drift'" \
	"bash -lc 'source .envrc && make test-capability-status'" | sort)
actual_runs=$(sed -n 's/^[[:space:]]*run: //p' <<<"$jobs" | sort)
if [[ "$actual_runs" != "$expected_runs" ]]; then
	printf 'capability status commands differ from the contract in %s\n' "$workflow" >&2
	exit 1
fi

if grep -Eq 'TestPluginIntegration|make test-integration|-write|generate-capabilities' <<<"$jobs"; then
	printf 'write-mode generation and real-process plugin integration are forbidden in %s\n' "$workflow" >&2
	exit 1
fi

require_make_target_body generate-capabilities \
	$'\t$(GO_CACHE_RUNNER) go run ./cmd/capability-gen -repo-root . -write'
require_make_target_body check-capability-drift \
	$'\t$(GO_CACHE_RUNNER) go run ./cmd/capability-gen -repo-root . -check'
status_target_body=$(printf '\t%s\n\t%s' \
	"\$(GO_CACHE_RUNNER) go test ./pkg/capability ./pkg/config ./pkg/plugin -run '^(TestLoadedManifest|TestManifest|TestProfileSelection|TestCapabilityManifest|TestCapabilityRegistry)' -count=1" \
	"APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 \$(GO_CACHE_RUNNER) go test ./t/plugin -run '^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestUpstreamCorpusAccountingWithoutSourceCheckout|TestCorpusEvidenceMatchesCompatibilityTarget)\$\$' -count=1")
require_make_target_body test-capability-status "$status_target_body"

printf 'capability status workflow contract: PASS\n'
