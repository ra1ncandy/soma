package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultAgentConfigPath returns the standard MCP config path for a supported agent source.
func DefaultAgentConfigPath(source, wsDir string) (string, error) {
	home, _ := os.UserHomeDir()
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "claude", "claude-code":
		if wsDir != "" {
			local := filepath.Join(wsDir, ".mcp.json")
			if _, err := os.Stat(local); err == nil {
				return local, nil
			}
		}
		return filepath.Join(home, ".claude.json"), nil
	case "cursor":
		if wsDir != "" {
			local := filepath.Join(wsDir, ".cursor", "mcp.json")
			if _, err := os.Stat(local); err == nil {
				return local, nil
			}
		}
		return filepath.Join(home, ".cursor", "mcp.json"), nil
	case "codex":
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "hermes":
		return filepath.Join(home, ".hermes", "config.yaml"), nil
	default:
		return "", fmt.Errorf("unknown import source %q (supported: claude-code, cursor, codex, hermes)", source)
	}
}

// ImportMCPFromFile parses MCP server definitions from Claude Code, Cursor, Codex, or Hermes config files.
func ImportMCPFromFile(source, path string) ([]ServerEndpoint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src := strings.ToLower(strings.TrimSpace(source))
	if src == "" || src == "auto" {
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".toml":
			src = "codex"
		case ".yaml", ".yml":
			src = "hermes"
		default:
			src = "claude-code"
		}
	}

	switch src {
	case "claude", "claude-code", "cursor":
		return parseMCPServersJSON(raw)
	case "hermes":
		return parseHermesYAML(raw)
	case "codex":
		return parseCodexTOML(string(raw))
	default:
		return nil, fmt.Errorf("unsupported MCP import source %q", source)
	}
}

type jsonMCPServer struct {
	Type           string            `json:"type,omitempty"`
	Transport      string            `json:"transport,omitempty"`
	URL            string            `json:"url,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	IncludeTools   []string          `json:"include_tools,omitempty"`
	ExcludeTools   []string          `json:"exclude_tools,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Disabled       bool              `json:"disabled,omitempty"`
}

func parseMCPServersJSON(raw []byte) ([]ServerEndpoint, error) {
	var doc struct {
		MCPServers map[string]jsonMCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse mcpServers JSON: %w", err)
	}
	names := make([]string, 0, len(doc.MCPServers))
	for k := range doc.MCPServers {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]ServerEndpoint, 0, len(names))
	for _, name := range names {
		s := doc.MCPServers[name]
		tr := s.Transport
		if tr == "" {
			tr = s.Type
		}
		ep := ServerEndpoint{
			Name:           name,
			Transport:      tr,
			URL:            s.URL,
			Headers:        s.Headers,
			Command:        s.Command,
			Args:           s.Args,
			Env:            s.Env,
			IncludeTools:   s.IncludeTools,
			ExcludeTools:   s.ExcludeTools,
			TimeoutSeconds: s.TimeoutSeconds,
		}
		if s.Disabled {
			f := false
			ep.Enabled = &f
		}
		out = append(out, ep)
	}
	return out, nil
}

func parseHermesYAML(raw []byte) ([]ServerEndpoint, error) {
	var doc struct {
		MCPServers map[string]ServerEndpoint `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse hermes YAML: %w", err)
	}
	names := make([]string, 0, len(doc.MCPServers))
	for k := range doc.MCPServers {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]ServerEndpoint, 0, len(names))
	for _, name := range names {
		ep := doc.MCPServers[name]
		ep.Name = name
		out = append(out, ep)
	}
	return out, nil
}

// parseCodexTOML extracts [mcp_servers.<name>] and [mcp_servers.<name>.env] sections from Codex config.toml.
func parseCodexTOML(content string) ([]ServerEndpoint, error) {
	servers := map[string]*ServerEndpoint{}
	var curName string
	var inEnv bool

	getOrCreate := func(name string) *ServerEndpoint {
		if ep, ok := servers[name]; ok {
			return ep
		}
		ep := &ServerEndpoint{Name: name}
		servers[name] = ep
		return ep
	}

	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sec := strings.TrimSpace(line[1 : len(line)-1])
			curName = ""
			inEnv = false
			if strings.HasPrefix(sec, "mcp_servers.") {
				rest := strings.TrimPrefix(sec, "mcp_servers.")
				if strings.HasSuffix(rest, ".env") {
					curName = strings.TrimSuffix(rest, ".env")
					inEnv = true
				} else if !strings.Contains(rest, ".") {
					curName = rest
				}
				if curName != "" {
					getOrCreate(curName)
				}
			}
			continue
		}
		if curName == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		ep := getOrCreate(curName)
		if inEnv {
			if ep.Env == nil {
				ep.Env = map[string]string{}
			}
			ep.Env[key] = unquoteTOMLString(val)
			continue
		}
		switch key {
		case "command":
			ep.Command = unquoteTOMLString(val)
		case "url":
			ep.URL = unquoteTOMLString(val)
		case "transport":
			ep.Transport = unquoteTOMLString(val)
		case "args":
			ep.Args = parseTOMLStringArray(val)
		case "enabled":
			b := val == "true"
			ep.Enabled = &b
		}
	}

	names := make([]string, 0, len(servers))
	for k := range servers {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]ServerEndpoint, 0, len(names))
	for _, n := range names {
		out = append(out, *servers[n])
	}
	return out, nil
}

func unquoteTOMLString(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func parseTOMLStringArray(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := unquoteTOMLString(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
