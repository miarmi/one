#!/usr/bin/env bash
# Tests for short-run.sh in a throwaway git repository: which changes cannot
# change an app's behaviour, including tricky file names.
# Usage: platform/scripts/short-run-test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/platform/scripts"
cp "$here/short-run.sh" "$tmp/platform/scripts/"
cd "$tmp"
git init -q -b main
git config user.email t@example.invalid
git config user.name test
mkdir -p apps/one/acceptance apps/one/static apps/oneplus
echo 'package main' >apps/one/main.go
echo '1. a' >apps/one/SPEC.md
echo '{}' >apps/one/app.json
echo a >apps/one/acceptance/one.spec.ts
echo a >apps/one/acceptance.lock
echo a >apps/oneplus/style.css
git add -A && git commit -qm base
x="$(git rev-parse HEAD)"

fail=0
# expect <name> <short|full> <files to write...>   (each on a fresh commit from x)
expect() {
  local name="$1" want="$2" f got
  shift 2
  git checkout -q -B t "$x"
  for f in "$@"; do
    mkdir -p "$(dirname "$f")"
    printf 'changed %s\n' "$RANDOM" >"$f"
  done
  git add -A && git commit -qm change --allow-empty
  got="$(platform/scripts/short-run.sh "$x" one)"
  if [[ "${got%%$'\n'*}" == "$want" ]]; then
    echo "ok   $name: $want (${got#*$'\n'})"
  else
    echo "FAIL $name: want $want, got: $got"
    fail=1
  fi
}

expect "nothing changed" short
expect "css" short apps/one/static/app.css
expect "image, font, readme" short apps/one/static/logo.png apps/one/static/f.woff2 apps/one/README.md
expect "new css next to go" short apps/one/new.css
expect "go code" full apps/one/main.go
expect "html template" full apps/one/index.html
expect "javascript" full apps/one/static/app.js
expect "SPEC.md" full apps/one/SPEC.md
expect "spec.md in other case" full apps/one/Spec.md
expect "SPEC.md in a subfolder" full apps/one/docs/SPEC.md
expect "app.json" full apps/one/app.json
expect "lock" full apps/one/acceptance.lock
expect "acceptance test" full apps/one/acceptance/one.spec.ts
expect "markdown inside acceptance/" full apps/one/acceptance/notes.md
expect "css inside a nested acceptance folder" full apps/one/static/acceptance/x.css
expect "go file named like a stylesheet" full apps/one/style.css.go
expect "stylesheet-looking name that is code" full apps/one/main.go.txt
expect "html named like a readme" full apps/one/README.md.html
expect "no extension" full apps/one/Makefile
expect "uppercase extension is not trusted" full apps/one/static/Style.CSS
expect "another app's css" full apps/oneplus/style.css
expect "harmless plus one risky file" full apps/one/a.css apps/one/b.go
expect "workflow" full .github/workflows/pipeline.yml
expect "go.mod" full go.mod
expect "e2e harness" full e2e/lib.sh
expect "docs outside the app" full CLAUDE.md
expect "a name with a newline and .go in it" full $'apps/one/a.css\nb.go'
expect "a name with spaces" short "apps/one/my style.css"

# verdict_is <name> <want>: the answer for the commit just made (against x)
verdict_is() {
  local got
  got="$(platform/scripts/short-run.sh "$x" one)"
  if [[ "${got%%$'\n'*}" == "$2" ]]; then echo "ok   $1: $2"; else echo "FAIL $1: want $2, got: $got"; fail=1; fi
}

# a deletion of code and a rename of code to css are not harmless
git checkout -q -B t "$x" && git rm -q apps/one/main.go && git commit -qm rm
verdict_is "deleting code" full
git checkout -q -B t "$x" && git mv apps/one/main.go apps/one/main.css && git commit -qm mv
verdict_is "renaming code to css" full

# a symlink named .css (even pointing at code)
git checkout -q -B t "$x" && ln -s main.go apps/one/link.css && git add -A && git commit -qm link
verdict_is "symlink named .css" full

# the earlier commit must be an ancestor and known
git checkout -q -B other "$x" && git commit -q --allow-empty -m side
side="$(git rev-parse HEAD)"
git checkout -q -B t "$x" && git commit -q --allow-empty -m mine
for c in "$side" 0123456789abcdef0123456789abcdef01234567; do
  got="$(platform/scripts/short-run.sh "$c" one)"
  if [[ "${got%%$'\n'*}" == full ]]; then echo "ok   not an ancestor or unknown: full (${got#*$'\n'})"; else echo "FAIL ancestor: $got"; fail=1; fi
done
got="$(platform/scripts/short-run.sh "$x" 'Bad Name')"
if [[ "${got%%$'\n'*}" == full ]]; then echo "ok   bad app name: full"; else echo "FAIL bad name: $got"; fail=1; fi

exit "$fail"
