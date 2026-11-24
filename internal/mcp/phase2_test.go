package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ra1ncandy/soma/internal/store"
)

func TestRegistrySaveLoadEnableRemove(t *testing.T) {
	ws := t.TempDir()
	cfgPath := DefaultConfigPath(ws, false)

	err := SaveServerEndpoint(cfgPath, ServerEndpoint{
		Name:      "github",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-github"},
	})
	if err != nil {
		t.Fatalf("SaveServerEndpoint: %v", err)
	}
	eps, err := LoadServerEndpoints(ws)
	if err != nil {
		t.Fatalf("LoadServerEndpoints: %v", err)
	}
	var found bool
	for _, ep := range eps {
		if ep.Name == "github" {
			found = true
			if !ep.IsEnabled() {
				t.Fatal("expected github to be enabled by default")
			}
		}
	}
	if !found {
		t.Fatal("expected github in LoadServerEndpoints")
	}

	if err := SetServerEnabled(cfgPath, "github", false); err != nil {
		t.Fatalf("SetServerEnabled: %v", err)
	}
	cfg, err := ReadConfigFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers["github"].IsEnabled() {
		t.Fatal("expected github to be disabled")
	}

	removed, err := RemoveServerEndpoint(cfgPath, "github")
	if err != nil || !removed {
		t.Fatalf("RemoveServerEndpoint: removed=%v err=%v", removed, err)
	}
}

func TestPresetsAndImporters(t *testing.T) {
	presets := ListPresets()
	if len(presets) < 10 {
		t.Fatalf("expected at least 10 curated presets, got %d", len(presets))
	}
	ep, info, err := GetPreset("filesystem", "/tmp/work")
	if err != nil {
		t.Fatalf("GetPreset(filesystem): %v", err)
	}
	if ep.Command != "npx" || len(ep.Args) < 3 || ep.Args[2] != "/tmp/work" || info.Name != "filesystem" {
		t.Fatalf("unexpected filesystem preset: %+v", ep)
	}
	lin, _, err := GetPreset("linear")
	if err != nil || lin.EffectiveTransport() != "streamable_http" || lin.OAuth == nil || !lin.OAuth.Enabled {
		t.Fatalf("unexpected linear preset: err=%v ep=%+v", err, lin)
	}

	dir := t.TempDir()

	// Claude Code / Cursor JSON
	claudeJSON := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{
  "mcpServers": {
    "sqlite": {
      "command": "uvx",
      "args": ["mcp-server-sqlite", "--db-path", "test.db"]
    },
    "remote": {
      "type": "streamable_http",
      "url": "https://mcp.example.com/mcp",
      "disabled": true
    }
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cEps, err := ImportMCPFromFile("claude-code", claudeJSON)
	if err != nil || len(cEps) != 2 {
		t.Fatalf("ImportMCPFromFile(claude-code): err=%v eps=%+v", err, cEps)
	}
	if cEps[0].Name != "remote" || cEps[0].IsEnabled() {
		t.Fatalf("expected disabled remote server, got %+v", cEps[0])
	}

	// Codex TOML
	codexTOML := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(codexTOML, []byte(`
[mcp_servers.github]
command = "npx"
args = ["-y", "@modelcontextprotocol/server-github"]

[mcp_servers.github.env]
GITHUB_PERSONAL_ACCESS_TOKEN = "${GITHUB_TOKEN}"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	tEps, err := ImportMCPFromFile("codex", codexTOML)
	if err != nil || len(tEps) != 1 || tEps[0].Name != "github" || tEps[0].Env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "${GITHUB_TOKEN}" {
		t.Fatalf("ImportMCPFromFile(codex): err=%v eps=%+v", err, tEps)
	}

	// Hermes YAML
	hermesYAML := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(hermesYAML, []byte(`
mcp_servers:
  fetch:
    command: uvx
    args: ["mcp-server-fetch"]
    include_tools: ["fetch"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	hEps, err := ImportMCPFromFile("hermes", hermesYAML)
	if err != nil || len(hEps) != 1 || len(hEps[0].IncludeTools) != 1 {
		t.Fatalf("ImportMCPFromFile(hermes): err=%v eps=%+v", err, hEps)
	}
}

func TestReverseMCPServe(t *testing.T) {
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewSomaMCPServer(st, ServeOptions{WorkspaceDir: ws})
	t1, t2 := sdkmcp.NewInMemoryTransports()

	go func() {
		_ = srv.Run(ctx, t1)
	}()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	sess, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("connect in-memory reverse mcp: %v", err)
	}
	defer func() { _ = sess.Close() }()

	toolsRes, err := sess.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil || len(toolsRes.Tools) < 4 {
		t.Fatalf("expected >=4 soma tools, got err=%v len=%d", err, len(toolsRes.Tools))
	}

	statusRes, err := sess.CallTool(ctx, &sdkmcp.CallToolParams{Name: "soma_status"})
	if err != nil || len(statusRes.Content) == 0 {
		t.Fatalf("soma_status failed: err=%v res=%+v", err, statusRes)
	}
	tc := statusRes.Content[0].(*sdkmcp.TextContent)
	if !strings.Contains(tc.Text, ws) {
		t.Fatalf("expected workspace path in soma_status, got %q", tc.Text)
	}

	propRes, err := sess.CallTool(ctx, &sdkmcp.CallToolParams{
		Name: "soma_memory_propose",
		Arguments: map[string]any{
			"scope":   "global",
			"kind":    "fact",
			"content": "external MCP client proposed fact",
		},
	})
	if err != nil {
		t.Fatalf("soma_memory_propose failed: %v", err)
	}
	ptc := propRes.Content[0].(*sdkmcp.TextContent)
	if !strings.Contains(ptc.Text, `"Status": "proposed"`) && !strings.Contains(ptc.Text, `"status": "proposed"`) {
		t.Fatalf("expected proposed quarantine status, got %q", ptc.Text)
	}
}
