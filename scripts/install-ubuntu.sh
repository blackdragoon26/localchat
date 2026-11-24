#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_SRC="${1:-$ROOT_DIR/dist/localchat-linux-amd64}"
BIN_DST="$HOME/.local/bin/localchat"

if [[ ! -f "$BIN_SRC" ]]; then
  echo "Binary not found at $BIN_SRC — run ./scripts/build.sh first." >&2
  exit 1
fi

mkdir -p "$HOME/.local/bin" "$HOME/.local/share/applications"
cp "$BIN_SRC" "$BIN_DST"
chmod +x "$BIN_DST"

DESKTOP_CONTENT="[Desktop Entry]
Type=Application
Name=LocalChat
Comment=Private Tailscale local chat bridge
Exec=$BIN_DST open
Icon=internet-chat
Terminal=false
Categories=Network;Chat;Utility;
"

APP_DESKTOP="$HOME/.local/share/applications/localchat.desktop"
printf '%s' "$DESKTOP_CONTENT" > "$APP_DESKTOP"
chmod +x "$APP_DESKTOP"

if [[ -d "$HOME/Desktop" ]]; then
  DESK_FILE="$HOME/Desktop/localchat.desktop"
  printf '%s' "$DESKTOP_CONTENT" > "$DESK_FILE"
  chmod +x "$DESK_FILE"
  gio set "$DESK_FILE" metadata::trusted true 2>/dev/null || true
fi

echo "Installed LocalChat binary to $BIN_DST"
echo "Installed launcher to $APP_DESKTOP (and ~/Desktop/localchat.desktop if present)"
