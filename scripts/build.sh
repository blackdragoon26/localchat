#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
mkdir -p "$DIST_DIR"

cd "$ROOT_DIR"

echo "Building linux/amd64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$DIST_DIR/localchat-linux-amd64" ./cmd/localchat

echo "Building darwin/arm64 (Apple Silicon)..."
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$DIST_DIR/localchat-darwin-arm64" ./cmd/localchat

(cd "$DIST_DIR" && sha256sum localchat-linux-amd64 localchat-darwin-arm64 > SHA256SUMS)
ls -lh "$DIST_DIR"
