#!/usr/bin/env bash

set -euo pipefail

# Candidate, fixtures, source accounting, and the immutable oracle must not
# inherit developer or CI proxy settings.
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY FTP_PROXY
unset http_proxy https_proxy all_proxy no_proxy ftp_proxy

readonly suite=apisix-3.17-plugin-differential-v1
readonly expected_source_commit=9ef2ecab67f652d38365049613610ef649bb4ad0
readonly -a required_stages=(
    generator_drift
    candidate_build
    plugin_units
    dependency_failure
    corpus
    real_process
    differential
)

die() {
    printf 'plugin behavior gate: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

sha256_file() {
    local file=$1
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
        return
    fi
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
        return
    fi
    die 'sha256sum or shasum is required'
}

sha256_stream() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
        return
    fi
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 | awk '{print $1}'
        return
    fi
    die 'sha256sum or shasum is required'
}

# Build a throwaway index from HEAD plus the current filesystem. Its sorted
# stage records provide the tracked and untracked-non-ignored path set without
# mutating the user's real index. The canonical stream includes existence,
# path, mode/type, byte length, and contents; tracked deletions are explicit.
compute_working_tree_sha256() {
    local root=$1 head=$2
    (
        local snapshot_dir index_file present_file missing_file
        local record metadata path mode object stage type size
        snapshot_dir=$(mktemp -d "${TMPDIR:-/tmp}/plugin-behavior-tree.XXXXXX")
        trap 'rm -rf "$snapshot_dir"' EXIT
        index_file=$snapshot_dir/index
        present_file=$snapshot_dir/present
        missing_file=$snapshot_dir/missing

        cd "$root"
        GIT_INDEX_FILE="$index_file" git read-tree "$head"
        GIT_INDEX_FILE="$index_file" git add -A -- .
        GIT_INDEX_FILE="$index_file" git ls-files --stage -z >"$present_file"
        GIT_INDEX_FILE="$index_file" git diff --cached --name-only --diff-filter=D -z "$head" -- \
            >"$missing_file"

        {
            while IFS= read -r -d '' record; do
                metadata=${record%%$'\t'*}
                path=${record#*$'\t'}
                read -r mode object stage <<<"$metadata"
                [[ "$stage" == 0 ]] || exit 1
                case "$mode" in
                    100644|100755)
                        [[ -f "$path" && ! -L "$path" ]] || exit 1
                        type='file'
                        size=$(wc -c <"$path" | tr -d '[:space:]')
                        printf 'present\0%s\0%s\0%s\0%s\0' "$path" "$mode" "$type" "$size"
                        cat -- "$path"
                        printf '\0'
                        ;;
                    120000)
                        [[ -L "$path" || -f "$path" ]] || exit 1
                        type='symlink'
                        size=$(git cat-file -s "$object")
                        printf 'present\0%s\0%s\0%s\0%s\0' "$path" "$mode" "$type" "$size"
                        git cat-file -p "$object"
                        printf '\0'
                        ;;
                    160000)
                        type='gitlink'
                        size=${#object}
                        printf 'present\0%s\0%s\0%s\0%s\0%s\0' \
                            "$path" "$mode" "$type" "$size" "$object"
                        ;;
                    *) exit 1 ;;
                esac
            done <"$present_file"
            while IFS= read -r -d '' path; do
                printf 'missing\0%s\0' "$path"
            done <"$missing_file"
        } | sha256_stream
    )
}

read_yaml_scalar() {
    local file=$1 key=$2
    awk -v key="$key" '
        $1 == key ":" {
            value = $2
            gsub(/^"|"$/, "", value)
            print value
            exit
        }
    ' "$file"
}

validate_artifact() {
    local artifact=$1
    [[ -s "$artifact" ]] || die "summary artifact is missing or empty: $artifact"
    jq -e \
        --arg suite "$suite" \
        --arg source "$expected_source_commit" \
        --argjson stages "$(printf '%s\n' "${required_stages[@]}" | jq -Rsc 'split("\n")[:-1]')" \
        '
            (keys | sort) == [
                "candidate", "case_counts", "differential", "first_attempt", "oracle",
                "result", "schema_version", "stages", "suite"
            ] and
            .schema_version == 1 and
            .suite == $suite and
            .result == "pass" and
            .first_attempt == true and
            (.candidate | keys | sort) == ["binary_sha256", "source_commit", "working_tree_sha256"] and
            (.candidate.source_commit | test("^[0-9a-f]{40}$")) and
            (.candidate.binary_sha256 | test("^[0-9a-f]{64}$")) and
            (.candidate.working_tree_sha256 | test("^[0-9a-f]{64}$")) and
            (.oracle | keys | sort) == ["image_linux_amd64_digest", "source_commit"] and
            .oracle.source_commit == $source and
            (.oracle.image_linux_amd64_digest | test("^sha256:[0-9a-f]{64}$")) and
            (.stages | map(.name)) == $stages and
            (.stages | length) == ($stages | length) and
            (all(.stages[];
                (keys | sort) == ["attempts", "first_attempt", "name", "output_sha256", "skipped", "status"] and
                .status == "pass" and
                .skipped == false and
                .attempts == 1 and
                .first_attempt == true and
                (.output_sha256 | test("^[0-9a-f]{64}$"))
            )) and
            (.case_counts | keys | sort) == [
                "corpus_dependency_test_blocks", "corpus_excluded_post_target_blocks",
                "corpus_non_plugin_blocks",
                "corpus_package_test_blocks", "corpus_pending_blocks",
                "corpus_platform_gap_blocks", "corpus_platform_test_blocks",
                "corpus_post_target_regression_blocks", "corpus_real_process_blocks",
                "dependency_failure_tests", "differential_cases", "plugin_unit_tests",
                "real_process_cases", "selected_plugins"
            ] and
            (all(.case_counts[]; type == "number" and floor == . and . >= 0)) and
            .case_counts.selected_plugins > 0 and
            .case_counts.plugin_unit_tests > 0 and
            .case_counts.dependency_failure_tests > 0 and
            .case_counts.corpus_real_process_blocks > 0 and
            .case_counts.corpus_pending_blocks == 0 and
            .case_counts.real_process_cases > 0 and
            .case_counts.differential_cases == 124 and
            (.differential | keys | sort) == [
                "artifact_sha256", "covered_plugins", "failed", "first_attempt", "passed",
                "required_plugins", "result"
            ] and
            .differential.result == "pass" and
            .differential.first_attempt == true and
            .differential.failed == 0 and
            .differential.required_plugins == .case_counts.selected_plugins and
            .differential.covered_plugins == .case_counts.selected_plugins and
            .differential.passed == .case_counts.differential_cases and
            (.differential.artifact_sha256 | test("^[0-9a-f]{64}$"))
        ' "$artifact" >/dev/null || die "summary artifact violates the fail-closed contract: $artifact"
}

validate_differential_artifact() {
    local artifact=$1 candidate_commit=$2 candidate_sha=$3 oracle_digest=$4 catalog_sha=$5
    [[ -s "$artifact" ]] || die "differential artifact is missing or empty: $artifact"
    jq -e \
        --arg candidate "$candidate_commit" \
        --arg candidate_sha "$candidate_sha" \
        --arg source "$expected_source_commit" \
        --arg digest "$oracle_digest" \
        --arg catalog_sha "$catalog_sha" \
        '
            .schema_version == 3 and
            .suite == "apisix-3.17-plugin-differential-v1" and
            .catalog_sha256 == $catalog_sha and
            (.selection | keys | sort) == [
                "cases", "full_catalog_run", "plugins", "selected_case_count",
                "shard_count", "shard_index"
            ] and
            .selection.plugins == [] and
            .selection.cases == [] and
            .selection.shard_index == 0 and
            .selection.shard_count == 1 and
            .selection.selected_case_count == 124 and
            .selection.full_catalog_run == true and
            .attempt == 1 and
            .first_attempt == true and
            .candidate.source_commit == $candidate and
            (.candidate.binary_sha256 | test("^[0-9a-f]{64}$")) and
            .candidate.binary_sha256 == $candidate_sha and
            .oracle.source_commit == $source and
            .oracle.image_linux_amd64_digest == $digest and
            .coverage.required_count == 111 and
            .coverage.covered_count == .coverage.required_count and
            (.coverage.required_plugins | length) == 111 and
            (.coverage.required_plugins | unique | length) == 111 and
            (.coverage.covered_plugins | length) == .coverage.covered_count and
            (.coverage.covered_plugins | unique | length) == .coverage.covered_count and
            .coverage.covered_plugins == .coverage.required_plugins and
            (.plugins | length) == .coverage.covered_count and
            ([.plugins[].plugin] == .coverage.covered_plugins) and
            ([.plugins[].obligations] | add) == (.cases | length) and
            (all(.plugins[];
                .obligations > 0 and
                .passed == .obligations and
                .first_attempt == true and
                .failed == 0 and
                .result == "pass"
            )) and
            (.cases | length) == 124 and
            .selection.selected_case_count == (.cases | length) and
            ([.cases[].name] | unique | length) == (.cases | length) and
            ([.cases[].plugin] | unique) == (.coverage.covered_plugins | unique) and
            (all(.cases[];
                (.name | type == "string" and length > 0) and
                (.plugin | type == "string" and length > 0) and
                (.obligation | type == "string" and length > 0) and
                .first_attempt == true and
                .passed == true and
                (.candidate | type == "object") and
                (.oracle | type == "object") and
                (.candidate_hash | test("^sha256:[0-9a-f]{64}$")) and
                (.oracle_hash | test("^sha256:[0-9a-f]{64}$"))
            )) and
            .passed == (.cases | length) and .failed == 0 and .result == "pass"
        ' "$artifact" >/dev/null || die \
        "differential artifact violates first-attempt, source, or binary identity contract: $artifact"
}

if (( $# == 2 )) && [[ $1 == --validate-artifact ]]; then
    require_command jq
    validate_artifact "$2"
    printf 'plugin behavior gate artifact: PASS (%s)\n' "$2"
    exit 0
fi
if (( $# == 6 )) && [[ $1 == --validate-differential-artifact ]]; then
    require_command jq
    validate_differential_artifact "$2" "$3" "$4" "$5" "$6"
    printf 'plugin differential artifact: PASS (%s)\n' "$2"
    exit 0
fi
if (( $# == 3 )) && [[ $1 == --working-tree-sha256 ]]; then
    require_command git
    tree_sha=$(compute_working_tree_sha256 "$2" "$3") || die \
        "cannot compute deterministic working-tree identity for $2"
    [[ "$tree_sha" =~ ^[0-9a-f]{64}$ ]] || die "working-tree identity is malformed: $tree_sha"
    printf '%s\n' "$tree_sha"
    exit 0
fi
if (( $# != 0 )); then
    printf 'usage: %s [--validate-artifact FILE | --validate-differential-artifact FILE CANDIDATE_COMMIT CANDIDATE_SHA ORACLE_DIGEST | --working-tree-sha256 ROOT HEAD]\n' "$0" >&2
    exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
manifest=$repo_root/pkg/capability/manifest.yaml
oracle_file=${APISIX_GO_ORACLE_FILE:-$repo_root/validation/compatibility/apisix-3.17/oracle.yaml}
source_dir=${APISIX_SOURCE_DIR:-$repo_root/.cache/apache-apisix}
container_bin=${CONTAINER_BIN:-${DOCKER_BIN:-podman}}
go_cache_runner=${APISIX_GO_CACHE_RUNNER:-$repo_root/scripts/go_cache.sh}
differential_script=$repo_root/scripts/validation/plugin_differential.sh
differential_catalog=$repo_root/validation/compatibility/apisix-3.17/cases.yaml
evidence_dir=${APISIX_GO_BEHAVIOR_GATE_EVIDENCE_DIR:-$repo_root/.cache/validation/plugin-behavior/attempt-1}
artifact=$evidence_dir/summary.json
differential_artifact=$evidence_dir/differential.json
candidate_bin=$evidence_dir/apisix

require_command git
require_command go
require_command jq
require_command "$container_bin"
[[ -f "$manifest" ]] || die "capability manifest does not exist: $manifest"
[[ -f "$oracle_file" ]] || die "oracle file does not exist: $oracle_file"
[[ -x "$go_cache_runner" ]] || die "Go cache runner is not executable: $go_cache_runner"
[[ -x "$differential_script" ]] || die "differential gate is not executable: $differential_script"
[[ -f "$differential_catalog" ]] || die "differential catalog does not exist: $differential_catalog"
[[ ! -e "$evidence_dir" ]] || die "evidence directory already exists; use a new attempt path: $evidence_dir"
mkdir -p "$evidence_dir/stages"

candidate_source_commit=${APISIX_GO_CANDIDATE_SOURCE_COMMIT:-}
repository_head=$(git -C "$repo_root" rev-parse HEAD) || die 'cannot resolve repository HEAD'
if [[ -z "$candidate_source_commit" ]]; then
    candidate_source_commit=$repository_head
fi
[[ "$candidate_source_commit" =~ ^[0-9a-f]{40}$ ]] || die \
    "candidate source commit must be an exact 40-character commit: $candidate_source_commit"
[[ "$candidate_source_commit" == "$repository_head" ]] || die \
    "candidate source commit must equal repository HEAD $repository_head: $candidate_source_commit"
git -C "$repo_root" cat-file -e "$candidate_source_commit^{commit}" 2>/dev/null || \
    die "candidate source commit does not exist in the repository: $candidate_source_commit"

manifest_source_commit=$(read_yaml_scalar "$manifest" source_commit)
oracle_source_commit=$(read_yaml_scalar "$oracle_file" source_commit)
oracle_digest=$(read_yaml_scalar "$oracle_file" image_linux_amd64_digest)
[[ "$manifest_source_commit" == "$expected_source_commit" ]] || die \
    "manifest target commit mismatch: $manifest_source_commit"
[[ "$oracle_source_commit" == "$expected_source_commit" ]] || die \
    "oracle source commit mismatch: $oracle_source_commit"
[[ "$oracle_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die \
    "oracle linux/amd64 digest is mutable or malformed: $oracle_digest"
[[ -d "$source_dir/.git" || -f "$source_dir/.git" ]] || die \
    "APISIX source checkout is unavailable: $source_dir"
resolved_source_commit=$(git -C "$source_dir" rev-parse 'refs/validation/apisix-3.17.0^{commit}') || \
    die 'cannot resolve refs/validation/apisix-3.17.0'
[[ "$resolved_source_commit" == "$expected_source_commit" ]] || die \
    "APISIX source ref mismatch: $resolved_source_commit"
working_tree_sha256=$(compute_working_tree_sha256 "$repo_root" "$repository_head") || die \
    'cannot compute deterministic working-tree identity'

run_stage() {
    local name=$1
    shift
    local output=$evidence_dir/stages/$name.out
    printf 'plugin behavior gate: running %s\n' "$name"
    set +e
    "$@" 2>&1 | tee "$output"
    local status=${PIPESTATUS[0]}
    set -e
    (( status == 0 )) || die "stage $name failed with status $status; partial evidence retained at $evidence_dir"
    [[ -s "$output" ]] || die "stage $name produced no auditable output"
}

run_go() {
    (
        cd "$repo_root"
        # shellcheck disable=SC1091
        source .envrc
        export GOFLAGS=-mod=readonly
        unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY FTP_PROXY
        unset http_proxy https_proxy all_proxy no_proxy ftp_proxy
        "$go_cache_runner" run -- "$@"
    )
}

run_generator_drift() {
    run_go go run ./cmd/capability-gen -repo-root . -check
    printf 'plugin registry drift: PASS\n'
}

run_candidate_build() {
    run_go go build -o "$candidate_bin" .
    [[ -x "$candidate_bin" ]] || die "candidate build did not produce an executable: $candidate_bin"
    printf 'candidate build: PASS\n'
}

run_plugin_units() {
    run_go go test ./pkg/plugin/... -count=1 -json
}

run_dependency_failure() {
    local packages=() package
    while IFS= read -r package; do
        packages+=("$package")
    done < <(
        find "$repo_root/pkg/plugin" -name '*dependency_failure_test.go' -print \
            | sed "s#^$repo_root/##; s#/[^/]*$##; s#^#./#" \
            | sort -u
    )
    (( ${#packages[@]} > 0 )) || die 'no focused dependency/failure packages were discovered'
    run_go go test "${packages[@]}" -count=1 -json
}

run_corpus() {
    (
        export APISIX_GO_REQUIRE_FULL_CORPUS=1
        export APISIX_SOURCE_DIR="$source_dir"
        run_go go test ./t/plugin \
            -run '^(TestManifestCorpusValidates|TestSourceCoverage|TestUpstreamCorpusAccounting|TestUpstreamCorpusCompletion)$' \
            -count=1 -json
    )
}

run_real_process() {
    unset APISIX_GO_SKIP_PLUGIN_INTEGRATION APISIX_GO_PLUGIN_SMOKE_CASE
    run_go go test ./t/plugin -run '^TestPluginIntegration$' -count=1 -json
}

run_differential() {
    local candidate_sha
    candidate_sha=$(sha256_file "$candidate_bin")
    APISIX_GO_DIFFERENTIAL_ARTIFACT="$differential_artifact" \
        APISIX_GO_CANDIDATE_BIN="$candidate_bin" \
        APISIX_GO_CANDIDATE_BINARY_SHA256="$candidate_sha" \
        APISIX_GO_CANDIDATE_SOURCE_COMMIT="$candidate_source_commit" \
        APISIX_GO_SKIP_BUILD=1 \
        APISIX_SOURCE_DIR="$source_dir" \
        CONTAINER_BIN="$container_bin" \
        "$differential_script"
}

run_stage generator_drift run_generator_drift

selected_plugins=111

run_stage candidate_build run_candidate_build
run_stage plugin_units run_plugin_units
run_stage dependency_failure run_dependency_failure
run_stage corpus run_corpus
run_stage real_process run_real_process
run_stage differential run_differential

for stage in plugin_units dependency_failure corpus real_process; do
    output=$evidence_dir/stages/$stage.out
    if grep -Fq '"Action":"skip"' "$output"; then
        die "stage $stage reported a skipped test"
    fi
done

count_passed_tests() {
    jq -Rsc '
        split("\n") |
        [ .[] | fromjson? | select(.Action == "pass" and (.Test // "") != "") ] |
        length
    ' "$1"
}

plugin_unit_tests=$(count_passed_tests "$evidence_dir/stages/plugin_units.out")
dependency_failure_tests=$(count_passed_tests "$evidence_dir/stages/dependency_failure.out")
real_process_cases=$(jq -Rsc '
    split("\n") |
    [ .[] | fromjson? | select(
        .Action == "pass" and ((.Test // "") | test("^TestPluginIntegration/[^/]+/[^/]+$"))
    ) ] |
    length
' "$evidence_dir/stages/real_process.out")

corpus_line=$(jq -Rr 'fromjson? | .Output? // empty' "$evidence_dir/stages/corpus.out" \
    | grep -F 'corpus completion:' | tail -n 1 || true)
corpus_pattern='corpus completion: ([0-9]+) real-process validation blocks, ([0-9]+) package-test blocks, ([0-9]+) dependency-test blocks, ([0-9]+) platform-test blocks, ([0-9]+) platform-gap blocks, ([0-9]+) post-target regression blocks, ([0-9]+) excluded post-target blocks, ([0-9]+) non-plugin blocks, ([0-9]+) pending/blocked plugin blocks across ([0-9]+) sources'
[[ "$corpus_line" =~ $corpus_pattern ]] || die 'corpus output did not contain deterministic completion counts'
corpus_real_process_blocks=${BASH_REMATCH[1]}
corpus_package_test_blocks=${BASH_REMATCH[2]}
corpus_dependency_test_blocks=${BASH_REMATCH[3]}
corpus_platform_test_blocks=${BASH_REMATCH[4]}
corpus_platform_gap_blocks=${BASH_REMATCH[5]}
corpus_post_target_regression_blocks=${BASH_REMATCH[6]}
corpus_excluded_post_target_blocks=${BASH_REMATCH[7]}
corpus_non_plugin_blocks=${BASH_REMATCH[8]}
corpus_pending_blocks=${BASH_REMATCH[9]}
corpus_pending_sources=${BASH_REMATCH[10]}
(( corpus_pending_blocks == 0 && corpus_pending_sources == 0 )) || die \
    "corpus retained $corpus_pending_blocks pending blocks across $corpus_pending_sources sources"

[[ -s "$differential_artifact" ]] || die 'differential stage did not produce its artifact'
candidate_binary_sha256=$(sha256_file "$candidate_bin")
catalog_sha256=sha256:$(sha256_file "$differential_catalog")
validate_differential_artifact \
    "$differential_artifact" "$candidate_source_commit" "$candidate_binary_sha256" "$oracle_digest" "$catalog_sha256"
differential_cases=$(jq -r '.cases | length' "$differential_artifact")
differential_passed=$(jq -r .passed "$differential_artifact")
differential_failed=$(jq -r .failed "$differential_artifact")
differential_required_plugins=$(jq -r '.coverage.required_count' "$differential_artifact")
differential_covered_plugins=$(jq -r '.coverage.covered_count' "$differential_artifact")
(( differential_required_plugins == selected_plugins )) || die \
    "differential artifact required set mismatch: $differential_required_plugins/$selected_plugins"
(( differential_covered_plugins == selected_plugins )) || die \
    "validation requires per-plugin differential coverage: $differential_covered_plugins/$selected_plugins"

stages_json='[]'
for stage in "${required_stages[@]}"; do
    output_sha=$(sha256_file "$evidence_dir/stages/$stage.out")
    stages_json=$(jq -cn \
        --argjson stages "$stages_json" \
        --arg name "$stage" \
        --arg sha "$output_sha" \
        '$stages + [{name: $name, status: "pass", skipped: false, attempts: 1, first_attempt: true, output_sha256: $sha}]')
done

final_working_tree_sha256=$(compute_working_tree_sha256 "$repo_root" "$repository_head") || die \
    'cannot recompute deterministic working-tree identity'
[[ "$final_working_tree_sha256" == "$working_tree_sha256" ]] || die \
    "working tree changed during validation: start=$working_tree_sha256 end=$final_working_tree_sha256"

jq -n \
    --arg suite "$suite" \
    --arg candidate "$candidate_source_commit" \
    --arg candidate_sha "$candidate_binary_sha256" \
    --arg working_tree_sha "$working_tree_sha256" \
    --arg source "$expected_source_commit" \
    --arg digest "$oracle_digest" \
    --arg differential_sha "$(sha256_file "$differential_artifact")" \
    --argjson selected "$selected_plugins" \
    --argjson units "$plugin_unit_tests" \
    --argjson dependencies "$dependency_failure_tests" \
    --argjson corpus_real "$corpus_real_process_blocks" \
    --argjson corpus_package "$corpus_package_test_blocks" \
    --argjson corpus_dependency "$corpus_dependency_test_blocks" \
    --argjson corpus_platform "$corpus_platform_test_blocks" \
    --argjson corpus_gap "$corpus_platform_gap_blocks" \
    --argjson corpus_regression "$corpus_post_target_regression_blocks" \
    --argjson corpus_excluded "$corpus_excluded_post_target_blocks" \
    --argjson corpus_non_plugin "$corpus_non_plugin_blocks" \
    --argjson corpus_pending "$corpus_pending_blocks" \
    --argjson real_process "$real_process_cases" \
    --argjson differential_cases "$differential_cases" \
    --argjson differential_passed "$differential_passed" \
    --argjson differential_failed "$differential_failed" \
    --argjson differential_required "$differential_required_plugins" \
    --argjson differential_covered "$differential_covered_plugins" \
    --argjson stages "$stages_json" \
    '{
        schema_version: 1,
        suite: $suite,
        candidate: {
            source_commit: $candidate,
            working_tree_sha256: $working_tree_sha,
            binary_sha256: $candidate_sha
        },
        oracle: {
            source_commit: $source,
            image_linux_amd64_digest: $digest
        },
        case_counts: {
            selected_plugins: $selected,
            plugin_unit_tests: $units,
            dependency_failure_tests: $dependencies,
            corpus_real_process_blocks: $corpus_real,
            corpus_package_test_blocks: $corpus_package,
            corpus_dependency_test_blocks: $corpus_dependency,
            corpus_platform_test_blocks: $corpus_platform,
            corpus_platform_gap_blocks: $corpus_gap,
            corpus_post_target_regression_blocks: $corpus_regression,
            corpus_excluded_post_target_blocks: $corpus_excluded,
            corpus_non_plugin_blocks: $corpus_non_plugin,
            corpus_pending_blocks: $corpus_pending,
            real_process_cases: $real_process,
            differential_cases: $differential_cases
        },
        first_attempt: true,
        stages: $stages,
        differential: {
            artifact_sha256: $differential_sha,
            result: "pass",
            first_attempt: true,
            required_plugins: $differential_required,
            covered_plugins: $differential_covered,
            passed: $differential_passed,
            failed: $differential_failed
        },
        result: "pass"
    }' >"$artifact"

validate_artifact "$artifact"
cat "$artifact"
printf 'plugin behavior gate: PASS (%s)\n' "$artifact"
