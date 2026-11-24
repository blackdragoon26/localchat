package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localchat/internal/config"
	"localchat/internal/lifecycle"
	"localchat/internal/store"
)

type testNode struct {
	id     string
	dir    string
	cfgMgr *config.Manager
	store  *store.Store
	syncer *Syncer
	srv    *httptest.Server
}

func newTestNode(t *testing.T, id, name, token string) *testNode {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		DeviceID:       id,
		DeviceName:     name,
		UIAddr:         "127.0.0.1:8787",
		SyncAddr:       "127.0.0.1:0",
		WorkspaceToken: token,
		MaxUploadBytes: 10 << 20,
		IdleTimeoutSec: 300,
		Peers:          []config.Peer{},
	}
	mgr := config.NewManager(filepath.Join(dir, "config.json"), cfg)
	mgr.AllowLoopbackPeers = true

	st, err := store.Open(dir, id, name)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", id, err)
	}
	tracker := lifecycle.NewTracker(5*time.Minute, false)
	syn := New(mgr, st, tracker)
	srv := httptest.NewServer(syn.NewHandler())
	t.Cleanup(srv.Close)

	return &testNode{
		id:     id,
		dir:    dir,
		cfgMgr: mgr,
		store:  st,
		syncer: syn,
		srv:    srv,
	}
}

func (n *testNode) addr() string {
	return strings.TrimPrefix(n.srv.URL, "http://")
}

func TestLivePushOfflineCatchupTombstoneAndThirdPeer(t *testing.T) {
	token, err := config.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	ubuntu := newTestNode(t, "ubuntu", "Ubuntu", token)
	mac := newTestNode(t, "mac", "Mac", token)

	// Pair Ubuntu <-> Mac.
	if _, err := ubuntu.cfgMgr.AddPeer(config.Peer{ID: "mac", Name: "Mac", Addr: mac.addr()}); err != nil {
		t.Fatalf("ubuntu AddPeer mac: %v", err)
	}
	if _, err := mac.cfgMgr.AddPeer(config.Peer{ID: "ubuntu", Name: "Ubuntu", Addr: ubuntu.addr()}); err != nil {
		t.Fatalf("mac AddPeer ubuntu: %v", err)
	}

	// 1. Live push with attachment from Ubuntu -> Mac.
	blob := []byte("binary png-ish payload for sync test")
	att, err := ubuntu.store.SaveUploadedAttachment(bytes.NewReader(blob), "screen.png", 10<<20)
	if err != nil {
		t.Fatalf("SaveUploadedAttachment: %v", err)
	}
	msg1, err := ubuntu.store.CreateLocalMessage("hello from ubuntu", att)
	if err != nil {
		t.Fatalf("CreateLocalMessage: %v", err)
	}
	ubuntu.syncer.PushRecordToAll(context.Background(), msg1)

	if d := ubuntu.store.DeliveryInfo(msg1, ubuntu.cfgMgr.Peers()); !d.SentAll {
		t.Fatalf("expected SentAll=true after live push, got %+v", d)
	}
	macMsgs, _, _ := mac.store.ListMessages(50, "")
	if len(macMsgs) != 1 || macMsgs[0].ID != msg1.ID || macMsgs[0].Attachment == nil {
		t.Fatalf("expected msg1 with attachment on Mac, got %+v", macMsgs)
	}
	macAttPath, _, err := mac.store.ResolveAttachment(att.ID)
	if err != nil {
		t.Fatalf("ResolveAttachment on Mac: %v", err)
	}
	gotBlob, _ := os.ReadFile(macAttPath)
	if !bytes.Equal(gotBlob, blob) {
		t.Fatalf("attachment content mismatch on Mac")
	}

	// 2. Offline send: point Ubuntu at a closed peer port, verify Not sent, then restore & catch up.
	secretText := "SECRET_TOKEN=delete-me-before-peer3"
	msgSecret, err := ubuntu.store.CreateLocalMessage(secretText, nil)
	if err != nil {
		t.Fatalf("create secret msg: %v", err)
	}
	// Do not push yet: verify PendingCount == 1 before catch-up.
	if got := ubuntu.store.PendingCount(ubuntu.cfgMgr.Peers()); got != 1 {
		t.Fatalf("expected 1 pending message before catch-up, got %d", got)
	}

	// Catch-up sync delivers msgSecret and flips PendingCount -> 0.
	ubuntu.syncer.SyncAllPeers(context.Background(), true)
	if got := ubuntu.store.PendingCount(ubuntu.cfgMgr.Peers()); got != 0 {
		t.Fatalf("expected 0 pending after catch-up, got %d", got)
	}
	if !mac.store.HasRecord(msgSecret.ID) {
		t.Fatalf("expected msgSecret synced to Mac")
	}

	// 3. Delete secret on Ubuntu -> sync tombstone -> Mac scrubs secret too.
	tomb, _, err := ubuntu.store.CreateLocalTombstone(msgSecret.ID)
	if err != nil {
		t.Fatalf("CreateLocalTombstone: %v", err)
	}
	ubuntu.syncer.PushRecordToAll(context.Background(), tomb)
	if !mac.store.IsTombstoned(msgSecret.ID) {
		t.Fatalf("expected msgSecret tombstoned on Mac")
	}
	macMsgs, _, _ = mac.store.ListMessages(50, "")
	if len(macMsgs) != 1 || macMsgs[0].ID != msg1.ID {
		t.Fatalf("expected only msg1 remaining on Mac, got %+v", macMsgs)
	}

	// 4. Add a 3rd device (desktop) and sync: should pull msg1 + tombstone, NEVER secretText.
	desktop := newTestNode(t, "desktop", "Desktop", token)
	if _, err := desktop.cfgMgr.AddPeer(config.Peer{ID: "ubuntu", Name: "Ubuntu", Addr: ubuntu.addr()}); err != nil {
		t.Fatalf("desktop AddPeer ubuntu: %v", err)
	}
	desktop.syncer.SyncAllPeers(context.Background(), true)

	deskMsgs, _, _ := desktop.store.ListMessages(50, "")
	if len(deskMsgs) != 1 || deskMsgs[0].ID != msg1.ID {
		t.Fatalf("expected 3rd peer to pull only live msg1, got %+v", deskMsgs)
	}
	if !desktop.store.IsTombstoned(msgSecret.ID) {
		t.Fatalf("expected 3rd peer to record tombstone for deleted secret")
	}

	// Verify Ubuntu automatically surfaces outbound-only 3rd peer ("desktop") as online via inbound sync presence.
	foundDesktop := false
	for _, ps := range ubuntu.syncer.PeerStatuses() {
		if ps.ID == "desktop" && ps.Online && ps.InboundOnly {
			foundDesktop = true
		}
	}
	if !foundDesktop {
		t.Fatalf("expected outbound-only peer 'desktop' to appear online in Ubuntu PeerStatuses(), got %+v", ubuntu.syncer.PeerStatuses())
	}

	// 5. Verify both synced devices report identical non-zero StorageStats before ClearAll.
	ubStats := ubuntu.store.StorageStats()
	macStats := mac.store.StorageStats()
	if ubStats != macStats || ubStats.TotalBytes == 0 || ubStats.AttachmentBytes != int64(len(blob)) {
		t.Fatalf("expected identical non-zero StorageStats on Ubuntu (%+v) and Mac (%+v)", ubStats, macStats)
	}

	// 6. ClearAll on Mac propagates tombstones, deletes attachments, and resets StorageStats to 0 on both.
	tombs, err := mac.store.ClearAll()
	if err != nil || len(tombs) != 1 {
		t.Fatalf("mac ClearAll: len=%d err=%v", len(tombs), err)
	}
	mac.syncer.SyncAllPeers(context.Background(), true)
	ubMsgs, _, _ := ubuntu.store.ListMessages(50, "")
	if len(ubMsgs) != 0 {
		t.Fatalf("expected 0 live messages on Ubuntu after Mac ClearAll sync, got %d", len(ubMsgs))
	}
	if _, _, err := ubuntu.store.ResolveAttachment(att.ID); err == nil {
		t.Fatalf("expected attachment blob removed on Ubuntu after Mac ClearAll sync")
	}
	if got := ubuntu.store.StorageStats(); got.TotalBytes != 0 {
		t.Fatalf("expected 0 TotalBytes on Ubuntu after ClearAll sync, got %+v", got)
	}
	if got := mac.store.StorageStats(); got.TotalBytes != 0 {
		t.Fatalf("expected 0 TotalBytes on Mac after ClearAll, got %+v", got)
	}
}

func TestSyncAuthETagAndMissingAttachment409(t *testing.T) {
	token, _ := config.GenerateToken()
	node := newTestNode(t, "ubuntu", "Ubuntu", token)

	// Missing Bearer token -> 401.
	resp, err := http.Get(node.srv.URL + "/sync/manifest")
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	// Valid Bearer token -> 200 with ETag, then 304 Not Modified on If-None-Match.
	req, _ := http.NewRequest(http.MethodGet, node.srv.URL+"/sync/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("auth manifest: %v", err)
	}
	etag := resp.Header.Get("Etag")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || etag == "" {
		t.Fatalf("expected 200 + ETag, got %d %q", resp.StatusCode, etag)
	}

	req2, _ := http.NewRequest(http.MethodGet, node.srv.URL+"/sync/manifest", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("conditional manifest: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", resp2.StatusCode)
	}

	// Pushing message whose attachment was not uploaded -> 409 Conflict.
	rec := store.Record{
		SchemaVersion: 1,
		Kind:          store.KindMessage,
		ID:            "01JDM4K8N27Q9X2V4R6W8Y0Z1A",
		DeviceID:      "mac",
		DeviceName:    "Mac",
		CreatedAtMs:   time.Now().UTC().UnixMilli(),
		Text:          "missing blob",
		Attachment: &store.AttachmentMeta{
			ID:         "01JDM4K8N27Q9X2V4R6W8Y0Z1B",
			Month:      "2026-11",
			Filename:   "a.png",
			StoredName: "01JDM4K8N27Q9X2V4R6W8Y0Z1B.png",
			MIMEType:   "image/png",
			SizeBytes:  123,
			SHA256:     strings.Repeat("a", 64),
		},
	}
	body, _ := json.Marshal(rec)
	pushReq, _ := http.NewRequest(http.MethodPost, node.srv.URL+"/sync/push", bytes.NewReader(body))
	pushReq.Header.Set("Authorization", "Bearer "+token)
	pushResp, err := http.DefaultClient.Do(pushReq)
	if err != nil {
		t.Fatalf("push missing attachment: %v", err)
	}
	pushResp.Body.Close()
	if pushResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for missing attachment, got %d", pushResp.StatusCode)
	}
}
