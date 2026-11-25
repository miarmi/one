#!/usr/bin/env bash
# Decide what a push changed. Prints JSON:
#   {"apps": [...], "own": [...], "canary": "<app>"|"", "platform": true|false,
#    "tests_changed": [...]}
# "apps" is what to build and give the quick checks (vet, unit tests; on main
# also the deploy): the apps whose folder changed, or every app when shared
# files (go.mod, platform/appkit, e2e/, build-app.sh) changed (the owner,
# 2026-11-20).
# "own" is the subset whose own folder changed. Only those get the browser
# matrix (the owner, 2026-11-22): an app that is in "apps" only because a shared
# file changed gets the quick checks only.
# "canary" is the one exception that keeps a safety net: when the test harness
# (e2e/) or the code every app imports (platform/appkit/) changed and no app's
# own folder did, one app's matrix still runs: the smallest app, hello (or
# the first app, if hello is gone). Empty otherwise. A change of go.mod,
# go.sum or build-app.sh alone needs no canary.
# Usage: changes.sh <before-sha or empty> <branch> [force] [running.json]
#
# On main, running.json says what the server runs ({"labd": "<commit>",
# "apps": {"<app>": "<commit>"}, "held": ["<rolled-back app>"]}, from
# /api/status). Then whatever production lacks ships too, not only what this
# push changed, so a change whose run was cancelled or failed still reaches
# production with the next run (security review M3).
set -euo pipefail

before="${1-}"
branch="${2:?usage: changes.sh <before> <branch> [force] [running.json]}"
force="${3:-false}"
running="${4:-}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

platform_paths='^(platform/|lab\.json$|\.github/)'
shared_app_paths='^(go\.mod$|go\.sum$|platform/appkit/|e2e/|platform/scripts/build-app\.sh$)'
# Shared files that can change what every app does in a browser.
canary_paths='^(e2e/|platform/appkit/)'
canary_app=hello
# What the acceptance tests run on: changing it can weaken every app's tests.
harness_paths='^(e2e/|platform/scripts/(app-state|acceptance-lock|changes|build-app)\.sh$|\.github/workflows/)'

all_apps() {
  for f in apps/*/app.json; do
    [[ -f "$f" ]] && basename "$(dirname "$f")"
  done
}
# emit <apps> <own apps> <canary wanted: true|false> <platform> <apps whose tests changed>
# (apps newline-separated). The canary is chosen here, only when wanted and no
# app's own folder changed.
emit() {
  local canary=""
  if [[ "$3" == true && -z "$(tr -d '[:space:]' <<<"$2")" ]]; then
    if [[ -f "apps/$canary_app/app.json" ]]; then
      canary="$canary_app"
    else
      canary="$(all_apps | sed -n 1p)"
    fi
  fi
  jq -cn --arg apps "$1" --arg own "$2" --arg canary "$canary" --argjson platform "$4" --arg locks "${5-}" \
    '{apps: ($apps | split("\n") | map(select(length > 0)) | unique),
      own: ($own | split("\n") | map(select(length > 0)) | unique),
      canary: $canary, platform: $platform,
      tests_changed: ($locks | split("\n") | map(select(length > 0)) | unique)}'
}
# changed_since <commit-ish> <regex>: did anything matching change since then?
# An unknown commit counts as changed.
changed_since() {
  local base files
  if ! base="$(git rev-parse -q --verify "$1^{commit}" 2>/dev/null)"; then
    return 0
  fi
  files="$(git diff --name-only "$base" HEAD)"
  grep -Eq "$2" <<<"$files"
}

if [[ "$force" == true ]]; then
  emit "$(all_apps)" "$(all_apps)" false true
  exit 0
fi

base=""
if [[ -n "$before" && ! "$before" =~ ^0+$ ]] && git cat-file -e "$before^{commit}" 2>/dev/null; then
  base="$before"
elif [[ "$branch" != main ]] && git rev-parse -q --verify origin/main >/dev/null; then
  base="$(git merge-base origin/main HEAD)"
fi

# Apps whose approved tests changed in this push (SPEC.md, acceptance/ or the
# lock), and the test harness itself.
locks=""
if [[ -n "$base" ]]; then
  pushed="$(git diff --name-only "$base" HEAD)"
  locks="$(sed -nE 's#^apps/([^/]+)/(SPEC\.md|acceptance/.*|acceptance\.lock)$#\1#p' <<<"$pushed")"
  if grep -Eq "$harness_paths" <<<"$pushed"; then
    locks+=$'\n'"the test harness"
  fi
fi

# What to build and test. On main: what this push changed (against the
# previous push), plus below whatever production lacks. On a branch: everything
# the branch changes against main, every push, so the "Ready to ship" verdict
# always covers the whole branch — a run that only tested the last push would
# call a branch ready after an earlier push's run was cancelled or failed.
build_base="$base"
if [[ "$branch" != main ]] && git rev-parse -q --verify origin/main >/dev/null; then
  build_base="$(git merge-base origin/main HEAD)"
fi
pushed_platform=false
pushed_apps=""
pushed_own=""
canary_wanted=false
if [[ -z "$build_base" ]]; then
  pushed_platform=true # first push of main or unknown history: everything
  pushed_apps="$(all_apps)"
  pushed_own="$(all_apps)"
else
  changed="$(git diff --name-only "$build_base" HEAD)"
  if grep -Eq "$platform_paths" <<<"$changed"; then
    pushed_platform=true
  fi
  if grep -Eq "$canary_paths" <<<"$changed"; then
    canary_wanted=true
  fi
  for app in $(all_apps); do
    if grep -q "^apps/$app/" <<<"$changed"; then
      pushed_own+="$app"$'\n'
    fi
  done
  if grep -Eq "$shared_app_paths" <<<"$changed"; then
    pushed_apps="$(all_apps)"
  else
    pushed_apps="$pushed_own"
  fi
fi

# On main, also whatever production lacks. An app that was rolled back is
# held: only a change to it ships it again, so an unrelated push doesn't
# bring back the release that was just taken out.
if [[ "$branch" == main && -n "$running" && -s "$running" ]]; then
  platform="$pushed_platform"
  if changed_since "$(jq -r '.labd // ""' "$running")" "$platform_paths"; then
    platform=true
  fi
  apps="$pushed_apps"$'\n'
  own="$pushed_own"$'\n'
  # The canary also counts when the harness changed since the canary app's
  # production release (a push whose run was cancelled still gets its canary).
  if changed_since "$(jq -r --arg a "$canary_app" '.apps[$a] // ""' "$running")" "$canary_paths"; then
    canary_wanted=true
  fi
  for app in $(all_apps); do
    if jq -e --arg a "$app" '(.held // []) | index($a)' "$running" >/dev/null; then
      continue
    fi
    if changed_since "$(jq -r --arg a "$app" '.apps[$a] // ""' "$running")" "^apps/$app/|$shared_app_paths"; then
      apps+="$app"$'\n'
      # Own folder changed since production's commit (an app that is not in
      # production yet counts as changed).
      if changed_since "$(jq -r --arg a "$app" '.apps[$a] // ""' "$running")" "^apps/$app/"; then
        own+="$app"$'\n'
      fi
    fi
  done
  emit "$apps" "$own" "$canary_wanted" "$platform" "$locks"
  exit 0
fi
emit "$pushed_apps" "$pushed_own" "$canary_wanted" "$pushed_platform" "$locks"
