package syncer

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"localchat/internal/config"
	"localchat/internal/lifecycle"
	"localchat/internal/store"
	"localchat/internal/ulid"
)

const (
	maxRecordsBatch     = 100
	maxPushBodyBytes    = 2 << 20 // 2 MiB
	maxRecordsReqBytes  = 1 << 20 // 1 MiB
	maxConcurrentBlobs  = 2
	dialTimeout         = 2 * time.Second
	responseHdrTimeout  = 30 * time.Second
	metadataCallTimeout = 15 * time.Second
)

// PeerStatus describes the live reachability and sync status of one peer.
type PeerStatus struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Addr         string `json:"addr"`
	Online       bool   `json:"online"`
	LastSeenMs   int64  `json:"last_seen_ms"`
	LastSyncMs   int64  `json:"last_sync_ms"`
	LastError    string `json:"last_error,omitempty"`
	PendingCount int    `json:"pending_count"`
}

type peerState struct {
	mu          sync.Mutex
	online      bool
	lastSeenMs  int64
	lastSyncMs  int64
	lastError   string
	lastETag    string
	failCount   int
	nextRetryAt time.Time
}

// Syncer handles both inbound Tailscale /sync/* requests and outbound peer sync.
type Syncer struct {
	cfgMgr  *config.Manager
	store   *store.Store
	tracker *lifecycle.Tracker
	client  *http.Client
	blobSem chan struct{}

	mu         sync.RWMutex
	peers      map[string]*peerState
	sending    map[string]int // recordID -> active outbound push count
	syncOnline bool
	syncError  string
	wg         sync.WaitGroup
}

// New creates a Syncer bound to cfgMgr, st, and tracker.
func New(cfgMgr *config.Manager, st *store.Store, tracker *lifecycle.Tracker) *Syncer {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: responseHdrTimeout,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
	}
	return &Syncer{
		cfgMgr:  cfgMgr,
		store:   st,
		tracker: tracker,
		client: &http.Client{
			Timeout:   0, // Streaming-safe; metadata RPCs use per-request contexts.
			Transport: transport,
		},
		blobSem: make(chan struct{}, maxConcurrentBlobs),
		peers:   make(map[string]*peerState),
		sending: make(map[string]int),
	}
}

// SetListenerStatus records whether the local Tailscale sync listener is active.
func (s *Syncer) SetListenerStatus(online bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncOnline = online
	s.syncError = errMsg
}

// ListenerStatus returns the current Tailscale sync listener status.
func (s *Syncer) ListenerStatus() (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.syncOnline, s.syncError
}

// Wait blocks until all detached outbound pushes have completed.
func (s *Syncer) Wait() {
	s.wg.Wait()
}

// IsSending reports whether an outbound push for recordID is currently in flight.
func (s *Syncer) IsSending(recordID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sending[recordID] > 0
}

func (s *Syncer) markSending(recordID string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.sending[recordID] + delta
	if n <= 0 {
		delete(s.sending, recordID)
	} else {
		s.sending[recordID] = n
	}
}

func (s *Syncer) getPeerState(peerID string) *peerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.peers[peerID]
	if !ok {
		ps = &peerState{}
		s.peers[peerID] = ps
	}
	return ps
}

// ResetBackoff resets offline retry backoff for all peers (e.g. on local send or Sync Now).
func (s *Syncer) ResetBackoff() {
	s.mu.RLock()
	list := make([]*peerState, 0, len(s.peers))
	for _, ps := range s.peers {
		list = append(list, ps)
	}
	s.mu.RUnlock()
	for _, ps := range list {
		ps.mu.Lock()
		ps.failCount = 0
		ps.nextRetryAt = time.Time{}
		ps.mu.Unlock()
	}
}

// PeerStatuses returns a snapshot of all configured peers and their live state.
func (s *Syncer) PeerStatuses() []PeerStatus {
	peers := s.cfgMgr.Peers()
	out := make([]PeerStatus, 0, len(peers))
	for _, p := range peers {
		ps := s.getPeerState(p.ID)
		ps.mu.Lock()
		st := PeerStatus{
			ID:         p.ID,
			Name:       p.Name,
			Addr:       p.Addr,
			Online:     ps.online,
			LastSeenMs: ps.lastSeenMs,
			LastSyncMs: ps.lastSyncMs,
			LastError:  ps.lastError,
		}
		ps.mu.Unlock()
		st.PendingCount = s.countPendingForPeer(p)
		out = append(out, st)
	}
	return out
}

func (s *Syncer) countPendingForPeer(p config.Peer) int {
	return s.store.PendingCount([]config.Peer{p})
}

// NewHandler returns the authenticated HTTP handler for the Tailscale sync port.
func (s *Syncer) NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sync/manifest", s.handleManifest)
	mux.HandleFunc("POST /sync/records", s.handleRecords)
	mux.HandleFunc("GET /sync/attachments/{id}", s.handleGetAttachment)
	mux.HandleFunc("PUT /sync/attachments/{id}", s.handlePutAttachment)
	mux.HandleFunc("POST /sync/push", s.handlePush)

	return s.authMiddleware(mux)
}

func (s *Syncer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.cfgMgr.Snapshot().WorkspaceToken
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got := auth[len(prefix):]
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Syncer) handleManifest(w http.ResponseWriter, r *http.Request) {
	m := s.store.Manifest()
	w.Header().Set("Etag", m.ETag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == m.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

func (s *Syncer) handleRecords(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRecordsReqBytes)
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.IDs) > maxRecordsBatch {
		http.Error(w, fmt.Sprintf("max %d ids per request", maxRecordsBatch), http.StatusBadRequest)
		return
	}
	recs := s.store.RecordsByID(req.IDs)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Records []store.Record `json:"records"`
	}{Records: recs})
}

func (s *Syncer) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
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

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Attachment-SHA256", meta.SHA256)
	w.Header().Set("X-Attachment-Size", strconv.FormatInt(meta.SizeBytes, 10))
	w.Header().Set("Etag", fmt.Sprintf("%q", meta.SHA256))
	http.ServeContent(w, r, meta.StoredName, info.ModTime(), f)
}

func (s *Syncer) handlePutAttachment(w http.ResponseWriter, r *http.Request) {
	s.tracker.BeginTransfer()
	defer s.tracker.EndTransfer()

	id := r.PathValue("id")
	month := r.URL.Query().Get("month")
	ext := r.URL.Query().Get("ext")
	shaHex := r.Header.Get("X-Attachment-SHA256")
	sizeStr := r.Header.Get("X-Attachment-Size")

	if !ulid.IsValid(id) || !store.ValidateMonthAndExt(month, ext) {
		http.Error(w, "invalid attachment parameters", http.StatusBadRequest)
		return
	}
	expectedSize, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || expectedSize < 0 {
		http.Error(w, "invalid X-Attachment-Size", http.StatusBadRequest)
		return
	}
	maxBytes := s.cfgMgr.Snapshot().MaxUploadBytes
	if expectedSize > maxBytes {
		http.Error(w, "attachment exceeds max size", http.StatusRequestEntityTooLarge)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024)
	err = s.store.SaveSyncedAttachment(id, month, ext, expectedSize, shaHex, r.Body, maxBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.Is(err, store.ErrTooLarge) || errors.As(err, &maxErr):
			http.Error(w, "attachment exceeds max size", http.StatusRequestEntityTooLarge)
		case errors.Is(err, store.ErrChecksumMismatch):
			http.Error(w, "checksum or size mismatch", http.StatusUnprocessableEntity)
		case errors.Is(err, store.ErrInvalidRecord):
			http.Error(w, "invalid attachment metadata", http.StatusBadRequest)
		default:
			slog.Error("save synced attachment failed", "id", id, "err", err)
			http.Error(w, "internal storage error", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Syncer) handlePush(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)
	var rec store.Record
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		http.Error(w, "invalid record json", http.StatusBadRequest)
		return
	}
	_, err := s.store.IngestRemoteRecord(rec)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrMissingAttachment):
			http.Error(w, "referenced attachment missing", http.StatusConflict)
		case errors.Is(err, store.ErrTooLarge):
			http.Error(w, "record too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, store.ErrInvalidRecord):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			slog.Error("ingest remote record failed", "id", rec.ID, "err", err)
			http.Error(w, "internal storage error", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}

// PushRecordAsync queues an asynchronous push of rec to all configured peers.
func (s *Syncer) PushRecordAsync(rec store.Record) {
	s.ResetBackoff()
	s.wg.Add(1)
	s.tracker.BeginTransfer()
	s.markSending(rec.ID, 1)
	go func() {
		defer s.wg.Done()
		defer s.tracker.EndTransfer()
		defer s.markSending(rec.ID, -1)
		s.PushRecordToAll(context.Background(), rec)
	}()
}

// SyncAllPeersAsync triggers a forced catch-up sync across all peers asynchronously
// (used after bulk operations like ClearAll to propagate tombstones promptly).
func (s *Syncer) SyncAllPeersAsync() {
	s.ResetBackoff()
	s.wg.Add(1)
	s.tracker.BeginTransfer()
	go func() {
		defer s.wg.Done()
		defer s.tracker.EndTransfer()
		s.SyncAllPeers(context.Background(), true)
	}()
}

// PushRecordToAll concurrently pushes a single record (and its attachment if any)
// to every configured peer.
func (s *Syncer) PushRecordToAll(ctx context.Context, rec store.Record) {
	peers := s.cfgMgr.Peers()
	if len(peers) == 0 {
		return
	}
	s.ResetBackoff()
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(p config.Peer) {
			defer wg.Done()
			ps := s.getPeerState(p.ID)
			ps.mu.Lock()
			defer ps.mu.Unlock()

			if err := s.pushRecordLocked(ctx, p, rec); err != nil {
				s.recordPeerFailureLocked(ps, err)
				return
			}
			nowMs := time.Now().UnixMilli()
			ps.online = true
			ps.lastSeenMs = nowMs
			ps.lastSyncMs = nowMs
			ps.lastError = ""
			ps.failCount = 0
			ps.nextRetryAt = time.Time{}
			_ = s.store.MarkAckedBatch(p.ID, []string{rec.ID})
		}(peer)
	}
	wg.Wait()
}

// SyncAllPeers runs bidirectional catch-up sync against every configured peer.
// If force is false, offline peers currently in backoff are skipped.
func (s *Syncer) SyncAllPeers(ctx context.Context, force bool) {
	if force {
		s.ResetBackoff()
	}
	peers := s.cfgMgr.Peers()
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(p config.Peer) {
			defer wg.Done()
			_ = s.SyncPeer(ctx, p, force)
		}(peer)
	}
	wg.Wait()
}

// SyncPeer executes a single-flight bidirectional catch-up sync with peer p.
func (s *Syncer) SyncPeer(ctx context.Context, p config.Peer, force bool) error {
	ps := s.getPeerState(p.ID)
	if !ps.mu.TryLock() {
		// Another sync/push is already running for this peer.
		return nil
	}
	defer ps.mu.Unlock()

	if !force && !ps.nextRetryAt.IsZero() && time.Now().Before(ps.nextRetryAt) {
		return nil
	}

	s.tracker.BeginTransfer()
	defer s.tracker.EndTransfer()

	hasPending := s.countPendingForPeer(p) > 0
	useETag := ""
	if !force && !hasPending {
		useETag = ps.lastETag
	}

	manifest, notModified, err := s.fetchManifest(ctx, p, useETag)
	if err != nil {
		s.recordPeerFailureLocked(ps, err)
		return err
	}

	selfID := s.cfgMgr.Snapshot().DeviceID
	if !notModified && manifest.DeviceID == selfID {
		err := errors.New("remote peer reports same device_id as local device")
		s.recordPeerFailureLocked(ps, err)
		return err
	}

	nowMs := time.Now().UnixMilli()
	ps.online = true
	ps.lastSeenMs = nowMs
	ps.lastError = ""
	ps.failCount = 0
	ps.nextRetryAt = time.Time{}

	if notModified {
		ps.lastSyncMs = nowMs
		return nil
	}

	ps.lastETag = manifest.ETag

	peerRecSet := make(map[string]struct{}, len(manifest.RecordIDs))
	for _, id := range manifest.RecordIDs {
		peerRecSet[id] = struct{}{}
	}
	peerTombSet := make(map[string]struct{}, len(manifest.TombstonedIDs))
	for _, id := range manifest.TombstonedIDs {
		peerTombSet[id] = struct{}{}
	}

	// 1. Identify remote records we do not have locally (excluding locally tombstoned messages).
	var missingRemoteIDs []string
	for _, id := range manifest.RecordIDs {
		if s.store.HasRecord(id) || s.store.IsTombstoned(id) {
			continue
		}
		missingRemoteIDs = append(missingRemoteIDs, id)
	}

	// 2. Pull missing remote records in chunks of 100; apply tombstones before messages.
	var pulledMessages []store.Record
	for i := 0; i < len(missingRemoteIDs); i += maxRecordsBatch {
		end := i + maxRecordsBatch
		if end > len(missingRemoteIDs) {
			end = len(missingRemoteIDs)
		}
		recs, ferr := s.fetchRecords(ctx, p, missingRemoteIDs[i:end])
		if ferr != nil {
			s.recordPeerFailureLocked(ps, ferr)
			return ferr
		}
		for _, rec := range recs {
			if rec.Kind == store.KindTombstone {
				if _, ierr := s.store.IngestRemoteRecord(rec); ierr != nil {
					slog.Warn("failed to ingest remote tombstone", "id", rec.ID, "err", ierr)
				}
			} else {
				pulledMessages = append(pulledMessages, rec)
			}
		}
	}

	for _, rec := range pulledMessages {
		if s.store.IsTombstoned(rec.ID) {
			continue
		}
		if rec.Attachment != nil {
			if _, _, rerr := s.store.ResolveAttachment(rec.Attachment.ID); rerr != nil {
				if derr := s.pullAttachment(ctx, p, *rec.Attachment); derr != nil {
					slog.Warn("failed to pull remote attachment", "id", rec.Attachment.ID, "err", derr)
					continue
				}
			}
		}
		if _, ierr := s.store.IngestRemoteRecord(rec); ierr != nil {
			slog.Warn("failed to ingest remote message", "id", rec.ID, "err", ierr)
		}
	}

	// 3. Push unsent local tombstones and live messages to peer.
	unsent := s.store.UnsentForPeer(p.ID, peerRecSet, peerTombSet)
	ackedIDs := make([]string, 0, len(unsent)+len(manifest.RecordIDs))

	// Reconcile local records already present in peer's manifest.
	for _, id := range manifest.RecordIDs {
		if s.store.HasRecord(id) {
			ackedIDs = append(ackedIDs, id)
		}
	}

	for _, rec := range unsent {
		if perr := s.pushRecordLocked(ctx, p, rec); perr != nil {
			s.recordPeerFailureLocked(ps, perr)
			_ = s.store.MarkAckedBatch(p.ID, ackedIDs)
			return perr
		}
		ackedIDs = append(ackedIDs, rec.ID)
	}

	_ = s.store.MarkAckedBatch(p.ID, ackedIDs)
	ps.lastSyncMs = time.Now().UnixMilli()
	return nil
}

func (s *Syncer) recordPeerFailureLocked(ps *peerState, err error) {
	ps.online = false
	ps.lastError = shortErr(err)
	ps.failCount++
	var delay time.Duration
	switch {
	case ps.failCount == 1:
		delay = 10 * time.Second
	case ps.failCount == 2:
		delay = 30 * time.Second
	default:
		delay = 60 * time.Second
	}
	ps.nextRetryAt = time.Now().Add(delay)
}

func (s *Syncer) pushRecordLocked(ctx context.Context, p config.Peer, rec store.Record) error {
	if rec.Kind == store.KindMessage && rec.Attachment != nil {
		if err := s.pushAttachment(ctx, p, *rec.Attachment); err != nil {
			return err
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, metadataCallTimeout)
	defer cancel()

	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, fmt.Sprintf("http://%s/sync/push", p.Addr), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfgMgr.Snapshot().WorkspaceToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer push returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Syncer) pushAttachment(ctx context.Context, p config.Peer, meta store.AttachmentMeta) error {
	select {
	case s.blobSem <- struct{}{}:
		defer func() { <-s.blobSem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	path, _, err := s.store.ResolveAttachment(meta.ID)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	ext := store.ExtractSafeExt(meta.StoredName)
	q := url.Values{}
	q.Set("month", meta.Month)
	q.Set("ext", ext)
	u := fmt.Sprintf("http://%s/sync/attachments/%s?%s", p.Addr, meta.ID, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, f)
	if err != nil {
		return err
	}
	req.ContentLength = meta.SizeBytes
	req.Header.Set("Authorization", "Bearer "+s.cfgMgr.Snapshot().WorkspaceToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Attachment-SHA256", meta.SHA256)
	req.Header.Set("X-Attachment-Size", strconv.FormatInt(meta.SizeBytes, 10))

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer attachment upload returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Syncer) pullAttachment(ctx context.Context, p config.Peer, meta store.AttachmentMeta) error {
	select {
	case s.blobSem <- struct{}{}:
		defer func() { <-s.blobSem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	u := fmt.Sprintf("http://%s/sync/attachments/%s", p.Addr, meta.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfgMgr.Snapshot().WorkspaceToken)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("peer attachment download returned HTTP %d", resp.StatusCode)
	}
	ext := store.ExtractSafeExt(meta.StoredName)
	maxBytes := s.cfgMgr.Snapshot().MaxUploadBytes
	return s.store.SaveSyncedAttachment(meta.ID, meta.Month, ext, meta.SizeBytes, meta.SHA256, resp.Body, maxBytes)
}

func (s *Syncer) fetchManifest(ctx context.Context, p config.Peer, etag string) (store.Manifest, bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, metadataCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, fmt.Sprintf("http://%s/sync/manifest", p.Addr), nil)
	if err != nil {
		return store.Manifest{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfgMgr.Snapshot().WorkspaceToken)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return store.Manifest{}, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return store.Manifest{ETag: etag}, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return store.Manifest{}, false, fmt.Errorf("manifest returned HTTP %d", resp.StatusCode)
	}
	var m store.Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&m); err != nil {
		return store.Manifest{}, false, err
	}
	m.ETag = resp.Header.Get("Etag")
	return m, false, nil
}

func (s *Syncer) fetchRecords(ctx context.Context, p config.Peer, ids []string) ([]store.Record, error) {
	callCtx, cancel := context.WithTimeout(ctx, metadataCallTimeout)
	defer cancel()

	body, err := json.Marshal(struct {
		IDs []string `json:"ids"`
	}{IDs: ids})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, fmt.Sprintf("http://%s/sync/records", p.Addr), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfgMgr.Snapshot().WorkspaceToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("records returned HTTP %d", resp.StatusCode)
	}
	var res struct {
		Records []store.Record `json:"records"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&res); err != nil {
		return nil, err
	}
	return res.Records, nil
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") {
		return "chat closed on peer"
	}
	if strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "deadline exceeded") {
		return "device offline or unreachable"
	}
	if len(msg) > 96 {
		return msg[:96]
	}
	return msg
}
