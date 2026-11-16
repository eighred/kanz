#!/usr/bin/env bash
set -euo pipefail

schema_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$schema_dir" rev-parse --show-toplevel)"
baseline_commit="$(git -C "$repo" rev-parse --verify 'origin/main^{commit}')"
baseline_dir=''

# During the directory migration, main still owns kanz-schemas/. Select from
# that revision's tree, never from the candidate's layout or an empty baseline.
for candidate in schemas kanz-schemas; do
  if git -C "$repo" cat-file -e "$baseline_commit:$candidate/buf.yaml" 2>/dev/null; then
    if [ -n "$baseline_dir" ]; then
      echo 'refusing ambiguous schema baseline: both known module roots exist' >&2
      exit 1
    fi
    baseline_dir="$candidate"
  fi
done
if [ -z "$baseline_dir" ]; then
  echo 'refusing missing schema baseline on origin/main' >&2
  exit 1
fi

printf 'Comparing schemas against %s:%s\n' "$baseline_commit" "$baseline_dir"
cd "$schema_dir"
buf breaking --against "$repo#format=git,ref=$baseline_commit,subdir=$baseline_dir"
