package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndPeerValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	tok, err := GenerateToken()
	if err != nil || len(tok) != 64 {
		t.Fatalf("GenerateToken: len=%d err=%v", len(tok), err)
	}

	cfg := Config{
		DeviceID:       "ubuntu",
		DeviceName:     "Ubuntu",
		UIAddr:         DefaultUIAddr,
		SyncAddr:       "100.124.162.117:8788",
		WorkspaceToken: tok,
		Peers:          []Peer{{ID: "mac", Name: "Mac", Addr: "100.124.162.95:8788"}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(false); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 perms on config.json, got %o", info.Mode().Perm())
	}

	mgr, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(mgr.Peers()) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(mgr.Peers()))
	}

	// Reject self ID, self sync addr, non-Tailscale IP, and scheme prefix.
	badPeers := []Peer{
		{ID: "ubuntu", Name: "Self", Addr: "100.100.100.1:8788"},
		{ID: "dupaddr", Name: "SelfAddr", Addr: "100.124.162.117:8788"},
		{ID: "pub", Name: "Public", Addr: "1.1.1.1:8788"},
		{ID: "scheme", Name: "Scheme", Addr: "http://100.100.100.2:8788"},
		{ID: "../evil", Name: "Evil", Addr: "100.100.100.2:8788"},
	}
	for _, bp := range badPeers {
		if _, err := mgr.AddPeer(bp); err == nil {
			t.Errorf("expected AddPeer error for %+v", bp)
		}
	}

	p, err := ParsePeerSpec("desk=Desktop@100.100.100.5:8788")
	if err != nil {
		t.Fatalf("ParsePeerSpec: %v", err)
	}
	if _, err := mgr.AddPeer(p); err != nil {
		t.Fatalf("AddPeer valid: %v", err)
	}
	if removed, err := mgr.RemovePeer("desk"); err != nil || !removed {
		t.Fatalf("RemovePeer: removed=%v err=%v", removed, err)
	}
}
