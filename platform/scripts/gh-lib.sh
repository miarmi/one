#!/usr/bin/env bash
# Small helpers for workflow steps. Source it: . platform/scripts/gh-lib.sh

# Escape a message for a GitHub workflow command (::notice:: etc.).
gh_escape() {
  local s="$1"
  s="${s//'%'/%25}"
  s="${s//$'\r'/%0D}"
  s="${s//$'\n'/%0A}"
  printf '%s' "$s"
}

# gh_annotate <notice|warning|error> <title> <message>
gh_annotate() {
  printf '::%s title=%s::%s\n' "$1" "$(gh_escape "$2")" "$(gh_escape "$3")"
}

# Append markdown to the job summary.
gh_summary() {
  printf '%s\n' "$@" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"
}
