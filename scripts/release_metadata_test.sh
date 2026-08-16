#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/apisix-release-metadata-test.XXXXXX")
trap 'rm -rf "$temp_dir"' EXIT

mkdir -p "$temp_dir/bin"
cat >"$temp_dir/bin/docker" <<'SH'
#!/usr/bin/env bash
printf 'docker must not be invoked by release metadata\n' >&2
exit 97
SH
chmod +x "$temp_dir/bin/docker"

printf 'zeta-content' >"$temp_dir/zeta.txt"
printf 'alpha-content' >"$temp_dir/alpha.txt"

digest_a="sha256:$(printf 'a%.0s' {1..64})"
digest_b="sha256:$(printf 'b%.0s' {1..64})"
rc_output="$temp_dir/rc-metadata.json"
rc_repeat_output="$temp_dir/rc-metadata-repeat.json"

run_metadata() {
    PATH="$temp_dir/bin:$PATH" \
    SOURCE_REF=refs/tags/v1.2.3 \
    SOURCE_COMMIT=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
        bash "$repo_root/scripts/release_metadata.sh" "$@"
}

run_metadata \
    apisix-go:v1.2.3 "$digest_a" "$rc_output" \
    "$temp_dir/zeta.txt" "$temp_dir/alpha.txt"

jq -e --arg digest "$digest_a" '
    .schema_version == 2 and
    .source.ref == "refs/tags/v1.2.3" and
    .source.commit == "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" and
    .image_reference == "apisix-go:v1.2.3" and
    .image_digest == $digest and
    (.artifacts == [
      {name: "alpha.txt", sha256: "3b867bbfe4d276005ffae3db17950428a12f1f61dd8ea55cd1eed11ea02b85a2"},
      {name: "zeta.txt", sha256: "7eb877826d096d05ff3fd03d102b816c78a71ce8d208fd6032e20127b37c9487"}
    ]) and
    (all(.artifacts[]; (.sha256 | test("^[0-9a-f]{64}$"))))
' "$rc_output" >/dev/null

run_metadata \
    apisix-go:v1.2.3 "$digest_a" "$rc_repeat_output" \
    "$temp_dir/alpha.txt" "$temp_dir/zeta.txt"
cmp -s "$rc_output" "$rc_repeat_output"

final_reference="ghcr.io/example/apisix-go@$digest_b"
final_output="$temp_dir/final-metadata.json"
run_metadata \
    "$final_reference" "$digest_b" "$final_output"

jq -e --arg reference "$final_reference" --arg digest "$digest_b" '
    .schema_version == 2 and
    .image_reference == $reference and
    .image_digest == $digest and
    (.artifacts | length) == 0
' "$final_output" >/dev/null

port_reference="registry.example.com:5000/example/apisix-go@$digest_a"
port_output="$temp_dir/port-metadata.json"
PUBLISH_IMAGE=true \
    run_metadata "$port_reference" "$digest_a" "$port_output"
jq -e --arg reference "$port_reference" '
    .image_reference == $reference and
    (.image_digest | test("^sha256:[0-9a-f]{64}$"))
' "$port_output" >/dev/null

fallback_output="$temp_dir/fallback-metadata.json"
fallback_ref=$(git -C "$repo_root" symbolic-ref -q HEAD || git -C "$repo_root" describe --tags --always)
fallback_commit=$(git -C "$repo_root" rev-parse HEAD)
(
    cd "$repo_root"
    env -u SOURCE_REF -u GITHUB_REF -u SOURCE_COMMIT -u GITHUB_SHA \
        PATH="$temp_dir/bin:$PATH" \
        bash "$repo_root/scripts/release_metadata.sh" \
            apisix-go:fallback "$digest_a" "$fallback_output"
)
jq -e --arg ref "$fallback_ref" --arg commit "$fallback_commit" '
    .source == {ref: $ref, commit: $commit}
' "$fallback_output" >/dev/null

assert_rejected() {
    local name=$1
    shift
    local output="$temp_dir/$name.json"
    printf 'must remain unchanged\n' >"$output"
    if run_metadata "$@" >"$temp_dir/$name.stdout" 2>"$temp_dir/$name.stderr"; then
        printf 'expected %s to be rejected\n' "$name" >&2
        exit 1
    fi
    if [[ $(<"$output") != 'must remain unchanged' ]]; then
        printf '%s wrote output before validation completed\n' "$name" >&2
        exit 1
    fi
}

assert_rejected \
    missing-artifact \
    apisix-go:v1.2.3 "$digest_a" "$temp_dir/missing-artifact.json" \
    "$temp_dir/does-not-exist.txt"

assert_rejected \
    invalid-digest \
    apisix-go:v1.2.3 sha256:not-a-lowercase-64-hex-digest "$temp_dir/invalid-digest.json"

uppercase_digest="sha256:$(printf 'A%.0s' {1..64})"
assert_rejected \
    uppercase-digest \
    apisix-go:v1.2.3 "$uppercase_digest" "$temp_dir/uppercase-digest.json"

PUBLISH_IMAGE=true \
    assert_rejected \
        tag-only-publish \
        apisix-go:v1.2.3 "$digest_a" "$temp_dir/tag-only-publish.json"

PUBLISH_IMAGE=true \
    assert_rejected \
        tag-and-digest-publish \
        "ghcr.io/example/apisix-go:v1.2.3@$digest_a" "$digest_a" \
        "$temp_dir/tag-and-digest-publish.json"

PUBLISH_IMAGE=true \
    assert_rejected \
        reference-digest-mismatch \
        "ghcr.io/example/apisix-go@$digest_a" "$digest_b" \
        "$temp_dir/reference-digest-mismatch.json"

cat >"$temp_dir/bin/sha256sum" <<'SH'
#!/usr/bin/env bash
exit 41
SH
chmod +x "$temp_dir/bin/sha256sum"
assert_rejected \
    hash-failure \
    apisix-go:v1.2.3 "$digest_a" "$temp_dir/hash-failure.json" \
    "$temp_dir/alpha.txt"

printf 'release metadata: PASS\n'
