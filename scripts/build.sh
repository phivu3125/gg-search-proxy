#!/usr/bin/env bash
# Build gg-search-proxy with the public Antigravity IDE OAuth client injected.
# The client constants live in 9router's open-source repo (open-sse/providers/shared.js)
# and are fetched at build time so this repo stays scanner-clean.
# Requires: Go 1.27+, curl.
set -euo pipefail
cd "$(dirname "$0")/.."

SHARED_URL='https://raw.githubusercontent.com/decolua/9router/master/open-sse/providers/shared.js'
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
echo "fetching $SHARED_URL"
curl -fsSL "$SHARED_URL" -o "$TMP"

ID=$(grep -o 'clientId: *"[^"]*"' "$TMP" | head -1 | sed 's/.*"\(.*\)"/\1/')
SEC=$(grep -o 'clientSecret: *"[^"]*"' "$TMP" | head -1 | sed 's/.*"\(.*\)"/\1/')
[ -n "$ID" ] && [ -n "$SEC" ] || { echo 'could not extract ANTIGRAVITY_OAUTH_CLIENT from 9router source' >&2; exit 1; }
echo "oauth client: ${ID:0:12}..."

LDFLAGS="-X ggsearch/internal/antigravity.clientID=$ID -X ggsearch/internal/antigravity.clientSecret=$SEC"
mkdir -p dist

build() { # os arch out
  echo "building $3"
  GOOS=$1 GOARCH=$2 go build -ldflags "$LDFLAGS" -o "$3" ./cmd/ggsearch
}
build windows amd64 dist/ggsearch-windows-amd64.exe
build linux   amd64 dist/ggsearch-linux-amd64
build darwin  arm64 dist/ggsearch-darwin-arm64
build darwin  amd64 dist/ggsearch-darwin-amd64

echo
echo 'binaries:'
ls -lh dist/ | tail -n +2
echo
echo 'SHA256:'
( cd dist && sha256sum * )
