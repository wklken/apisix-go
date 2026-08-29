#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
destination=${1:-"$repo_root/.cache/apache-apisix"}
official_repository=https://github.com/apache/apisix.git
target_tag=3.17.0
target_commit=9ef2ecab67f652d38365049613610ef649bb4ad0
historical_commit=c3d7d5ec69774121f53d2e20d29d09c816795dd7

die() {
    printf 'fetch APISIX 3.17 source: %s\n' "$*" >&2
    exit 1
}

command -v git >/dev/null 2>&1 || die 'git is required'

if [[ ! -e "$destination/.git" ]]; then
    [[ ! -e "$destination" || -d "$destination" ]] || die "destination is not a directory: $destination"
    mkdir -p "$destination"
    git -C "$destination" init --quiet
    git -C "$destination" remote add origin "$official_repository"
fi

origin=$(git -C "$destination" remote get-url origin 2>/dev/null) || \
    die "destination has no origin remote: $destination"
[[ "$origin" == "$official_repository" ]] || \
    die "origin must be $official_repository, got $origin"

git -C "$destination" fetch --quiet --no-tags --depth=1 origin "refs/tags/$target_tag"
resolved_target=$(git -C "$destination" rev-parse 'FETCH_HEAD^{commit}')
[[ "$resolved_target" == "$target_commit" ]] || \
    die "tag $target_tag resolved to $resolved_target, expected $target_commit"
git -C "$destination" update-ref refs/qualification/apisix-3.17.0 "$resolved_target"

# Keep the previous corpus snapshot reachable until every ledger row has been
# reconciled against the 3.17.0 target. It is evidence input, never the target.
git -C "$destination" fetch --quiet --no-tags --depth=1 origin "$historical_commit"
resolved_historical=$(git -C "$destination" rev-parse 'FETCH_HEAD^{commit}')
[[ "$resolved_historical" == "$historical_commit" ]] || \
    die "historical corpus commit resolved to $resolved_historical, expected $historical_commit"
git -C "$destination" update-ref refs/qualification/historical-corpus "$resolved_historical"

printf 'APISIX source ready: path=%s target=%s historical=%s origin=%s\n' \
    "$destination" "$resolved_target" "$resolved_historical" "$origin"
