#!/usr/bin/env bash
# Build an app, run it locally with an empty database, and run its browser
# acceptance tests against it: one app instance per browser project, so the
# projects run at the same time without sharing state (see playwright.config.ts).
# Usage: e2e/run-local.sh <app> [extra playwright args...]
# E2E_PROJECTS="chromium-desktop webkit-phone" runs only those projects (a
# short run); without it every project in projects.json runs.
set -euo pipefail

app="${1:?usage: e2e/run-local.sh <app> [playwright args...]}"
shift
# shellcheck source=e2e/lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

mapfile -t projects < <(jq -r 'keys_unsorted[]' "$E2E_DIR/projects.json")
if [[ -n "${E2E_PROJECTS:-}" ]]; then
  read -r -a wanted <<<"$E2E_PROJECTS"
  for p in "${wanted[@]}"; do
    if ! printf '%s\n' "${projects[@]}" | grep -qxF -- "$p"; then
      echo "E2E_PROJECTS names an unknown project: $p" >&2
      exit 2
    fi
  done
  projects=("${wanted[@]}")
fi
start_app "$app" "${#projects[@]}"
ensure_node_modules
urls="{}"
for i in "${!projects[@]}"; do
  urls="$(jq -c --arg p "${projects[$i]}" --arg u "${APP_URLS[$i]}" '. + {($p): $u}' <<<"$urls")"
done
status=0
(cd "$E2E_DIR" && APP="$app" BASE_URL="$APP_URL" LAB_PROJECT_URLS="$urls" npx playwright test "$@") || status=$?
if [[ $status -ne 0 ]]; then
  for i in "${!projects[@]}"; do
    echo "--- $app log (${projects[$i]}) ---" >&2
    tail -n 100 "${APP_LOGS[$i]}" >&2
  done
fi
exit "$status"
