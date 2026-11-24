package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFile represents the YAML schema for .soma/mcp.yaml and ~/.soma/mcp.yaml.
type ConfigFile struct {
	MCPServers map[string]ServerEndpoint `yaml:"mcp_servers"`
}

// GlobalConfigPath returns ~/.soma/mcp.yaml.
func GlobalConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".soma", "mcp.yaml")
	}
	return filepath.Join(home, ".soma", "mcp.yaml")
}

// WorkspaceConfigPath returns <wsDir>/.soma/mcp.yaml.
func WorkspaceConfigPath(wsDir string) string {
	if strings.TrimSpace(wsDir) == "" {
		wsDir = "."
	}
	return filepath.Join(wsDir, ".soma", "mcp.yaml")
}

// DefaultConfigPath returns either the global or workspace mcp.yaml path.
func DefaultConfigPath(wsDir string, global bool) string {
	if global {
		return GlobalConfigPath()
	}
	return WorkspaceConfigPath(wsDir)
}

// ReadConfigFile reads a single mcp.yaml file; returns an empty ConfigFile if the file does not exist.
func ReadConfigFile(path string) (ConfigFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ConfigFile{MCPServers: map[string]ServerEndpoint{}}, nil
		}
		return ConfigFile{}, err
	}
	var cfg ConfigFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return ConfigFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerEndpoint{}
	}
	for name, ep := range cfg.MCPServers {
		if ep.Name == "" {
			ep.Name = name
			cfg.MCPServers[name] = ep
		}
	}
	return cfg, nil
}

// WriteConfigFile writes cfg to path, creating parent directories if needed.
func WriteConfigFile(path string, cfg ConfigFile) error {
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerEndpoint{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// LoadServerEndpoints loads global (~/.soma/mcp.yaml) and workspace (<wsDir>/.soma/mcp.yaml)
// endpoints, with workspace entries overriding global entries of the same name.
func LoadServerEndpoints(wsDir string) ([]ServerEndpoint, error) {
	merged := map[string]ServerEndpoint{}

	if gCfg, err := ReadConfigFile(GlobalConfigPath()); err == nil {
		for k, ep := range gCfg.MCPServers {
			ep.Name = k
			merged[k] = ep
		}
	}
	if strings.TrimSpace(wsDir) != "" {
		wCfg, err := ReadConfigFile(WorkspaceConfigPath(wsDir))
		if err != nil {
			return nil, err
		}
		for k, ep := range wCfg.MCPServers {
			ep.Name = k
			merged[k] = ep
		}
	}

	names := make([]string, 0, len(merged))
	for k := range merged {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]ServerEndpoint, 0, len(names))
	for _, n := range names {
		out = append(out, merged[n])
	}
	return out, nil
}

// SaveServerEndpoint adds or updates ep in the mcp.yaml at path.
func SaveServerEndpoint(path string, ep ServerEndpoint) error {
	name := strings.TrimSpace(ep.Name)
	if name == "" {
		return errors.New("mcp config: server name is required")
	}
	ep.Name = name
	cfg, err := ReadConfigFile(path)
	if err != nil {
		return err
	}
	cfg.MCPServers[name] = ep
	return WriteConfigFile(path, cfg)
}

// RemoveServerEndpoint removes name from the mcp.yaml at path.
func RemoveServerEndpoint(path, name string) (bool, error) {
	name = strings.TrimSpace(name)
	cfg, err := ReadConfigFile(path)
	if err != nil {
		return false, err
	}
	if _, ok := cfg.MCPServers[name]; !ok {
		return false, nil
	}
	delete(cfg.MCPServers, name)
	return true, WriteConfigFile(path, cfg)
}

// SetServerEnabled toggles Enabled for name in the mcp.yaml at path.
func SetServerEnabled(path, name string, enabled bool) error {
	name = strings.TrimSpace(name)
	cfg, err := ReadConfigFile(path)
	if err != nil {
		return err
	}
	ep, ok := cfg.MCPServers[name]
	if !ok {
		return fmt.Errorf("mcp server %q not found in %s", name, path)
	}
	ep.Enabled = &enabled
	cfg.MCPServers[name] = ep
	return WriteConfigFile(path, cfg)
}
