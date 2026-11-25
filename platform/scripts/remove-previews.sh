#!/usr/bin/env bash
# Remove every preview instance with this name, whatever app it belongs to
# (including apps that only ever existed on that branch).
# Usage: remove-previews.sh <preview-name>
set -euo pipefail

preview="${1:?usage: remove-previews.sh <preview-name>}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
status="$("$here/lab-api.sh" GET /api/status)"
removed=()
for app in $(jq -r --arg env "$preview" '.instances[] | select(.env == $env) | .app' <<<"$status"); do
  LAB_API_ALLOW_404=1 "$here/lab-api.sh" DELETE "/api/instances?app=$app&env=$preview" >/dev/null
  removed+=("$app")
done
if ((${#removed[@]})); then
  echo "removed previews of: ${removed[*]}"
else
  echo "no previews named $preview"
fi
