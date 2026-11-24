# LocalChat

A single-binary, zero-dependency Go chat webpage for personal devices over Tailscale. No Syncthing, no Docker, and no 24/7 background daemon: clicking the desktop icon starts the local server on demand (`< 15 MB` RAM) and shuts it down 5 minutes after you close all chat tabs (0 MB idle RAM).

## Features

* **Offline independence:** Every device keeps its own full local history and attachments in `~/LocalChat/`.
* **Clear delivery states:** Items sent while a peer is offline or closed stay saved locally with an explicit **`Not sent`** badge and an offline banner, and sync automatically (or via **`Sync Now`**) once both devices open LocalChat together.
* **Code, links, files & media:** Fenced code blocks (` ```lang `) with one-click Copy, clickable URLs, clipboard image paste, drag-and-drop uploads (up to 250 MB streamed directly to disk), and inline image/audio/video playback with HTTP Range seeking.
* **Cross-device deletion:** Deleting a message removes its attachment blob and scrubs the message text from disk on every synced device.
* **Multi-device ready:** Add more Tailscale devices anytime via **`+ Add Device`** in the webpage or `~/LocalChat/config.json`.

## Build (Linux + Apple Silicon macOS)

```bash
chmod +x scripts/build.sh scripts/install-ubuntu.sh scripts/install-macos.sh
./scripts/build.sh
```

This creates:
* `dist/localchat-linux-amd64` (Ubuntu)
* `dist/localchat-darwin-arm64` (Apple Silicon Mac)

## Setup on Ubuntu

1. Install the binary and desktop icon:
   ```bash
   ./scripts/install-ubuntu.sh
   ```
2. Initialize your local config (this generates your shared `workspace_token`):
   ```bash
   ~/.local/bin/localchat init \
     --id ubuntu \
     --name Ubuntu \
     --peer mac=Mac@100.124.162.95:8788
   ```
   Copy the printed **Token** for your Mac setup.
3. Click the **LocalChat** desktop icon (or run `~/.local/bin/localchat open`).

## Setup on Apple Silicon Mac (`darwin/arm64`)

1. Copy `dist/localchat-darwin-arm64` and `scripts/install-macos.sh` to your Mac (for example via `tailscale file cp`).
2. Run the Mac installer:
   ```bash
   chmod +x install-macos.sh
   ./install-macos.sh ./localchat-darwin-arm64
   ```
3. Initialize your Mac with the **same token** printed on Ubuntu:
   ```bash
   ~/.local/bin/localchat init \
     --id mac \
     --name Mac \
     --token "<PASTE_TOKEN_FROM_UBUNTU>" \
     --peer ubuntu=Ubuntu@100.124.162.117:8788
   ```
4. Double-click **LocalChat** on your Desktop (or `~/Applications/LocalChat.app`).

## Adding a 3rd Device Later

1. Copy the matching binary to the new Tailscale device and run `localchat init --id <id> --name <Name> --token "<SAME_TOKEN>" --peer ubuntu=Ubuntu@100.124.162.117:8788 --peer mac=Mac@100.124.162.95:8788`.
2. On Ubuntu and Mac, click **`+ Add Device`** in the LocalChat webpage and enter the new device's ID, display name, and `<tailscale-ip>:8788`.

## Verify Locally

```bash
gofmt -l .
go vet ./...
go test -race -cover ./...
```
