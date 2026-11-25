#!/usr/bin/env bash
# Where an app stands. Prints one word, then a line saying why:
#   nospec    no SPEC.md yet: the pipeline builds nothing for it
#   untested  SPEC.md, but no acceptance tests or no lock yet (a prototype)
#   outdated  the spec or tests changed and the lock doesn't match (tests pending)
#   ready     SPEC.md, acceptance tests and a matching lock: may ship
# Usage: app-state.sh <app>
set -euo pipefail

app="${1:?usage: app-state.sh <app>}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="$root/apps/$app"

if [[ ! -f "$dir/SPEC.md" ]]; then
  printf 'nospec\nno SPEC.md yet\n'
  exit 0
fi
missing=()
compgen -G "$dir/acceptance/*.spec.ts" >/dev/null || missing+=("no acceptance tests")
[[ -f "$dir/acceptance.lock" ]] || missing+=("no acceptance.lock")
if ((${#missing[@]})); then
  printf 'untested\n%s yet\n' "$(IFS=','; printf '%s' "${missing[*]}" | sed 's/,/ and /g')"
  exit 0
fi
if "$root/platform/scripts/acceptance-lock.sh" "$app" >/dev/null 2>&1; then
  printf 'ready\nSPEC.md, acceptance tests and lock match\n'
else
  printf 'outdated\nits acceptance tests do not match its SPEC.md yet\n'
fi
