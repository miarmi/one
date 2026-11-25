#!/usr/bin/env bash
# Tests for release-kind.sh in a throwaway git repository.
# Usage: platform/scripts/release-kind-test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/platform/scripts" "$tmp/apps/hello" "$tmp/apps/fresh"
cp "$here/release-kind.sh" "$tmp/platform/scripts/"
cd "$tmp"
git init -q
git config user.email t@example.invalid
git config user.name test
printf '1. says hello\n' >apps/hello/SPEC.md
printf '1. is new\n' >apps/fresh/SPEC.md
echo 'package main' >apps/hello/main.go
git add -A && git commit -qm one
prod="$(git rev-parse HEAD)"
jq -n --arg c "$prod" '{apps: {hello: $c}}' >running.json

fail=0
expect() { # name, want, app, running
  local got
  got="$(platform/scripts/release-kind.sh "$3" "$4")"
  if [[ "${got%%$'\n'*}" == "$2" ]]; then
    echo "ok   $1: $2 (${got#*$'\n'})"
  else
    echo "FAIL $1: want $2, got: $got"
    fail=1
  fi
}

echo 'package main // 1.01' >apps/hello/main.go
git commit -qam code
expect "code change only" small hello running.json
expect "not in production yet" big fresh running.json
expect "status unavailable" big hello ""
echo '{}' >empty.json
expect "nothing in production" big hello empty.json
jq -n '{apps: {hello: "0123456789abcdef0123456789abcdef01234567"}}' >unknown.json
expect "unknown production commit" big hello unknown.json

printf '1. says hello\n2. says goodbye\n' >apps/hello/SPEC.md
git commit -qam spec
expect "spec changed" big hello running.json

printf '1. says hello\n' >apps/hello/SPEC.md
git commit -qam back
expect "spec changed and changed back" small hello running.json

git rm -q apps/hello/SPEC.md && git commit -qm nospec
expect "spec removed" big hello running.json

exit "$fail"
