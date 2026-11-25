#!/usr/bin/env bash
# Check (default) or update apps/<app>/acceptance.lock, the SHA-256 hashes of
# SPEC.md and every file under acceptance/. CI runs the check; only the
# test-writing agent runs --update, after the owner approved a spec change.
# Usage: platform/scripts/acceptance-lock.sh <app> [--update]
set -euo pipefail

name="${1:?usage: acceptance-lock.sh <app> [--update]}"
mode="${2:-check}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root/apps/$name"

compute() {
  {
    echo SPEC.md
    if [[ -d acceptance ]]; then find acceptance -type f | LC_ALL=C sort; fi
  } | while IFS= read -r f; do sha256sum "$f"; done
}

if [[ "$mode" == "--update" ]]; then
  compute > acceptance.lock
  echo "apps/$name/acceptance.lock updated"
  exit 0
fi

if [[ ! -f acceptance.lock ]]; then
  echo "apps/$name/acceptance.lock is missing" >&2
  exit 1
fi
if diff -u acceptance.lock <(compute); then
  echo "apps/$name: acceptance tests match the lock"
else
  echo "apps/$name: SPEC.md or acceptance tests changed without updating acceptance.lock" >&2
  exit 1
fi
