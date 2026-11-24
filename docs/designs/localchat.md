# LocalChat: Tailscale On-Demand Local Chat Bridge

*   **Author:** blackdragoon26
*   **Status:** Approved
*   **Date:** 2026-11-24

## Overview

`localchat` is a standalone, zero-dependency Go binary that gives Tailscale-connected personal devices—starting with an Ubuntu 24.04 (`linux/amd64`, 5.7 GB RAM) laptop and an Apple Silicon MacBook Air (`darwin/arm64`)—a private local chat webpage for passing code blocks, links, files, and media back and forth.

Each device stores its own independent history and attachments on disk and opens the chat webpage at `http://127.0.0.1:<ui-port>` via a single-click desktop icon. There is no Syncthing, no Docker, and no always-on background daemon: clicking the icon starts the local Go binary on demand (targeting `< 15 MB` RSS while open) and stops it with exit code `0` roughly 5 minutes after all chat tabs are closed (0 MB RAM when idle).

Whenever you send an item, it is written to local disk first and pushed directly over Tailscale to every configured peer that currently has `localchat` open. If a peer is offline or closed, the webpage displays an offline notice and tags the message **`Not sent`** (per peer) so undelivered items remain visible—and automatically syncs all queued items once the devices are open together.

## The Problem

Passing code snippets, links, screenshots, and media files between a personal Ubuntu laptop and a MacBook Air over Tailscale currently forces a three-way trade-off:

1. **Single-host web servers break independence:** Hosting a server like Memos or Prosody on one laptop means the other laptop cannot read history or queue outgoing items whenever the host sleeps or powers off.
2. **Heavy chat stacks waste RAM or require Docker:** Running Matrix (`Continuwuity`) on macOS requires a Docker Linux VM (~500 MB–1 GB+ RAM) or compiling Rust+RocksDB from source; Electron desktop clients consume 300–800 MB RAM on a 5.7 GB Ubuntu laptop.
3. **File sync daemons add background overhead and sync-order races:** Pairing Syncthing with a local webpage requires maintaining two separate background tools per OS, risks RAM spikes during large media hashing, and introduces partial-file races where small message files arrive before large videos.

`localchat` resolves this by collapsing the local webpage and direct Tailscale peer sync into one on-demand Go binary with an explicit local outbox so all configured devices stay independent offline while keeping idle RAM at zero.

## Goals

*   **Zero-dependency cross-OS binary:** Compile one pure-Go (`CGO_ENABLED=0`) binary for both `linux/amd64` (Ubuntu 24.04) and `darwin/arm64` (Apple Silicon MacBook Air), with all HTML/CSS/JS embedded inside the binary (zero external CDN requests, zero Docker).
*   **Zero idle footprint (on-demand lifecycle):** Start the Go process automatically when the desktop icon is clicked and shut it down after 5 minutes with no open browser tab or active transfer, avoiding permanent `systemd`/`launchd` background daemons.
*   **Full offline independence:** Allow reading complete local history, composing messages, attaching files, and deleting items on any device even while Tailscale is down or all other devices are powered off.
*   **Explicit delivery state and automatic catch-up:** Save every outgoing item locally first, display an immediate offline notice when a peer is unreachable, tag undelivered messages **`Not sent`** (with a **`Sync Now`** control), and automatically exchange missed messages, attachments, and deletions whenever peers are open together.
*   **Developer-focused content and media support:** Support plain text, fenced code blocks with one-click copy, clickable URLs, clipboard image paste, drag-and-drop files up to 250 MB (streamed directly to disk without RAM buffering), inline image/audio/video playback with HTTP `Range` seeking, and conflict-safe deletion (scrubbing deleted text and removing blobs) across peers.
*   **Configurable multi-device peer list:** Store peers as a configurable list (`peers: [{id, name, addr}]`) rather than hardcoding a single peer, track delivery acknowledgments per peer, and support adding or removing Tailscale devices via `config.json` or an in-page `+ Add Device` panel without changing storage or sync formats.
*   **Local and Tailscale network isolation:** Bind the chat UI to `127.0.0.1`, bind the peer sync API to the device's Tailscale CGNAT address (`100.64.0.0/10`) behind a shared 256-bit secret token, and guard against XSS, path traversal, and localhost CSRF.

## Non-Goals

*   **Background delivery while the receiving chat window is closed:** Accepted trade-off of the on-demand zero-idle-RAM lifecycle; both devices must have `localchat` open during an overlapping window to sync.
*   **Third always-on relay or cloud server:** No VPS, Raspberry Pi, or cloud storage dependency.
*   **Syncthing, Docker, or external databases:** No SQLite, Postgres, or external sync daemon.
*   **Multi-user permissions or group chat rooms:** All configured devices belong to the same personal owner and share one unified timeline.
*   **Message editing, emoji reactions, typing indicators, server-side media transcoding, or full-text search indexes** in v1.
*   **Native mobile apps (iOS/Android)** in v1 (though any future client speaking the HTTP sync API can join the peer list).

## Open Questions

None. Message storage uses per-device monthly NDJSON logs, and multi-device sync uses a single shared 256-bit workspace token.

## Critical User Journeys (CUJs)

### CUJ 1: One-Click Launch & Live Send (Both Devices Open)

*   **Given**: `localchat init` has been run once on both the Ubuntu laptop (`100.124.162.117`) and the MacBook Air (`100.124.162.95`), both are connected over Tailscale, and the user has clicked the desktop icon on both laptops.
*   **When**: The user pastes a screenshot or fenced code block on Ubuntu and presses Send.
*   **Then**:
    1. Ubuntu saves the attachment (if any) and message record atomically to `~/LocalChat` and displays it immediately.
    2. Ubuntu pushes the item directly to the Mac's Tailscale sync port (transferring and verifying the attachment before committing the message on Mac).
    3. The message status on Ubuntu flips to **`Sent ✓`**, and the Mac's open webpage displays the message and inline preview within 4 seconds without a manual page refresh.

### CUJ 2: Send While Peer Is Offline (Local Outbox & Later Catch-Up)

*   **Given**: The MacBook Air is asleep or its `localchat` window is closed, and the user opens `localchat` on Ubuntu (even if Tailscale itself is temporarily offline).
*   **When**: The user drops a 40 MB video and a link on Ubuntu and presses Send.
*   **Then**:
    1. Ubuntu streams the video directly to disk (within its `< 15 MB` RSS target) and saves the message locally.
    2. Because the Mac is unreachable, Ubuntu immediately shows an offline notice in the header (`Mac offline — saved locally`) and tags the message **`Not sent`** alongside a **`Sync Now`** button.
    3. The user closes all Ubuntu chat tabs; ~5 minutes later, Ubuntu's `localchat` binary exits with code `0` (0 MB RAM).
    4. Later, when the user opens `localchat` on both Ubuntu and Mac together (or clicks **`Sync Now`**), both peers compare manifests, transfer the verified video and message automatically, and flip the Ubuntu badge from **`Not sent`** to **`Sent ✓`**.

### CUJ 3: Delete Accidental Secret or Large Video Across Devices

*   **Given**: A message containing an API key or a 200 MB video was previously synced between Ubuntu and Mac.
*   **When**: The user clicks Delete on that message in the Ubuntu webpage.
*   **Then**:
    1. Ubuntu writes a delete tombstone record, rewrites that monthly NDJSON file atomically to scrub the deleted message's text from disk, removes the local attachment blob immediately, and hides the item from the feed.
    2. If the Mac is currently open (or on the next sync when both are open), the tombstone syncs to the Mac, which scrubs its local monthly NDJSON copy, deletes its attachment blob, and hides the message.

### CUJ 4: Add a New Tailscale Device to the Configured Devices

*   **Given**: Ubuntu and Mac are already paired, and the user installs `localchat` on a 3rd Linux/macOS device on their Tailscale network using `localchat init --token <workspace_token>` with existing peers configured.
*   **When**: The user adds the new device's ID, display name, and Tailscale address via the `+ Add Device` UI panel (or `config.json`) on existing devices and opens `localchat` on the new device.
*   **Then**:
    1. Existing devices persist the new peer in their local `config.json` without restarting.
    2. On sync, the new device pulls all non-deleted messages, attachments, and tombstones (never receiving scrubbed/deleted message text or purged blobs), and future messages track delivery per peer (e.g., `Sent to Mac ✓ · Not sent to Desktop`).

## Technical Plan (Architecture)

### 1. System Topology and Dual-Listener Design

Each device runs one self-contained Go binary (`localchat`, `CGO_ENABLED=0`, zero third-party Go modules, embedded web assets). Inside one process, two HTTP servers separate local UI traffic from Tailscale peer sync:

```text
  [Desktop Icon -> localchat open]
             |  (reads ui_addr from config.json; if closed -> start localchat serve)
             v
  +-------------------------------------------------------------------+
  |  localchat (Go binary, <15 MB RSS, auto-exits after 5m idle)      |
  |                                                                   |
  |  UI Listener: 127.0.0.1:8787       Sync Listener: 100.64.x.x:8788 |
  |  (Host/Origin CSRF + CORP guard)   (Tailscale IP + Bearer token)  |
  |    - GET  /{$} (embedded SPA)        - GET  /sync/manifest        |
  |    - GET  /api/health                - POST /sync/records         |
  |    - GET  /api/heartbeat             - GET  /sync/attachments/{id}|
  |    - GET  /api/status                - PUT  /sync/attachments/{id}|
  |    - GET  /api/messages              - POST /sync/push            |
  |    - POST /api/messages                                           |
  |    - DELETE /api/messages/{id}                                    |
  |    - GET  /api/attachments/{id}                                   |
  |    - GET/POST/DELETE /api/peers                                   |
  |    - POST /api/sync                                               |
  |                                                                   |
  |  Local Store (~/LocalChat/, mode 0700)                            |
  |    - config.json (0600)       (device id, token, peers[])         |
  |    - localchat.lock           (single-instance file lock)         |
  |    - state/acked.json         (per-record peer delivery ledger)   |
  |    - messages/<dev>-YYYY-MM.ndjson  (monthly per-device logs)     |
  |    - attachments/YYYY/MM/<ulid>.<ext>                             |
  |    - tmp/                     (upload/download staging)           |
  +-------------------------------------------------------------------+
                                    |
                          Tailscale WireGuard
                                    |
                  [Peer 1: MacBook Air 100.124.162.95:8788]
                  [Peer N: Future Tailscale devices...]
```

### 2. On-Demand Process Lifecycle & Offline Degraded Startup

*   **First-run setup (`localchat init`):** Creates `~/LocalChat` (`0700`) and `config.json` (`0600`). Fails if `config.json` already exists unless `--force` is passed. If `localchat serve` or `localchat open` runs without `config.json`, it returns a clear setup error pointing to `localchat init`.
*   **Single-instance guard:** On startup, `localchat serve` acquires a non-blocking Unix file lock on `~/LocalChat/localchat.lock` **before** sweeping `tmp/`. If another instance already holds the lock, it exits with code `0`. The lockfile is never unlinked on release.
*   **Desktop launcher (`localchat open`):** Instead of shell scripts depending on `curl` or hardcoding port `8787`, the Go binary itself implements `localchat open`: it reads `ui_addr` from `config.json`, probes `GET /api/health` (checking `{"app":"localchat","ok":true}`), spawns `localchat serve` detached (`</dev/null >>~/LocalChat/localchat.log 2>&1`) if not running, waits up to 2 seconds for `/api/health`, and launches the default browser (`xdg-open` on Linux, `open` on macOS). If `/api/health` fails after 2 seconds, it shows an OS notification/error rather than opening a broken tab.
*   **Offline-safe sync listener:** `127.0.0.1:<ui_port>` always starts unconditionally. If binding `sync_addr` (the Tailscale IP) fails (e.g., airplane mode or Tailscale waking up), `localchat` stays running in local mode, surfaces `"Tailscale offline — local mode"` in `/api/status`, and retries binding `sync_addr` every 10 seconds and on `POST /api/sync`.
*   **Idle auto-shutdown (with background-tab and transfer safety):**
    *   `lastUIActivity` updates on UI `/api/*` requests (including a lightweight 60-second `GET /api/heartbeat` sent while `document.hidden === true`, excluding `/api/health` probes). Passive `GET /sync/manifest` polls from peers **never** refresh `lastUIActivity`, preventing two closed-tab peers from keeping each other alive forever.
    *   `inFlightTransfers` (`atomic.Int64`) tracks active uploads, downloads, and outbound peer sync transfers.
    *   A 30-second idle reaper exits gracefully (`Shutdown(10s)` + draining active pushes) only when `now - lastUIActivity >= idle_timeout_sec` (default `300s`, disabled if `--no-idle-exit` is passed) **and** `inFlightTransfers == 0`.
    *   If a browser tab wakes up after the server exited, the UI shows a banner: `"LocalChat went to sleep — click the LocalChat desktop icon to reconnect."`

### 3. Storage Layout, Monthly NDJSON Logs, and Secret Scrubbing

All data lives in `~/LocalChat` by default (configurable via `--data` or `LOCALCHAT_DATA`, outside macOS TCC—Transparency, Consent, and Control—protected folders like `~/Documents` and `~/Desktop`):

*   **`config.json` (mode `0600`, device-local, guarded by `sync.RWMutex` at runtime):**
    ```json
    {
      "device_id": "ubuntu-vivobook",
      "device_name": "Ubuntu",
      "ui_addr": "127.0.0.1:8787",
      "sync_addr": "100.124.162.117:8788",
      "workspace_token": "<64-hex-char-random-secret>",
      "max_upload_bytes": 262144000,
      "idle_timeout_sec": 300,
      "peers": [
        { "id": "mac-air", "name": "Mac", "addr": "100.124.162.95:8788" }
      ]
    }
    ```
    *Validation:* `device_id` and `peer.id` must match `^[a-z0-9][a-z0-9_-]{0,63}$`. `peer.addr` must be a valid `host:port` (no `http://` scheme) inside Tailscale's CGNAT range `100.64.0.0/10` (loopback permitted only via an internal test flag) and must not equal self `sync_addr` or `device_id`. `DetectTailscaleIP()` scans local network interfaces for `100.64.0.0/10`, preferring `tailscale*` (Linux) / `utun*` (macOS) interfaces.
*   **`messages/<origin-device-id>-YYYY-MM.ndjson`:**
    Month buckets (`YYYY-MM`) are derived strictly in **UTC** from `created_at_ms`. Each line is a compact JSON record (`schema_version: 1`, `kind: "message" | "tombstone"`, 26-char Crockford Base32 ULID `id`, `device_id`, `device_name`, UTC `created_at_ms`, `text` capped at 1 MiB, optional `attachment` `{id, month, filename, stored_name, mime_type, size_bytes, sha256}`, and `target_id` for tombstones).
    *   **Deterministic ordering & crash recovery:** In-memory index sorts by `(created_at_ms, id)`. Startup parses lines with `bufio.Reader.ReadBytes('\n')` (no 64 KiB `bufio.Scanner` cap) and permissive JSON decoding; truncated/malformed trailing lines are logged and skipped. Two-pass/set-based loading ensures tombstones apply regardless of file load order.
    *   **Tombstone scrubbing (CUJ 3):** Applying a local or remote tombstone appends the tombstone record, immediately deletes the local attachment blob (if present), and atomically rewrites the affected `<origin-device-id>-YYYY-MM.ndjson` file (staging via `os.CreateTemp` in the same directory + `fsync` + `os.Rename`) to drop the deleted message's record/text so deleted secrets do not remain on disk or sync to newly joined devices.
*   **`attachments/YYYY/MM/<ulid>.<safe-ext>` + `tmp/`:**
    Opaque ASCII filenames only (`ext` matches `^[a-z0-9]{1,10}$`, otherwise `bin`). Uploads and synced blobs stream via unique `os.CreateTemp(tmpDir, "part-*")` files with SHA-256 verification and `fsync` before atomic rename into `attachments/YYYY/MM/`. After acquiring the single-instance lock on startup, `localchat` sweeps `tmp/` and removes any orphaned files in `attachments/` not referenced by a non-tombstoned message.
*   **`state/acked.json` (local delivery ledger):**
    Maps locally originated non-tombstoned message IDs and tombstone IDs to arrays of acknowledging `peer.id`s (serialized under a mutex, batched per sync cycle, written via same-directory `os.CreateTemp` + `fsync` + `os.Rename`). Deleting a peer via `DELETE /api/peers/{id}` filters that peer out of future UI pending states while preserving historical messages already received from that peer. When `peers` is empty, locally saved messages show `Saved locally`.

### 4. Direct Tailscale Peer Sync Protocol

All `/sync/*` endpoints require `Authorization: Bearer <workspace_token>` verified in constant time and reject requests whose peer `device_id == self.device_id`. HTTP servers set `ReadHeaderTimeout: 10s, ReadTimeout: 0, WriteTimeout: 0`; outbound clients use `Timeout: 0` with a `2s` dial timeout and `30s` response header timeout (plus `15s` context timeouts on JSON metadata calls) so 250 MB media streams never abort prematurely.

*   **Per-peer single-flight lock & transfer concurrency:** Each peer has a dedicated sync mutex so overlapping push, 10s catch-up, and **`Sync Now`** triggers coalesce rather than double-uploading a 250 MB file. Concurrent attachment transfers are bounded by a 2-slot semaphore.
*   **Manifest contract (`GET /sync/manifest`):**
    *   Returns `{device_id, device_name, record_ids: [...], tombstoned_ids: [...]}` with an `ETag` digest (`If-None-Match` → `304 Not Modified`).
    *   `record_ids` contains all **non-tombstoned** message IDs plus all **tombstone record IDs** in the local store (enabling transitive catch-up across 3+ peers without ever advertising deleted messages). `tombstoned_ids` contains all deleted message `target_id`s.
*   **Immediate send (`POST /api/messages` & `DELETE /api/messages/{id}`):**
    *   Commits locally first. For text-only messages and tombstones, waits up to `250ms` for concurrent `PushRecordToAll` (returning `Sent ✓` immediately on fast Tailscale links); for messages with attachments, runs `PushRecordToAll` asynchronously and returns `201 Created` immediately with initial `Sending…` / `Not sent` status so large uploads never stall the composer.
    *   For each peer: uploads attachment first via `PUT /sync/attachments/{id}?month=YYYY-MM&ext=<ext>` (validated regexes, `X-Attachment-SHA256`, `X-Attachment-Size`, idempotent `200 OK` if identical blob already exists), then sends `POST /sync/push` with the record. If the target message is already tombstoned on the receiver, `POST /sync/push` deletes any uploaded blob and returns `200 OK` without storing the message; if a non-tombstoned message's attachment is missing on disk, it returns `409 Conflict`.
*   **Bidirectional catch-up sync (on startup, `POST /api/sync`, and every 10s while `lastUIActivity` is within 60s, with 10s → 30s → 60s backoff for offline peers reset on local send or `Sync Now`):**
    1. Fetch peer's `GET /sync/manifest` (skipping diff on `304`).
    2. **Pull & apply tombstones first:** Identify missing tombstone record IDs and request them in chunks of at most 100 IDs via `POST /sync/records` (`{ids: [...]}` → `{records: [...]}`), applying each tombstone locally (scrubbing text and removing blobs).
    3. **Pull missing live messages:** Diff remaining `record_ids` excluding local and remote `tombstoned_ids`; fetch in chunks of 100 via `POST /sync/records`. For each message with an attachment not yet on disk, stream and verify `GET /sync/attachments/{id}` into `tmp/` → atomic rename before ingesting the record.
    4. **Push unsent local records:** Push any local tombstones and non-tombstoned messages absent from the peer's manifest (excluding `peer.tombstoned_ids`), attachment first then record, and batch-update `state/acked.json`.
    5. **Reconcile acks:** Any local record already present in `peer.record_ids` (or already tombstoned) is marked resolved in `state/acked.json`.

### 5. Embedded Web UI and Revision-Based Polling

*   **Revision-based polling (fixes out-of-order backfills, deletes, and ack flips):** The store maintains an atomic monotonic `store_rev` (`uint64`) incremented on every local message, remote ingest, tombstone, or ack change.
    *   While `!document.hidden`, the webpage polls `GET /api/status` every `4s` (and immediately on `focus`/`visibilitychange`). Only when `store_rev` changes does it re-fetch `GET /api/messages?limit=50` (and `"Load older"` uses `GET /api/messages?limit=50&before=<oldest_id>`).
    *   While `document.hidden === true`, the webpage sends only a lightweight `GET /api/heartbeat` every `60s` so backgrounded tabs stay alive until actually closed.
*   **Feed and composer ergonomics:**
    *   Top status bar shows per-peer online/offline badges, total unsent count, **`Sync Now`**, and **`+ Add Device`** (with peer add/remove controls).
    *   When a revision refresh adds messages older than the current bottom visible message, displays a `"New messages synced above ↑"` pill.
    *   Renders fenced ` ``` ` code blocks (gracefully closing unclosed fences, with a `Copy` button confirming `"Copied!"`) and `https?://` clickable links using DOM text nodes only (zero `innerHTML` on user content).
    *   Composer supports clipboard image paste (`paste` event), drag-and-drop file upload (single file per message; dropping multiple files sends them sequentially) with upload progress bar, `Ctrl`/`Cmd+Enter` to send (`Enter` inserts newline for multi-line code), per-message **`Sent ✓`** / **`Sending…`** / **`Not sent`** badges, and a Delete button with confirmation.

## Alternatives Considered

### Alternative A: Syncthing + Local Go Webpage

*   **Pros**: Uses an established file sync engine with block-level resume for large videos and zero custom sync code.
*   **Cons**: Requires running and configuring two separate tools on every device, adds 25–100+ MB RAM spikes during large media hashing on the 5.7 GB Ubuntu laptop, requires manually disabling Syncthing public relays/discovery, and introduces partial-file sync races (`.syncthing.*.tmp` and message-before-attachment arrival).
*   **Reason for Rejection**: Rejected against the RAM, single-binary, and delivery-status Goals: direct HTTP sync inside one standalone Go binary eliminates the second daemon and gives direct control over attachment-before-message delivery and per-message **`Not sent`** status.

### Alternative B: Always-On Background Go Binary

*   **Pros**: Syncs queued messages automatically whenever both laptops happen to be awake at the same time, even when all chat browser tabs are closed.
*   **Cons**: Keeps RAM allocated continuously while each laptop is awake and requires maintaining OS-specific background services (`systemd --user` with linger on Ubuntu and `launchd` plists on macOS).
*   **Reason for Rejection**: Rejected against the zero-idle-RAM and zero-background-daemon Goals; explicit **`Not sent`** badges keep pending items visible until both devices open the chat together.

### Alternative C: Rust or WebAssembly Implementation

*   **Pros**: A Rust binary idles slightly lower (~2–5 MB RAM vs. `< 15 MB` RSS in Go).
*   **Cons**: Browser WebAssembly cannot bind Tailscale sockets or write freely to local disk without permission prompts, while standalone Wasm runtimes add ~30–50 MB overhead. Rust cross-compilation from Ubuntu to Apple Silicon macOS (`darwin/arm64`) requires a macOS SDK cross-linker or compiling separately on the Mac, plus a ~300–500 MB Cargo build directory.
*   **Reason for Rejection**: Rejected against the cross-OS zero-dependency build Goal: Go produces single `CGO_ENABLED=0` binaries for both Ubuntu and macOS straight from Ubuntu with zero external packages.

### Alternative D: Embedding `tailcat` / `tsnet` as the Transport Layer

*   **Pros**: `tailcat` (`github.com/tailscale/tailcat`) connects machines outside the same Tailscale account using standalone `tc...` WireGuard/DERP addresses without OS-level Tailscale.
*   **Cons**: Pulls in Tailscale's Go dependency tree and `gVisor` userspace networking stack, growing the binary from ~8 MB to ~35–50 MB and runtime RAM to ~40–80 MB while duplicating the WireGuard tunnel already provided by Tailscale on both laptops.
*   **Reason for Rejection**: Rejected against the RAM and zero-dependency Goals: existing OS-level Tailscale IPs (`100.64.0.0/10`) already provide WireGuard encryption and NAT traversal with zero dependency overhead.

### Alternative E: Off-the-Shelf Chat Servers (Matrix `Continuwuity`, Memos, XMPP `Prosody`)

*   **Pros**: Existing open-source ecosystems with built-in UIs.
*   **Cons**:
    *   *Matrix (`Continuwuity`):* No official native macOS binary without running a Docker Linux VM (~500 MB–1 GB+ RAM) or compiling Rust+RocksDB from source, plus hosting a separate web client.
    *   *Memos (`usememos/memos`):* Single-host mode leaves Ubuntu offline when Mac sleeps; running two Memos instances over a synced SQLite database risks database corruption.
    *   *XMPP (`Prosody` + `mod_conversejs`):* Single-host mode breaks Ubuntu independence when Mac is off, and dual federated Prosody servers bounce offline `s2s` messages after short retry windows instead of holding them for hours.
*   **Reason for Rejection**: Rejected against the offline-independence and zero-Docker macOS Goals.

### Alternative F: Native P2P Desktop Apps (`BeeBEEP`, `Jami`) or a Third Always-On Host

*   **Pros**: `BeeBEEP` and `Jami` run peer-to-peer with no central server; a third always-on VPS/Raspberry Pi enables async delivery even if the two laptops are never awake together.
*   **Cons**: `BeeBEEP` and `Jami` are standalone GUI apps rather than a local webpage (`Jami` uses ~150–300 MB RAM; `BeeBEEP` has basic code/media formatting); a third cloud/Pi host adds another machine to maintain.
*   **Reason for Rejection**: Rejected against the browser-based webpage and two-laptop-only Goals.

## Non-Functional Requirements

*   **Security & Privacy:**
    *   `~/LocalChat` is created with mode `0700` and `config.json` with mode `0600`.
    *   UI server binds to `127.0.0.1:<ui_port>`, sets `Cross-Origin-Resource-Policy: same-origin` on all UI routes, sets `Content-Security-Policy: default-src 'self'; media-src 'self'; img-src 'self' blob: data:; style-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'`, validates `Host` (`127.0.0.1:<port>` or case-insensitive `localhost:<port>`), and fails closed (`403`) on `POST`/`DELETE` unless `Sec-Fetch-Site: same-origin` or an exact matching `Origin` header is present.
    *   Sync server binds to the device's Tailscale IPv4 address (`100.64.0.0/10`) and verifies the 256-bit `workspace_token` in constant time on every route. `POST /api/peers` restricts peer addresses to `100.64.0.0/10` so tokens are never sent over cleartext non-Tailscale networks.
    *   Attachments are served via `http.ServeContent` after explicitly setting `Etag: "<sha256>"`, `X-Content-Type-Options: nosniff`, RFC 5987 `Content-Disposition` (via `mime.FormatMediaType`), and a validated `Content-Type`. Inline rendering requires **both** an allowlisted extension (`png`, `jpg`, `jpeg`, `gif`, `webp`, `mp4`, `webm`, `mp3`, `wav`, `ogg`, `m4a`) and magic-byte verification via `http.DetectContentType` (preventing HTML/SVG disguised as images); all other types force `Content-Disposition: attachment`, `Content-Type: application/octet-stream`, and `Content-Security-Policy: sandbox`.
*   **Performance & Resource Budget:**
    *   Zero RAM and zero CPU when closed (idle auto-shutdown).
    *   Target `< 15 MB` RSS while open; constant heap during 250 MB uploads and peer transfers via direct disk streaming (`r.MultipartReader()` + `io.Copy`).
*   **Build, Rollout & Rollback:**
    *   Cross-compiled from Ubuntu with `CGO_ENABLED=0`: `GOOS=linux GOARCH=amd64` and `GOOS=darwin GOARCH=arm64`.
    *   Data directory (`~/LocalChat`) is separate from installed binaries (`~/.local/bin/localchat`), so upgrading or reverting a binary never touches chat history.
    *   macOS installation documents one-time `xattr -d com.apple.quarantine ... 2>/dev/null || true` and `codesign --force --sign -` for unsigned cross-compiled Apple Silicon binaries.

## Future Improvements

*   **Optional `tailcat` guest transport:** Add an optional build-tagged `tailcat` (`github.com/tailscale/tailcat`) adapter only if non-tailnet guest devices ever need to join the peer list.
*   **Per-peer token rotation:** Upgrade `peers[]` to allow optional per-peer token overrides if revoking a single lost device without rotating the shared workspace token becomes necessary.

## Detailed Implementation Plan

All files below live under `/home/motherfunder/agy-workspaces/localchat` (Go standard library only, zero external `require` entries in `go.mod`, `CGO_ENABLED=0`).

### Target Files to Create/Modify

#### 1. `go.mod` (Create)
*   **Type / Scope**: Go Module Definition
*   **Proposed Changes**: Declare `module localchat` with `go 1.22` minimum directive (for `net/http` method+path routing, `log/slog`, and `slices`) and zero external dependencies.

#### 2. `cmd/localchat/main.go` (Create)
*   **Type / Scope**: CLI Entrypoint & Lifecycle Coordinator
*   **Proposed Changes**:
    *   Subcommands:
        *   `serve` (default): Loads `config.json`, acquires `localchat.lock`, opens `store`, starts `127.0.0.1:<ui_port>` unconditionally, starts Tailscale `sync_addr` under a background bind-retry supervisor (every 10s + on `POST /api/sync`), triggers initial non-blocking `SyncAllPeers`, and runs the `lifecycle.Tracker` idle reaper (disabled with `--no-idle-exit`) plus `SIGINT`/`SIGTERM` graceful shutdown (`10s` timeout).
        *   `open`: Reads `ui_addr` from `config.json`, probes `GET /api/health` verifying `{"app":"localchat","ok":true}`, spawns `localchat serve` detached (`</dev/null >>~/LocalChat/localchat.log 2>&1`) if not running, waits up to `2s` for readiness, and opens the default browser (`xdg-open` on Linux, `open` on macOS).
        *   `init`: Bootstraps `~/LocalChat` (`0700`) and `config.json` (`0600`) with flags `--id`, `--name`, `--ui-addr` (default `127.0.0.1:8787`), `--sync-addr` (auto-detected Tailscale IP + `:8788`), `--token`, repeatable `--peer id=name@host:port`, and `--force`.
        *   `version`: Prints build version and target OS/arch.
    *   Global flags: `--data` / `LOCALCHAT_DATA` (default `~/LocalChat`), `--idle-timeout`, `--no-idle-exit`.

#### 3. `internal/config/config.go` (Create)
*   **Type / Scope**: Configuration Manager & Tailscale Discovery
*   **Proposed Changes**:
    *   Thread-safe `Manager` (`sync.RWMutex`) wrapping `Config` (`DeviceID`, `DeviceName`, `UIAddr`, `SyncAddr`, `WorkspaceToken`, `MaxUploadBytes`, `IdleTimeoutSec`, `Peers []Peer`) and returning defensive peer slices.
    *   `Load(path)`, atomic `Save(path)` (`0600`, same-dir `os.CreateTemp` + `fsync` + `os.Rename`), `GenerateToken()`, `DetectTailscaleIP()` (prefers `tailscale*`/`utun*` matching `100.64.0.0/10`), `ValidateID` (`^[a-z0-9][a-z0-9_-]{0,63}$`), and `AddPeer`/`RemovePeer` enforcing Tailscale `100.64.0.0/10` CIDR `host:port` validation (with an explicit `AllowLoopbackPeers` option for tests).

#### 4. `internal/ulid/ulid.go` (Create)
*   **Type / Scope**: Pure-Stdlib Monotonic ULID Generator
*   **Proposed Changes**: `sync.Mutex`-guarded Crockford Base32 26-char ULID generator (`New(now time.Time) string`) with 48-bit UTC millisecond prefix, 80-bit `crypto/rand` suffix, monotonic same-ms increment, and `IsValid(id string) bool`.

#### 5. `internal/lock/lock_unix.go` (Create)
*   **Type / Scope**: Single-Instance File Lock (Linux & macOS)
*   **Proposed Changes**: Include explicit `//go:build unix` build constraint; `Acquire(path)` creates parent dirs (`0700`), opens lockfile, runs non-blocking `syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)` checking `EWOULDBLOCK`/`EAGAIN`, retains `*os.File` until `Release()`, and never unlinks the lockfile.

#### 6. `internal/lifecycle/tracker.go` (Create)
*   **Type / Scope**: Idle Shutdown & In-Flight Transfer Tracker
*   **Proposed Changes**: Provides testable `Tracker` tracking `lastUIActivity` (`atomic.Int64`, updated by UI `/api/*` + `/api/heartbeat`), `inFlightTransfers` (`atomic.Int64`), injectable clock/timeout, `BeginTransfer()` / `EndTransfer()`, `ShouldExit(now)`, and `UIActiveWithin(d)`.

#### 7. `internal/store/store.go` (Create)
*   **Type / Scope**: Per-Device Monthly NDJSON Store, Attachment Manager & Delivery Ledger
*   **Proposed Changes**:
    *   `Open(dataDir, selfID, selfName)`: Creates `messages/`, `attachments/`, `state/`, `tmp/` (`0700`), sweeps `tmp/`, loads `state/acked.json`, parses all `messages/*.ndjson` via `bufio.Reader.ReadBytes('\n')` with permissive JSON decoding (logging/skipping corrupt trailing lines), builds the two-pass in-memory index (applying tombstones before filtering live messages, sorting by `(created_at_ms, id)`), runs startup orphan-attachment GC, and initializes `store_rev` (`atomic.Uint64`) and cached manifest `ETag`.
    *   `CreateLocalMessage(text, attMeta)`: Enforces max text size (`1 MiB`), appends single line + `fsync` to UTC `messages/<self>-YYYY-MM.ndjson`, updates index + `store_rev` + `ETag`.
    *   `CreateLocalTombstone(targetID)`: Idempotent (returns `404` if unknown, `200` if already tombstoned); appends tombstone to self monthly NDJSON, deletes local attachment blob, atomically rewrites target message's monthly NDJSON file to scrub its record/text, marks target resolved in `acked.json`, and bumps `store_rev`.
    *   `IngestRemoteRecord(rec)`: Validates `rec.DeviceID` regex, `rec.ID` ULID, and month format; if `kind == "tombstone"`, applies tombstone + scrubs target's monthly log + removes local blob; if `kind == "message"` and `rec.ID` is already tombstoned, deletes any staged attachment blob and returns `nil` (idempotent `200 OK`); otherwise verifies referenced attachment exists on disk with matching size before appending to UTC `messages/<rec.DeviceID>-YYYY-MM.ndjson`.
    *   Attachment streaming: `SaveUploadedAttachment(reader, origFilename, maxBytes)` and `SaveSyncedAttachment(id, month, ext, expectedSize, expectedSHA256, reader)` stream via `os.CreateTemp(tmpDir, "part-*")` + `sha256.New()`, enforce `maxBytes`, `fsync`, and atomically rename into `attachments/YYYY/MM/<ulid>.<safe-ext>`. `ResolveAttachment(attID)` looks up via in-memory map with path containment check.
    *   Query & ledger methods: `Revision() uint64`, `ListMessages(limit, beforeID)`, `Manifest()` (live non-tombstoned message IDs + tombstone IDs, `tombstoned_ids`, and `ETag`), `RecordsByID(ids)` (never returns tombstoned message bodies), `MarkAckedBatch(updates)` (mutex-guarded same-dir temp + rename), `DeliveryState(rec, peers)`, and `UnsentForPeer(peerID, peerTombstones)`.

#### 8. `internal/syncer/syncer.go` (Create)
*   **Type / Scope**: Tailscale Peer Sync HTTP Server & Outbox Client
*   **Proposed Changes**:
    *   Sync HTTP handler (`NewHandler() http.Handler`) with constant-time Bearer token auth and `http.MaxBytesReader` caps on all routes:
        *   `GET /sync/manifest`: Supports `If-None-Match` → `304 Not Modified`; returns `{device_id, device_name, record_ids, tombstoned_ids}` + `ETag`.
        *   `POST /sync/records`: Accepts `{ids: [...]}` (capped at 100 IDs) and returns `{records: [...]}`.
        *   `GET /sync/attachments/{id}`: Streams attachment blob with `X-Attachment-SHA256` and `X-Attachment-Size` while incrementing `inFlightTransfers`.
        *   `PUT /sync/attachments/{id}`: Validates `id` (ULID), `month` (`^\d{4}-(0[1-9]|1[0-2])$`), `ext` (`^[a-z0-9]{1,10}$`), `X-Attachment-SHA256`, `X-Attachment-Size`, enforces `max_upload_bytes` (`413`), idempotent `200 OK` if identical blob already exists, verifies size + SHA-256 (`422` on mismatch).
        *   `POST /sync/push`: Ingests one `Record`, returns `409 Conflict` if non-tombstoned attachment is missing on disk, otherwise `200 OK`.
    *   Outbound client (`Syncer`):
        *   `http.Client{Timeout: 0}` with `2s` dial timeout and `30s` response header timeout (15s context timeout on metadata calls).
        *   Per-peer `sync.Mutex` (single-flight) + 2-slot attachment transfer semaphore + per-peer backoff (`10s` → `30s` → `60s`, reset on local send or `Sync Now`).
        *   `PushRecordToAll(ctx, rec)`, `SyncPeer(ctx, peer)` (rejects self `device_id`; pulls & applies tombstones first, pulls live messages + verified attachments in 100-ID chunks, pushes unsent tombstones & non-tombstoned messages, batch-updates `acked.json`), and `SyncAllPeers(ctx)`.

#### 9. `internal/ui/server.go` (Create)
*   **Type / Scope**: Localhost Web UI Server & Security Middleware
*   **Proposed Changes**:
    *   Embed `web/` via `//go:embed web/*` with startup `modTime`.
    *   Middleware: `Host` allowlist (`127.0.0.1:<port>` / case-insensitive `localhost:<port>`), `Cross-Origin-Resource-Policy: same-origin`, fail-closed CSRF check on `POST`/`DELETE` (`Sec-Fetch-Site: same-origin` or exact matching `Origin`), and `tracker.TouchUI()` (excluding `/api/health`).
    *   Routes (Go 1.22+ exact root `GET /{$}`):
        *   `GET /{$}`, `GET /style.css`, `GET /app.js`: Serve embedded assets with strict CSP (`frame-ancestors 'none'; object-src 'none'; base-uri 'none'`).
        *   `GET /api/health`: Returns `{"app":"localchat","ok":true}`.
        *   `GET /api/heartbeat`: Touches `lastUIActivity` and returns `204 No Content`.
        *   `GET /api/status`: Returns device info, `store_rev`, sync listener status (`sync_online`, `sync_error`), per-peer states, and total unsent count.
        *   `GET /api/messages`: Supports `?limit=50&before=<id>`; returns `{rev, messages, has_more}` with per-peer delivery states (`sent_all`, `sending`, `acked_by`, `pending_peers`).
        *   `POST /api/messages`: Streams `multipart/form-data` (`text` capped at 1 MiB, optional `file` capped at `max_upload_bytes`, `MaxBytesReader` set to `max_upload_bytes + 2 MiB`, unwrapping `*http.MaxBytesError` to `413`; removes staged blob on mid-request error). Commits locally; waits up to `250ms` for text-only push (async for attachments) and returns `201 Created`.
        *   `DELETE /api/messages/{id}`: Creates local tombstone, scrubs text, removes blob, pushes asynchronously.
        *   `GET /api/attachments/{id}`: Sets explicit `Etag: "<sha256>"`, `X-Content-Type-Options: nosniff`, RFC 5987 `Content-Disposition` (`mime.FormatMediaType`), verifies magic bytes (`http.DetectContentType`) + extension allowlist for inline media (otherwise `application/octet-stream` + `attachment` + `CSP: sandbox`), and serves via `http.ServeContent`.
        *   `GET /api/peers`, `POST /api/peers`, `DELETE /api/peers/{id}`: Manage `config.peers` atomically and trigger `SyncPeer` on add.
        *   `POST /api/sync`: Retries sync listener bind if degraded and runs `SyncAllPeers()`.

#### 10. `internal/ui/web/index.html`, `internal/ui/web/style.css`, `internal/ui/web/app.js` (Create)
*   **Type / Scope**: Embedded Zero-Dependency Frontend SPA
*   **Proposed Changes**:
    *   `index.html` & `style.css`: Responsive light/dark UI with status bar (peer badges, offline warning, unsent count, **`Sync Now`**, **`+ Add Device`**), scrollable feed with `"Load older"` and `"New messages synced above ↑"` pill, sleep/reconnect banner if localhost stops responding, and bottom composer (textarea, file picker, drag-drop zone, upload progress bar, `Ctrl`/`Cmd+Enter` send hint).
    *   `app.js`: Polls `GET /api/status` every 4s while visible (re-fetching `/api/messages` only when `store_rev` changes) + `GET /api/heartbeat` every 60s while hidden; safe DOM-only renderer for fenced ` ``` ` code blocks (with `Copy` -> `"Copied!"`) and `https?://` clickable links; lazy inline media previews; clipboard image paste and multi-file sequential drag-and-drop upload; per-message **`Sent ✓`** / **`Sending…`** / **`Not sent`** badges and Delete button; `+ Add Device` modal with add/remove peer controls.

#### 11. `scripts/build.sh` (Create)
*   **Type / Scope**: Cross-Platform Build Script
*   **Proposed Changes**: Builds `CGO_ENABLED=0` stripped binaries (`-ldflags="-s -w"`) into `dist/localchat-linux-amd64` and `dist/localchat-darwin-arm64` plus `dist/SHA256SUMS`.

#### 12. `scripts/install-ubuntu.sh` (Create)
*   **Type / Scope**: Ubuntu 24.04 Installer & Desktop Launcher Setup
*   **Proposed Changes**: Copies `localchat-linux-amd64` to `~/.local/bin/localchat` and writes `~/.local/share/applications/localchat.desktop` + `~/Desktop/localchat.desktop` using expanded absolute `$HOME/.local/bin/localchat open`, `Terminal=false`, and `gio set ... metadata::trusted true 2>/dev/null || true`.

#### 13. `scripts/install-macos.sh` (Create)
*   **Type / Scope**: macOS Apple Silicon Installer & `.app` Bundle Setup
*   **Proposed Changes**: Runs `xattr -d com.apple.quarantine ... 2>/dev/null || true`, ad-hoc signs with `codesign --force --sign -`, installs binary to `~/.local/bin/localchat`, and creates `~/Applications/LocalChat.app` (`LSUIElement=true` in `Info.plist`, `Contents/MacOS/localchat-launch` calling `"$HOME/.local/bin/localchat" open` with no lingering Terminal window) plus an optional Desktop symlink/alias.

#### 14. Unit and Integration Test Suites (Create)
*   **`internal/ulid/ulid_test.go`**: Monotonicity, lexical sort, concurrency (`-race`), validation.
*   **`internal/store/store_test.go`**: Monthly NDJSON append/reload, >64 KiB code paste reload, truncated trailing line recovery, streaming attachment + SHA-256 mismatch rejection, path-traversal prevention, tombstone text scrubbing + blob removal + tombstone-before-message reload, pagination, and `acked.json` persistence.
*   **`internal/syncer/syncer_test.go`**: Starts two and three in-process `localchat` sync servers on loopback and tests: live push with attachment-before-message verification, offline queuing (`Not sent`) -> catch-up sync (`Sent ✓`), tombstone deletion scrubbing peer text & deleting blob, 3rd peer onboarding never receiving scrubbed secrets or deleted blobs, `ETag` `304 Not Modified`, invalid Bearer token (`401`), and missing attachment push (`409`).
*   **`internal/ui/server_test.go`**: `GET /{$}` vs `404`, `Host` and fail-closed CSRF (`403`), streaming multipart upload cap (`413`), disguised HTML-as-PNG / `.svg` forced download + `nosniff` + `CSP: sandbox`, `.mp4` HTTP `Range` (`206 Partial Content`), `+ Add Device` Tailscale CIDR enforcement, degraded startup without Tailscale IP, and `lifecycle.Tracker` idle-shutdown behavior (with hidden-tab heartbeat and in-flight transfer protection).

#### 15. `README.md` (Create)
*   **Type / Scope**: Setup & Usage Documentation
*   **Proposed Changes**: Step-by-step setup for Ubuntu 24.04 and macOS Apple Silicon (`localchat init`, pairing with the shared token, desktop icon install, and adding a 3rd Tailscale device).

### Verification Plan

*   **Compilation / Cross-Build**:
    *   `./scripts/build.sh` exits `0` and produces `dist/localchat-linux-amd64` and `dist/localchat-darwin-arm64` with `CGO_ENABLED=0` and zero third-party entries in `go.mod`.
*   **Static Analysis & Formatting**:
    *   `gofmt -l .` produces no output; `go vet ./...` exits `0`.
*   **Unit & Integration Tests**:
    *   `go test -race -cover ./...` exits `0` across every `internal/*` package (targeting >=80% statement coverage on core packages).
*   **Local Smoke & Resource Check (Ubuntu 24.04)**:
    *   Run `localchat init` and `localchat serve` on Ubuntu, verify `GET /api/health` returns `{"app":"localchat","ok":true}`, verify RSS memory stays `< 15 MB` while open, test uploading a fenced code block + image + video with HTTP `Range` (`206`), verify offline **`Not sent`** badge when Mac peer is unreachable, verify idle exit after `--idle-timeout 3s`, and verify `localchat open` / desktop launcher starts the server and opens `http://127.0.0.1:8787` with no error output.

--------------------------------------------------------------------------------

*Drafted and Reviewed with EGM*
