#!/usr/bin/env bash
# Build a verified single-file panel from a pinned upstream and focused patches.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
output=${1:?output HTML path required}
mkdir -p "$(dirname "$output")"
output=$(cd "$(dirname "$output")" && pwd)/$(basename "$output")
revision=$(cat "$root/upstream-revision")
[[ "$revision" =~ ^[0-9a-f]{40}$ ]]
source_dir=$(mktemp -d)
trap 'rm -rf "$source_dir"' EXIT
git -C "$source_dir" init -q
git -C "$source_dir" remote add origin https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git
git -C "$source_dir" -c http.version=HTTP/1.1 fetch --depth=1 origin "$revision"
git -C "$source_dir" checkout -q --detach FETCH_HEAD
git -C "$source_dir" apply --check "$root/bps-quota.patch"
git -C "$source_dir" apply "$root/bps-quota.patch"
git -C "$source_dir" apply --check "$root/bps-quota-style.patch"
git -C "$source_dir" apply "$root/bps-quota-style.patch"
cd "$source_dir"
bun=${BUN:-bun}
test "$("$bun" --version)" = 1.4.3
"$bun" install --frozen-lockfile
VERSION=bps-v0.5.8 "$bun" run verify
install -m 644 dist/index.html "$output"
