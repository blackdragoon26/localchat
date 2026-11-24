package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localchat/internal/config"
	"localchat/internal/ulid"
)

func TestStoreLargeTextAndCorruptTrailingLine(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, "ubuntu", "Ubuntu")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Exceeds default 64 KiB bufio.Scanner limit.
	bigCode := "```go\n" + strings.Repeat("fmt.Println(\"hello world\")\n", 4000) + "```"
	if len(bigCode) <= 65536 {
		t.Fatalf("expected >64KiB text, got %d", len(bigCode))
	}
	rec, err := st.CreateLocalMessage(bigCode, nil)
	if err != nil {
		t.Fatalf("CreateLocalMessage: %v", err)
	}

	// Append a simulated truncated trailing line from a sudden crash.
	month := time.UnixMilli(rec.CreatedAtMs).UTC().Format("2006-01")
	logPath := filepath.Join(dir, "messages", "ubuntu-"+month+".ndjson")
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	_, _ = f.WriteString(`{"schema_version":1,"kind":"message","id":"truncated`)
	_ = f.Close()

	// Reopen should skip corrupt trailing line and retain bigCode intact.
	st2, err := Open(dir, "ubuntu", "Ubuntu")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	msgs, hasMore, _ := st2.ListMessages(50, "")
	if hasMore || len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d (hasMore=%v)", len(msgs), hasMore)
	}
	if msgs[0].Text != bigCode {
		t.Fatalf("big text mismatch after reload: got len %d, want %d", len(msgs[0].Text), len(bigCode))
	}
}

func TestAttachmentStreamingTombstoneScrubAndReload(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, "ubuntu", "Ubuntu")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	payload := []byte("super secret video payload bytes")
	att, err := st.SaveUploadedAttachment(bytes.NewReader(payload), "../../evil:name?.mp4", 1024)
	if err != nil {
		t.Fatalf("SaveUploadedAttachment: %v", err)
	}
	if strings.Contains(att.StoredName, "..") || !strings.HasSuffix(att.StoredName, ".mp4") {
		t.Fatalf("unexpected stored name %q", att.StoredName)
	}

	secret := "API_KEY=sk-live-super-secret-999"
	rec, err := st.CreateLocalMessage(secret, att)
	if err != nil {
		t.Fatalf("CreateLocalMessage: %v", err)
	}

	attPath, _, err := st.ResolveAttachment(att.ID)
	if err != nil {
		t.Fatalf("ResolveAttachment: %v", err)
	}

	// Delete message -> scrubs text from NDJSON and removes blob.
	tomb, created, err := st.CreateLocalTombstone(rec.ID)
	if err != nil || !created {
		t.Fatalf("CreateLocalTombstone: created=%v err=%v", created, err)
	}
	if _, err := os.Stat(attPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected attachment blob deleted, stat err=%v", err)
	}

	// Verify secret is scrubbed from on-disk NDJSON.
	month := time.UnixMilli(rec.CreatedAtMs).UTC().Format("2006-01")
	rawLog, err := os.ReadFile(filepath.Join(dir, "messages", "ubuntu-"+month+".ndjson"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(rawLog), secret) {
		t.Fatalf("deleted secret still present on disk in NDJSON log")
	}

	// Idempotent second delete returns created=false.
	tomb2, created2, err := st.CreateLocalTombstone(rec.ID)
	if err != nil || created2 || tomb2.ID != tomb.ID {
		t.Fatalf("expected idempotent tombstone, got created=%v err=%v", created2, err)
	}

	// Reopen confirms deleted message stays gone and manifest excludes deleted message ID.
	st2, err := Open(dir, "ubuntu", "Ubuntu")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	msgs, _, _ := st2.ListMessages(50, "")
	if len(msgs) != 0 {
		t.Fatalf("expected 0 live messages after tombstone reload, got %d", len(msgs))
	}
	m := st2.Manifest()
	for _, id := range m.RecordIDs {
		if id == rec.ID {
			t.Fatalf("deleted message ID %q leaked into manifest record_ids", rec.ID)
		}
	}
}

func TestSaveSyncedAttachmentChecksumAndSizeGuard(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, "ubuntu", "Ubuntu")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	data := []byte("hello synced blob")
	sum := sha256.Sum256(data)
	goodSHA := hex.EncodeToString(sum[:])
	badSHA := strings.Repeat("0", 64)
	id := ulid.New(time.Now().UTC())

	if err := st.SaveSyncedAttachment(id, "2026-11", "txt", int64(len(data)), badSHA, bytes.NewReader(data), 1024); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
	if err := st.SaveSyncedAttachment(id, "2026-11", "txt", 1000, goodSHA, bytes.NewReader(data), 500); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	if err := st.SaveSyncedAttachment(id, "2026-11", "txt", int64(len(data)), goodSHA, bytes.NewReader(data), 1024); err != nil {
		t.Fatalf("expected valid synced attachment, got %v", err)
	}
}

func TestAckedPersistenceAndPagination(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, "ubuntu", "Ubuntu")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var ids []string
	for i := 0; i < 5; i++ {
		r, err := st.CreateLocalMessage("msg", nil)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, r.ID)
	}

	if err := st.MarkAckedBatch("mac", ids[:2]); err != nil {
		t.Fatalf("MarkAckedBatch: %v", err)
	}

	st2, err := Open(dir, "ubuntu", "Ubuntu")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	peers := []config.Peer{{ID: "mac", Name: "Mac", Addr: "100.124.162.95:8788"}}
	if got := st2.PendingCount(peers); got != 3 {
		t.Fatalf("expected 3 pending after reload, got %d", got)
	}

	page1, hasMore1, _ := st2.ListMessages(2, "")
	if len(page1) != 2 || !hasMore1 || page1[0].ID != ids[3] || page1[1].ID != ids[4] {
		t.Fatalf("unexpected page1: len=%d hasMore=%v", len(page1), hasMore1)
	}
	page2, hasMore2, _ := st2.ListMessages(2, page1[0].ID)
	if len(page2) != 2 || !hasMore2 || page2[0].ID != ids[1] || page2[1].ID != ids[2] {
		t.Fatalf("unexpected page2: len=%d hasMore=%v", len(page2), hasMore2)
	}
}
