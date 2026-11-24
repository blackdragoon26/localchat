# Tailscale On-Demand Local Chat Bridge (Standalone Go)

* **Author:** blackdragoon26
* **Date:** 2026-11-24

## Overview

This tool is a single, zero-dependency Go binary installed on two Tailscale-connected laptops—an Ubuntu 24.04 x86_64 laptop (`motherfunder-vivobook-asuslaptop-x515da`, 5.7 GB RAM) and a macOS MacBook Air (`sankalps-macbook-air-1`).

There is no Syncthing, no Docker, and no 24/7 background daemon. Clicking a desktop icon on either laptop starts the local Go server on demand (~5–10 MB RAM while open) and opens a local chat webpage (`http://127.0.0.1:<port>`). Roughly 5 minutes after the chat tab is closed, the Go process exits cleanly so idle RAM and CPU return to zero.

Each laptop keeps its own independent local history and attachments on disk. When you send text, fenced code blocks, links, files, or media, the item is saved locally first and pushed immediately over Tailscale to the other laptop. If the other laptop is off, asleep, or its chat window is closed, the webpage notifies you right away and marks that message **`Not sent (saved locally)`** in your feed so you never forget it—and automatically delivers all queued items (or lets you click **Sync Now**) the next time both laptops have the chat open together.

## Analysis

Seven independent review lenses audited the original two-daemon Syncthing proposal and surfaced key risks around sync-order races, RAM spikes, OS daemon friction, and invisible failure states. Pivoting to a **single on-demand Go binary with direct Tailscale HTTP sync** resolves the majority of those risks directly:

### 1. Eliminating Two-Daemon & Filesystem-IPC Friction (Zoom-Out & Inventive Catalyst)
Dropping Syncthing and keeping the Go binary on demand eliminates:
* Syncthing's 25–100+ MB RAM footprint and large-file hashing spikes on the 5.7 GB Ubuntu laptop.
* Cross-OS background service maintenance (`systemd --user` linger rules on Ubuntu and `launchd` plists on macOS).
* Syncthing temporary-file races (`.syncthing.*.tmp`) and OS file-watcher complexity (`fsnotify` / `inotify` / `kqueue`).

**Trade-off accepted by design:** Because the Go process shuts down 5 minutes after closing the tab, both laptops must have the chat open within the same window to exchange queued items. To prevent silent drops or confusion, delivery state is made first-class in the UI.

### 2. Explicit Local Outbox & Delivery State (Comprehension & Design)
In an on-demand peer-to-peer tool, the user must always know whether an item actually crossed over to the other laptop.
* **Write-local-first:** Every outgoing message and attachment is saved atomically to the sender's local storage before attempting network delivery.
* **Immediate peer check & intimation:** On send (and on page open / periodic 3–5s visibility-aware poll), the local server contacts the peer over Tailscale (`http://<peer-tailscale-ip>:<sync-port>`). If unreachable, the header shows **`Other laptop offline — saved locally`** and the message renders with a visible **`Not sent`** badge and a **`Retry / Sync Now`** action.
* **Automatic catch-up:** When both laptops are open together, they exchange lightweight message manifests (`GET /sync/manifest`) and pull any missing messages, attachments, and delete tombstones automatically—flipping every delivered item from **`Not sent`** to **`Sent ✓`**.
* **Attachment-before-message integrity:** During sync, a receiver downloads and verifies an item's attachment (`sha256` + byte size) into a local temp file and renames it into place *before* marking the message complete (showing `"Syncing attachment…"` while transferring so the UI never displays broken media previews or 404s).

### 3. Cross-OS Storage Safety, Schema Versioning, and Cleanup (Implementation Readiness & Structural)
* **Opaque ASCII attachment paths:** Attachments are stored on disk under `attachments/YYYY/MM/<ulid>.<safe-ext>` (avoiding Linux `ext4` vs. macOS `APFS` case-sensitivity, Unicode NFD/NFC, and path-traversal bugs). Original display filename, MIME type, byte size, and SHA-256 checksum live in the message JSON, and downloads use clean `Content-Disposition` filenames.
* **Per-device monthly append-only logs (or atomic per-message JSON):** Each laptop writes versioned JSON records (`"schema_version": 1`, UTC millisecond timestamp, device label, ULID tie-breaker, attachment metadata, and local sync status) with permissive JSON decoding so both laptops can upgrade independently.
* **Conflict-safe deletion & size cap:** Deleting a message or large video writes a small delete tombstone record and removes the local attachment blob; when peers next sync, the peer applies the tombstone and deletes its copy too. Uploads stream directly from `r.MultipartReader()` to disk (never buffering in RAM or `/tmp` tmpfs) with a default 250 MB per-file cap.

### 4. Security, Network Separation, and Browser Ergonomics (Codebase Details & Comprehension)
* **Strict listener separation:** The interactive chat UI and mutating user API bind strictly to `127.0.0.1:<ui-port>` (protected with `Host` and `Origin`/`Sec-Fetch-Site` checks to prevent drive-by browser CSRF/DNS rebinding). The peer-to-peer sync API binds **only** to the laptop's Tailscale IP (`100.x.x.x:<sync-port>`) and requires a shared pre-shared token configured once on both laptops, so nothing is exposed to public Wi-Fi or local LAN.
* **Zero external web assets & safe rendering:** All HTML/CSS/JS is bundled inside the Go binary via `//go:embed` (zero CDN calls, works fully offline). Rendering parses only fenced ` ``` ` code blocks (with one-click Copy + `"Copied!"` feedback) and `https?://` auto-links using safe DOM text nodes—blocking XSS from pasted snippets. Inline media uses Go's `http.ServeContent` (for HTTP `Range` seeking in Safari/Chrome) with an explicit inline MIME allowlist (forcing download + `X-Content-Type-Options: nosniff` on `.svg`, `.html`, and unknown files).
* **Composer & launcher ergonomics:** Support clipboard image paste, drag-and-drop file upload, `loading="lazy"` / `preload="metadata"` with pagination (latest 50–100 messages), and a desktop launcher on each OS (`.desktop` on Ubuntu, `.app` bundle or launcher script on macOS) that starts the binary if not running and opens `http://127.0.0.1:<ui-port>` without spawning duplicate server instances.
* **macOS cross-compile readiness:** Build with `CGO_ENABLED=0` straight from Ubuntu (`linux/amd64` + `darwin/<mac-arch>`), store data outside macOS TCC-protected folders (e.g., `~/LocalChat`, not `~/Documents`), and document one-time quarantine removal + ad-hoc signing (`xattr -d com.apple.quarantine` and `codesign --force --sign -`) on macOS.

### Ideal Final Result (IFR)
A single ~8 MB native Go binary on Ubuntu and macOS with zero background RAM when closed. Clicking the desktop icon opens a fast local chat webpage with full offline history, fenced code blocks with copy buttons, clipboard image paste, drag-and-drop files, inline media playback, and unmistakable **`Sent ✓`** vs. **`Not sent (saved locally)`** states that automatically sync over Tailscale the moment both laptops have the chat open.

## Goals vs. Non-Goals

* **In-Scope Goals:**
  * Single zero-dependency Go binary compiled natively for Ubuntu (`linux/amd64`) and macOS (`darwin/arm64` or `darwin/amd64`).
  * On-demand lifecycle: launched via a 1-click desktop icon on each OS, auto-exits after ~5 minutes with no open chat tab (0 MB idle RAM, zero `systemd`/`launchd` background daemons).
  * Complete offline independence on every device: read full history, compose messages, attach files, and delete items while other devices are off.
  * Direct Tailscale peer-to-peer sync with explicit delivery status (**`Sent ✓`** vs. **`Not sent (saved locally)`**, expandable to per-peer status as more devices join), clear offline intimation banner, **`Retry / Sync Now`** trigger, and automatic catch-up when peers are open together.
  * Support for plain text, fenced code blocks with one-click copy, clickable URLs, clipboard image paste, drag-and-drop files (up to 250 MB default, streamed directly to disk), inline image/audio/video playback with HTTP Range seeking, and conflict-safe delete/prune across peers.
  * Plain-English visibility-aware updates (3–5s polling only while the tab is visible + immediate refresh on focus).
  * **Secondary feature (Multi-device ecosystem):** Store peers as an extensible list (`peers: [{id, name, addr}]`) rather than hardcoding a single peer, track delivery acknowledgments per peer, and support adding new Tailscale devices via config (and a UI `+ Add Device` flow) without changing the storage schema or sync protocol.

* **Out-of-Scope Non-Goals:**
  * Background delivery while a receiving device's chat window is closed (accepted trade-off of Option 1B on-demand zero-idle-RAM design).
  * Syncthing, Docker, external databases, or a third always-on cloud/Pi relay server.
  * Multi-user accounts, per-device ACLs/private rooms between different users, message editing, emoji reactions, typing indicators, server-side media transcoding, or full-text search indexes.
  * Exposure on LAN or public internet (UI binds strictly to `127.0.0.1`; sync binds strictly to the Tailscale IP with a shared token).
  * Native mobile apps (iOS/Android) in v1 (though any future device speaking the same HTTP sync protocol can join the peer list).

## Alternatives Weighed

* **Syncthing + Local Go Webpage:** Rejected by author after review—adds a second daemon on both laptops, 25–100+ MB RAM overhead during media hashing, separate pairing/config, and filesystem sync-order races; direct Go HTTP sync over Tailscale is simpler for two fixed peers.
* **Always-On Background Go Binary (Option 1A):** Rejected by author—preferred zero background RAM and zero `systemd`/`launchd` setup over automatic syncing while the chat window is closed.
* **Rust or WebAssembly Binary:** Rejected by author—browser Wasm is sandboxed from raw Tailscale sockets and local disk writes; Rust only saves ~3–5 MB RAM over Go while adding macOS cross-compilation friction and a ~300–500 MB build folder.
* **Matrix (`Continuwuity` + web client):** Rejected by author—no official native macOS binary without a Docker Linux VM or building Rust+RocksDB from source.
* **Memos (`usememos/memos`):** Rejected by author—single-host mode breaks Ubuntu independence when Mac is off, and syncing SQLite across two offline writers risks corruption.
* **XMPP (`Prosody` + `mod_conversejs`):** Rejected by author—single-host mode breaks offline independence, and dual federated servers bounce offline `s2s` messages.
* **BeeBEEP / Jami:** Rejected by author—desktop GUI apps rather than a simple local webpage, with basic code/media rendering (BeeBEEP) or higher RAM usage (Jami).

## Key Decisions & Staging for Next Steps

All major product and architecture decisions are resolved for formal design drafting (`egm-draft`):

1. **Runtime & Language:** Single zero-dependency Go binary (`CGO_ENABLED=0`, UI embedded via `//go:embed`), built on Ubuntu (`go 1.27.4`) for both Ubuntu and macOS.
2. **Lifecycle (Option 1B):** Desktop icon launches the binary on demand if the local UI port is not listening, opens `http://127.0.0.1:<ui-port>`, and shuts the binary down cleanly after 5 minutes with no active client polls/heartbeat.
3. **Sync & Outbox Contract:** Write locally first; bind sync API strictly to the Tailscale IP (`100.x.x.x:<sync-port>`) with a shared secret token; mark undelivered items **`Not sent (saved locally)`** with an offline banner and **`Retry / Sync Now`**, automatically syncing all queued messages, attachments, and tombstones whenever both laptops are open.
4. **Storage & Media:** Data stored in `~/LocalChat` (outside macOS TCC folders), versioned JSON (`schema_version: 1`), opaque ASCII attachment paths (`attachments/YYYY/MM/<ulid>.<ext>`), streaming uploads capped at 250 MB default, HTTP `Range` media serving, and tombstone-based deletion.
5. **One remaining detail to confirm during drafting/setup:** The MacBook Air's CPU architecture (`arm64` Apple Silicon vs. `amd64` Intel) so we compile the matching macOS binary.
