package ui

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localchat/internal/config"
	"localchat/internal/lifecycle"
	"localchat/internal/store"
	"localchat/internal/syncer"
)

func setupTestUI(t *testing.T, maxUpload int64, clock func() time.Time) (*httptest.Server, *store.Store, *lifecycle.Tracker) {
	t.Helper()
	dir := t.TempDir()
	tok, _ := config.GenerateToken()
	cfg := config.Config{
		DeviceID:       "ubuntu",
		DeviceName:     "Ubuntu",
		UIAddr:         "127.0.0.1:8787",
		SyncAddr:       "100.124.162.117:8788",
		WorkspaceToken: tok,
		MaxUploadBytes: maxUpload,
		IdleTimeoutSec: 300,
		Peers:          []config.Peer{},
	}
	mgr := config.NewManager(filepath.Join(dir, "config.json"), cfg)
	st, err := store.Open(dir, cfg.DeviceID, cfg.DeviceName)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	tracker := lifecycle.NewTrackerWithClock(5*time.Minute, false, clock)
	syn := syncer.New(mgr, st, tracker)
	srv := NewServer(mgr, st, syn, tracker, nil)
	srv.allowAnyPort = true
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, tracker
}

func TestRootExactMatchHostAndCSRFFailClosed(t *testing.T) {
	ts, _, _ := setupTestUI(t, 1<<20, time.Now)

	// Exact root returns 200 with strict CSP + CORP.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("missing frame-ancestors CSP: %q", resp.Header.Get("Content-Security-Policy"))
	}
	if resp.Header.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("missing CORP header")
	}

	// Unknown route must return 404 (not index.html).
	resp, _ = http.Get(ts.URL + "/does-not-exist")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on unknown route, got %d", resp.StatusCode)
	}

	// Evil Host header -> 403.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.Host = "evil.example:8787"
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for evil Host, got %d", resp.StatusCode)
	}

	// POST with neither Origin nor Sec-Fetch-Site -> fail closed 403.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/messages", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for missing CSRF headers, got %d", resp.StatusCode)
	}

	// POST with cross-site Sec-Fetch-Site -> 403.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/messages", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-site, got %d", resp.StatusCode)
	}

	// POST with Sec-Fetch-Site: same-origin -> 201.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/messages", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for same-origin POST, got %d", resp.StatusCode)
	}

	// DELETE /api/messages (Clear Chat) requires same-origin and clears all messages.
	badClear, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/messages", nil)
	badClearResp, _ := http.DefaultClient.Do(badClear)
	badClearResp.Body.Close()
	if badClearResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthenticated CSRF DELETE /api/messages, got %d", badClearResp.StatusCode)
	}

	clearReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/messages", nil)
	clearReq.Header.Set("Sec-Fetch-Site", "same-origin")
	clearResp, _ := http.DefaultClient.Do(clearReq)
	clearResp.Body.Close()
	if clearResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for same-origin DELETE /api/messages, got %d", clearResp.StatusCode)
	}
}

func TestAttachmentSecurityMIMECapAndRange(t *testing.T) {
	ts, _, _ := setupTestUI(t, 1024, time.Now) // 1 KiB cap

	upload := func(filename string, content []byte) *http.Response {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write(content)
		_ = mw.Close()

		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/messages", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("upload %s: %v", filename, err)
		}
		return resp
	}

	// 1. Oversized upload (> 1024 bytes) -> 413.
	bigResp := upload("too_big.bin", bytes.Repeat([]byte("A"), 2048))
	bigResp.Body.Close()
	if bigResp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized upload, got %d", bigResp.StatusCode)
	}

	// 2. HTML disguised as .png -> forced attachment + application/octet-stream + sandbox + nosniff.
	fakeResp := upload("fake.png", []byte("<html><script>alert(1)</script></html>"))
	var created struct {
		Message struct {
			Attachment struct {
				ID string `json:"id"`
			} `json:"attachment"`
		} `json:"message"`
	}
	_ = json.NewDecoder(fakeResp.Body).Decode(&created)
	fakeResp.Body.Close()
	if fakeResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for disguised upload, got %d", fakeResp.StatusCode)
	}

	getResp, err := http.Get(ts.URL + "/api/attachments/" + created.Message.Attachment.ID)
	if err != nil {
		t.Fatalf("get fake.png: %v", err)
	}
	getResp.Body.Close()
	if getResp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("disguised HTML must not serve inline: got Content-Type=%q", getResp.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(getResp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("expected attachment disposition, got %q", getResp.Header.Get("Content-Disposition"))
	}
	if getResp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("expected sandbox CSP, got %q", getResp.Header.Get("Content-Security-Policy"))
	}

	// 3. MP4 file supports HTTP Range -> 206 Partial Content.
	mp4Bytes := append([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}, bytes.Repeat([]byte("X"), 100)...)
	mp4Resp := upload("clip.mp4", mp4Bytes)
	var mp4Created struct {
		Message struct {
			Attachment struct {
				ID string `json:"id"`
			} `json:"attachment"`
		} `json:"message"`
	}
	_ = json.NewDecoder(mp4Resp.Body).Decode(&mp4Created)
	mp4Resp.Body.Close()

	rangeReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/attachments/"+mp4Created.Message.Attachment.ID, nil)
	rangeReq.Header.Set("Range", "bytes=0-15")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatalf("range request: %v", err)
	}
	chunk, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || len(chunk) != 16 {
		t.Fatalf("expected 206 Partial Content (16 bytes), got status=%d len=%d", rangeResp.StatusCode, len(chunk))
	}
	if rangeResp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("expected inline video/mp4, got %q", rangeResp.Header.Get("Content-Type"))
	}
}

func TestAddAndRemovePeerTailscaleCIDR(t *testing.T) {
	ts, _, _ := setupTestUI(t, 1<<20, time.Now)

	// Non-Tailscale peer IP -> rejected 400 (prevents Bearer token leak outside Tailscale).
	badReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/peers", strings.NewReader(`{"id":"bad","name":"Bad","addr":"8.8.8.8:8788"}`))
	badReq.Header.Set("Sec-Fetch-Site", "same-origin")
	badResp, _ := http.DefaultClient.Do(badReq)
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-Tailscale peer IP, got %d", badResp.StatusCode)
	}

	// Valid Tailscale peer IP -> 201 Created.
	okReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/peers", strings.NewReader(`{"id":"mac","name":"Mac","addr":"100.124.162.95:8788"}`))
	okReq.Header.Set("Sec-Fetch-Site", "same-origin")
	okResp, _ := http.DefaultClient.Do(okReq)
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for Tailscale peer, got %d", okResp.StatusCode)
	}

	// Delete peer -> 200 OK.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/peers/mac", nil)
	delReq.Header.Set("Sec-Fetch-Site", "same-origin")
	delResp, _ := http.DefaultClient.Do(delReq)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 deleting peer, got %d", delResp.StatusCode)
	}
}

func TestIdleLifecycleAndHeartbeat(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ts, _, tracker := setupTestUI(t, 1<<20, func() time.Time { return now })

	// Advance 4m -> heartbeat keeps server alive.
	now = now.Add(4 * time.Minute)
	if tracker.ShouldExit() {
		t.Fatalf("should not exit at 4m")
	}
	hbResp, _ := http.Get(ts.URL + "/api/heartbeat")
	hbResp.Body.Close()
	if hbResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 heartbeat, got %d", hbResp.StatusCode)
	}

	// /api/health probe must NOT reset lastUIActivity.
	now = now.Add(6 * time.Minute)
	hResp, _ := http.Get(ts.URL + "/api/health")
	hResp.Body.Close()
	if hResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 health, got %d", hResp.StatusCode)
	}

	// In-flight transfer prevents idle shutdown.
	tracker.BeginTransfer()
	if tracker.ShouldExit() {
		t.Fatalf("must not exit while transfer is in flight")
	}
	tracker.EndTransfer()
	if !tracker.ShouldExit() {
		t.Fatalf("expected ShouldExit=true after 6m idle with no transfers")
	}
}
