#!/usr/bin/env bash
set -euo pipefail

BIN_SRC="${1:-./localchat-darwin-arm64}"
BIN_DST="$HOME/.local/bin/localchat"
APP_DIR="$HOME/Applications/LocalChat.app"

if [[ ! -f "$BIN_SRC" ]]; then
  echo "Binary not found at $BIN_SRC" >&2
  exit 1
fi

mkdir -p "$HOME/.local/bin" "$HOME/Applications"
cp "$BIN_SRC" "$BIN_DST"
chmod +x "$BIN_DST"

# Clear macOS Gatekeeper quarantine if copied via browser/AirDrop, then ad-hoc sign for Apple Silicon.
xattr -d com.apple.quarantine "$BIN_DST" 2>/dev/null || true
codesign --force --sign - "$BIN_DST"

mkdir -p "$APP_DIR/Contents/MacOS"
cat > "$APP_DIR/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>
  <string>LocalChat</string>
  <key>CFBundleIdentifier</key>
  <string>local.chat.bridge</string>
  <key>CFBundleVersion</key>
  <string>0.1.0</string>
  <key>CFBundleExecutable</key>
  <string>localchat-launch</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>LSUIElement</key>
  <true/>
</dict>
</plist>
EOF

cat > "$APP_DIR/Contents/MacOS/localchat-launch" <<EOF
#!/usr/bin/env bash
export GOMEMLIMIT=64MiB
exec "$BIN_DST" open
EOF
chmod +x "$APP_DIR/Contents/MacOS/localchat-launch"

if [[ -d "$HOME/Desktop" && ! -e "$HOME/Desktop/LocalChat.app" ]]; then
  ln -s "$APP_DIR" "$HOME/Desktop/LocalChat.app"
fi

echo "Installed LocalChat binary to $BIN_DST"
echo "Installed macOS launcher at $APP_DIR (linked on Desktop)"
