#!/usr/bin/env bash
# Tests for changes.sh in a throwaway git repository: what a push builds
# ("apps"), which of those get the browser matrix because their own folder
# changed ("own"), and the canary (hello) when only e2e/ or platform/appkit/
# changed (the owner, 2026-11-22).
# Usage: platform/scripts/changes-test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/platform/scripts" "$tmp/platform/appkit" "$tmp/e2e"
cp "$here/changes.sh" "$tmp/platform/scripts/"
cd "$tmp"
git init -q -b main
git config user.email t@example.invalid
git config user.name test
for app in hello one oneplus; do
  mkdir -p "apps/$app"
  echo "{\"name\": \"$app\"}" >"apps/$app/app.json"
  echo "1. works" >"apps/$app/SPEC.md"
done
echo x >e2e/lib.sh
echo m >go.mod
echo k >platform/appkit/kit.go
git add -A && git commit -qm base
git update-ref refs/remotes/origin/main HEAD

fail=0
all='["hello","one","oneplus"]'
expect() { # name, want (apps), then the changes.sh arguments
  local name="$1" want="$2" got apps
  shift 2
  got="$(platform/scripts/changes.sh "$@")"
  apps="$(jq -c .apps <<<"$got")"
  if [[ "$apps" == "$want" ]]; then echo "ok   $name: $apps"; else echo "FAIL $name: want $want, got $got"; fail=1; fi
}
# expect_own name, want own, want canary, then the changes.sh arguments
expect_own() {
  local name="$1" want_own="$2" want_canary="$3" got own canary
  shift 3
  got="$(platform/scripts/changes.sh "$@")"
  own="$(jq -c .own <<<"$got")"
  canary="$(jq -r .canary <<<"$got")"
  if [[ "$own" == "$want_own" && "$canary" == "$want_canary" ]]; then
    echo "ok   $name: own $own, canary '$canary'"
  else
    echo "FAIL $name: want own $want_own canary '$want_canary', got $got"; fail=1
  fi
}
branch() { git checkout -q -B "$1" origin/main; }
commit() { git add -A && git commit -qm change; }

branch idea
expect "nothing changed: none" '[]' "" idea
echo c >apps/one/style.css && commit
expect "only apps/one changed: one" '["one"]' "" idea
echo c >apps/hello/style.css && commit
expect "apps/one and apps/hello changed: both" '["hello","one"]' "" idea

branch deps; echo y >>go.mod && commit
expect "go.mod changed: all" "$all" "" deps
branch kit; echo y >>platform/appkit/kit.go && commit
expect "platform/appkit changed: all" "$all" "" kit
branch harness; echo y >>e2e/lib.sh && commit
expect "e2e/ changed: all" "$all" "" harness
branch docs; echo y >CLAUDE.md && mkdir -p platform/x && echo y >platform/x/f && commit
expect "no app and no shared file: none" '[]' "" docs

# 'one' must not drag in 'oneplus'
branch prefix; echo c >apps/oneplus/x.css && commit
expect "name prefix: only oneplus" '["oneplus"]' "" prefix

branch later; echo c >apps/hello/x.css && commit
before="$(git rev-parse HEAD)"
echo d >apps/one/x.css && commit
expect "a later push still covers the whole branch" '["hello","one"]' "$before" later
expect "force: everything" "$all" "" later true

echo "--- own folders and the canary (shared-file rule) ---"
branch deps; echo y >>go.mod && commit
expect_own "only go.mod: every app builds, none is own, no canary" '[]' "" "" deps
branch sums; echo y >go.sum && commit
expect_own "only go.sum: no own app, no canary" '[]' "" "" sums
branch bld; echo y >>platform/scripts/build-app.sh && commit
expect_own "only build-app.sh: no own app, no canary" '[]' "" "" bld
branch one-deps; echo c >apps/one/x.css; echo y >>go.mod && commit
expect "one folder and go.mod: every app is built" "$all" "" one-deps
expect_own "one folder and go.mod: only one is own" '["one"]' "" "" one-deps
branch kit-one; echo c >apps/one/x.css; echo y >>platform/appkit/kit.go && commit
expect_own "one folder and appkit: only one is own, no canary needed" '["one"]' "" "" kit-one
branch harness; echo y >>e2e/lib.sh && commit
expect "e2e/ only: every app is built" "$all" "" harness
expect_own "e2e/ only: no own app, hello is the canary" '[]' "hello" "" harness
branch kit; echo y >>platform/appkit/kit.go && commit
expect_own "appkit only: hello is the canary" '[]' "hello" "" kit
branch hello-e2e; echo c >apps/hello/x.css; echo y >>e2e/lib.sh && commit
expect_own "hello's own folder and e2e/: hello is own, no extra canary" '["hello"]' "" "" hello-e2e
branch docs; echo y >CLAUDE.md && commit
expect_own "no app and no shared file: nothing" '[]' "" "" docs
branch force; echo y >>go.mod && commit
expect_own "force: every app is own, no canary" '["hello","one","oneplus"]' "" "" force true
# without hello the first app is the canary
branch nohello; git rm -rq apps/hello; echo y >>e2e/lib.sh && commit
expect_own "hello gone: the first app is the canary" '[]' "one" "" nohello

echo "--- main after such a branch ---"
# Main compares with what production runs: apps and own come from there.
cd "$tmp"; git checkout -q main
prod="$(git rev-parse HEAD)"
running() { jq -cn --arg p "$prod" '{labd: $p, apps: {hello: $p, one: $p, oneplus: $p}}' >"$tmp/running.json"; }
running
git checkout -q -B shipped origin/main
echo y >>go.mod && git add -A && git commit -qm "go.mod only"
got="$(platform/scripts/changes.sh "$prod" main false "$tmp/running.json")"
if [[ "$(jq -c '[.apps, .own, .canary]' <<<"$got")" == "[$all,[],\"\"]" ]]; then echo "ok   main, only go.mod: every app built, none own, no canary"; else echo "FAIL main go.mod: $got"; fail=1; fi
echo y >>e2e/lib.sh && git add -A && git commit -qm "e2e"
got="$(platform/scripts/changes.sh "$prod" main false "$tmp/running.json")"
if [[ "$(jq -c '[.apps, .own, .canary]' <<<"$got")" == "[$all,[],\"hello\"]" ]]; then echo "ok   main, e2e/ and go.mod: every app built, none own, canary hello"; else echo "FAIL main e2e: $got"; fail=1; fi
echo c >apps/one/x.css && git add -A && git commit -qm "one"
got="$(platform/scripts/changes.sh "$prod" main false "$tmp/running.json")"
if [[ "$(jq -c '[.apps, .own, .canary]' <<<"$got")" == "[$all,[\"one\"],\"\"]" ]]; then echo "ok   main, one folder and shared files: only one is own"; else echo "FAIL main one: $got"; fail=1; fi
# production already runs the new commit for everything but oneplus (not running): it is own
head="$(git rev-parse HEAD)"
jq -cn --arg h "$head" '{labd: $h, apps: {hello: $h, one: $h}}' >"$tmp/running.json"
got="$(platform/scripts/changes.sh "$head" main false "$tmp/running.json")"
if [[ "$(jq -c '[.apps, .own, .canary]' <<<"$got")" == '[["oneplus"],["oneplus"],""]' ]]; then echo "ok   main: an app production does not run yet is own"; else echo "FAIL main not running: $got"; fail=1; fi

exit "$fail"
