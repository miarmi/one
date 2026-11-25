#!/usr/bin/env bash
# Find an earlier "Ready to ship" verdict of this workflow through the GitHub
# API (the owner, 2026-11-20). Anything missing, wrong or unclear counts as no
# verdict: the caller then runs the whole matrix. Always exits 0.
#
# The verdict is the notice "Ready to ship" that the `result` job of a full run
# writes; its message ends with "Tested apps: a, b." and "Big releases tested:
# a." (only the apps that really had browser tests). Only runs of
# pipeline.yml in this repository count, that concluded success, and whose
# result job succeeded. On main the run must be on exactly the commit being
# shipped, so the workflow that produced the verdict is the very file main
# runs now.
#
#   verdict.sh accepted <sha> <big-apps-json> [label]
#     A full run on a branch (event workflow_dispatch) for exactly <sha> that
#     tested some of those big releases (the apps after "Big releases tested:").
#     With label "Tested apps" it asks instead whether the apps were
#     browser-tested at all: that is how main accepts the canary (hello, whose
#     matrix runs when e2e/ or platform/appkit/ changed). Apps that got the
#     quick checks only are in neither list, so main never skips a matrix the
#     branch did not run. Prints
#     {"accepted": [apps], "run": "<url>", "note": "<why not>"}
#   verdict.sh earlier <branch> <head-sha> <apps-json>
#     For each app the newest earlier full run on <branch> for an ancestor of
#     <head-sha> that tested it and ended "Ready to ship". Prints
#     {"<app>": {"sha": "<commit>", "run": "<url>"}, ...} (missing: none found)
#
# Needs GITHUB_REPOSITORY and a token in GH_TOKEN (the job's own, with
# actions: read and checks: read). Uses `gh api`; without gh, curl.
set -uo pipefail

repo="${GITHUB_REPOSITORY:-}"

api() { # api <path-with-query>: the answer's JSON, or failure
  if [[ "${VERDICT_API:-}" != curl ]] && command -v gh >/dev/null 2>&1; then
    gh api "$1"
  else
    curl -fsS -m 30 -H "Authorization: Bearer ${GH_TOKEN:-${GITHUB_TOKEN:-}}" \
      -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" \
      "https://api.github.com/$1"
  fi
}

# The "Ready to ship" message of a run's result job; empty if there is none.
# Fails when the API does not answer.
ready_message() { # <run id>
  local jobs jid ann
  jobs="$(api "repos/$repo/actions/runs/$1/jobs?per_page=100")" || return 1
  jid="$(jq -r '[.jobs[]? | select(.name == "result" and .conclusion == "success")][0].id // empty' <<<"$jobs" 2>/dev/null)" || return 1
  [[ "$jid" =~ ^[0-9]+$ ]] || return 0
  ann="$(api "repos/$repo/check-runs/$jid/annotations?per_page=100")" || return 1
  jq -r '[.[]? | select(.title == "Ready to ship")][0].message // empty' <<<"$ann" 2>/dev/null
}

# apps_in <message> <label>: a JSON list of the apps after "<label>: ".
apps_in() {
  local list
  list="$(grep -oE "$2: [a-z0-9, ]+\\." <<<"$1" | head -n 1)"
  list="${list#"$2: "}"
  list="${list%.}"
  tr ',' '\n' <<<"$list" | sed -E 's/^ +//; s/ +$//' |
    grep -E '^[a-z][a-z0-9]{0,19}$' | jq -R . | jq -sc .
}

none() { jq -cn --arg note "$1" '{accepted: [], run: "", note: $note}'; }

accepted() {
  local sha="$1" big="$2" label="${3:-Big releases tested}" runs ids id url msg covered hit note="no full run on a branch for this commit"
  [[ "$label" == "Big releases tested" || "$label" == "Tested apps" ]] || { none "unknown list"; return; }
  [[ -n "$repo" ]] || { none "no repository given"; return; }
  [[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { none "no commit given"; return; }
  jq -e 'type == "array"' <<<"$big" >/dev/null 2>&1 || { none "no list of big releases"; return; }
  runs="$(api "repos/$repo/actions/workflows/pipeline.yml/runs?head_sha=$sha&event=workflow_dispatch&per_page=50")" ||
    { none "the GitHub API did not answer"; return; }
  ids="$(jq -r --arg sha "$sha" '[.workflow_runs[]?
      | select(.head_sha == $sha and .event == "workflow_dispatch" and .conclusion == "success" and .head_branch != "main")]
      | sort_by(.created_at) | reverse | .[] | "\(.id) \(.html_url)"' <<<"$runs" 2>/dev/null)" ||
    { none "the GitHub API answer could not be read"; return; }
  while read -r id url; do
    [[ "$id" =~ ^[0-9]+$ ]] || continue
    if ! msg="$(ready_message "$id")"; then
      note="the GitHub API did not answer for run $id"
      continue
    fi
    if [[ -z "$msg" ]]; then
      note="run $id has no \"Ready to ship\" verdict"
      continue
    fi
    covered="$(apps_in "$msg" "$label")"
    hit="$(jq -c --argjson b "$big" '. as $c | $b | map(select(. as $a | $c | index($a)))' <<<"${covered:-[]}" 2>/dev/null || echo '[]')"
    if [[ -n "$hit" && "$hit" != "[]" ]]; then
      jq -cn --argjson a "$hit" --arg run "$url" '{accepted: $a, run: $run, note: ""}'
      return
    fi
    if [[ "$label" == "Tested apps" ]]; then
      note="run $id does not name these as apps it tested"
    else
      note="run $id does not name these as big releases it tested"
    fi
  done <<<"$ids"
  none "$note"
}

earlier() {
  local branch="$1" head="$2" apps="$3" runs ids id url sha msg tested app found="{}"
  [[ -n "$repo" ]] || { echo '{}'; return; }
  jq -e 'type == "array"' <<<"$apps" >/dev/null 2>&1 || { echo '{}'; return; }
  runs="$(api "repos/$repo/actions/workflows/pipeline.yml/runs?branch=$branch&event=workflow_dispatch&per_page=50")" ||
    { echo '{}'; return; }
  ids="$(jq -r --arg b "$branch" '[.workflow_runs[]?
      | select(.head_branch == $b and .event == "workflow_dispatch" and .conclusion == "success")]
      | sort_by(.created_at) | reverse | .[] | "\(.id) \(.head_sha) \(.html_url)"' <<<"$runs" 2>/dev/null)" ||
    { echo '{}'; return; }
  while read -r id sha url; do
    [[ "$id" =~ ^[0-9]+$ && "$sha" =~ ^[0-9a-f]{40}$ ]] || continue
    git merge-base --is-ancestor "$sha" "$head" 2>/dev/null || continue
    msg="$(ready_message "$id")" || continue
    [[ -n "$msg" ]] || continue
    tested="$(apps_in "$msg" "Tested apps")"
    for app in $(jq -r '.[]' <<<"$apps"); do
      jq -e --arg a "$app" 'has($a)' <<<"$found" >/dev/null && continue
      jq -e --arg a "$app" 'index($a)' <<<"${tested:-[]}" >/dev/null 2>&1 || continue
      found="$(jq -c --arg a "$app" --arg s "$sha" --arg u "$url" '. + {($a): {sha: $s, run: $u}}' <<<"$found")"
    done
    jq -en --argjson a "$apps" --argjson f "$found" '($a - ($f | keys)) | length == 0' >/dev/null && break
  done <<<"$ids"
  echo "$found"
}

case "${1:-}" in
  accepted) accepted "${2:-}" "${3:-[]}" "${4:-}" ;;
  earlier) earlier "${2:-}" "${3:-}" "${4:-[]}" ;;
  *) echo "usage: verdict.sh accepted <sha> <apps-json> [label] | earlier <branch> <head-sha> <apps-json>" >&2 ;;
esac
exit 0
