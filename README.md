# LocalChat

A single-binary, zero-dependency Go web chat for personal devices connected over [Tailscale](https://tailscale.com/).

No Docker, no Syncthing, no external database, and no 24/7 background daemon. Click the desktop icon on any device to launch a local webpage (`~8.5 MB` RAM while open); close all chat tabs and the process shuts down automatically after 5 minutes (`0 MB` idle RAM).

## Features

* **Offline-independent devices:** Every machine stores its own full history and attachments in `~/LocalChat/`. Read, write, attach files, or clear history even when other devices or Tailscale are offline.
* **Explicit delivery queue:** Messages sent while a peer is offline or closed stay saved locally with a **`Not sent`** badge and header notice, then sync automatically (or via **`Sync Now`**) once both devices open LocalChat together.
* **Code, links, files & inline media:** Fenced code blocks (` ```lang `) with one-click copy, clickable URLs, clipboard image paste, drag-and-drop uploads up to 250 MB (streamed directly to disk), and inline image/audio/video playback with HTTP `Range` seeking.
* **Live storage tracker & cross-device Clear Chat:** Top bar shows exact active chat storage (identical across synced devices). Deleting a message or clicking **Clear Chat** deletes attachment blobs, scrubs message text from disk, propagates tombstones to peers, and resets storage to **`0 B`**.
* **Multi-device ready:** Add or remove Tailscale peers anytime via **`+ Add Device`** in the UI or `~/LocalChat/config.json`.
* **Minimalist UI:** Low-glare charcoal dark and warm-paper light themes with a 1-click **Light / Dark** toggle; color is reserved for live peer status and sending states.

## How It Works

* **Dual listeners:**
  * **UI (`127.0.0.1:8787`):** Serves the embedded HTML/CSS/JS app with `Host` and fail-closed `Origin`/`Sec-Fetch-Site` CSRF checks, strict CSP, and magic-byte MIME sandboxing (disguised HTML/SVG files are forced to download).
  * **Peer Sync (`<tailscale-ip>:8788`):** Bound strictly to your Tailscale IPv4 (`100.64.0.0/10`) behind a shared 256-bit Bearer token.
* **Storage:** Per-device monthly NDJSON logs (`~/LocalChat/messages/`), opaque ASCII attachment blobs (`~/LocalChat/attachments/YYYY/MM/`), and a local per-peer delivery ledger (`~/LocalChat/state/acked.json`).
* **Full design spec:** See [`docs/designs/localchat.md`](docs/designs/localchat.md).

## Requirements

* [Tailscale](https://tailscale.com/) connected on your devices (`tailscale ip -4` to check each device's `100.x.x.x` IP)
* Go `1.22+` (only needed to build from source)

## Quick Start

### 1. Build

```bash
git clone git@github.com:blackdragoon26/localchat.git
cd localchat
./scripts/build.sh
```

Produces stripped, pure-Go (`CGO_ENABLED=0`) binaries in `dist/`:
* `dist/localchat-linux-amd64` (~7.8 MB)
* `dist/localchat-darwin-arm64` (~7.4 MB, Apple Silicon)

### 2. Install & Initialize on First Device (Ubuntu / Linux)

```bash
./scripts/install-ubuntu.sh
~/.local/bin/localchat init \
  --id ubuntu \
  --name Ubuntu \
  --peer mac=Mac@<MAC_TAILSCALE_IP>:8788
```

Save the **Token** printed by `init`. Click the **LocalChat** desktop icon (or run `~/.local/bin/localchat open`).

### 3. Install & Initialize on Second Device (Apple Silicon macOS)

```bash
git clone --depth 1 git@github.com:blackdragoon26/localchat.git /tmp/localchat
/tmp/localchat/scripts/build.sh
/tmp/localchat/scripts/install-macos.sh /tmp/localchat/dist/localchat-darwin-arm64
rm -rf /tmp/localchat

~/.local/bin/localchat init \
  --id mac \
  --name Mac \
  --token "<TOKEN_FROM_FIRST_DEVICE>" \
  --peer ubuntu=Ubuntu@<LINUX_TAILSCALE_IP>:8788
```

Double-click **LocalChat** on your Desktop (or `~/Applications/LocalChat.app`).

### 4. Adding More Devices

1. Install the binary on the new Tailscale device and run `localchat init` with the **same** `--token` and existing `--peer` flags.
2. On existing devices, click **`+ Add Device`** in the LocalChat webpage and enter the new device's ID, display name, and `<NEW_TAILSCALE_IP>:8788`.

## Updating & Uninstalling

**Update on Linux (inside cloned repo):**
```bash
git pull && ./scripts/build.sh && (pkill -x localchat || true) && ./scripts/install-ubuntu.sh
```

**Update on macOS (without keeping a local clone):**
```bash
TMP=$(mktemp -d) && git clone --depth 1 git@github.com:blackdragoon26/localchat.git "$TMP" && "$TMP/scripts/build.sh" && (pkill -x localchat || true) && "$TMP/scripts/install-macos.sh" "$TMP/dist/localchat-darwin-arm64" && rm -rf "$TMP"
```

**Uninstall:**
* **Linux:** `(pkill -x localchat || true) && rm -rf ~/.local/bin/localchat ~/.local/share/applications/localchat.desktop ~/Desktop/localchat.desktop ~/LocalChat`
* **macOS:** `(pkill -x localchat || true) && rm -rf ~/.local/bin/localchat ~/Applications/LocalChat.app ~/Desktop/LocalChat.app ~/LocalChat`

## CLI Reference

| Command | Description |
| :--- | :--- |
| `localchat init` | Creates `~/LocalChat/config.json` (`0600`), auto-detects Tailscale IP, generates or accepts `--token`, and adds `--peer id=name@ip:port` entries. |
| `localchat open` | Starts `localchat serve` detached if not already running and opens the configured UI URL in your default browser. |
| `localchat serve` | Runs UI + Tailscale sync listeners directly (`--data`, `--idle-timeout`, `--no-idle-exit`). |
| `localchat version` | Prints version and OS/architecture. |

## Development & Tests

```bash
gofmt -l .
go vet ./...
go test -race -cover ./...
```
