#!/usr/bin/env bash
# Shows upstream drift for every file vendored from unreal-agent.
#
#   scripts/vendor-diff.sh [TO]            diff pinned commit -> TO (default: origin/main)
#   scripts/vendor-diff.sh --update [TO]   also refresh the pristine copies and UPSTREAM_COMMIT
#
# Apply the printed diff to the modified copies listed in VENDORED.md, then
# rerun with --update so the next bump diffs from the new base.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
pristine="$root/third_party/unreal-agent"
cache="$root/.cache/unreal-agent"
upstream=https://github.com/unreallabsai/unreal-agent.git

update=false
if [[ "${1:-}" == "--update" ]]; then
  update=true
  shift
fi
to=${1:-origin/main}
from=$(cat "$pristine/UPSTREAM_COMMIT")

if [[ ! -d "$cache/.git" ]]; then
  git clone --quiet "$upstream" "$cache"
fi
git -C "$cache" fetch --quiet origin
to_commit=$(git -C "$cache" rev-parse "$to^{commit}")

files=()
while IFS= read -r path; do
  files+=("${path#"$pristine"/}")
done < <(find "$pristine" -name '*.go' -type f | sort)

git -C "$cache" --no-pager diff --stat "$from" "$to_commit" -- "${files[@]}"
git -C "$cache" --no-pager diff "$from" "$to_commit" -- "${files[@]}"

if $update; then
  for path in "${files[@]}"; do
    git -C "$cache" show "$to_commit:$path" > "$pristine/$path"
  done
  echo "$to_commit" > "$pristine/UPSTREAM_COMMIT"
  echo "Pristine copies refreshed to $to_commit. Bump go.mod to the same commit." >&2
fi
