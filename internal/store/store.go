package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"localchat/internal/config"
	"localchat/internal/ulid"
)

const (
	SchemaVersion = 1
	KindMessage   = "message"
	KindTombstone = "tombstone"
	MaxTextBytes  = 1 << 20 // 1 MiB
)

var (
	ErrNotFound          = errors.New("record not found")
	ErrMissingAttachment = errors.New("referenced attachment missing on disk")
	ErrInvalidRecord     = errors.New("invalid record")
	ErrTooLarge          = errors.New("payload exceeds maximum allowed size")
	ErrChecksumMismatch  = errors.New("attachment checksum or size mismatch")

	monthPattern = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)
	extPattern   = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
	shaPattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// AttachmentMeta stores immutable metadata about an attachment blob on disk.
type AttachmentMeta struct {
	ID         string `json:"id"`
	Month      string `json:"month"`
	Filename   string `json:"filename"`
	StoredName string `json:"stored_name"`
	MIMEType   string `json:"mime_type"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
}

// Record is one event line in messages/<origin-device-id>-YYYY-MM.ndjson.
type Record struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          string          `json:"kind"`
	ID            string          `json:"id"`
	DeviceID      string          `json:"device_id"`
	DeviceName    string          `json:"device_name"`
	CreatedAtMs   int64           `json:"created_at_ms"`
	Text          string          `json:"text,omitempty"`
	Attachment    *AttachmentMeta `json:"attachment,omitempty"`
	TargetID      string          `json:"target_id,omitempty"`
}

// Manifest summarizes all live messages and tombstones held by this device.
type Manifest struct {
	DeviceID      string   `json:"device_id"`
	DeviceName    string   `json:"device_name"`
	RecordIDs     []string `json:"record_ids"`
	TombstonedIDs []string `json:"tombstoned_ids"`
	ETag          string   `json:"-"`
}

// DeliveryStatus summarizes per-peer delivery acknowledgments for a message.
type DeliveryStatus struct {
	SentAll      bool     `json:"sent_all"`
	AckedBy      []string `json:"acked_by"`
	PendingPeers []string `json:"pending_peers"`
}

// Store manages local NDJSON logs, attachment blobs, and the delivery ledger.
type Store struct {
	dataDir  string
	selfID   string
	selfName string

	mu          sync.RWMutex
	messages    map[string]Record         // live non-tombstoned messages by ID
	tombstones  map[string]Record         // tombstone records by tombstone ID
	tombstoned  map[string]string         // target message ID -> tombstone ID
	attachments map[string]AttachmentMeta // attachment ID -> meta (for live messages)
	orderedIDs  []string                  // live message IDs sorted by (created_at_ms, id)
	acked       map[string][]string       // record ID -> sorted peer IDs
	manifestTag string

	rev atomic.Uint64
}

// Open initializes directory layout, cleans temporary files, loads all monthly
// NDJSON logs and state/acked.json, and removes orphaned attachment blobs.
func Open(dataDir, selfID, selfName string) (*Store, error) {
	for _, sub := range []string{"", "messages", "attachments", "state", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create dir %q: %w", sub, err)
		}
	}

	// Sweep stale staging files after lock has been acquired by caller.
	tmpDir := filepath.Join(dataDir, "tmp")
	if entries, err := os.ReadDir(tmpDir); err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(tmpDir, e.Name()))
		}
	}

	s := &Store{
		dataDir:     dataDir,
		selfID:      selfID,
		selfName:    selfName,
		messages:    make(map[string]Record),
		tombstones:  make(map[string]Record),
		tombstoned:  make(map[string]string),
		attachments: make(map[string]AttachmentMeta),
		acked:       make(map[string][]string),
	}

	if err := s.loadAcked(); err != nil {
		return nil, err
	}
	if err := s.loadAllLogs(); err != nil {
		return nil, err
	}
	s.gcOrphanAttachments()
	s.rebuildOrderAndETagLocked()
	s.rev.Store(1)
	return s, nil
}

// DataDir returns the root local data directory.
func (s *Store) DataDir() string { return s.dataDir }

// Revision returns the monotonic local mutation counter.
func (s *Store) Revision() uint64 { return s.rev.Load() }

func (s *Store) bumpRevisionLocked() {
	s.rebuildOrderAndETagLocked()
	s.rev.Add(1)
}

func (s *Store) loadAcked() error {
	p := filepath.Join(s.dataDir, "state", "acked.json")
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read acked.json: %w", err)
	}
	var ledger map[string][]string
	if err := json.Unmarshal(data, &ledger); err != nil {
		slog.Warn("corrupt acked.json reset to empty", "err", err)
		return nil
	}
	if ledger != nil {
		s.acked = ledger
	}
	return nil
}

func (s *Store) saveAckedLocked() error {
	dir := filepath.Join(s.dataDir, "state")
	p := filepath.Join(dir, "acked.json")
	payload, err := json.MarshalIndent(s.acked, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return atomicWriteFile(dir, p, payload, 0o600)
}

func (s *Store) loadAllLogs() error {
	msgDir := filepath.Join(s.dataDir, "messages")
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		return err
	}

	var allRecords []Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ndjson") {
			continue
		}
		path := filepath.Join(msgDir, entry.Name())
		recs, err := readNDJSONFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		allRecords = append(allRecords, recs...)
	}

	// Pass 1: apply all tombstones first so out-of-order files never resurrect deleted messages.
	for _, rec := range allRecords {
		if rec.Kind == KindTombstone && ulid.IsValid(rec.ID) && ulid.IsValid(rec.TargetID) {
			s.tombstones[rec.ID] = rec
			s.tombstoned[rec.TargetID] = rec.ID
		}
	}

	// Pass 2: load non-tombstoned messages.
	for _, rec := range allRecords {
		if rec.Kind != KindMessage || !ulid.IsValid(rec.ID) {
			continue
		}
		if _, dead := s.tombstoned[rec.ID]; dead {
			// Ensure deleted attachment blob is removed if a prior crash left it.
			if rec.Attachment != nil {
				if p, err := s.attachmentPathForMeta(*rec.Attachment); err == nil {
					_ = os.Remove(p)
				}
			}
			continue
		}
		s.messages[rec.ID] = rec
		if rec.Attachment != nil {
			s.attachments[rec.Attachment.ID] = *rec.Attachment
		}
	}
	return nil
}

func readNDJSONFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	var out []Record
	lineNum := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNum++
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var rec Record
				if uerr := json.Unmarshal(trimmed, &rec); uerr != nil {
					slog.Warn("skipping malformed NDJSON line", "file", filepath.Base(path), "line", lineNum, "err", uerr)
				} else {
					out = append(out, rec)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) gcOrphanAttachments() {
	attRoot := filepath.Join(s.dataDir, "attachments")
	validPaths := make(map[string]struct{}, len(s.attachments))
	for _, meta := range s.attachments {
		if p, err := s.attachmentPathForMeta(meta); err == nil {
			validPaths[p] = struct{}{}
		}
	}
	_ = filepath.WalkDir(attRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if _, ok := validPaths[path]; !ok {
			slog.Info("removing orphan attachment blob", "path", path)
			_ = os.Remove(path)
		}
		return nil
	})
}

func (s *Store) rebuildOrderAndETagLocked() {
	ids := make([]string, 0, len(s.messages))
	for id := range s.messages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a := s.messages[ids[i]]
		b := s.messages[ids[j]]
		if a.CreatedAtMs != b.CreatedAtMs {
			return a.CreatedAtMs < b.CreatedAtMs
		}
		return a.ID < b.ID
	})
	s.orderedIDs = ids

	// Build deterministic manifest digest.
	recIDs := make([]string, 0, len(s.messages)+len(s.tombstones))
	for id := range s.messages {
		recIDs = append(recIDs, id)
	}
	for id := range s.tombstones {
		recIDs = append(recIDs, id)
	}
	sort.Strings(recIDs)

	tombIDs := make([]string, 0, len(s.tombstoned))
	for id := range s.tombstoned {
		tombIDs = append(tombIDs, id)
	}
	sort.Strings(tombIDs)

	h := sha256.New()
	for _, id := range recIDs {
		h.Write([]byte("R:"))
		h.Write([]byte(id))
		h.Write([]byte{'\n'})
	}
	for _, id := range tombIDs {
		h.Write([]byte("T:"))
		h.Write([]byte(id))
		h.Write([]byte{'\n'})
	}
	s.manifestTag = fmt.Sprintf(`"%x"`, h.Sum(nil)[:16])
}

func (s *Store) monthlyLogPath(deviceID string, createdAtMs int64) string {
	month := time.UnixMilli(createdAtMs).UTC().Format("2006-01")
	return filepath.Join(s.dataDir, "messages", fmt.Sprintf("%s-%s.ndjson", deviceID, month))
}

func appendNDJSONLine(path string, rec Record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	return f.Sync()
}

// scrubMessageFromMonthlyLog rewrites a single monthly NDJSON file atomically,
// omitting the record with targetID so deleted secrets do not remain on disk.
func (s *Store) scrubMessageFromMonthlyLog(deviceID string, createdAtMs int64, targetID string) error {
	path := s.monthlyLogPath(deviceID, createdAtMs)
	recs, err := readNDJSONFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, r := range recs {
		if r.Kind == KindMessage && r.ID == targetID {
			continue
		}
		line, merr := json.Marshal(r)
		if merr != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return atomicWriteFile(filepath.Dir(path), path, buf.Bytes(), 0o600)
}

func atomicWriteFile(dir, targetPath string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, ".atomic-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// CreateLocalMessage appends a new message authored by this device.
func (s *Store) CreateLocalMessage(text string, att *AttachmentMeta) (Record, error) {
	if len(text) > MaxTextBytes {
		return Record{}, ErrTooLarge
	}
	if strings.TrimSpace(text) == "" && att == nil {
		return Record{}, fmt.Errorf("%w: empty message and no attachment", ErrInvalidRecord)
	}

	now := time.Now().UTC()
	rec := Record{
		SchemaVersion: SchemaVersion,
		Kind:          KindMessage,
		ID:            ulid.New(now),
		DeviceID:      s.selfID,
		DeviceName:    s.selfName,
		CreatedAtMs:   now.UnixMilli(),
		Text:          text,
		Attachment:    att,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := appendNDJSONLine(s.monthlyLogPath(s.selfID, rec.CreatedAtMs), rec); err != nil {
		return Record{}, err
	}
	s.messages[rec.ID] = rec
	if att != nil {
		s.attachments[att.ID] = *att
	}
	s.bumpRevisionLocked()
	return rec, nil
}

// CreateLocalTombstone deletes targetID locally, scrubs its text from its monthly
// NDJSON log, removes its attachment blob, and returns the tombstone record.
func (s *Store) CreateLocalTombstone(targetID string) (Record, bool, error) {
	if !ulid.IsValid(targetID) {
		return Record{}, false, ErrNotFound
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existingTombID, dead := s.tombstoned[targetID]; dead {
		return s.tombstones[existingTombID], false, nil
	}
	target, ok := s.messages[targetID]
	if !ok {
		return Record{}, false, ErrNotFound
	}

	now := time.Now().UTC()
	tomb := Record{
		SchemaVersion: SchemaVersion,
		Kind:          KindTombstone,
		ID:            ulid.New(now),
		DeviceID:      s.selfID,
		DeviceName:    s.selfName,
		CreatedAtMs:   now.UnixMilli(),
		TargetID:      targetID,
	}

	if err := appendNDJSONLine(s.monthlyLogPath(s.selfID, tomb.CreatedAtMs), tomb); err != nil {
		return Record{}, false, err
	}
	s.applyTombstoneEffectsLocked(tomb, target)
	delete(s.acked, targetID)
	_ = s.saveAckedLocked()
	s.bumpRevisionLocked()
	return tomb, true, nil
}

// ClearAll creates tombstones for every live message, deletes all local attachment
// blobs, and scrubs cleared message records from monthly NDJSON logs in one pass.
// Because tombstones are persisted, peers automatically clear their storage on sync.
func (s *Store) ClearAll() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.orderedIDs) == 0 {
		return nil, nil
	}

	now := time.Now().UTC()
	tombs := make([]Record, 0, len(s.orderedIDs))
	affectedLogs := make(map[string]map[string]struct{}) // logPath -> set of targetIDs to scrub

	for _, targetID := range s.orderedIDs {
		target := s.messages[targetID]
		tomb := Record{
			SchemaVersion: SchemaVersion,
			Kind:          KindTombstone,
			ID:            ulid.New(now),
			DeviceID:      s.selfID,
			DeviceName:    s.selfName,
			CreatedAtMs:   now.UnixMilli(),
			TargetID:      targetID,
		}
		tombs = append(tombs, tomb)

		logPath := s.monthlyLogPath(target.DeviceID, target.CreatedAtMs)
		if affectedLogs[logPath] == nil {
			affectedLogs[logPath] = make(map[string]struct{})
		}
		affectedLogs[logPath][targetID] = struct{}{}
	}

	// Append all tombstones to self monthly log in a single write + fsync.
	selfLogPath := s.monthlyLogPath(s.selfID, now.UnixMilli())
	var tombBuf bytes.Buffer
	for _, tomb := range tombs {
		raw, err := json.Marshal(tomb)
		if err != nil {
			return nil, err
		}
		tombBuf.Write(raw)
		tombBuf.WriteByte('\n')
	}
	f, err := os.OpenFile(selfLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(tombBuf.Bytes()); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	// Update in-memory maps and delete all attachment blobs.
	for _, tomb := range tombs {
		target := s.messages[tomb.TargetID]
		s.tombstones[tomb.ID] = tomb
		s.tombstoned[tomb.TargetID] = tomb.ID
		delete(s.messages, tomb.TargetID)
		delete(s.acked, tomb.TargetID)
		if target.Attachment != nil {
			delete(s.attachments, target.Attachment.ID)
			if p, perr := s.attachmentPathForMeta(*target.Attachment); perr == nil {
				_ = os.Remove(p)
			}
		}
	}

	// Scrub cleared messages from each affected monthly log in one atomic rewrite per file.
	for logPath, dropIDs := range affectedLogs {
		recs, rerr := readNDJSONFile(logPath)
		if rerr != nil {
			continue
		}
		var buf bytes.Buffer
		for _, r := range recs {
			if r.Kind == KindMessage {
				if _, drop := dropIDs[r.ID]; drop {
					continue
				}
			}
			line, merr := json.Marshal(r)
			if merr != nil {
				continue
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if werr := atomicWriteFile(filepath.Dir(logPath), logPath, buf.Bytes(), 0o600); werr != nil {
			slog.Warn("failed to scrub monthly log during ClearAll", "file", filepath.Base(logPath), "err", werr)
		}
	}

	_ = s.saveAckedLocked()
	s.bumpRevisionLocked()
	return tombs, nil
}

func (s *Store) applyTombstoneEffectsLocked(tomb Record, target Record) {
	s.tombstones[tomb.ID] = tomb
	s.tombstoned[tomb.TargetID] = tomb.ID
	delete(s.messages, tomb.TargetID)
	if target.ID != "" {
		if target.Attachment != nil {
			delete(s.attachments, target.Attachment.ID)
			if p, err := s.attachmentPathForMeta(*target.Attachment); err == nil {
				_ = os.Remove(p)
			}
		}
		if err := s.scrubMessageFromMonthlyLog(target.DeviceID, target.CreatedAtMs, target.ID); err != nil {
			slog.Warn("failed to scrub deleted message from monthly log", "id", target.ID, "err", err)
		}
	}
}

// IngestRemoteRecord validates and stores a record received from a peer.
// Returns (inserted, error). Duplicate records return (false, nil).
func (s *Store) IngestRemoteRecord(rec Record) (bool, error) {
	if err := validateRemoteRecord(rec); err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if rec.Kind == KindTombstone {
		if _, exists := s.tombstones[rec.ID]; exists {
			return false, nil
		}
		if err := appendNDJSONLine(s.monthlyLogPath(rec.DeviceID, rec.CreatedAtMs), rec); err != nil {
			return false, err
		}
		target := s.messages[rec.TargetID]
		s.applyTombstoneEffectsLocked(rec, target)
		delete(s.acked, rec.TargetID)
		_ = s.saveAckedLocked()
		s.bumpRevisionLocked()
		return true, nil
	}

	// KindMessage
	if _, exists := s.messages[rec.ID]; exists {
		return false, nil
	}
	if _, dead := s.tombstoned[rec.ID]; dead {
		// Already tombstoned: delete any attachment blob that was just uploaded and accept idempotently.
		if rec.Attachment != nil {
			if p, err := s.attachmentPathForMeta(*rec.Attachment); err == nil {
				_ = os.Remove(p)
			}
		}
		return false, nil
	}

	if rec.Attachment != nil {
		p, err := s.attachmentPathForMeta(*rec.Attachment)
		if err != nil {
			return false, err
		}
		info, err := os.Stat(p)
		if err != nil {
			return false, ErrMissingAttachment
		}
		if info.Size() != rec.Attachment.SizeBytes {
			return false, ErrMissingAttachment
		}
	}

	if err := appendNDJSONLine(s.monthlyLogPath(rec.DeviceID, rec.CreatedAtMs), rec); err != nil {
		return false, err
	}
	s.messages[rec.ID] = rec
	if rec.Attachment != nil {
		s.attachments[rec.Attachment.ID] = *rec.Attachment
	}
	s.bumpRevisionLocked()
	return true, nil
}

func validateRemoteRecord(rec Record) error {
	if rec.SchemaVersion < 1 {
		return fmt.Errorf("%w: schema_version must be >= 1", ErrInvalidRecord)
	}
	if !ulid.IsValid(rec.ID) {
		return fmt.Errorf("%w: invalid record id", ErrInvalidRecord)
	}
	if !config.ValidateID(rec.DeviceID) {
		return fmt.Errorf("%w: invalid device_id", ErrInvalidRecord)
	}
	if rec.CreatedAtMs <= 0 {
		return fmt.Errorf("%w: invalid created_at_ms", ErrInvalidRecord)
	}
	switch rec.Kind {
	case KindMessage:
		if len(rec.Text) > MaxTextBytes {
			return ErrTooLarge
		}
		if strings.TrimSpace(rec.Text) == "" && rec.Attachment == nil {
			return fmt.Errorf("%w: empty message", ErrInvalidRecord)
		}
		if rec.Attachment != nil {
			if err := validateAttachmentMeta(*rec.Attachment); err != nil {
				return err
			}
		}
		return nil
	case KindTombstone:
		if !ulid.IsValid(rec.TargetID) {
			return fmt.Errorf("%w: invalid target_id", ErrInvalidRecord)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidRecord, rec.Kind)
	}
}

func validateAttachmentMeta(m AttachmentMeta) error {
	if !ulid.IsValid(m.ID) {
		return fmt.Errorf("%w: invalid attachment id", ErrInvalidRecord)
	}
	if !monthPattern.MatchString(m.Month) {
		return fmt.Errorf("%w: invalid attachment month", ErrInvalidRecord)
	}
	ext := ExtractSafeExt(m.StoredName)
	expectedStored := m.ID + "." + ext
	if m.StoredName != expectedStored {
		return fmt.Errorf("%w: stored_name mismatch", ErrInvalidRecord)
	}
	if m.SizeBytes < 0 {
		return fmt.Errorf("%w: negative size_bytes", ErrInvalidRecord)
	}
	if !shaPattern.MatchString(m.SHA256) {
		return fmt.Errorf("%w: invalid sha256", ErrInvalidRecord)
	}
	return nil
}

// ExtractSafeExt extracts a lowercased alphanumeric extension (1..10 chars) or "bin".
func ExtractSafeExt(filename string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	if extPattern.MatchString(ext) {
		return ext
	}
	return "bin"
}

// ValidateMonthAndExt validates query parameters for PUT /sync/attachments/{id}.
func ValidateMonthAndExt(month, ext string) bool {
	return monthPattern.MatchString(month) && extPattern.MatchString(ext)
}

func (s *Store) attachmentPathForMeta(m AttachmentMeta) (string, error) {
	if !ulid.IsValid(m.ID) || !monthPattern.MatchString(m.Month) {
		return "", ErrInvalidRecord
	}
	ext := ExtractSafeExt(m.StoredName)
	if m.StoredName != m.ID+"."+ext {
		return "", ErrInvalidRecord
	}
	parts := strings.Split(m.Month, "-")
	full := filepath.Join(s.dataDir, "attachments", parts[0], parts[1], m.StoredName)
	root := filepath.Join(s.dataDir, "attachments") + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(full), root) {
		return "", ErrInvalidRecord
	}
	return full, nil
}

// SaveUploadedAttachment streams a local browser upload to tmp/, computes SHA-256
// and magic-byte MIME type, and atomically moves it into attachments/YYYY/MM/.
func (s *Store) SaveUploadedAttachment(r io.Reader, origFilename string, maxBytes int64) (*AttachmentMeta, error) {
	now := time.Now().UTC()
	id := ulid.New(now)
	month := now.Format("2006-01")
	ext := ExtractSafeExt(origFilename)
	storedName := id + "." + ext

	meta, err := s.streamToAttachment(r, id, month, ext, storedName, filepath.Base(origFilename), maxBytes, -1, "")
	if err != nil {
		return nil, err
	}
	return meta, nil
}

// SaveSyncedAttachment streams an attachment pushed/pulled from a peer, verifying
// exact byte count and SHA-256 checksum before atomically placing it on disk.
func (s *Store) SaveSyncedAttachment(id, month, ext string, expectedSize int64, expectedSHA256 string, r io.Reader, maxBytes int64) error {
	if !ulid.IsValid(id) || !ValidateMonthAndExt(month, ext) || !shaPattern.MatchString(expectedSHA256) || expectedSize < 0 {
		return ErrInvalidRecord
	}
	if expectedSize > maxBytes {
		return ErrTooLarge
	}
	storedName := id + "." + ext
	meta := AttachmentMeta{ID: id, Month: month, StoredName: storedName}
	dest, err := s.attachmentPathForMeta(meta)
	if err != nil {
		return err
	}
	// Idempotent fast-path: if file already exists with exact expected size & SHA-256.
	if info, statErr := os.Stat(dest); statErr == nil && info.Size() == expectedSize {
		if existingHash, herr := fileSHA256(dest); herr == nil && existingHash == expectedSHA256 {
			_, _ = io.Copy(io.Discard, io.LimitReader(r, maxBytes+1))
			return nil
		}
	}
	_, err = s.streamToAttachment(r, id, month, ext, storedName, storedName, maxBytes, expectedSize, expectedSHA256)
	return err
}

func (s *Store) streamToAttachment(r io.Reader, id, month, ext, storedName, origFilename string, maxBytes, expectedSize int64, expectedSHA string) (*AttachmentMeta, error) {
	tmpDir := filepath.Join(s.dataDir, "tmp")
	tmp, err := os.CreateTemp(tmpDir, "part-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	var sniffBuf [512]byte
	sniffLen := 0

	limited := io.LimitReader(r, maxBytes+1)
	buf := make([]byte, 32*1024)
	var written int64
	for {
		n, rerr := limited.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxBytes {
				_ = tmp.Close()
				return nil, ErrTooLarge
			}
			if sniffLen < len(sniffBuf) {
				cp := copy(sniffBuf[sniffLen:], buf[:n])
				sniffLen += cp
			}
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				_ = tmp.Close()
				return nil, werr
			}
			hasher.Write(buf[:n])
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			_ = tmp.Close()
			return nil, rerr
		}
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	if expectedSize >= 0 && written != expectedSize {
		return nil, ErrChecksumMismatch
	}
	if expectedSHA != "" && sum != expectedSHA {
		return nil, ErrChecksumMismatch
	}

	meta := &AttachmentMeta{
		ID:         id,
		Month:      month,
		Filename:   origFilename,
		StoredName: storedName,
		MIMEType:   http.DetectContentType(sniffBuf[:sniffLen]),
		SizeBytes:  written,
		SHA256:     sum,
	}
	dest, err := s.attachmentPathForMeta(*meta)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return nil, err
	}
	cleanup = false
	return meta, nil
}

// RemoveStagedAttachment deletes an attachment blob if message creation fails mid-request.
func (s *Store) RemoveStagedAttachment(meta *AttachmentMeta) {
	if meta == nil {
		return
	}
	if p, err := s.attachmentPathForMeta(*meta); err == nil {
		_ = os.Remove(p)
	}
}

// ResolveAttachment returns the on-disk path and metadata for a live attachment ID.
func (s *Store) ResolveAttachment(attID string) (string, AttachmentMeta, error) {
	if !ulid.IsValid(attID) {
		return "", AttachmentMeta{}, ErrNotFound
	}
	s.mu.RLock()
	meta, ok := s.attachments[attID]
	s.mu.RUnlock()
	if !ok {
		return "", AttachmentMeta{}, ErrNotFound
	}
	p, err := s.attachmentPathForMeta(meta)
	if err != nil {
		return "", AttachmentMeta{}, ErrNotFound
	}
	if _, err := os.Stat(p); err != nil {
		return "", AttachmentMeta{}, ErrNotFound
	}
	return p, meta, nil
}

// ListMessages returns up to limit live messages ordered chronologically (oldest first
// within the page). If beforeID is empty, returns the newest limit messages.
func (s *Store) ListMessages(limit int, beforeID string) ([]Record, bool, uint64) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	end := len(s.orderedIDs)
	if beforeID != "" {
		if idx := slices.Index(s.orderedIDs, beforeID); idx >= 0 {
			end = idx
		} else {
			// Find insertion point by ID if beforeID was deleted.
			end = 0
			for i, id := range s.orderedIDs {
				if id < beforeID {
					end = i + 1
				}
			}
		}
	}
	start := end - limit
	hasMore := false
	if start > 0 {
		hasMore = true
	} else {
		start = 0
	}

	out := make([]Record, 0, end-start)
	for _, id := range s.orderedIDs[start:end] {
		out = append(out, s.messages[id])
	}
	return out, hasMore, s.rev.Load()
}

// Manifest returns all live message IDs + tombstone record IDs, deleted target IDs, and ETag.
func (s *Store) Manifest() Manifest {
	s.mu.RLock()
	defer s.mu.RUnlock()

	recIDs := make([]string, 0, len(s.messages)+len(s.tombstones))
	for id := range s.messages {
		recIDs = append(recIDs, id)
	}
	for id := range s.tombstones {
		recIDs = append(recIDs, id)
	}
	sort.Strings(recIDs)

	tombIDs := make([]string, 0, len(s.tombstoned))
	for id := range s.tombstoned {
		tombIDs = append(tombIDs, id)
	}
	sort.Strings(tombIDs)

	return Manifest{
		DeviceID:      s.selfID,
		DeviceName:    s.selfName,
		RecordIDs:     recIDs,
		TombstonedIDs: tombIDs,
		ETag:          s.manifestTag,
	}
}

// RecordsByID returns matching non-tombstoned messages and tombstones for the given IDs.
func (s *Store) RecordsByID(ids []string) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		if rec, ok := s.messages[id]; ok {
			out = append(out, rec)
		} else if tomb, ok := s.tombstones[id]; ok {
			out = append(out, tomb)
		}
	}
	return out
}

// HasRecord reports whether id is already stored as a live message or tombstone.
func (s *Store) HasRecord(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.messages[id]; ok {
		return true
	}
	if _, ok := s.tombstones[id]; ok {
		return true
	}
	return false
}

// IsTombstoned reports whether a message targetID has been deleted.
func (s *Store) IsTombstoned(targetID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, dead := s.tombstoned[targetID]
	return dead
}

// MarkAckedBatch records peer acknowledgments for one or more record IDs in a single atomic save.
func (s *Store) MarkAckedBatch(peerID string, recordIDs []string) error {
	if len(recordIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for _, id := range recordIDs {
		// Only track acks for records that still exist (live message or tombstone).
		if _, isMsg := s.messages[id]; !isMsg {
			if _, isTomb := s.tombstones[id]; !isTomb {
				continue
			}
		}
		peers := s.acked[id]
		if !slices.Contains(peers, peerID) {
			peers = append(peers, peerID)
			sort.Strings(peers)
			s.acked[id] = peers
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := s.saveAckedLocked(); err != nil {
		return err
	}
	s.rev.Add(1)
	return nil
}

// DeliveryInfo computes the per-peer delivery state for a message against current peers.
func (s *Store) DeliveryInfo(rec Record, peers []config.Peer) DeliveryStatus {
	s.mu.RLock()
	ackedPeers := append([]string(nil), s.acked[rec.ID]...)
	s.mu.RUnlock()

	if len(peers) == 0 {
		return DeliveryStatus{SentAll: true, AckedBy: []string{}, PendingPeers: []string{}}
	}

	// For a remote-originated message, the originating device already holds it.
	if rec.DeviceID != s.selfID && !slices.Contains(ackedPeers, rec.DeviceID) {
		ackedPeers = append(ackedPeers, rec.DeviceID)
	}

	ackedNames := make([]string, 0, len(peers))
	pendingNames := make([]string, 0, len(peers))
	for _, p := range peers {
		if slices.Contains(ackedPeers, p.ID) {
			ackedNames = append(ackedNames, p.Name)
		} else {
			pendingNames = append(pendingNames, p.Name)
		}
	}
	return DeliveryStatus{
		SentAll:      len(pendingNames) == 0,
		AckedBy:      ackedNames,
		PendingPeers: pendingNames,
	}
}

// UnsentForPeer returns local tombstones first, then non-tombstoned messages that
// are absent from peerRecordSet and not in peerTombstonedSet.
func (s *Store) UnsentForPeer(peerID string, peerRecordSet, peerTombstonedSet map[string]struct{}) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var tombs []Record
	for id, tomb := range s.tombstones {
		if _, has := peerRecordSet[id]; !has {
			tombs = append(tombs, tomb)
		}
	}
	sort.Slice(tombs, func(i, j int) bool { return tombs[i].CreatedAtMs < tombs[j].CreatedAtMs })

	var msgs []Record
	for _, id := range s.orderedIDs {
		if _, has := peerRecordSet[id]; has {
			continue
		}
		if _, dead := peerTombstonedSet[id]; dead {
			continue
		}
		msgs = append(msgs, s.messages[id])
	}
	return append(tombs, msgs...)
}

// PendingCount returns how many locally authored live messages have at least one unacknowledged peer.
func (s *Store) PendingCount(peers []config.Peer) int {
	if len(peers) == 0 {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for id, rec := range s.messages {
		if rec.DeviceID != s.selfID {
			continue
		}
		acked := s.acked[id]
		for _, p := range peers {
			if !slices.Contains(acked, p.ID) {
				count++
				break
			}
		}
	}
	return count
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
