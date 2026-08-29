#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
gate=$repo_root/scripts/validation/plugin_behavior_gate.sh
test_root=$(mktemp -d "${TMPDIR:-/tmp}/plugin-behavior-gate-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT

fail() {
    printf 'plugin behavior gate test: %s\n' "$*" >&2
    exit 1
}

readonly source_commit=9ef2ecab67f652d38365049613610ef649bb4ad0
readonly candidate_commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly image_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
readonly output_sha=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc

write_valid_artifact() {
    local artifact=$1
    jq -n \
        --arg source "$source_commit" \
        --arg candidate "$candidate_commit" \
        --arg digest "$image_digest" \
        --arg sha "$output_sha" \
        '{
            schema_version: 1,
            suite: "apisix-3.17-plugin-differential-v1",
            candidate: {source_commit: $candidate, working_tree_sha256: $sha, binary_sha256: $sha},
            oracle: {source_commit: $source, image_linux_amd64_digest: $digest},
            case_counts: {
                selected_plugins: 111,
                capability_contract_tests: 12,
                plugin_unit_tests: 800,
                dependency_failure_tests: 30,
                corpus_real_process_blocks: 3751,
                corpus_package_test_blocks: 363,
                corpus_dependency_test_blocks: 2,
                corpus_platform_test_blocks: 14,
                corpus_platform_gap_blocks: 124,
                corpus_post_target_regression_blocks: 150,
                corpus_excluded_post_target_blocks: 278,
                corpus_non_plugin_blocks: 288,
                corpus_pending_blocks: 0,
                real_process_cases: 500,
                differential_cases: 124
            },
            first_attempt: true,
            stages: [
                "capability_contract", "generator_drift", "candidate_build", "plugin_units", "dependency_failure",
                "corpus", "real_process", "differential"
            ] | map({name: ., status: "pass", skipped: false, attempts: 1, first_attempt: true, output_sha256: $sha}),
            differential: {
                artifact_sha256: $sha,
                result: "pass",
                first_attempt: true,
                required_plugins: 111,
                covered_plugins: 111,
                passed: 124,
                failed: 0
            },
            result: "pass"
        }' >"$artifact"
}

expect_rejected() {
    local name=$1 artifact=$2
    if "$gate" --validate-artifact "$artifact" >"$test_root/$name.out" 2>&1; then
        fail "$name artifact was accepted"
    fi
    grep -Fq 'violates the fail-closed contract' "$test_root/$name.out" || \
        fail "$name rejection did not name the artifact contract"
}

valid=$test_root/valid.json
write_valid_artifact "$valid"
"$gate" --validate-artifact "$valid" >/dev/null

skipped=$test_root/skipped.json
jq '(.stages[] | select(.name == "real_process") | .skipped) = true' "$valid" >"$skipped"
expect_rejected skipped-stage "$skipped"

identity=$test_root/identity.json
jq '.oracle.source_commit = "dddddddddddddddddddddddddddddddddddddddd"' "$valid" >"$identity"
expect_rejected identity-mismatch "$identity"

retry=$test_root/retry.json
jq '(.stages[] | select(.name == "differential") | .attempts) = 2 |
    (.stages[] | select(.name == "differential") | .first_attempt) = false |
    .differential.first_attempt = false' "$valid" >"$retry"
expect_rejected retry-only-success "$retry"

missing=$test_root/missing-field.json
jq 'del(.case_counts.corpus_pending_blocks)' "$valid" >"$missing"
expect_rejected missing-field "$missing"

malformed=$test_root/malformed.json
printf '{"schema_version":1,"result":"pass"' >"$malformed"
expect_rejected malformed-json "$malformed"

mutable=$test_root/mutable-identity.json
jq '.oracle.image_linux_amd64_digest = "apache/apisix:3.17.0"' "$valid" >"$mutable"
expect_rejected mutable-identity "$mutable"

missing_tree=$test_root/missing-working-tree.json
jq 'del(.candidate.working_tree_sha256)' "$valid" >"$missing_tree"
expect_rejected missing-working-tree "$missing_tree"

malformed_tree=$test_root/malformed-working-tree.json
jq '.candidate.working_tree_sha256 = "sha256:mutable"' "$valid" >"$malformed_tree"
expect_rejected malformed-working-tree "$malformed_tree"

pending=$test_root/pending-corpus.json
jq '.case_counts.corpus_pending_blocks = 1' "$valid" >"$pending"
expect_rejected pending-corpus "$pending"

flaky=$test_root/flaky-stage.json
jq '(.stages[] | select(.name == "dependency_failure") | .status) = "flaky"' "$valid" >"$flaky"
expect_rejected flaky-stage "$flaky"

differential=$test_root/differential.json
jq -n \
    --arg source "$source_commit" \
    --arg candidate "$candidate_commit" \
    --arg digest "$image_digest" \
    --arg sha "$output_sha" \
    '{
        schema_version: 3,
        suite: "apisix-3.17-plugin-differential-v1",
        catalog_sha256: ("sha256:" + $sha),
        selection: {
            plugins: [],
            cases: [],
            shard_index: 0,
            shard_count: 1,
            selected_case_count: 124,
            full_catalog_run: true
        },
        attempt: 1,
        first_attempt: true,
        candidate: {source_commit: $candidate, binary_sha256: $sha},
        oracle: {source_commit: $source, image_linux_amd64_digest: $digest},
        coverage: {
            required_count: 111,
            covered_count: 111,
            required_plugins: [range(0; 111) | "plugin-\(.)"],
            covered_plugins: [range(0; 111) | "plugin-\(.)"]
        },
        plugins: [range(0; 111) | {
            plugin: "plugin-\(.)",
            obligations: (if . < 13 then 2 else 1 end),
            passed: (if . < 13 then 2 else 1 end),
            failed: 0,
            first_attempt: true,
            result: "pass"
        }],
        cases: [range(0; 124) as $case | ($case % 111) as $plugin | {
            name: "case-\($case)",
            plugin: "plugin-\($plugin)",
            obligation: "obligation-\($case)",
            first_attempt: true,
            passed: true,
            candidate: {},
            oracle: {},
            candidate_hash: ("sha256:" + $sha),
            oracle_hash: ("sha256:" + $sha)
        }],
        passed: 124,
        failed: 0,
        result: "pass"
    }' >"$differential"
"$gate" --validate-differential-artifact \
    "$differential" "$candidate_commit" "$output_sha" "$image_digest" "sha256:$output_sha" >/dev/null

partial_selection=$test_root/partial-selection.json
jq '.selection.full_catalog_run = false | .selection.plugins = ["plugin-0"]' \
    "$differential" >"$partial_selection"
if "$gate" --validate-differential-artifact \
    "$partial_selection" "$candidate_commit" "$output_sha" "$image_digest" "sha256:$output_sha" >/dev/null 2>&1; then
    fail 'partial differential selection was accepted as full validation'
fi

sharded_selection=$test_root/sharded-selection.json
jq '.selection.shard_count = 2' "$differential" >"$sharded_selection"
if "$gate" --validate-differential-artifact \
    "$sharded_selection" "$candidate_commit" "$output_sha" "$image_digest" "sha256:$output_sha" >/dev/null 2>&1; then
    fail 'sharded differential selection was accepted as full validation'
fi

missing_binary=$test_root/missing-differential-binary.json
jq 'del(.candidate.binary_sha256)' "$differential" >"$missing_binary"
if "$gate" --validate-differential-artifact \
    "$missing_binary" "$candidate_commit" "$output_sha" "$image_digest" "sha256:$output_sha" >/dev/null 2>&1; then
    fail 'differential artifact without candidate binary identity was accepted'
fi

wrong_binary=$test_root/wrong-differential-binary.json
jq '.candidate.binary_sha256 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"' \
    "$differential" >"$wrong_binary"
if "$gate" --validate-differential-artifact \
    "$wrong_binary" "$candidate_commit" "$output_sha" "$image_digest" "sha256:$output_sha" >/dev/null 2>&1; then
    fail 'differential artifact for a different candidate binary was accepted'
fi

tree_repo=$test_root/tree-repo
mkdir -p "$tree_repo"
git -C "$tree_repo" init -q
git -C "$tree_repo" config user.name 'Gate Test'
git -C "$tree_repo" config user.email gate@example.test
printf 'ignored.txt\n' >"$tree_repo/.gitignore"
printf 'tracked-v1\n' >"$tree_repo/tracked.txt"
printf 'deleted\n' >"$tree_repo/deleted.txt"
git -C "$tree_repo" add .gitignore tracked.txt deleted.txt
git -C "$tree_repo" commit -qm initial
printf 'tracked-v2\n' >"$tree_repo/tracked.txt"
rm "$tree_repo/deleted.txt"
printf 'untracked-v1\n' >"$tree_repo/untracked.txt"
printf 'ignored-v1\n' >"$tree_repo/ignored.txt"

tree_sha=$("$gate" --working-tree-sha256 "$tree_repo" HEAD)
tree_sha_repeat=$("$gate" --working-tree-sha256 "$tree_repo" HEAD)
[[ "$tree_sha" == "$tree_sha_repeat" && "$tree_sha" =~ ^[0-9a-f]{64}$ ]] || \
    fail 'working-tree identity is not deterministic'
printf 'ignored-v2\n' >"$tree_repo/ignored.txt"
[[ $("$gate" --working-tree-sha256 "$tree_repo" HEAD) == "$tree_sha" ]] || \
    fail 'ignored content changed the working-tree identity'
printf 'untracked-v2\n' >"$tree_repo/untracked.txt"
[[ $("$gate" --working-tree-sha256 "$tree_repo" HEAD) != "$tree_sha" ]] || \
    fail 'untracked non-ignored content did not change the working-tree identity'

printf 'plugin behavior gate tests passed\n'
