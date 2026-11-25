# shellcheck shell=bash
# Sourced by run-local.sh and screenshot.sh.
# start_app <app> builds the app, runs it on a free local port with an empty
# database and waits until it is healthy. It sets APP_URL and APP_LOG, and
# stops the app when the calling script exits.
# start_app <app> <n> starts n such instances (built once): APP_URL and
# APP_LOG belong to the first, APP_URLS and APP_LOGS list them all in order.

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LAB_ROOT="$(cd "$E2E_DIR/.." && pwd)"
APP_URL=""
APP_LOG=""
APP_URLS=()
APP_LOGS=()
_app_pids=()
_app_dirs=()

# Runs from the EXIT trap set in start_app.
# shellcheck disable=SC2317,SC2329
_stop_app() {
  local pid dir
  for pid in "${_app_pids[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  for dir in "${_app_dirs[@]}"; do
    rm -rf "$dir"
  done
}

start_app() {
  local app="$1" count="${2:-1}" port dir url log pid i
  "$LAB_ROOT/platform/scripts/build-app.sh" "$app" >/dev/null
  trap _stop_app EXIT
  for ((i = 0; i < count; i++)); do
    port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
    dir="$(mktemp -d)"
    mkdir "$dir/data"
    log="$dir/app.log"
    url="http://127.0.0.1:$port"
    _app_dirs+=("$dir")
    PORT="$port" DATA_DIR="$dir/data" APP_NAME="$app" APP_ENV="" APP_VERSION="" \
      BASE_URL="$url" "$LAB_ROOT/dist/$app/app" >"$log" 2>&1 &
    pid=$!
    _app_pids+=("$pid")
    APP_URLS+=("$url")
    APP_LOGS+=("$log")
  done
  # Used by the sourcing scripts.
  # shellcheck disable=SC2034
  APP_URL="${APP_URLS[0]}"
  # shellcheck disable=SC2034
  APP_LOG="${APP_LOGS[0]}"
  for ((i = 0; i < count; i++)); do
    _wait_healthy "$app" "${APP_URLS[$i]}" "${_app_pids[$i]}" "${APP_LOGS[$i]}" || return 1
  done
}

_wait_healthy() { # app url pid log
  local app="$1" url="$2" pid="$3" log="$4"
  for _ in $(seq 1 80); do
    if curl -fsS "$url/healthz" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "$app exited during startup:" >&2
      cat "$log" >&2
      return 1
    fi
    sleep 0.25
  done
  echo "$app did not become healthy in 20 s:" >&2
  cat "$log" >&2
  return 1
}

ensure_node_modules() {
  if [[ ! -d "$E2E_DIR/node_modules" ]]; then
    (cd "$E2E_DIR" && npm ci --no-audit --no-fund)
  fi
}
