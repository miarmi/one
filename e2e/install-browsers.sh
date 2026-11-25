#!/usr/bin/env bash
# Install Playwright's browsers and their system libraries on a CI runner.
# Usage: e2e/install-browsers.sh chromium firefox webkit
#
# The libraries come from Ubuntu's archive through apt. GitHub's runners use
# an Azure mirror that now and then crawls at a few kB/s (seen 2026-11-06:
# over 40 minutes for the WebKit libraries, on three runs in a row), and a
# job that waits for it burns Actions minutes for nothing. So the install
# gets a time limit; when it runs out, the mirror is swapped for the main
# archive and the install runs again.
set -euo pipefail

if (($# == 0)); then
  echo "usage: install-browsers.sh <browser>..." >&2
  exit 2
fi
cd "$(dirname "${BASH_SOURCE[0]}")"

# On the lab's own test server the jobs have no root; its setup installed the
# system libraries already (platform/runner/install-browser-libs).
if [[ "${LAB_RUNNER:-}" == 1 ]]; then
  exec npx playwright install "$@"
fi
if timeout --kill-after=15s 6m npx playwright install --with-deps "$@"; then
  exit 0
fi
echo "::warning::Installing the browsers took over 6 minutes or failed; retrying with the main Ubuntu archive"
if [[ "$(id -u)" != 0 ]]; then
  sudo="sudo"
else
  sudo=""
fi
# The interrupted apt may still hold its lock or a half-configured package.
$sudo pkill -f 'apt-get|dpkg' 2>/dev/null || true
sleep 2
$sudo dpkg --configure -a || true
for f in /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources; do
  if [[ -f "$f" ]]; then
    $sudo sed -i 's|http://azure\.archive\.ubuntu\.com/|http://archive.ubuntu.com/|g' "$f"
  fi
done
npx playwright install --with-deps "$@"
