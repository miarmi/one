#!/usr/bin/env bash
# Call labd's API from GitHub Actions with an OIDC token bound to exactly this
# request (method, URI and body), as platform/CONTRACT.md describes.
# Usage: lab-api.sh METHOD URI [BODY_FILE]
#   LAB_API_ALLOW_404=1   treat 404 as success (cleanup of previews)
#   LAB_API_PRINT_AUD=1   only print the audience (used by tests)
set -euo pipefail

method="${1:?usage: lab-api.sh METHOD URI [BODY_FILE]}"
uri="${2:?usage: lab-api.sh METHOD URI [BODY_FILE]}"
body="${3:-}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if [[ -n "$body" ]]; then
  body_sha="$(sha256sum "$body" | cut -d' ' -f1)"
else
  body_sha="$(printf '' | sha256sum | cut -d' ' -f1)"
fi
if [[ "$uri" == /api/platform* ]]; then
  aud="lab-platform:sha256:$body_sha"
else
  aud="lab-api:sha256:$(printf '%s %s\n%s' "$method" "$uri" "$body_sha" | sha256sum | cut -d' ' -f1)"
fi
if [[ "${LAB_API_PRINT_AUD:-}" == 1 ]]; then
  printf '%s\n' "$aud"
  exit 0
fi

server_ip="$(jq -r '.server_ip // ""' "$root/lab.json")"
domain="$(jq -r '.domain // ""' "$root/lab.json")"
if [[ -z "$server_ip" ]]; then
  echo "lab.json has no server_ip" >&2
  exit 2
fi
# In domain mode the lab answers on its domain itself (apps on names below
# it). Platform updates and settings always go by IP: they work for the seed
# and before any certificate exists.
if [[ -z "$domain" || "$uri" == /api/platform* || "$uri" == /api/settings* ]]; then
  base="http://$server_ip"
else
  base="https://$domain"
fi
if [[ -z "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" || -z "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]]; then
  echo "no OIDC token available: the job needs 'permissions: id-token: write'" >&2
  exit 2
fi

out="$(mktemp)"
trap 'rm -f "$out"' EXIT
aud_q="$(jq -rn --arg v "$aud" '$v|@uri')"
code=""
for attempt in 1 2 3 4 5 6; do
  # A fresh token for every attempt: labd accepts each token only once.
  token="$(curl -fsS -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
    "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$aud_q" | jq -r '.value')"
  # On stderr: stdout carries only the response body, so callers can capture it.
  echo "::add-mask::$token" >&2
  args=(-sS -X "$method" -o "$out" -w '%{http_code}' --connect-timeout 10 --max-time 600
    -H "Authorization: Bearer $token")
  if [[ -n "$body" ]]; then
    args+=(--data-binary "@$body" -H "Content-Type: application/octet-stream")
  fi
  set +e
  code="$(curl "${args[@]}" "$base$uri")"
  rc=$?
  set -e
  # Retry only when labd surely didn't act on the request: no connection,
  # nginx couldn't reach labd, or labd turned it away before reading it.
  # A timeout or a cut connection may come after labd already deployed or
  # rolled back, so only reads are retried then.
  retry=false
  case "$rc:$code" in
    6:* | 7:* | 0:502 | 0:503 | 0:429) retry=true ;;
    0:504 | 28:* | 52:* | 56:*) [[ "$method" == GET ]] && retry=true ;;
  esac
  if [[ "$retry" == true && "$attempt" -lt 6 ]]; then
    echo "attempt $attempt: no answer from labd (curl $rc, HTTP $code); retrying" >&2
    sleep $((attempt * 3))
    continue
  fi
  break
done
cat "$out"
echo
if [[ "$code" == 404 && "${LAB_API_ALLOW_404:-}" == 1 ]]; then
  exit 0
fi
if [[ ! "$code" =~ ^2 ]]; then
  echo "$method $uri failed with HTTP $code" >&2
  exit 1
fi
