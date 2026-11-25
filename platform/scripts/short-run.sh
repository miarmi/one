#!/usr/bin/env bash
# Can a full run on a branch be short for an app? (the owner, 2026-11-20.) It can
# when an earlier full run ended "Ready to ship" for the ancestor commit <base>
# and since then only files that cannot change behaviour changed. Prints
# "short" and the changed files, or "full" and why not.
#   Behaviour-free, all under apps/<app>/: *.css, images (png jpg jpeg gif webp
#   avif svg ico), fonts (woff woff2 ttf otf eot) and *.md except SPEC.md.
#   Never: anything under acceptance/, acceptance.lock, app.json, a SPEC.md in
#   any case, .go, .html, .js, symlinks, anything outside apps/<app>/ (the
#   workflow, e2e/, go.mod, other apps, ...).
# Usage: short-run.sh <base-commit> <app>
set -uo pipefail

base="${1:?usage: short-run.sh <base-commit> <app>}"
app="${2:?usage: short-run.sh <base-commit> <app>}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root" || exit 0

full() { printf 'full\n%s\n' "$1"; exit 0; }

[[ "$app" =~ ^[a-z][a-z0-9]{1,19}$ ]] || full "not an app name"
commit="$(git rev-parse -q --verify "$base^{commit}" 2>/dev/null)" || full "the earlier commit is unknown here"
git merge-base --is-ancestor "$commit" HEAD 2>/dev/null || full "the earlier commit is not an ancestor of this one"

# Is this changed file one that cannot change behaviour?
harmless() {
  local rel base lower
  [[ "$1" == "apps/$app/"* ]] || return 1
  rel="${1#"apps/$app/"}"
  case "/$rel" in */acceptance/*) return 1 ;; esac
  base="${rel##*/}"
  lower="${base,,}"
  [[ "$lower" == spec.md || "$rel" == acceptance.lock || "$rel" == app.json ]] && return 1
  case "$rel" in
    *.css | *.png | *.jpg | *.jpeg | *.gif | *.webp | *.avif | *.svg | *.ico) return 0 ;;
    *.woff | *.woff2 | *.ttf | *.otf | *.eot) return 0 ;;
    *.md) return 0 ;;
  esac
  return 1
}

files=()
# --raw -z: exact names (any character), modes (symlinks), no rename guessing:
# a rename is a deletion plus an addition and both must be harmless.
while IFS= read -r -d '' meta && IFS= read -r -d '' path; do
  read -r oldmode newmode _ <<<"$meta"
  for mode in "${oldmode#:}" "$newmode"; do
    [[ "$mode" == 120000 || "$mode" == 160000 ]] && full "$path is a link or a submodule"
  done
  harmless "$path" || full "$path can change behaviour"
  files+=("$path")
done < <(git diff --raw -z --no-renames --no-abbrev "$commit" HEAD) || full "the difference could not be read"

printf 'short\n'
((${#files[@]} == 0)) || printf '%s\n' "${files[@]}"
