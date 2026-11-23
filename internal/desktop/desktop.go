package desktop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/mcp"
)

const (
	DefaultImage      = "soma-desktop:latest"
	DefaultPort       = 6080
	DefaultResolution = "1280x800"
)

// Config configures soma's isolated Docker virtual desktop container.
type Config struct {
	WorkspaceDir string
	Image        string
	DockerDir    string // Optional path to docker/desktop/ for automatic local build when Image is missing.
	NoVNCPort    int
	Resolution   string // WxH, e.g. "1280x800"
	NetworkMode  string // "bridge" (default) | "none"
	VNCPassword  string
}

// State records a running isolated desktop session persisted in .soma/desktop.json.
type State struct {
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
	WorkspaceDir  string `json:"workspace_dir"`
	NoVNCPort     int    `json:"novnc_port"`
	VNCPassword   string `json:"vnc_password"`
	Resolution    string `json:"resolution"`
	StartedAt     int64  `json:"started_at"`
}

// NoVNCURL returns the loopback-only authenticated noVNC viewer URL.
func (s State) NoVNCURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/vnc.html?autoconnect=true&resize=scale&password=%s", s.NoVNCPort, s.VNCPassword)
}

// Manager controls the lifecycle of soma's isolated Docker desktop.
type Manager struct {
	DockerBin string
}

// NewManager creates a Docker-backed desktop Manager.
func NewManager() *Manager {
	bin, _ := exec.LookPath("docker")
	return &Manager{DockerBin: bin}
}

// ContainerNameForWorkspace derives a deterministic Docker container name for wsDir.
func ContainerNameForWorkspace(wsDir string) string {
	abs, err := filepath.Abs(wsDir)
	if err != nil {
		abs = wsDir
	}
	sum := sha256.Sum256([]byte(abs))
	return "soma-desktop-" + hex.EncodeToString(sum[:6])
}

// BuildRunArgs constructs the hardened `docker run` argument slice for cfg.
func BuildRunArgs(cfg Config, containerName, vncPass string) ([]string, error) {
	if strings.TrimSpace(cfg.WorkspaceDir) == "" {
		return nil, errors.New("desktop: WorkspaceDir is required")
	}
	absWS, err := filepath.Abs(cfg.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	img := strings.TrimSpace(cfg.Image)
	if img == "" {
		img = DefaultImage
	}
	port := cfg.NoVNCPort
	if port <= 0 {
		port = DefaultPort
	}
	w, h := parseResolution(cfg.Resolution)
	netMode := strings.TrimSpace(cfg.NetworkMode)
	if netMode == "" {
		netMode = "bridge"
	}
	args := []string{
		"run", "-d", "--rm",
		"--name", containerName,
		"--shm-size=2g",
		"--network", netMode,
		// Strictly bind noVNC to loopback only; never expose VNC to 0.0.0.0.
		"-p", fmt.Sprintf("127.0.0.1:%d:6080", port),
		"-v", absWS + ":/workspace",
		"-e", "SCREEN_WIDTH=" + strconv.Itoa(w),
		"-e", "SCREEN_HEIGHT=" + strconv.Itoa(h),
		"-e", "SOMA_VNC_PASSWORD=" + vncPass,
		img,
	}
	return args, nil
}

// Start ensures the desktop image exists, launches the isolated container, and saves .soma/desktop.json.
func (m *Manager) Start(ctx context.Context, cfg Config) (State, error) {
	if m.DockerBin == "" {
		return State{}, errors.New("desktop: docker binary not found in PATH")
	}
	if strings.TrimSpace(cfg.Image) == "" {
		cfg.Image = DefaultImage
	}
	if cfg.NoVNCPort <= 0 {
		cfg.NoVNCPort = DefaultPort
	}
	if strings.TrimSpace(cfg.Resolution) == "" {
		cfg.Resolution = DefaultResolution
	}
	vncPass := strings.TrimSpace(cfg.VNCPassword)
	if vncPass == "" {
		vncPass = randomHex(8)
	}

	containerName := ContainerNameForWorkspace(cfg.WorkspaceDir)
	if st, running, err := m.Status(ctx, cfg.WorkspaceDir); err == nil && running {
		return st, nil
	}

	// Build local image automatically if it does not exist yet and DockerDir is provided.
	inspectCmd := exec.CommandContext(ctx, m.DockerBin, "image", "inspect", cfg.Image)
	if inspectCmd.Run() != nil {
		dockerDir := cfg.DockerDir
		if dockerDir == "" {
			if _, err := os.Stat(filepath.Join(cfg.WorkspaceDir, "docker", "desktop", "Dockerfile")); err == nil {
				dockerDir = filepath.Join(cfg.WorkspaceDir, "docker", "desktop")
			}
		}
		if dockerDir == "" {
			return State{}, fmt.Errorf("desktop: docker image %q not found (pass --docker-dir to build docker/desktop/Dockerfile)", cfg.Image)
		}
		buildCmd := exec.CommandContext(ctx, m.DockerBin, "build", "-t", cfg.Image, dockerDir)
		if out, err := buildCmd.CombinedOutput(); err != nil {
			return State{}, fmt.Errorf("desktop: docker build %s failed: %w\n%s", cfg.Image, err, string(out))
		}
	}

	args, err := BuildRunArgs(cfg, containerName, vncPass)
	if err != nil {
		return State{}, err
	}
	runCmd := exec.CommandContext(ctx, m.DockerBin, args...)
	if out, err := runCmd.CombinedOutput(); err != nil {
		return State{}, fmt.Errorf("desktop: docker run failed: %w\n%s", err, string(out))
	}

	state := State{
		ContainerName: containerName,
		Image:         cfg.Image,
		WorkspaceDir:  cfg.WorkspaceDir,
		NoVNCPort:     cfg.NoVNCPort,
		VNCPassword:   vncPass,
		Resolution:    cfg.Resolution,
		StartedAt:     time.Now().Unix(),
	}
	_ = saveState(cfg.WorkspaceDir, state)
	return state, nil
}

// Stop terminates the isolated desktop container for wsDir and removes .soma/desktop.json.
func (m *Manager) Stop(ctx context.Context, wsDir string) error {
	if m.DockerBin == "" {
		return errors.New("desktop: docker binary not found in PATH")
	}
	name := ContainerNameForWorkspace(wsDir)
	if st, err := loadState(wsDir); err == nil && st.ContainerName != "" {
		name = st.ContainerName
	}
	cmd := exec.CommandContext(ctx, m.DockerBin, "rm", "-f", name)
	_ = cmd.Run()
	_ = os.Remove(statePath(wsDir))
	return nil
}

// Status reports the persisted desktop state and whether its Docker container is currently running.
func (m *Manager) Status(ctx context.Context, wsDir string) (State, bool, error) {
	st, err := loadState(wsDir)
	if err != nil {
		return State{ContainerName: ContainerNameForWorkspace(wsDir)}, false, nil
	}
	if m.DockerBin == "" {
		return st, false, nil
	}
	cmd := exec.CommandContext(ctx, m.DockerBin, "inspect", "-f", "{{.State.Running}}", st.ContainerName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return st, false, nil
	}
	return st, strings.TrimSpace(string(out)) == "true", nil
}

// MCPEndpoints returns the stdio MCP endpoints (desktop + optional headed Playwright) for a running desktop.
func (m *Manager) MCPEndpoints(st State, includePlaywright bool) []mcp.ServerEndpoint {
	eps := []mcp.ServerEndpoint{mcp.DockerDesktopPreset(st.ContainerName)}
	if includePlaywright {
		eps = append(eps, mcp.DockerPlaywrightPreset(st.ContainerName))
	}
	return eps
}

func statePath(wsDir string) string {
	return filepath.Join(wsDir, ".soma", "desktop.json")
}

func saveState(wsDir string, st State) error {
	if err := os.MkdirAll(filepath.Join(wsDir, ".soma"), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(wsDir), b, 0o600)
}

func loadState(wsDir string) (State, error) {
	b, err := os.ReadFile(statePath(wsDir))
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

func parseResolution(res string) (int, int) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(res)), "x")
	if len(parts) == 2 {
		w, err1 := strconv.Atoi(parts[0])
		h, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && w >= 640 && h >= 480 {
			return w, h
		}
	}
	return 1280, 800
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
