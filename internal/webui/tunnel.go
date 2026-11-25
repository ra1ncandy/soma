package webui

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var tryCloudflareURLRe = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

// TunnelConfig persists cloudflared remote access settings in .soma/harness.json.
type TunnelConfig struct {
	Mode        string `json:"mode"`                   // "quick" | "token" | "config"
	TunnelToken string `json:"tunnel_token,omitempty"` // Cloudflare Zero Trust tunnel token (when mode == "token")
	ConfigPath  string `json:"config_path,omitempty"`  // Local cloudflared YAML config path (when mode == "config")
	Hostname    string `json:"hostname,omitempty"`     // Optional custom hostname (when mode == "token" or "config")
	AuthToken   string `json:"auth_token,omitempty"`   // Required dashboard Bearer / session token for external access
	AutoStart   bool   `json:"auto_start,omitempty"`   // Automatically start cloudflared when soma web boots
}

// TunnelDTO is the secret-safe status view returned in HarnessDTO.
type TunnelDTO struct {
	Installed       bool   `json:"installed"`
	Running         bool   `json:"running"`
	AutoStart       bool   `json:"auto_start"`
	Mode            string `json:"mode"`
	ConfigPath      string `json:"config_path,omitempty"`
	PublicURL       string `json:"public_url,omitempty"`
	Hostname        string `json:"hostname,omitempty"`
	HasTunnelToken  bool   `json:"has_tunnel_token"`
	HasAuthToken    bool   `json:"has_auth_token"`
	AuthTokenMasked string `json:"auth_token_masked,omitempty"`
	LastError       string `json:"last_error,omitempty"`
}

// TunnelManager supervises a local cloudflared subprocess for authenticated external dashboard access.
type TunnelManager struct {
	startMu   sync.Mutex // serialises Start so two calls cannot each leave a cloudflared running
	mu        sync.Mutex
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	running   bool
	mode      string
	publicURL string
	lastErr   string
}

// NewTunnelManager creates a TunnelManager.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{}
}

// IsInstalled reports whether cloudflared is available in PATH.
func (tm *TunnelManager) IsInstalled() bool {
	_, err := exec.LookPath("cloudflared")
	return err == nil
}

// Status returns the current runtime state combined with cfg.
func (tm *TunnelManager) Status(cfg TunnelConfig) TunnelDTO {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" {
		mode = "quick"
	}
	pub := tm.publicURL
	if pub == "" && (mode == "token" || mode == "config") && strings.TrimSpace(cfg.Hostname) != "" && tm.running {
		pub = "https://" + strings.TrimPrefix(strings.TrimSpace(cfg.Hostname), "https://")
	}
	return TunnelDTO{
		Installed:       tm.IsInstalled(),
		Running:         tm.running,
		AutoStart:       cfg.AutoStart,
		Mode:            mode,
		ConfigPath:      cfg.ConfigPath,
		PublicURL:       pub,
		Hostname:        cfg.Hostname,
		HasTunnelToken:  strings.TrimSpace(cfg.TunnelToken) != "",
		HasAuthToken:    strings.TrimSpace(cfg.AuthToken) != "",
		AuthTokenMasked: maskSecret(cfg.AuthToken),
		LastError:       tm.lastErr,
	}
}

// Start launches cloudflared pointing at localURL (e.g. "http://127.0.0.1:7420").
// It requires cfg.AuthToken to be set so the exposed dashboard is never unauthenticated.
func (tm *TunnelManager) Start(parentCtx context.Context, localURL string, cfg TunnelConfig) error {
	tm.startMu.Lock()
	defer tm.startMu.Unlock()
	if strings.TrimSpace(cfg.AuthToken) == "" {
		return errors.New("tunnel: dashboard auth_token is required before exposing externally")
	}
	binPath, err := exec.LookPath("cloudflared")
	if err != nil {
		return errors.New("cloudflared binary not found in PATH (install from https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/)")
	}

	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = "quick"
	}

	var args []string
	switch mode {
	case "quick":
		args = []string{"tunnel", "--no-autoupdate", "--url", localURL}
	case "token":
		if strings.TrimSpace(cfg.TunnelToken) == "" {
			return errors.New("tunnel: tunnel_token is required for named token mode")
		}
		args = []string{"tunnel", "--no-autoupdate", "run", "--token", strings.TrimSpace(cfg.TunnelToken)}
	case "config":
		if strings.TrimSpace(cfg.ConfigPath) == "" {
			return errors.New("tunnel: config_path is required for config mode")
		}
		args = []string{"tunnel", "--no-autoupdate", "--config", strings.TrimSpace(cfg.ConfigPath), "run"}
	default:
		return fmt.Errorf("tunnel: unsupported mode %q (expected quick, token, or config)", cfg.Mode)
	}

	tm.Stop()

	ctx, cancel := context.WithCancel(parentCtx)
	cmd := exec.CommandContext(ctx, binPath, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start cloudflared: %w", err)
	}

	tm.mu.Lock()
	tm.cmd = cmd
	tm.cancel = cancel
	tm.running = true
	tm.mode = mode
	tm.publicURL = ""
	tm.lastErr = ""
	tm.mu.Unlock()

	urlFound := make(chan string, 1)
	scanPipe := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			if m := tryCloudflareURLRe.FindString(line); m != "" {
				tm.mu.Lock()
				tm.publicURL = m
				tm.mu.Unlock()
				select {
				case urlFound <- m:
				default:
				}
			}
		}
	}
	go scanPipe(stderr)
	go scanPipe(stdout)
	go func() {
		waitErr := cmd.Wait()
		tm.mu.Lock()
		// Only the current process may report "stopped": a previous tunnel's exit must not mark
		// its replacement (which is live and public) as down.
		if tm.cmd == cmd {
			tm.running = false
			if waitErr != nil && ctx.Err() == nil {
				tm.lastErr = waitErr.Error()
			}
		}
		tm.mu.Unlock()
	}()

	if mode == "quick" {
		select {
		case <-urlFound:
		case <-time.After(8 * time.Second):
		case <-parentCtx.Done():
		}
	}
	return nil
}

// Stop terminates any active cloudflared subprocess.
func (tm *TunnelManager) Stop() {
	tm.mu.Lock()
	cancel := tm.cancel
	cmd := tm.cmd
	tm.cancel = nil
	tm.cmd = nil
	tm.running = false
	tm.publicURL = ""
	tm.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func generateRandomAuthToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
