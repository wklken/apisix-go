#!/usr/bin/env bash
set -euo pipefail

required_records=(journal generation)

fail() {
    printf 'platform recovery evidence: %s\n' "$1" >&2
    return 1
}

json_string() {
    local file=$1 field=$2
    sed -n "s/.*\"$field\":\"\([^\"]*\)\".*/\1/p" "$file" | head -n 1
}

validate_record_identity() {
    local file=$1 expected_source=$2 expected_image=$3 expected_config=$4
    local before after before_identity after_identity probe_count window_probe_count
    [[ $(json_string "$file" source_commit) == "$expected_source" ]] || { fail "$file source_commit mismatch"; return 1; }
    [[ $(json_string "$file" image_id) == "$expected_image" ]] || { fail "$file image_id mismatch"; return 1; }
    [[ $(json_string "$file" config_sha256) == "$expected_config" ]] || { fail "$file config_sha256 mismatch"; return 1; }
    [[ $(json_string "$file" scope) == platform-recovery-v1 ]] || { fail "$file scope mismatch"; return 1; }
    [[ $(json_string "$file" probe_result) == pass ]] || { fail "$file probe did not pass"; return 1; }
    [[ -z $(json_string "$file" plugin) ]] || { fail "$file must not claim plugin recovery"; return 1; }
    before=$(json_string "$file" before_generation)
    after=$(json_string "$file" after_generation)
    [[ "$before" =~ ^[1-9][0-9]*$ && "$after" =~ ^[1-9][0-9]*$ ]] || \
        { fail "$file generation bounds are invalid"; return 1; }
    (( after > before )) || { fail "$file generation did not advance"; return 1; }
    before_identity=$(json_string "$file" replica_before_identity)
    after_identity=$(json_string "$file" replica_after_identity)
    [[ -n "$before_identity" && -n "$after_identity" && "$before_identity" != "$after_identity" ]] || \
        { fail "$file replica restart identity did not change"; return 1; }
    probe_count=$(json_string "$file" survivor_probe_count)
    window_probe_count=$(json_string "$file" survivor_window_probe_count)
    if [[ ! "$probe_count" =~ ^[0-9]+$ ]] || (( probe_count < 2 )); then
        fail "$file survivor probe count is invalid"
        return 1
    fi
    [[ "$window_probe_count" =~ ^[1-9][0-9]*$ ]] || \
        { fail "$file restart-window survivor probe count is invalid"; return 1; }
    [[ $(json_string "$file" output_sha256) =~ ^[0-9a-f]{64}$ ]] || { fail "$file output_sha256 is invalid"; return 1; }
    [[ $(json_string "$file" etcd_tls_peer) == etcd:2379 ]] || { fail "$file TLS peer mismatch"; return 1; }
    [[ $(json_string "$file" etcd_ca_sha256) =~ ^[0-9a-f]{64}$ ]] || { fail "$file CA fingerprint is invalid"; return 1; }
    [[ $(json_string "$file" etcd_server_cert_sha256) =~ ^[0-9a-f]{64}$ ]] || \
        { fail "$file server certificate fingerprint is invalid"; return 1; }
}

validate_evidence_dir() {
    local evidence_dir=$1 first source image config record_name file
    first="$evidence_dir/${required_records[0]}.json"
    [[ -s "$first" ]] || { fail "missing ${required_records[0]}.json"; return 1; }
    source=$(json_string "$first" source_commit)
    image=$(json_string "$first" image_id)
    config=$(json_string "$first" config_sha256)
    [[ "$source" =~ ^[0-9a-f]{40}$ ]] || { fail 'source_commit is not an exact commit'; return 1; }
    [[ "$image" =~ ^sha256:[0-9a-f]{64}$ ]] || { fail 'image_id is not immutable'; return 1; }
    [[ "$config" =~ ^[0-9a-f]{64}$ ]] || { fail 'config_sha256 is invalid'; return 1; }

    for record_name in "${required_records[@]}"; do
        file="$evidence_dir/$record_name.json"
        [[ -s "$file" ]] || { fail "missing $record_name.json"; return 1; }
        [[ $(json_string "$file" record) == "$record_name" ]] || { fail "$file record mismatch"; return 1; }
        validate_record_identity "$file" "$source" "$image" "$config" || return 1
    done
}

write_fixture() {
    local evidence_dir=$1 source=$2 image=$3 config=$4 output=$5 record_name
    mkdir -p "$evidence_dir"
    for record_name in "${required_records[@]}"; do
        printf '{"scope":"platform-recovery-v1","record":"%s","source_commit":"%s","image_id":"%s","config_sha256":"%s","before_generation":"41","after_generation":"42","probe_result":"pass","etcd_tls_peer":"etcd:2379","etcd_ca_sha256":"%s","etcd_server_cert_sha256":"%s","replica_before_identity":"replica 2026-08-29T05:00:00Z","replica_after_identity":"replica 2026-08-29T05:00:01Z","survivor_probe_count":"2","survivor_window_probe_count":"1","attempt":"test","output_sha256":"%s"}\n' \
            "$record_name" "$source" "$image" "$config" "$config" "$config" "$output" >"$evidence_dir/$record_name.json"
    done
}

if (( $# == 2 )) && [[ $1 == --evidence-dir ]]; then
    validate_evidence_dir "$2"
    printf 'platform recovery evidence: PASS\n'
    exit 0
fi
if (( $# != 0 )); then
    printf 'usage: %s [--evidence-dir DIR]\n' "$0" >&2
    exit 2
fi

test_root=$(mktemp -d "${TMPDIR:-/tmp}/platform-recovery-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT
source_commit=$(printf 'a%.0s' {1..40})
image_id=sha256:$(printf 'b%.0s' {1..64})
config_sha=$(printf 'c%.0s' {1..64})
output_sha=$(printf 'd%.0s' {1..64})

write_fixture "$test_root/valid" "$source_commit" "$image_id" "$config_sha" "$output_sha"
validate_evidence_dir "$test_root/valid"

write_fixture "$test_root/double-digit-probes" "$source_commit" "$image_id" "$config_sha" "$output_sha"
sed -i.bak 's/"survivor_probe_count":"2"/"survivor_probe_count":"10"/' \
    "$test_root/double-digit-probes/journal.json" "$test_root/double-digit-probes/generation.json"
rm "$test_root/double-digit-probes/journal.json.bak" "$test_root/double-digit-probes/generation.json.bak"
validate_evidence_dir "$test_root/double-digit-probes"

write_fixture "$test_root/missing" "$source_commit" "$image_id" "$config_sha" "$output_sha"
rm "$test_root/missing/generation.json"
if validate_evidence_dir "$test_root/missing" 2>/dev/null; then
    fail 'missing generation record was accepted'
fi

write_fixture "$test_root/mismatch" "$source_commit" "$image_id" "$config_sha" "$output_sha"
sed -i.bak 's/"source_commit":"[a-f0-9]*"/"source_commit":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"/' \
    "$test_root/mismatch/generation.json"
rm "$test_root/mismatch/generation.json.bak"
if validate_evidence_dir "$test_root/mismatch" 2>/dev/null; then
    fail 'mixed source identities were accepted'
fi

write_fixture "$test_root/plugin-coupled" "$source_commit" "$image_id" "$config_sha" "$output_sha"
sed -i.bak 's/"record":"journal"/"record":"journal","plugin":"key-auth"/' \
    "$test_root/plugin-coupled/journal.json"
rm "$test_root/plugin-coupled/journal.json.bak"
if validate_evidence_dir "$test_root/plugin-coupled" 2>/dev/null; then
    fail 'plugin-coupled recovery evidence was accepted'
fi

write_fixture "$test_root/missing-window-probe" "$source_commit" "$image_id" "$config_sha" "$output_sha"
sed -i.bak 's/"survivor_window_probe_count":"1"/"survivor_window_probe_count":"0"/' \
    "$test_root/missing-window-probe/generation.json"
rm "$test_root/missing-window-probe/generation.json.bak"
if validate_evidence_dir "$test_root/missing-window-probe" 2>/dev/null; then
    fail 'recovery evidence without a restart-window survivor probe was accepted'
fi

printf 'platform recovery evidence: PASS\n'
