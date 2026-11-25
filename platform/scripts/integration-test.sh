#!/usr/bin/env bash
# Run labd for real on this machine, the way the server runs it (systemd,
# nginx, root), and check the app sandbox, the access gate, preview data
# copies, host hardening and platform updates (platform/labd/integration_test.go).
# It changes the machine's system settings: only for a throwaway machine such
# as the pipeline's runner. Usage, as root: platform/scripts/integration-test.sh
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
# shellcheck source=platform/scripts/gh-lib.sh
. platform/scripts/gh-lib.sh
if [[ "$(id -u)" != 0 || ! -d /run/systemd/system ]]; then
  echo "integration-test.sh needs root on a machine that runs systemd" >&2
  exit 2
fi
for tool in nginx sqlite3 nft systemd-run python3 go; do
  command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 2; }
done

# The job log is hard to reach from outside GitHub, so what went wrong is
# repeated as annotations (at most 10, a few thousand characters each).
annotate_failure() { # $1 = title, $2 = file with the output
  [[ -n "${GITHUB_ACTIONS:-}" ]] || return 0
  local summary tail n=0
  summary="$(grep -E -- '--- FAIL|_test\.go:[0-9]+:|^FAIL|panic:|error' "$2" | head -c 5500 || true)"
  gh_annotate error "$1: summary" "${summary:-no test output}"
  tail="$(tail -c 36000 "$2")"
  while [[ -n "$tail" && $n -lt 6 ]]; do
    n=$((n + 1))
    gh_annotate error "$1: output part $n" "${tail:0:6000}"
    tail="${tail:6000}"
  done
}

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

# Every app on this branch, built the way the pipeline builds them: each is
# deployed and has to come up in the hardened sandbox.
for f in apps/*/app.json; do
  app="$(basename "$(dirname "$f")")"
  if ! platform/scripts/build-app.sh "$app" >"$out" 2>&1; then
    cat "$out"
    annotate_failure "Building $app" "$out"
    exit 1
  fi
done

cd platform/labd
set +e
LAB_INTEGRATION=1 go test -tags integration -count=1 -v -timeout 20m -run '^TestIntegration$' . 2>&1 | tee "$out"
rc=${PIPESTATUS[0]}
set -e
if [[ $rc -ne 0 ]]; then
  annotate_failure "Integration test" "$out"
elif [[ -n "${GITHUB_ACTIONS:-}" ]]; then
  gh_annotate notice "Integration test passed" "$(grep -E -- '--- PASS|sandbox( ok)?: ' "$out" | sed -E 's/^ +//' | head -c 6000)"
fi
exit "$rc"
