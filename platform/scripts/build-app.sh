#!/usr/bin/env bash
# Build one app into dist/<name>.tar.gz: a static Linux binary plus app.json.
# Usage: platform/scripts/build-app.sh <app>
set -euo pipefail

name="${1:?usage: build-app.sh <app>}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
app_dir="$root/apps/$name"

if [[ ! "$name" =~ ^[a-z][a-z0-9]{1,19}$ ]]; then
  echo "invalid app name: $name" >&2
  exit 2
fi
if [[ ! -f "$app_dir/app.json" ]]; then
  echo "missing apps/$name/app.json" >&2
  exit 2
fi
if ! grep -Eq "\"name\"[[:space:]]*:[[:space:]]*\"$name\"" "$app_dir/app.json"; then
  echo "apps/$name/app.json must contain \"name\": \"$name\"" >&2
  exit 2
fi

out="$root/dist/$name"
rm -rf "$out" "$root/dist/$name.tar.gz"
mkdir -p "$out"

cd "$root"
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath \
  -tags 'netgo osusergo sqlite_omit_load_extension' \
  -ldflags '-s -w -linkmode external -extldflags "-static"' \
  -o "$out/app" "./apps/$name"
cp "$app_dir/app.json" "$out/app.json"
tar -C "$out" -czf "$root/dist/$name.tar.gz" app app.json
echo "$root/dist/$name.tar.gz"
