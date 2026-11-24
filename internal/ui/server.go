package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"localchat/internal/config"
	"localchat/internal/lifecycle"
	"localchat/internal/store"
	"localchat/internal/syncer"
)

//go:embed web/*
var webFS embed.FS

const (
	cspHeader = "default-src 'self'; media-src 'self'; img-src 'self' blob: data:; style-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'"
)

// Allowed inline media mappings: both sanitized extension AND magic-byte sniffed MIME must match.
var inlineExtToMIME = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"mp4":  "video/mp4",
	"webm": "video/webm",
	"mp3":  "audio/mpeg",
	"wav":  "audio/wav",
	"ogg":  "audio/ogg",
	"m4a":  "audio/mp4",
}

// Server serves the localhost embedded web UI and local JSON/upload APIs.
type Server struct {
	cfgMgr       *config.Manager
	store        *store.Store
	syncer       *syncer.Syncer
	tracker      *lifecycle.Tracker
	onSyncNow    func()
	startTime    time.Time
	allowAnyPort bool // Used only in httptest unit tests with ephemeral ports
}

// NewServer constructs the UI HTTP handler.
func NewServer(cfgMgr *config.Manager, st *store.Store, syn *syncer.Syncer, tracker *lifecycle.Tracker, onSyncNow func()) *Server {
	return &Server{
		cfgMgr:    cfgMgr,
		store:     st,
		syncer:    syn,
		tracker:   tracker,
		onSyncNow: onSyncNow,
		startTime: time.Now().UTC(),
	}
}

// Handler returns the wrapped localhost HTTP handler with security middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleAsset("web/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /style.css", s.handleAsset("web/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.handleAsset("web/app.js", "application/javascript; charset=utf-8"))

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/messages", s.handleListMessages)
	mux.HandleFunc("POST /api/messages", s.handleCreateMessage)
	mux.HandleFunc("DELETE /api/messages", s.handleClearMessages)
	mux.HandleFunc("DELETE /api/messages/{id}", s.handleDeleteMessage)
	mux.HandleFunc("GET /api/attachments/{id}", s.handleGetAttachment)
	mux.HandleFunc("GET /api/peers", s.handleGetPeers)
	mux.HandleFunc("POST /api/peers", s.handleAddPeer)
	mux.HandleFunc("DELETE /api/peers/{id}", s.handleDeletePeer)
	mux.HandleFunc("POST /api/sync", s.handleSyncNow)

	return s.securityMiddleware(mux)
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if !s.isAllowedHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}

		// Fail-closed CSRF check on all mutating endpoints.
		if r.Method == http.MethodPost || r.Method == http.MethodDelete || r.Method == http.MethodPut {
			if !isSameOriginMutation(r) {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
		}

		// Touch local UI activity on every UI request except /api/health probes.
		if r.URL.Path != "/api/health" {
			s.tracker.TouchUI()
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) isAllowedHost(reqHost string) bool {
	if reqHost == "" {
		return false
	}
	host, port, err := net.SplitHostPort(reqHost)
	if err != nil {
		host = reqHost
		port = ""
	}
	lowerHost := strings.ToLower(host)
	if lowerHost != "127.0.0.1" && lowerHost != "localhost" {
		return false
	}
	if s.allowAnyPort {
		return true
	}
	_, cfgPort, err := net.SplitHostPort(s.cfgMgr.Snapshot().UIAddr)
	if err != nil {
		return false
	}
	return port == cfgPort
}

func isSameOriginMutation(r *http.Request) bool {
	sfs := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
	origin := strings.TrimSpace(r.Header.Get("Origin"))

	if sfs != "" {
		if sfs != "same-origin" {
			return false
		}
		if origin == "" {
			return true
		}
	}
	if origin == "" {
		// Fail closed when both Sec-Fetch-Site and Origin are absent.
		return false
	}
	expected := "http://" + strings.ToLower(r.Host)
	return strings.ToLower(origin) == expected
}

func (s *Server) handleAsset(path, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(webFS, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Security-Policy", cspHeader)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, path, s.startTime, strings.NewReader(string(data)))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"app": "localchat",
		"ok":  true,
	})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := s.cfgMgr.Snapshot()
	syncOnline, syncErr := s.syncer.ListenerStatus()
	peers := s.syncer.PeerStatuses()
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":        cfg.DeviceID,
		"device_name":      cfg.DeviceName,
		"sync_addr":        cfg.SyncAddr,
		"workspace_token":  cfg.WorkspaceToken,
		"max_upload_bytes": cfg.MaxUploadBytes,
		"store_rev":        s.store.Revision(),
		"sync_online":      syncOnline,
		"sync_error":       syncErr,
		"pending_count":    s.store.PendingCount(cfg.Peers),
		"storage":          s.store.StorageStats(),
		"peers":            peers,
	})
}

type uiMessage struct {
	store.Record
	Delivery uiDelivery `json:"delivery"`
}

type uiDelivery struct {
	SentAll      bool     `json:"sent_all"`
	Sending      bool     `json:"sending"`
	AckedBy      []string `json:"acked_by"`
	PendingPeers []string `json:"pending_peers"`
}

func (s *Server) annotateMessage(rec store.Record, peers []config.Peer) uiMessage {
	d := s.store.DeliveryInfo(rec, peers)
	return uiMessage{
		Record: rec,
		Delivery: uiDelivery{
			SentAll:      d.SentAll,
			Sending:      s.syncer.IsSending(rec.ID),
			AckedBy:      d.AckedBy,
			PendingPeers: d.PendingPeers,
		},
	}
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	before := r.URL.Query().Get("before")
	recs, hasMore, rev := s.store.ListMessages(limit, before)
	peers := s.cfgMgr.Peers()

	msgs := make([]uiMessage, 0, len(recs))
	for _, rec := range recs {
		msgs = append(msgs, s.annotateMessage(rec, peers))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rev":      rev,
		"has_more": hasMore,
		"messages": msgs,
	})
}

func (s *Server) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	s.tracker.BeginTransfer()
	defer s.tracker.EndTransfer()

	cfg := s.cfgMgr.Snapshot()
	var (
		text    string
		attMeta *store.AttachmentMeta
	)

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		// Add 2 MiB framing headroom on raw request body; exact file cap enforced in SaveUploadedAttachment.
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxUploadBytes+(2<<20))
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "invalid multipart request", http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				s.store.RemoveStagedAttachment(attMeta)
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					http.Error(w, "upload exceeds max file size", http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, "failed reading multipart upload", http.StatusBadRequest)
				return
			}

			switch part.FormName() {
			case "text":
				raw, rerr := io.ReadAll(io.LimitReader(part, store.MaxTextBytes+1))
				_ = part.Close()
				if rerr != nil {
					s.store.RemoveStagedAttachment(attMeta)
					http.Error(w, "failed reading text field", http.StatusBadRequest)
					return
				}
				if len(raw) > store.MaxTextBytes {
					s.store.RemoveStagedAttachment(attMeta)
					http.Error(w, "message text exceeds 1 MiB limit", http.StatusRequestEntityTooLarge)
					return
				}
				text = string(raw)
			case "file":
				if attMeta != nil {
					_ = part.Close()
					continue
				}
				filename := part.FileName()
				if filename == "" {
					filename = "attachment.bin"
				}
				meta, serr := s.store.SaveUploadedAttachment(part, filename, cfg.MaxUploadBytes)
				_ = part.Close()
				if serr != nil {
					var maxErr *http.MaxBytesError
					if errors.Is(serr, store.ErrTooLarge) || errors.As(serr, &maxErr) {
						http.Error(w, "file exceeds max upload size", http.StatusRequestEntityTooLarge)
						return
					}
					slog.Error("save upload failed", "err", serr)
					http.Error(w, "failed saving attachment", http.StatusInternalServerError)
					return
				}
				attMeta = meta
			default:
				_ = part.Close()
			}
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, store.MaxTextBytes+4096)
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "message exceeds max size", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid JSON payload", http.StatusBadRequest)
			return
		}
		text = payload.Text
	}

	rec, err := s.store.CreateLocalMessage(text, attMeta)
	if err != nil {
		s.store.RemoveStagedAttachment(attMeta)
		if errors.Is(err, store.ErrTooLarge) {
			http.Error(w, "message text too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// For text-only messages, allow a brief 250ms push window so fast Tailscale links
	// return Sent immediately; large attachments always push asynchronously.
	if attMeta == nil && len(cfg.Peers) > 0 {
		done := make(chan struct{})
		go func() {
			s.syncer.PushRecordToAll(context.Background(), rec)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
		}
	} else if len(cfg.Peers) > 0 {
		s.syncer.PushRecordAsync(rec)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"message": s.annotateMessage(rec, s.cfgMgr.Peers()),
		"rev":     s.store.Revision(),
	})
}

func (s *Server) handleClearMessages(w http.ResponseWriter, _ *http.Request) {
	tombs, err := s.store.ClearAll()
	if err != nil {
		slog.Error("clear chat failed", "err", err)
		http.Error(w, "failed to clear chat", http.StatusInternalServerError)
		return
	}
	if len(tombs) > 0 {
		s.syncer.SyncAllPeersAsync()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cleared_count": len(tombs),
		"rev":           s.store.Revision(),
	})
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tomb, created, err := s.store.CreateLocalTombstone(id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("delete message failed", "id", id, "err", err)
		http.Error(w, "failed to delete message", http.StatusInternalServerError)
		return
	}
	if created {
		s.syncer.PushRecordAsync(tomb)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted_id":   id,
		"tombstone_id": tomb.ID,
		"rev":          s.store.Revision(),
	})
}

func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	s.tracker.BeginTransfer()
	defer s.tracker.EndTransfer()

	id := r.PathValue("id")
	path, meta, err := s.store.ResolveAttachment(id)
	if err != nil {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}

	// Verify magic bytes before allowing inline rendering.
	var head [512]byte
	n, _ := f.Read(head[:])
	_, _ = f.Seek(0, io.SeekStart)
	sniffed := http.DetectContentType(head[:n])
	ext := store.ExtractSafeExt(meta.StoredName)

	forceDownload := r.URL.Query().Get("download") == "1"
	inlineMIME, allowInline := verifiedInlineMIME(ext, sniffed)

	displayName := meta.Filename
	if displayName == "" {
		displayName = meta.StoredName
	}

	w.Header().Set("Etag", fmt.Sprintf("%q", meta.SHA256))
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if allowInline && !forceDownload {
		w.Header().Set("Content-Type", inlineMIME)
		if cd := mime.FormatMediaType("inline", map[string]string{"filename": displayName}); cd != "" {
			w.Header().Set("Content-Disposition", cd)
		}
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Security-Policy", "sandbox")
		if cd := mime.FormatMediaType("attachment", map[string]string{"filename": displayName}); cd != "" {
			w.Header().Set("Content-Disposition", cd)
		} else {
			w.Header().Set("Content-Disposition", "attachment")
		}
	}

	http.ServeContent(w, r, displayName, info.ModTime(), f)
}

func verifiedInlineMIME(ext, sniffed string) (string, bool) {
	expected, ok := inlineExtToMIME[ext]
	if !ok {
		return "", false
	}
	// Strip charset parameters from sniffed MIME.
	baseSniffed := strings.TrimSpace(strings.SplitN(sniffed, ";", 2)[0])
	// Explicitly block HTML/SVG/XML/script disguised with a media extension.
	if strings.Contains(baseSniffed, "html") || strings.Contains(baseSniffed, "xml") || strings.Contains(baseSniffed, "svg") || strings.Contains(baseSniffed, "javascript") {
		return "", false
	}
	switch {
	case strings.HasPrefix(expected, "image/"):
		return expected, baseSniffed == expected
	case expected == "video/mp4" || expected == "audio/mp4":
		return expected, baseSniffed == "video/mp4" || baseSniffed == "audio/mp4" || baseSniffed == "application/octet-stream"
	case expected == "video/webm":
		return expected, baseSniffed == "video/webm" || baseSniffed == "application/octet-stream"
	case expected == "audio/mpeg":
		return expected, baseSniffed == "audio/mpeg" || baseSniffed == "application/octet-stream"
	case expected == "audio/wav":
		return expected, baseSniffed == "audio/wave" || baseSniffed == "audio/wav" || baseSniffed == "audio/x-wav"
	case expected == "audio/ogg":
		return expected, baseSniffed == "application/ogg" || baseSniffed == "audio/ogg"
	default:
		return "", false
	}
}

func (s *Server) handleGetPeers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"peers": s.syncer.PeerStatuses(),
	})
}

func (s *Server) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var req config.Peer
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid peer json", http.StatusBadRequest)
		return
	}
	added, err := s.cfgMgr.AddPeer(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	go func() {
		_ = s.syncer.SyncPeer(context.Background(), added, true)
	}()
	writeJSON(w, http.StatusCreated, map[string]any{
		"peer":  added,
		"peers": s.syncer.PeerStatuses(),
	})
}

func (s *Server) handleDeletePeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	removed, err := s.cfgMgr.RemovePeer(id)
	if err != nil {
		http.Error(w, "failed to save config", http.StatusInternalServerError)
		return
	}
	if !removed {
		http.Error(w, "peer not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"removed_id": id,
		"peers":      s.syncer.PeerStatuses(),
	})
}

func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if s.onSyncNow != nil {
		s.onSyncNow()
	}
	s.syncer.SyncAllPeers(r.Context(), true)
	writeJSON(w, http.StatusOK, map[string]any{
		"store_rev":     s.store.Revision(),
		"peers":         s.syncer.PeerStatuses(),
		"pending_count": s.store.PendingCount(s.cfgMgr.Peers()),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
