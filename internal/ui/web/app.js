(() => {
  const state = {
    selfId: "",
    selfName: "",
    syncAddr: "",
    workspaceToken: "",
    tokenRevealed: false,
    rev: 0,
    messages: [],
    loadedCount: 50,
    hasMore: false,
    peers: [],
    pendingFile: null,
    sending: false,
  };

  const el = {
    selfBadge: document.getElementById("self-badge"),
    peerBar: document.getElementById("peer-bar"),
    pendingPill: document.getElementById("pending-pill"),
    storagePill: document.getElementById("storage-pill"),
    themeBtn: document.getElementById("theme-btn"),
    clearBtn: document.getElementById("clear-btn"),
    syncBtn: document.getElementById("sync-btn"),
    devicesBtn: document.getElementById("devices-btn"),
    offlineBanner: document.getElementById("offline-banner"),
    sleepBanner: document.getElementById("sleep-banner"),
    dropOverlay: document.getElementById("drop-overlay"),
    newAbovePill: document.getElementById("new-above-pill"),
    feedWrap: document.getElementById("feed-wrap"),
    loadMoreBtn: document.getElementById("load-more-btn"),
    feed: document.getElementById("feed"),
    toast: document.getElementById("toast"),
    pendingFileBar: document.getElementById("pending-file-bar"),
    pendingFileName: document.getElementById("pending-file-name"),
    clearFileBtn: document.getElementById("clear-file-btn"),
    progressWrap: document.getElementById("upload-progress-wrap"),
    progressBar: document.getElementById("upload-progress-bar"),
    composer: document.getElementById("composer"),
    fileInput: document.getElementById("file-input"),
    attachBtn: document.getElementById("attach-btn"),
    msgInput: document.getElementById("msg-input"),
    sendBtn: document.getElementById("send-btn"),
    shortcutHint: document.getElementById("shortcut-hint"),
    modal: document.getElementById("devices-modal"),
    closeModalBtn: document.getElementById("close-modal-btn"),
    modalSelfInfo: document.getElementById("modal-self-info"),
    modalToken: document.getElementById("modal-token"),
    revealTokenBtn: document.getElementById("reveal-token-btn"),
    copyTokenBtn: document.getElementById("copy-token-btn"),
    profileForm: document.getElementById("profile-form"),
    profileNameInput: document.getElementById("profile-name-input"),
    profileTokenInput: document.getElementById("profile-token-input"),
    profileErr: document.getElementById("profile-err"),
    modalPeerList: document.getElementById("modal-peer-list"),
    addPeerForm: document.getElementById("add-peer-form"),
    peerIdInput: document.getElementById("peer-id-input"),
    peerNameInput: document.getElementById("peer-name-input"),
    peerAddrInput: document.getElementById("peer-addr-input"),
    addPeerErr: document.getElementById("add-peer-err"),
  };

  const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  el.shortcutHint.textContent = `${isMac ? "Cmd" : "Ctrl"}+Enter to send · Enter for new line · Paste images or drag-and-drop files`;

  function applyTheme(mode) {
    document.documentElement.setAttribute("data-theme", mode);
    el.themeBtn.textContent = mode === "light" ? "Dark" : "Light";
  }
  const savedTheme = localStorage.getItem("localchat_theme");
  const initialTheme = savedTheme || (window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  applyTheme(initialTheme);
  el.themeBtn.addEventListener("click", () => {
    const next = document.documentElement.getAttribute("data-theme") === "light" ? "dark" : "light";
    localStorage.setItem("localchat_theme", next);
    applyTheme(next);
  });

  let toastTimer = null;
  function showToast(msg) {
    el.toast.textContent = msg;
    el.toast.classList.remove("hidden");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.toast.classList.add("hidden"), 4500);
  }

  function formatBytes(n) {
    if (!n || n <= 0) return "0 B";
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
    return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
  }

  function formatTime(ms) {
    const d = new Date(ms);
    return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  }

  function isNearBottom() {
    return el.feedWrap.scrollHeight - el.feedWrap.scrollTop - el.feedWrap.clientHeight < 90;
  }

  function scrollToBottom() {
    el.feedWrap.scrollTop = el.feedWrap.scrollHeight;
  }

  function setPendingFile(file) {
    state.pendingFile = file || null;
    if (file) {
      el.pendingFileName.textContent = `${file.name} (${formatBytes(file.size)})`;
      el.pendingFileBar.classList.remove("hidden");
    } else {
      el.pendingFileBar.classList.add("hidden");
      el.fileInput.value = "";
    }
  }

  async function fetchStatus(forceMessages = false) {
    try {
      const resp = await fetch("/api/status");
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const data = await resp.json();
      el.sleepBanner.classList.add("hidden");

      state.selfId = data.device_id;
      state.selfName = data.device_name;
      state.syncAddr = data.sync_addr;
      state.workspaceToken = data.workspace_token || "";
      state.peers = data.peers || [];

      renderHeader(data);
      if (el.modal.open) renderModal();

      if (forceMessages || data.store_rev !== state.rev) {
        await fetchMessages(false);
      }
    } catch {
      el.sleepBanner.classList.remove("hidden");
    }
  }

  async function fetchMessages(loadOlder = false) {
    const prevIds = new Set(state.messages.map((m) => m.id));
    const prevLastId = state.messages.length ? state.messages[state.messages.length - 1].id : "";
    const wasBottom = isNearBottom();
    const prevScrollHeight = el.feedWrap.scrollHeight;

    let url = `/api/messages?limit=${state.loadedCount}`;
    if (loadOlder && state.messages.length > 0) {
      url = `/api/messages?limit=50&before=${encodeURIComponent(state.messages[0].id)}`;
    }

    const resp = await fetch(url);
    if (!resp.ok) return;
    const data = await resp.json();
    state.rev = data.rev;
    state.hasMore = Boolean(data.has_more);

    if (loadOlder) {
      state.messages = (data.messages || []).concat(state.messages);
      state.loadedCount = Math.min(200, state.messages.length);
      renderFeed();
      el.feedWrap.scrollTop = el.feedWrap.scrollHeight - prevScrollHeight;
      return;
    }

    const incoming = data.messages || [];
    let addedOlderAbove = false;
    if (prevIds.size > 0 && prevLastId) {
      for (const m of incoming) {
        if (!prevIds.has(m.id) && m.id < prevLastId) {
          addedOlderAbove = true;
          break;
        }
      }
    }

    state.messages = incoming;
    renderFeed();

    if (addedOlderAbove) {
      el.newAbovePill.classList.remove("hidden");
    }
    if (wasBottom || prevIds.size === 0) {
      scrollToBottom();
    }
  }

  function renderHeader(data) {
    el.selfBadge.textContent = `${data.device_name} (this device)`;
    el.peerBar.textContent = "";

    const offlinePeers = [];
    for (const p of state.peers) {
      const b = document.createElement("span");
      b.className = `badge ${p.online ? "badge-online" : "badge-offline"}`;
      b.textContent = p.online ? `● ${p.name} online` : `○ ${p.name} offline`;
      if (!p.online) {
        if (p.last_error) b.title = p.last_error;
        offlinePeers.push(p.name);
      }
      el.peerBar.appendChild(b);
    }

    if (data.pending_count > 0) {
      el.pendingPill.textContent = `${data.pending_count} not sent`;
      el.pendingPill.classList.remove("hidden");
    } else {
      el.pendingPill.classList.add("hidden");
    }

    const st = data.storage || { total_bytes: 0, message_count: 0, attachment_count: 0 };
    const totalLabel = formatBytes(st.total_bytes);
    el.storagePill.textContent = totalLabel;
    el.storagePill.title = st.total_bytes > 0
      ? `Chat storage: ${totalLabel} (${st.message_count} messages, ${st.attachment_count} files — mirrored on synced devices)`
      : "Chat storage: 0 B (empty)";

    const warnings = [];
    if (!data.sync_online) {
      warnings.push("Tailscale offline — local mode (messages will be saved locally until Tailscale reconnects)");
    }
    if (offlinePeers.length > 0 && data.pending_count > 0) {
      warnings.push(`${offlinePeers.join(", ")} offline — unsent items are saved locally and marked Not sent`);
    }
    if (warnings.length > 0) {
      el.offlineBanner.textContent = warnings.join(" · ");
      el.offlineBanner.classList.remove("hidden");
    } else {
      el.offlineBanner.classList.add("hidden");
    }
  }

  function renderFeed() {
    el.loadMoreBtn.classList.toggle("hidden", !state.hasMore);
    el.feed.textContent = "";

    if (state.messages.length === 0) {
      const empty = document.createElement("div");
      empty.className = "empty-state";
      empty.textContent = "No messages yet. Paste code, links, screenshots, or drop a file below.";
      el.feed.appendChild(empty);
      return;
    }

    for (const msg of state.messages) {
      el.feed.appendChild(buildMessageNode(msg));
    }
  }

  function buildMessageNode(msg) {
    const isSelf = msg.device_id === state.selfId;
    const card = document.createElement("article");
    card.className = `msg ${isSelf ? "msg-self" : ""}`;
    card.dataset.id = msg.id;

    const head = document.createElement("div");
    head.className = "msg-head";

    const author = document.createElement("span");
    author.className = "msg-author";
    author.textContent = isSelf ? `${msg.device_name} (you)` : msg.device_name;

    const meta = document.createElement("span");
    meta.className = "msg-meta";
    const ts = document.createElement("span");
    ts.textContent = formatTime(msg.created_at_ms);

    const delBtn = document.createElement("button");
    delBtn.className = "btn btn-ghost btn-sm btn-danger";
    delBtn.type = "button";
    delBtn.textContent = "Delete";
    delBtn.addEventListener("click", () => deleteMessage(msg.id));

    meta.appendChild(ts);
    meta.appendChild(delBtn);
    head.appendChild(author);
    head.appendChild(meta);
    card.appendChild(head);

    if (msg.text) {
      const body = document.createElement("div");
      body.className = "msg-body";
      renderSafeText(body, msg.text);
      card.appendChild(body);
    }

    if (msg.attachment) {
      card.appendChild(buildAttachmentNode(msg.attachment));
    }

    if (isSelf || (msg.delivery && !msg.delivery.sent_all)) {
      const foot = document.createElement("div");
      foot.className = "msg-foot";
      const badge = document.createElement("span");
      const d = msg.delivery || { sent_all: true, sending: false, pending_peers: [] };
      if (state.peers.length === 0) {
        badge.className = "status-sent";
        badge.textContent = "Saved locally";
      } else if (d.sent_all) {
        badge.className = "status-sent";
        badge.textContent = "Sent ✓";
      } else if (d.sending) {
        badge.className = "status-sending";
        badge.textContent = "Sending…";
      } else {
        badge.className = "status-unsent";
        const pending = d.pending_peers || [];
        badge.textContent = pending.length === 1 && state.peers.length === 1
          ? "Not sent (saved locally)"
          : `Not sent to ${pending.join(", ")}`;
      }
      foot.appendChild(badge);
      card.appendChild(foot);
    }

    return card;
  }

  // Safe DOM-only parser for fenced ``` code blocks and https?:// links (zero innerHTML).
  function renderSafeText(container, text) {
    const lines = text.split("\n");
    let inCode = false;
    let codeLang = "";
    let codeLines = [];
    let textLines = [];

    function flushText() {
      if (textLines.length === 0) return;
      const p = document.createElement("div");
      appendLinkedText(p, textLines.join("\n"));
      container.appendChild(p);
      textLines = [];
    }

    function flushCode() {
      const wrap = document.createElement("div");
      wrap.className = "code-block";

      const head = document.createElement("div");
      head.className = "code-head";
      const langEl = document.createElement("span");
      langEl.textContent = codeLang || "code";

      const copyBtn = document.createElement("button");
      copyBtn.className = "btn btn-ghost btn-sm";
      copyBtn.type = "button";
      copyBtn.textContent = "Copy";
      const codeStr = codeLines.join("\n");
      copyBtn.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(codeStr);
          copyBtn.textContent = "Copied!";
          setTimeout(() => (copyBtn.textContent = "Copy"), 1500);
        } catch {
          showToast("Clipboard copy failed");
        }
      });

      head.appendChild(langEl);
      head.appendChild(copyBtn);

      const pre = document.createElement("pre");
      const code = document.createElement("code");
      code.textContent = codeStr;
      pre.appendChild(code);

      wrap.appendChild(head);
      wrap.appendChild(pre);
      container.appendChild(wrap);
      codeLines = [];
      codeLang = "";
    }

    for (const line of lines) {
      const trimmed = line.trim();
      if (!inCode && trimmed.startsWith("```")) {
        flushText();
        inCode = true;
        codeLang = trimmed.slice(3).trim();
      } else if (inCode && trimmed === "```") {
        flushCode();
        inCode = false;
      } else if (inCode) {
        codeLines.push(line);
      } else {
        textLines.push(line);
      }
    }
    if (inCode) flushCode();
    flushText();
  }

  function appendLinkedText(parent, text) {
    const urlRegex = /https?:\/\/[^\s<>"']+/g;
    let last = 0;
    let match;
    while ((match = urlRegex.exec(text)) !== null) {
      if (match.index > last) {
        parent.appendChild(document.createTextNode(text.slice(last, match.index)));
      }
      const url = match[0];
      const a = document.createElement("a");
      a.href = url;
      a.target = "_blank";
      a.rel = "noopener noreferrer";
      a.textContent = url;
      parent.appendChild(a);
      last = match.index + url.length;
    }
    if (last < text.length) {
      parent.appendChild(document.createTextNode(text.slice(last)));
    }
  }

  function buildAttachmentNode(att) {
    const box = document.createElement("div");
    box.className = "attachment-box";

    const url = `/api/attachments/${encodeURIComponent(att.id)}`;
    const dlUrl = `${url}?download=1`;
    const ext = (att.stored_name || "").split(".").pop().toLowerCase();

    if (["png", "jpg", "jpeg", "gif", "webp"].includes(ext)) {
      const img = document.createElement("img");
      img.className = "attachment-img";
      img.loading = "lazy";
      img.src = url;
      img.alt = att.filename;
      box.appendChild(img);
    } else if (["mp4", "webm"].includes(ext)) {
      const vid = document.createElement("video");
      vid.className = "attachment-video";
      vid.controls = true;
      vid.preload = "metadata";
      vid.src = url;
      box.appendChild(vid);
    } else if (["mp3", "wav", "ogg", "m4a"].includes(ext)) {
      const aud = document.createElement("audio");
      aud.className = "attachment-audio";
      aud.controls = true;
      aud.preload = "metadata";
      aud.src = url;
      box.appendChild(aud);
    }

    const link = document.createElement("a");
    link.className = "file-card";
    link.href = dlUrl;
    link.download = att.filename;
    const nameSpan = document.createElement("span");
    nameSpan.textContent = att.filename;
    const sizeSpan = document.createElement("span");
    sizeSpan.className = "file-size";
    sizeSpan.textContent = formatBytes(att.size_bytes);
    link.appendChild(nameSpan);
    link.appendChild(sizeSpan);
    box.appendChild(link);
    return box;
  }

  function sendOneMessage(text, file) {
    return new Promise((resolve, reject) => {
      const fd = new FormData();
      if (text) fd.append("text", text);
      if (file) fd.append("file", file, file.name);

      const xhr = new XMLHttpRequest();
      xhr.open("POST", "/api/messages");

      if (file) {
        el.progressWrap.classList.remove("hidden");
        el.progressBar.style.width = "0%";
        xhr.upload.onprogress = (ev) => {
          if (ev.lengthComputable) {
            el.progressBar.style.width = `${Math.round((ev.loaded / ev.total) * 100)}%`;
          }
        };
      }

      xhr.onload = () => {
        el.progressWrap.classList.add("hidden");
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve();
        } else {
          reject(new Error(xhr.responseText || `HTTP ${xhr.status}`));
        }
      };
      xhr.onerror = () => {
        el.progressWrap.classList.add("hidden");
        reject(new Error("Network error contacting LocalChat"));
      };
      xhr.send(fd);
    });
  }

  async function handleSubmit(ev) {
    if (ev) ev.preventDefault();
    if (state.sending) return;
    const text = el.msgInput.value;
    const file = state.pendingFile;
    if (!text.trim() && !file) return;

    state.sending = true;
    el.sendBtn.disabled = true;
    try {
      await sendOneMessage(text, file);
      el.msgInput.value = "";
      setPendingFile(null);
      await fetchStatus(true);
      scrollToBottom();
    } catch (err) {
      showToast(err.message);
    } finally {
      state.sending = false;
      el.sendBtn.disabled = false;
    }
  }

  async function sendMultipleFiles(files) {
    if (!files || files.length === 0) return;
    if (files.length === 1) {
      setPendingFile(files[0]);
      return;
    }
    state.sending = true;
    el.sendBtn.disabled = true;
    try {
      for (const f of files) {
        await sendOneMessage("", f);
      }
      await fetchStatus(true);
      scrollToBottom();
    } catch (err) {
      showToast(err.message);
    } finally {
      state.sending = false;
      el.sendBtn.disabled = false;
    }
  }

  async function deleteMessage(id) {
    if (!confirm("Delete this message and its attachment on all devices?")) return;
    const resp = await fetch(`/api/messages/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!resp.ok) {
      showToast("Delete failed");
      return;
    }
    await fetchStatus(true);
  }

  function renderModal() {
    el.modalSelfInfo.textContent = state.syncAddr
      ? `${state.selfName} (${state.selfId}) · ${state.syncAddr}`
      : `${state.selfName} (${state.selfId}) · outbound sync`;
    el.modalToken.textContent = state.tokenRevealed ? state.workspaceToken : "••••••••••••••••";
    el.revealTokenBtn.textContent = state.tokenRevealed ? "Hide" : "Show";
    if (!el.profileNameInput.value) el.profileNameInput.value = state.selfName;

    el.modalPeerList.textContent = "";
    if (state.peers.length === 0) {
      const p = document.createElement("div");
      p.className = "file-size";
      p.textContent = "No peers configured yet.";
      el.modalPeerList.appendChild(p);
      return;
    }
    for (const peer of state.peers) {
      const row = document.createElement("div");
      row.className = "modal-peer-item";
      const info = document.createElement("div");
      info.textContent = `${peer.name} (${peer.id}) — ${peer.addr}`;
      const rm = document.createElement("button");
      rm.className = "btn btn-ghost btn-sm btn-danger";
      rm.type = "button";
      rm.textContent = "Remove";
      rm.addEventListener("click", async () => {
        const resp = await fetch(`/api/peers/${encodeURIComponent(peer.id)}`, { method: "DELETE" });
        if (resp.ok) await fetchStatus(true);
      });
      row.appendChild(info);
      row.appendChild(rm);
      el.modalPeerList.appendChild(row);
    }
  }

  // Event wiring
  el.composer.addEventListener("submit", handleSubmit);
  el.msgInput.addEventListener("keydown", (ev) => {
    if ((ev.ctrlKey || ev.metaKey) && ev.key === "Enter") {
      handleSubmit(ev);
    }
  });
  el.msgInput.addEventListener("paste", (ev) => {
    const items = ev.clipboardData && ev.clipboardData.items;
    if (!items) return;
    for (const item of items) {
      if (item.kind === "file") {
        const f = item.getAsFile();
        if (f) {
          ev.preventDefault();
          setPendingFile(f);
          return;
        }
      }
    }
  });

  el.attachBtn.addEventListener("click", () => el.fileInput.click());
  el.fileInput.addEventListener("change", () => sendMultipleFiles(Array.from(el.fileInput.files || [])));
  el.clearFileBtn.addEventListener("click", () => setPendingFile(null));

  let dragDepth = 0;
  document.addEventListener("dragenter", (ev) => {
    ev.preventDefault();
    dragDepth++;
    el.dropOverlay.classList.remove("hidden");
  });
  document.addEventListener("dragover", (ev) => ev.preventDefault());
  document.addEventListener("dragleave", () => {
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) el.dropOverlay.classList.add("hidden");
  });
  document.addEventListener("drop", (ev) => {
    ev.preventDefault();
    dragDepth = 0;
    el.dropOverlay.classList.add("hidden");
    const files = Array.from((ev.dataTransfer && ev.dataTransfer.files) || []);
    sendMultipleFiles(files);
  });

  el.loadMoreBtn.addEventListener("click", () => fetchMessages(true));
  el.newAbovePill.addEventListener("click", () => {
    el.feedWrap.scrollTop = 0;
    el.newAbovePill.classList.add("hidden");
  });
  el.feedWrap.addEventListener("scroll", () => {
    if (el.feedWrap.scrollTop < 40) el.newAbovePill.classList.add("hidden");
  });

  el.clearBtn.addEventListener("click", async () => {
    if (state.messages.length === 0) return;
    if (!confirm("Clear all messages and delete all attachments on this device and synced peers?")) return;
    el.clearBtn.disabled = true;
    try {
      const resp = await fetch("/api/messages", { method: "DELETE" });
      if (!resp.ok) {
        showToast("Clear chat failed");
        return;
      }
      state.loadedCount = 50;
      await fetchStatus(true);
    } catch {
      el.sleepBanner.classList.remove("hidden");
    } finally {
      el.clearBtn.disabled = false;
    }
  });

  el.syncBtn.addEventListener("click", async () => {
    el.syncBtn.disabled = true;
    el.syncBtn.textContent = "Syncing…";
    try {
      await fetch("/api/sync", { method: "POST" });
      await fetchStatus(true);
    } catch {
      el.sleepBanner.classList.remove("hidden");
    } finally {
      el.syncBtn.disabled = false;
      el.syncBtn.textContent = "Sync Now";
    }
  });

  el.devicesBtn.addEventListener("click", () => {
    el.addPeerErr.classList.add("hidden");
    renderModal();
    el.modal.showModal();
  });
  el.closeModalBtn.addEventListener("click", () => el.modal.close());
  el.revealTokenBtn.addEventListener("click", () => {
    state.tokenRevealed = !state.tokenRevealed;
    renderModal();
  });
  el.copyTokenBtn.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(state.workspaceToken);
      el.copyTokenBtn.textContent = "Copied!";
      setTimeout(() => (el.copyTokenBtn.textContent = "Copy"), 1500);
    } catch {
      showToast("Clipboard copy failed");
    }
  });

  el.profileForm.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    el.profileErr.classList.add("hidden");
    const payload = {
      device_name: el.profileNameInput.value.trim(),
      workspace_token: el.profileTokenInput.value.trim(),
    };
    const resp = await fetch("/api/config", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!resp.ok) {
      el.profileErr.textContent = await resp.text();
      el.profileErr.classList.remove("hidden");
      return;
    }
    el.profileTokenInput.value = "";
    await fetchStatus(true);
    renderModal();
  });

  el.addPeerForm.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    el.addPeerErr.classList.add("hidden");
    const payload = {
      id: el.peerIdInput.value.trim(),
      name: el.peerNameInput.value.trim(),
      addr: el.peerAddrInput.value.trim(),
    };
    const resp = await fetch("/api/peers", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    if (!resp.ok) {
      el.addPeerErr.textContent = await resp.text();
      el.addPeerErr.classList.remove("hidden");
      return;
    }
    el.peerIdInput.value = "";
    el.peerNameInput.value = "";
    el.peerAddrInput.value = "";
    await fetchStatus(true);
    renderModal();
  });

  // Visibility-aware polling (4s when visible) + 60s heartbeat when hidden.
  setInterval(() => {
    if (!document.hidden) fetchStatus(false);
  }, 4000);

  setInterval(() => {
    if (document.hidden) fetch("/api/heartbeat").catch(() => {});
  }, 60000);

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) fetchStatus(false);
  });
  window.addEventListener("focus", () => fetchStatus(false));

  fetchStatus(true).then(() => {
    if (state.peers.length === 0 && !el.modal.open) {
      renderModal();
      el.modal.showModal();
    }
  });
})();
