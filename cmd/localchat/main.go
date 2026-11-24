package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"localchat/internal/config"
	"localchat/internal/lifecycle"
	"localchat/internal/lock"
	"localchat/internal/store"
	"localchat/internal/syncer"
	"localchat/internal/ui"
)

var version = "0.1.0"

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	subcmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcmd = args[0]
		args = args[1:]
	}

	var err error
	switch subcmd {
	case "serve":
		err = runServe(args)
	case "open":
		err = runOpen(args)
	case "init":
		err = runInit(args)
	case "version":
		fmt.Printf("localchat %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (expected: serve, open, init, version)\n", subcmd)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "localchat %s: %v\n", subcmd, err)
		os.Exit(1)
	}
}

func resolveDataDir(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if env := os.Getenv("LOCALCHAT_DATA"); env != "" {
		return env, nil
	}
	return config.DefaultDataDir()
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dataFlag := fs.String("data", "", "data directory (default ~/LocalChat)")
	idFlag := fs.String("id", "", "short device ID (lowercase alphanumeric, e.g. ubuntu)")
	nameFlag := fs.String("name", "", "display name (e.g. Ubuntu)")
	uiAddrFlag := fs.String("ui-addr", config.DefaultUIAddr, "local UI bind address")
	syncAddrFlag := fs.String("sync-addr", "", "Tailscale sync bind address (default auto-detected 100.64.x.x:8788)")
	tokenFlag := fs.String("token", "", "shared 256-bit workspace token (auto-generated if omitted)")
	forceFlag := fs.Bool("force", false, "overwrite existing config.json")
	var peerFlags stringSliceFlag
	fs.Var(&peerFlags, "peer", "peer in format id=name@ip:port (repeatable)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	dataDir, err := resolveDataDir(*dataFlag)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(dataDir, "config.json")
	if !*forceFlag {
		if _, err := os.Stat(cfgPath); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", cfgPath)
		}
	}

	devID := strings.TrimSpace(*idFlag)
	devName := strings.TrimSpace(*nameFlag)
	if devID == "" {
		host, _ := os.Hostname()
		devID = sanitizeDeviceID(host)
	}
	if devName == "" {
		if runtime.GOOS == "darwin" {
			devName = "Mac"
		} else {
			devName = "Ubuntu"
		}
	}

	syncAddr := strings.TrimSpace(*syncAddrFlag)
	if syncAddr == "" {
		ip, derr := config.DetectTailscaleIP()
		if derr != nil {
			return fmt.Errorf("auto-detect Tailscale IP failed (%w); pass --sync-addr <tailscale-ip>:8788", derr)
		}
		syncAddr = fmt.Sprintf("%s:%d", ip, config.DefaultSyncPort)
	}

	tok := strings.TrimSpace(*tokenFlag)
	if tok == "" {
		tok, err = config.GenerateToken()
		if err != nil {
			return err
		}
	}

	peers := make([]config.Peer, 0, len(peerFlags))
	for _, spec := range peerFlags {
		p, perr := config.ParsePeerSpec(spec)
		if perr != nil {
			return perr
		}
		peers = append(peers, p)
	}

	cfg := config.Config{
		DeviceID:       devID,
		DeviceName:     devName,
		UIAddr:         *uiAddrFlag,
		SyncAddr:       syncAddr,
		WorkspaceToken: tok,
		MaxUploadBytes: config.DefaultMaxUploadBytes,
		IdleTimeoutSec: config.DefaultIdleTimeoutSec,
		Peers:          peers,
	}
	if err := cfg.Validate(false); err != nil {
		return err
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}

	fmt.Printf("Initialized LocalChat config at %s\n", cfgPath)
	fmt.Printf("  Device:     %s (%s)\n", cfg.DeviceName, cfg.DeviceID)
	fmt.Printf("  UI URL:     http://%s\n", cfg.UIAddr)
	fmt.Printf("  Sync Addr:  %s\n", cfg.SyncAddr)
	fmt.Printf("  Token:      %s\n", cfg.WorkspaceToken)
	return nil
}

func sanitizeDeviceID(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" || !config.ValidateID(out) {
		return runtime.GOOS
	}
	return out
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataFlag := fs.String("data", "", "data directory (default ~/LocalChat)")
	idleFlag := fs.Duration("idle-timeout", 0, "override idle shutdown duration (e.g. 5m, 3s)")
	noIdleFlag := fs.Bool("no-idle-exit", false, "disable automatic idle shutdown")
	outboundOnlyFlag := fs.Bool("outbound-only", false, "skip inbound Tailscale listener (sync outbound to configured peers)")
	autoInitFlag := fs.Bool("auto-init", false, "auto-create default config.json on first run (for mobile wrapper)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dataDir, err := resolveDataDir(*dataFlag)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(dataDir, "config.json")
	cfgMgr, err := config.Load(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && *autoInitFlag {
			tok, gerr := config.GenerateToken()
			if gerr != nil {
				return gerr
			}
			syncAddr := ""
			if !*outboundOnlyFlag {
				if ip, derr := config.DetectTailscaleIP(); derr == nil {
					syncAddr = fmt.Sprintf("%s:%d", ip, config.DefaultSyncPort)
				}
			}
			defName := "Phone"
			defID := "phone"
			if runtime.GOOS == "darwin" {
				defName, defID = "Mac", "mac"
			}
			initCfg := config.Config{
				DeviceID:       defID,
				DeviceName:     defName,
				UIAddr:         config.DefaultUIAddr,
				SyncAddr:       syncAddr,
				WorkspaceToken: tok,
				MaxUploadBytes: config.DefaultMaxUploadBytes,
				IdleTimeoutSec: config.DefaultIdleTimeoutSec,
				Peers:          []config.Peer{},
			}
			if serr := config.Save(cfgPath, initCfg); serr != nil {
				return serr
			}
			cfgMgr = config.NewManager(cfgPath, initCfg)
		} else if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing %s — run 'localchat init' first", cfgPath)
		} else {
			return err
		}
	}
	cfg := cfgMgr.Snapshot()

	fileLock, acquired, err := lock.Acquire(filepath.Join(dataDir, "localchat.lock"))
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	if !acquired {
		slog.Info("localchat already running for data dir, exiting", "data_dir", dataDir)
		return nil
	}
	defer fileLock.Release()

	st, err := store.Open(dataDir, cfg.DeviceID, cfg.DeviceName)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	idleTimeout := time.Duration(cfg.IdleTimeoutSec) * time.Second
	if *idleFlag > 0 {
		idleTimeout = *idleFlag
	}
	tracker := lifecycle.NewTracker(idleTimeout, *noIdleFlag)
	syn := syncer.New(cfgMgr, st, tracker)

	// Start Tailscale sync listener under a supervisor so offline mode never blocks UI startup.
	var (
		syncSrvMu sync.Mutex
		syncSrv   *http.Server
	)
	tryBindSync := func() {
		syncSrvMu.Lock()
		defer syncSrvMu.Unlock()
		if syncSrv != nil {
			return
		}
		addr := strings.TrimSpace(cfgMgr.Snapshot().SyncAddr)
		if *outboundOnlyFlag || addr == "" {
			syn.SetListenerStatus(true, "")
			return
		}
		ln, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			syn.SetListenerStatus(false, lerr.Error())
			slog.Warn("sync listener unavailable; running in local offline mode", "addr", addr, "err", lerr)
			return
		}
		srv := &http.Server{
			Handler:           syn.NewHandler(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       0,
			WriteTimeout:      0,
		}
		syncSrv = srv
		syn.SetListenerStatus(true, "")
		slog.Info("sync listener active", "addr", addr)
		go func() {
			if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				slog.Warn("sync listener stopped", "err", serr)
				syncSrvMu.Lock()
				if syncSrv == srv {
					syncSrv = nil
				}
				syncSrvMu.Unlock()
				syn.SetListenerStatus(false, serr.Error())
			}
		}()
	}

	tryBindSync()

	uiSrvObj := ui.NewServer(cfgMgr, st, syn, tracker, tryBindSync)
	uiLn, err := net.Listen("tcp", cfg.UIAddr)
	if err != nil {
		return fmt.Errorf("bind UI listener %s: %w", cfg.UIAddr, err)
	}
	uiHTTP := &http.Server{
		Handler:           uiSrvObj.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
	}
	slog.Info("localchat UI listening", "url", "http://"+cfg.UIAddr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Initial non-blocking catch-up sync.
	go syn.SyncAllPeers(ctx, true)

	// Periodic sync & bind-retry loop (10s).
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tryBindSync()
				if tracker.UIActiveWithin(60 * time.Second) {
					syn.SyncAllPeers(ctx, false)
				}
			}
		}
	}()

	// Idle reaper loop.
	checkInterval := 30 * time.Second
	if idleTimeout > 0 && idleTimeout < checkInterval {
		checkInterval = idleTimeout / 2
		if checkInterval < 200*time.Millisecond {
			checkInterval = 200 * time.Millisecond
		}
	}
	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if tracker.ShouldExit() {
					slog.Info("idle timeout reached, shutting down")
					stop()
					return
				}
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		if serr := uiHTTP.Serve(uiLn); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
		}
	}()

	select {
	case <-ctx.Done():
	case serr := <-errCh:
		return serr
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = uiHTTP.Shutdown(shutCtx)
	syncSrvMu.Lock()
	if syncSrv != nil {
		_ = syncSrv.Shutdown(shutCtx)
	}
	syncSrvMu.Unlock()
	syn.Wait()
	return nil
}

func runOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	dataFlag := fs.String("data", "", "data directory (default ~/LocalChat)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dataDir, err := resolveDataDir(*dataFlag)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(dataDir, "config.json")
	cfgMgr, err := config.Load(cfgPath)
	if err != nil {
		msg := fmt.Sprintf("LocalChat is not configured yet (%s missing). Run 'localchat init' in a terminal.", cfgPath)
		notifyError(msg)
		return errors.New(msg)
	}
	uiURL := "http://" + cfgMgr.Snapshot().UIAddr

	if !checkHealth(uiURL) {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return err
		}
		logFile, err := os.OpenFile(filepath.Join(dataDir, "localchat.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		cmdArgs := []string{"serve", "--data", dataDir}
		cmd := exec.Command(exe, cmdArgs...)
		cmd.Stdin = nil
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			notifyError("Failed to start LocalChat: " + err.Error())
			return err
		}
		_ = logFile.Close()

		deadline := time.Now().Add(2 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			if checkHealth(uiURL) {
				ready = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ready {
			msg := fmt.Sprintf("LocalChat failed to start on %s — check %s", uiURL, filepath.Join(dataDir, "localchat.log"))
			notifyError(msg)
			return errors.New(msg)
		}
	}

	return openBrowser(uiURL)
}

func checkHealth(uiURL string) bool {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(uiURL + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		App string `json:"app"`
		OK  bool   `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false
	}
	return payload.App == "localchat" && payload.OK
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", url)
	} else {
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func notifyError(msg string) {
	if runtime.GOOS == "darwin" {
		_ = exec.Command("osascript", "-e", fmt.Sprintf(`display notification %q with title "LocalChat"`, msg)).Run()
	} else {
		_ = exec.Command("notify-send", "LocalChat", msg).Run()
	}
}
