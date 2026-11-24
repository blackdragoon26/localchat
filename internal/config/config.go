package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	DefaultUIAddr         = "127.0.0.1:8787"
	DefaultSyncPort       = 8788
	DefaultMaxUploadBytes = 250 * 1024 * 1024 // 250 MiB
	DefaultIdleTimeoutSec = 300               // 5 minutes
)

var (
	idPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	tailscalePrefix = netip.MustParsePrefix("100.64.0.0/10")
)

// Peer represents another Tailscale device in the localchat peer list.
type Peer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// Config is the device-local configuration persisted at ~/LocalChat/config.json.
type Config struct {
	DeviceID       string `json:"device_id"`
	DeviceName     string `json:"device_name"`
	UIAddr         string `json:"ui_addr"`
	SyncAddr       string `json:"sync_addr"`
	WorkspaceToken string `json:"workspace_token"`
	MaxUploadBytes int64  `json:"max_upload_bytes"`
	IdleTimeoutSec int    `json:"idle_timeout_sec"`
	Peers          []Peer `json:"peers"`
}

// Manager provides thread-safe reads and atomic persistence for Config.
type Manager struct {
	mu                 sync.RWMutex
	path               string
	cfg                Config
	AllowLoopbackPeers bool
}

// NewManager wraps an existing Config with its file path.
func NewManager(path string, cfg Config) *Manager {
	if cfg.Peers == nil {
		cfg.Peers = []Peer{}
	}
	return &Manager{path: path, cfg: cfg}
}

// Load reads and validates a Config from disk.
func Load(path string) (*Manager, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(false); err != nil {
		return nil, err
	}
	return NewManager(path, cfg), nil
}

// ApplyDefaults fills missing optional fields with standard defaults.
func (c *Config) ApplyDefaults() {
	if strings.TrimSpace(c.UIAddr) == "" {
		c.UIAddr = DefaultUIAddr
	}
	if c.MaxUploadBytes <= 0 {
		c.MaxUploadBytes = DefaultMaxUploadBytes
	}
	if c.IdleTimeoutSec <= 0 {
		c.IdleTimeoutSec = DefaultIdleTimeoutSec
	}
	if c.Peers == nil {
		c.Peers = []Peer{}
	}
}

// Validate checks required fields and format constraints.
func (c *Config) Validate(allowLoopback bool) error {
	if !ValidateID(c.DeviceID) {
		return fmt.Errorf("invalid device_id %q: must match ^[a-z0-9][a-z0-9_-]{0,63}$", c.DeviceID)
	}
	if strings.TrimSpace(c.DeviceName) == "" {
		return errors.New("device_name is required")
	}
	if _, _, err := net.SplitHostPort(c.UIAddr); err != nil {
		return fmt.Errorf("invalid ui_addr %q: %w", c.UIAddr, err)
	}
	if _, _, err := net.SplitHostPort(c.SyncAddr); err != nil {
		return fmt.Errorf("invalid sync_addr %q: %w", c.SyncAddr, err)
	}
	if len(strings.TrimSpace(c.WorkspaceToken)) < 32 {
		return errors.New("workspace_token must be at least 32 characters")
	}
	seen := make(map[string]struct{}, len(c.Peers))
	for _, p := range c.Peers {
		if err := ValidatePeer(p, c.DeviceID, c.SyncAddr, allowLoopback); err != nil {
			return err
		}
		if _, dup := seen[p.ID]; dup {
			return fmt.Errorf("duplicate peer id %q", p.ID)
		}
		seen[p.ID] = struct{}{}
	}
	return nil
}

// ValidateID checks whether id conforms to the safe ASCII device/peer ID regex.
func ValidateID(id string) bool {
	return idPattern.MatchString(id)
}

// ValidatePeer validates a single Peer entry.
func ValidatePeer(p Peer, selfID, selfSyncAddr string, allowLoopback bool) error {
	if !ValidateID(p.ID) {
		return fmt.Errorf("invalid peer id %q: must match ^[a-z0-9][a-z0-9_-]{0,63}$", p.ID)
	}
	if p.ID == selfID {
		return fmt.Errorf("peer id %q cannot equal local device_id", p.ID)
	}
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("peer name is required")
	}
	if strings.Contains(p.Addr, "://") {
		return fmt.Errorf("peer addr %q must be host:port without scheme", p.Addr)
	}
	host, port, err := net.SplitHostPort(p.Addr)
	if err != nil || port == "" {
		return fmt.Errorf("invalid peer addr %q: must be host:port", p.Addr)
	}
	if p.Addr == selfSyncAddr {
		return fmt.Errorf("peer addr %q cannot equal local sync_addr", p.Addr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("peer host %q must be a Tailscale IPv4 address (100.64.0.0/10)", host)
	}
	if tailscalePrefix.Contains(ip) {
		return nil
	}
	if allowLoopback && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("peer IP %q is outside Tailscale CGNAT range 100.64.0.0/10", host)
}

// Snapshot returns a copy of the current Config with a cloned Peers slice.
func (m *Manager) Snapshot() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := m.cfg
	out.Peers = append([]Peer(nil), m.cfg.Peers...)
	return out
}

// Peers returns a defensive copy of the configured peers.
func (m *Manager) Peers() []Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Peer(nil), m.cfg.Peers...)
}

// AddPeer validates, adds, and atomically persists a new peer.
func (m *Manager) AddPeer(p Peer) (Peer, error) {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.Addr = strings.TrimSpace(p.Addr)

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ValidatePeer(p, m.cfg.DeviceID, m.cfg.SyncAddr, m.AllowLoopbackPeers); err != nil {
		return Peer{}, err
	}
	for _, existing := range m.cfg.Peers {
		if existing.ID == p.ID {
			return Peer{}, fmt.Errorf("peer %q already exists", p.ID)
		}
		if existing.Addr == p.Addr {
			return Peer{}, fmt.Errorf("peer address %q already configured", p.Addr)
		}
	}
	next := m.cfg
	next.Peers = append(append([]Peer(nil), m.cfg.Peers...), p)
	if err := Save(m.path, next); err != nil {
		return Peer{}, err
	}
	m.cfg = next
	return p, nil
}

// RemovePeer removes a peer by ID and atomically persists config.json.
func (m *Manager) RemovePeer(peerID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := -1
	for i, p := range m.cfg.Peers {
		if p.ID == peerID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, nil
	}
	next := m.cfg
	next.Peers = make([]Peer, 0, len(m.cfg.Peers)-1)
	next.Peers = append(next.Peers, m.cfg.Peers[:idx]...)
	next.Peers = append(next.Peers, m.cfg.Peers[idx+1:]...)
	if err := Save(m.path, next); err != nil {
		return false, err
	}
	m.cfg = next
	return true, nil
}

// Save writes cfg atomically with mode 0600 inside a 0700 parent directory.
func Save(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
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

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// GenerateToken returns a 64-hex-character (256-bit) cryptographically random secret.
func GenerateToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// DetectTailscaleIP returns the local Tailscale IPv4 address in 100.64.0.0/10,
// preferring interfaces named tailscale* (Linux) or utun* (macOS).
func DetectTailscaleIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var fallback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		lower := strings.ToLower(iface.Name)
		preferred := strings.HasPrefix(lower, "tailscale") || strings.HasPrefix(lower, "utun")
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			v4 := ipNet.IP.To4()
			if v4 == nil {
				continue
			}
			addr, ok := netip.AddrFromSlice(v4)
			if !ok || !tailscalePrefix.Contains(addr) {
				continue
			}
			if preferred {
				return addr.String(), nil
			}
			if fallback == "" {
				fallback = addr.String()
			}
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errors.New("no Tailscale IPv4 address (100.64.0.0/10) found on active interfaces")
}

// DefaultDataDir returns ~/LocalChat.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "LocalChat"), nil
}

// ParsePeerSpec parses "id=name@host:port" CLI flags for `localchat init`.
func ParsePeerSpec(spec string) (Peer, error) {
	eq := strings.IndexByte(spec, '=')
	at := strings.LastIndexByte(spec, '@')
	if eq <= 0 || at <= eq+1 || at >= len(spec)-1 {
		return Peer{}, fmt.Errorf("invalid peer spec %q: expected id=name@host:port", spec)
	}
	return Peer{
		ID:   strings.TrimSpace(spec[:eq]),
		Name: strings.TrimSpace(spec[eq+1 : at]),
		Addr: strings.TrimSpace(spec[at+1:]),
	}, nil
}
