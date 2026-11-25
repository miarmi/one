#!/usr/bin/env bash
# Big or small release of an app (the owner's rule of 2026-11-16: the long checks
# only for "version 1, not 1.01"). Prints one word, then a line saying why:
#   big    SPEC.md differs from the work toolduction runs, or the app is not in
#          production yet, or we cannot tell: full acceptance tests
#   small  SPEC.md is the same as production's: no acceptance tests
# Production's commit comes from labd (GET /api/status, the commit each
# running production instance was deployed from), never from git history
# alone, so a cancelled or failed run on main cannot make a spec change look
# already released.
# Usage: release-kind.sh <app> [running.json]
#   running.json: {"apps": {"<app>": "<commit>"}, ...} as the pipeline's plan
#   writes it. Missing or empty: big.
set -euo pipefail

app="${1:?usage: release-kind.sh <app> [running.json]}"
running="${2:-}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

spec="apps/$app/SPEC.md"
if [[ -z "$running" || ! -s "$running" ]]; then
  printf 'big\nproduction status unavailable, so treated as a big release\n'
  exit 0
fi
commit="$(jq -r --arg a "$app" '(.apps // {})[$a] // ""' "$running" 2>/dev/null || true)"
if [[ -z "$commit" ]]; then
  printf 'big\nnot in production yet\n'
  exit 0
fi
if ! base="$(git rev-parse -q --verify "$commit^{commit}" 2>/dev/null)"; then
  printf 'big\nthe commit production runs (%s) is unknown here, so treated as a big release\n' "${commit::7}"
  exit 0
fi
if [[ ! -f "$spec" ]]; then
  printf 'big\nno SPEC.md\n'
  exit 0
fi
# Compare file contents, not history: a spec changed and changed back is small.
if ! prod_spec="$(git show "$base:$spec" 2>/dev/null)"; then
  printf 'big\nproduction (%s) has no SPEC.md for it\n' "${commit::7}"
  exit 0
fi
if [[ "$prod_spec" == "$(cat "$spec")" ]]; then
  printf 'small\nSPEC.md unchanged since production (%s)\n' "${commit::7}"
else
  printf 'big\nSPEC.md changed since production (%s)\n' "${commit::7}"
fi
