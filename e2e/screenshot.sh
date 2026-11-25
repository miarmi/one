#!/usr/bin/env bash
# Run an app locally and take phone and desktop screenshots of some of its
# pages, to show a prototype before it is even deployed.
# Usage: [PREPARE=<script>] e2e/screenshot.sh <app> [path ...]   (default path: /)
# Pictures land in dist/screenshots/<app>/<page>-phone.png and -desktop.png.
set -euo pipefail

app="${1:?usage: e2e/screenshot.sh <app> [path ...]}"
shift
# shellcheck source=e2e/lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

start_app "$app"
ensure_node_modules
# Optional: a script that fills the app with demo data first, given its URL.
if [[ -n "${PREPARE:-}" ]]; then
  "$PREPARE" "$APP_URL"
fi
out="$LAB_ROOT/dist/screenshots/$app"
rm -rf "$out"
mkdir -p "$out"
if (($# == 0)); then
  set -- /
fi
node "$E2E_DIR/screenshot.js" "$APP_URL" "$out" "$@"
