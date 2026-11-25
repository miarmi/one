#!/usr/bin/env bash
# Tests for verdict.sh with a fake GitHub API (a fake `gh` and a fake `curl`
# that answer from files): found, wrong commit, wrong event, failed run, app
# missing from the verdict, API errors. Every doubt must give "no verdict".
# Usage: platform/scripts/verdict-test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/api" "$tmp/repo/platform/scripts"
cp "$here/verdict.sh" "$tmp/repo/platform/scripts/"

# The fake API: the path (with its query) selects a file in $FAKE_API; a
# missing file is an API error; "fail" in $FAKE_FAIL makes every call fail.
cat >"$tmp/bin/fake" <<'FAKE'
#!/usr/bin/env bash
path="${!#}"
path="${path#https://api.github.com/}"
[[ -z "${FAKE_FAIL:-}" ]] || exit 1
case "$path" in
  repos/o/r/actions/workflows/pipeline.yml/runs*) f=runs ;;
  repos/o/r/actions/runs/*/jobs*) f="jobs-$(sed -E 's#.*/runs/([0-9]+)/jobs.*#\1#' <<<"$path")" ;;
  repos/o/r/check-runs/*/annotations*) f="ann-$(sed -E 's#.*/check-runs/([0-9]+)/annotations.*#\1#' <<<"$path")" ;;
  *) exit 1 ;;
esac
[[ -f "$FAKE_API/$f.json" ]] || exit 1
cat "$FAKE_API/$f.json"
FAKE
chmod +x "$tmp/bin/fake"
ln -s fake "$tmp/bin/gh"
ln -s fake "$tmp/bin/curl"
export FAKE_API="$tmp/api" PATH="$tmp/bin:$PATH" GITHUB_REPOSITORY=o/r GH_TOKEN=t

cd "$tmp/repo"
git init -q -b main && git config user.email t@example.invalid && git config user.name test
git commit -q --allow-empty -m a && a="$(git rev-parse HEAD)"
git commit -q --allow-empty -m b && b="$(git rev-parse HEAD)"
git checkout -q -b side "$a" && git commit -q --allow-empty -m side && side="$(git rev-parse HEAD)"
git checkout -q main
other="0123456789abcdef0123456789abcdef01234567"

fail=0
# run <id> <sha> <event> <conclusion> <branch>: one entry of runs.json
runs() { jq -n --argjson r "$1" '{workflow_runs: $r}' >"$FAKE_API/runs.json"; }
r() { jq -cn --arg id "$1" --arg sha "$2" --arg ev "$3" --arg c "$4" --arg br "$5" \
  '{id: ($id | tonumber), head_sha: $sha, event: $ev, conclusion: $c, head_branch: $br,
    created_at: "2026-11-20T10:0\($id | .[-1:]):00Z", html_url: "https://example.invalid/run/\($id)"}'; }
# verdict <id> <job conclusion> <annotation title> <message>
verdict() {
  jq -n --arg c "$2" '{jobs: [{id: 1, name: "plan", conclusion: "success"}, {id: 9000, name: "result", conclusion: $c}]}' >"$FAKE_API/jobs-$1.json"
  jq -n --arg t "$3" --arg m "$4" '[{title: "Ready to ship", message: $m, annotation_level: "notice"}] | .[0].title = $t' >"$FAKE_API/ann-9000.json"
}
ready_big="big release: full tests for one. Every check passed; merging this branch into main deploys it. Tested apps: one, two. Big releases tested: one."

check() { # name, command output, jq filter, wanted
  local got
  got="$(jq -c "$3" <<<"$2")"
  if [[ "$got" == "$4" ]]; then echo "ok   $1: $3 = $got"; else echo "FAIL $1: $3 = $got, wanted $4 ($2)"; fail=1; fi
}
accepted() { platform/scripts/verdict.sh accepted "$1" "$2"; }

rm -f "$FAKE_API"/*
runs "[$(r 11 "$b" workflow_dispatch success feature)]"
verdict 11 success "Ready to ship" "$ready_big"
check "found" "$(accepted "$b" '["one"]')" .accepted '["one"]'
check "found: run is linked" "$(accepted "$b" '["one"]')" .run '"https://example.invalid/run/11"'
check "found, two big apps, only one covered" "$(accepted "$b" '["one","two"]')" .accepted '["one"]'
check "found through curl, without gh" "$(VERDICT_API=curl accepted "$b" '["one"]')" .accepted '["one"]'

check "app missing from the verdict" "$(accepted "$b" '["two"]')" .accepted '[]'
check "app is only tested, not a big release" "$(accepted "$b" '["two"]')" .note '"run 11 does not name these as big releases it tested"'
check "app named by a longer name" "$(accepted "$b" '["on"]')" .accepted '[]'
check "no big releases" "$(accepted "$b" '[]')" .accepted '[]'
check "wrong sha asked (the fake API ignores the query, the filter must reject)" "$(accepted "$other" '["one"]')" .accepted '[]'
runs "[$(r 11 "$a" workflow_dispatch success feature)]"
check "run is for another commit" "$(accepted "$b" '["one"]')" .accepted '[]'
runs "[$(r 11 "$b" push success feature)]"
check "wrong event (a push)" "$(accepted "$b" '["one"]')" .accepted '[]'
runs "[$(r 11 "$b" workflow_dispatch failure feature)]"
check "failed run" "$(accepted "$b" '["one"]')" .accepted '[]'
runs "[$(r 11 "$b" workflow_dispatch cancelled feature)]"
check "cancelled run" "$(accepted "$b" '["one"]')" .accepted '[]'
runs "[$(r 11 "$b" workflow_dispatch success main)]"
check "a run on main itself" "$(accepted "$b" '["one"]')" .accepted '[]'
runs '[]'
check "no run at all" "$(accepted "$b" '["one"]')" .accepted '[]'

runs "[$(r 11 "$b" workflow_dispatch success feature)]"
verdict 11 failure "Ready to ship" "$ready_big"
check "result job did not succeed" "$(accepted "$b" '["one"]')" .accepted '[]'
verdict 11 success "Not ready to ship" "$ready_big"
check "the notice is 'Not ready to ship'" "$(accepted "$b" '["one"]')" .accepted '[]'
verdict 11 success "Ready to ship" "small release of one: acceptance tests skipped, SPEC.md unchanged. Every check passed."
check "a small release verdict" "$(accepted "$b" '["one"]')" .accepted '[]'
verdict 11 success "Ready to ship" "Tested apps: one. Big releases tested: oneplus."
check "a similar app name" "$(accepted "$b" '["one"]')" .accepted '[]'
verdict 11 success "Ready to ship" "Big releases tested: one"
check "unterminated list" "$(accepted "$b" '["one"]')" .accepted '[]'
echo '[]' >"$FAKE_API/ann-9000.json"
check "no annotation" "$(accepted "$b" '["one"]')" .accepted '[]'
echo 'not json' >"$FAKE_API/ann-9000.json"
check "unreadable annotations" "$(accepted "$b" '["one"]')" .accepted '[]'
rm "$FAKE_API/ann-9000.json"
check "annotations API error" "$(accepted "$b" '["one"]')" .accepted '[]'
rm "$FAKE_API/jobs-11.json"
check "jobs API error" "$(accepted "$b" '["one"]')" .accepted '[]'
verdict 11 success "Ready to ship" "$ready_big"
check "API down" "$(FAKE_FAIL=1 accepted "$b" '["one"]')" .accepted '[]'
echo 'not json' >"$FAKE_API/runs.json"
check "unreadable run list" "$(accepted "$b" '["one"]')" .accepted '[]'
rm "$FAKE_API/runs.json"
check "run list API error" "$(accepted "$b" '["one"]')" .accepted '[]'
check "not a commit" "$(accepted "main" '["one"]')" .accepted '[]'
check "not a list" "$(accepted "$b" 'one')" .accepted '[]'
check "no repository" "$(GITHUB_REPOSITORY='' accepted "$b" '["one"]')" .accepted '[]'

# newest run wins; an older failed or empty run is skipped
runs "[$(r 11 "$b" workflow_dispatch success feature), $(r 12 "$b" workflow_dispatch success feature)]"
jq -n '{jobs: [{id: 9001, name: "result", conclusion: "success"}]}' >"$FAKE_API/jobs-12.json"
echo '[]' >"$FAKE_API/ann-9001.json"
verdict 11 success "Ready to ship" "$ready_big"
check "newest run has no verdict, an older one has" "$(accepted "$b" '["one"]')" .run '"https://example.invalid/run/11"'

echo "--- shared files: quick checks only, and the canary (the owner, 2026-11-22) ---"
rm -f "$FAKE_API"/*
runs "[$(r 31 "$b" workflow_dispatch success feature)]"
# only go.mod changed: every app got the quick checks only, no list at all
verdict 31 success "Ready to ship" "quick checks only: shared files changed (hello, one, two). Every check passed; merging this branch into main deploys it."
check "go.mod only: the verdict skips no matrix for a big release" "$(accepted "$b" '["one"]')" .accepted '[]'
check "go.mod only: no canary accepted either" "$(platform/scripts/verdict.sh accepted "$b" '["hello"]' "Tested apps")" .accepted '[]'
# one app's folder plus go.mod: that app tested and big, the others quick
verdict 31 success "Ready to ship" "big release: full tests for one; quick checks only for hello, two: shared files changed. Every check passed; merging this branch into main deploys it. Tested apps: one. Big releases tested: one."
check "one folder and go.mod: its big release is accepted" "$(accepted "$b" '["one"]')" .accepted '["one"]'
check "one folder and go.mod: a quick app is not accepted as a big release" "$(accepted "$b" '["one","two"]')" .accepted '["one"]'
check "one folder and go.mod: a quick app is not accepted as tested" "$(platform/scripts/verdict.sh accepted "$b" '["two"]' "Tested apps")" .accepted '[]'
# e2e/ changed, no app folder: hello's matrix ran as the canary
verdict 31 success "Ready to ship" "quick checks only: shared files changed (hello, one, two). Every check passed; merging this branch into main deploys it. Tested apps: hello."
check "canary: main accepts hello as browser-tested" "$(platform/scripts/verdict.sh accepted "$b" '["hello"]' "Tested apps")" .accepted '["hello"]'
check "canary: its run is linked" "$(platform/scripts/verdict.sh accepted "$b" '["hello"]' "Tested apps")" .run '"https://example.invalid/run/31"'
check "canary: that is not a big release, so a big release is not accepted" "$(accepted "$b" '["hello"]')" .accepted '[]'
check "canary: another app is not accepted as tested" "$(platform/scripts/verdict.sh accepted "$b" '["one"]' "Tested apps")" .accepted '[]'
check "canary: the note says why" "$(platform/scripts/verdict.sh accepted "$b" '["one"]' "Tested apps")" .note '"run 31 does not name these as apps it tested"'
check "canary: a run for another commit does not count" "$(platform/scripts/verdict.sh accepted "$other" '["hello"]' "Tested apps")" .accepted '[]'
check "canary: an unknown list label is refused" "$(platform/scripts/verdict.sh accepted "$b" '["hello"]' "Anything")" .accepted '[]'
verdict 31 success "Not ready to ship" "Tested apps: hello."
check "canary: a Not ready verdict does not count" "$(platform/scripts/verdict.sh accepted "$b" '["hello"]' "Tested apps")" .accepted '[]'
runs "[$(r 31 "$b" push success feature)]"
verdict 31 success "Ready to ship" "Tested apps: hello."
check "canary: a push run (no browser tests) does not count" "$(platform/scripts/verdict.sh accepted "$b" '["hello"]' "Tested apps")" .accepted '[]'

echo "--- earlier (short run) ---"
earlier() { platform/scripts/verdict.sh earlier feature "$1" "$2"; }
rm -f "$FAKE_API"/*
runs "[$(r 21 "$a" workflow_dispatch success feature)]"
verdict 21 success "Ready to ship" "$ready_big"
check "earlier: ancestor, tested" "$(earlier "$b" '["one"]')" ".one.sha == \"$a\"" true
check "earlier: tested but not big is enough" "$(earlier "$b" '["two"]')" ".two.run" '"https://example.invalid/run/21"'
check "earlier: app not tested" "$(earlier "$b" '["three"]')" . '{}'
check "earlier: sha is HEAD itself" "$(earlier "$a" '["one"]')" ".one.sha == \"$a\"" true
check "earlier: HEAD is on a side branch, the run is on its parent" "$(earlier "$side" '["one"]')" ".one.sha == \"$a\"" true  # a is the parent of side
runs "[$(r 21 "$b" workflow_dispatch success feature)]"
check "earlier: run is for a later commit" "$(earlier "$a" '["one"]')" . '{}'
runs "[$(r 21 "$side" workflow_dispatch success feature)]"
check "earlier: run is on a side commit" "$(earlier "$b" '["one"]')" . '{}'
runs "[$(r 21 "$a" workflow_dispatch failure feature)]"
check "earlier: failed run" "$(earlier "$b" '["one"]')" . '{}'
runs "[$(r 21 "$a" push success feature)]"
check "earlier: push run" "$(earlier "$b" '["one"]')" . '{}'
runs "[$(r 21 "$a" workflow_dispatch success other)]"
check "earlier: another branch" "$(earlier "$b" '["one"]')" . '{}'
runs "[$(r 21 "$a" workflow_dispatch success feature)]"
verdict 21 success "Not ready to ship" "$ready_big"
check "earlier: not ready" "$(earlier "$b" '["one"]')" . '{}'
check "earlier: API down" "$(FAKE_FAIL=1 earlier "$b" '["one"]')" . '{}'
rm "$FAKE_API/runs.json"
check "earlier: run list API error" "$(earlier "$b" '["one"]')" . '{}'

exit "$fail"
