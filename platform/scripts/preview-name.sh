#!/usr/bin/env bash
# Print the preview name for a branch, exactly like labd's previewName
# (platform/CONTRACT.md): the branch name itself, when it is lowercase
# letters, digits and single hyphens, at most 30 characters, not prod or main.
# Any other branch gets no preview, so two branches never share one.
# Usage: preview-name.sh <branch>
set -euo pipefail

branch="${1-}"
if [[ ! "$branch" =~ ^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$ || "$branch" == *--* || "$branch" == prod || "$branch" == main ]]; then
  echo "branch '$branch' is not a preview name: use lowercase letters, digits and single hyphens, at most 30 characters" >&2
  exit 1
fi
printf '%s\n' "$branch"
