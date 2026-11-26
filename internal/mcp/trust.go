package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A workspace .soma/mcp.yaml travels with the repository, so a cloned project could make soma
// spawn arbitrary host commands the moment a task starts. Stdio servers from a workspace config
// are therefore launched only after the operator trusts that exact entry (command, args and env
// names) for that project; anything soma itself wrote (soma mcp add, the dashboard) is trusted
// as it is written. Trust lives in ~/.soma/mcp_trust.json, outside every repository.

var trustMu sync.Mutex

func trustPath() string {
	if p := strings.TrimSpace(os.Getenv("SOMA_MCP_TRUST_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".soma", "mcp_trust.json")
}

// trustKey fingerprints a stdio entry so any change to what would run requires new trust.
func trustKey(wsDir string, ep ServerEndpoint) string {
	abs, err := filepath.Abs(wsDir)
	if err != nil {
		abs = wsDir
	}
	envNames := make([]string, 0, len(ep.Env))
	for k, v := range ep.Env {
		envNames = append(envNames, k+"="+v)
	}
	sort.Strings(envNames)
	h := sha256.Sum256([]byte(strings.Join([]string{abs, ep.Name, ep.Command, strings.Join(ep.Args, "\x1f"), strings.Join(envNames, "\x1f")}, "\x00")))
	return hex.EncodeToString(h[:])
}

func readTrust() map[string]bool {
	out := map[string]bool{}
	p := trustPath()
	if p == "" {
		return out
	}
	if raw, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func writeTrust(m map[string]bool) error {
	p := trustPath()
	if p == "" {
		return os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(p, raw, 0o600)
}

// TrustWorkspaceServers records operator trust for stdio entries of a workspace config.
func TrustWorkspaceServers(wsDir string, eps ...ServerEndpoint) error {
	trustMu.Lock()
	defer trustMu.Unlock()
	m := readTrust()
	for _, ep := range eps {
		if ep.EffectiveTransport() == "stdio" {
			m[trustKey(wsDir, ep)] = true
		}
	}
	return writeTrust(m)
}

// IsWorkspaceServerTrusted reports whether a workspace stdio entry may be launched.
func IsWorkspaceServerTrusted(wsDir string, ep ServerEndpoint) bool {
	if ep.EffectiveTransport() != "stdio" {
		return true
	}
	trustMu.Lock()
	defer trustMu.Unlock()
	return readTrust()[trustKey(wsDir, ep)]
}

// UntrustedWorkspaceServers lists workspace stdio entries that will not be launched until trusted.
func UntrustedWorkspaceServers(wsDir string) []ServerEndpoint {
	if strings.TrimSpace(wsDir) == "" {
		return nil
	}
	cfg, err := ReadConfigFile(WorkspaceConfigPath(wsDir))
	if err != nil {
		return nil
	}
	var out []ServerEndpoint
	for name, ep := range cfg.MCPServers {
		ep.Name = name
		if !IsWorkspaceServerTrusted(wsDir, ep) {
			out = append(out, ep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// workspaceOfConfig returns the project directory when path is a workspace .soma/mcp.yaml.
func workspaceOfConfig(path string) (string, bool) {
	clean := filepath.Clean(path)
	if filepath.Base(clean) != "mcp.yaml" || filepath.Base(filepath.Dir(clean)) != ".soma" {
		return "", false
	}
	if g := GlobalConfigPath(); g != "" && filepath.Clean(g) == clean {
		return "", false
	}
	return filepath.Dir(filepath.Dir(clean)), true
}

// minimalServerEnv is the environment of a stdio MCP server: enough to run, no secrets. Declared
// per-server env (with ${VAR} expansion) is added on top.
func minimalServerEnv(declared map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "PATH", k == "HOME", k == "USER", k == "LOGNAME", k == "LANG", k == "LC_ALL", k == "TERM",
			k == "SHELL", k == "TMPDIR", k == "TZ", strings.HasPrefix(k, "XDG_"), strings.HasPrefix(k, "LC_"):
			env = append(env, kv)
		}
	}
	for k, v := range declared {
		env = append(env, k+"="+v)
	}
	return env
}
