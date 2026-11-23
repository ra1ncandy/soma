package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type managedServer struct {
	ep      ServerEndpoint
	session *sdkmcp.ClientSession
	tools   []sdkmcp.Tool
}

// Manager manages live stdio and HTTP MCP server connections for soma runs, supporting
// progressive tool discovery and taint-tagged multimodal tool invocation.
type Manager struct {
	mu         sync.RWMutex
	httpClient *Client
	servers    map[string]*managedServer
}

// NewManager creates a unified MCP Manager supporting both stdio and HTTP servers.
func NewManager() *Manager {
	return &Manager{
		httpClient: NewClient(),
		servers:    make(map[string]*managedServer),
	}
}

// Connect connects to ep (via stdio subprocess or stateless HTTP), lists its tools, and registers it.
func (m *Manager) Connect(ctx context.Context, ep ServerEndpoint) error {
	if strings.TrimSpace(ep.Name) == "" {
		return errors.New("mcp manager: empty server name")
	}
	if !ep.IsEnabled() {
		return nil
	}

	ms := &managedServer{ep: ep}
	switch ep.EffectiveTransport() {
	case "stdio":
		if strings.TrimSpace(ep.Command) == "" {
			return fmt.Errorf("mcp manager: stdio server %q requires command", ep.Name)
		}
		cmd := exec.Command(ep.Command, ep.Args...)
		if len(ep.Env) > 0 {
			env := os.Environ()
			for k, v := range ep.Env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env
		}
		client := sdkmcp.NewClient(&sdkmcp.Implementation{
			Name:    "soma",
			Version: "0.5.0",
		}, nil)
		sess, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			return fmt.Errorf("mcp stdio connect %s: %w", ep.Name, err)
		}
		ms.session = sess
		res, err := sess.ListTools(ctx, &sdkmcp.ListToolsParams{})
		if err != nil {
			_ = sess.Close()
			return fmt.Errorf("mcp stdio list_tools %s: %w", ep.Name, err)
		}
		for _, t := range res.Tools {
			if t != nil {
				ms.tools = append(ms.tools, *t)
			}
		}
	default:
		if strings.TrimSpace(ep.URL) == "" {
			return fmt.Errorf("mcp manager: http server %q requires url", ep.Name)
		}
		tools, err := m.httpClient.ListTools(ctx, ep)
		if err != nil {
			return fmt.Errorf("mcp http list_tools %s: %w", ep.Name, err)
		}
		ms.tools = tools
	}

	m.mu.Lock()
	if prev, ok := m.servers[ep.Name]; ok && prev.session != nil {
		_ = prev.session.Close()
	}
	m.servers[ep.Name] = ms
	m.mu.Unlock()
	return nil
}

// ConnectAll connects all enabled endpoints, returning any per-server errors aggregated cleanly.
func (m *Manager) ConnectAll(ctx context.Context, endpoints []ServerEndpoint) error {
	var errs []string
	for _, ep := range endpoints {
		if !ep.IsEnabled() {
			continue
		}
		if err := m.Connect(ctx, ep); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// IsIsolatedDesktop reports whether serverName is explicitly marked as running inside soma's
// isolated desktop sandbox (so GUI actions are EffectWorkspaceWrite instead of EffectExternalWrite).
func (m *Manager) IsIsolatedDesktop(serverName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ms, ok := m.servers[strings.TrimSpace(serverName)]; ok {
		return ms.ep.IsolatedDesktop
	}
	return false
}

// ListServers returns sorted names of all connected MCP servers.
func (m *Manager) ListServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.servers))
	for name := range m.servers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ListTools returns all tools exposed by serverName (or all servers when serverName == "").
func (m *Manager) ListTools(serverName string) map[string][]sdkmcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]sdkmcp.Tool)
	for name, ms := range m.servers {
		if serverName != "" && name != serverName {
			continue
		}
		out[name] = append([]sdkmcp.Tool(nil), ms.tools...)
	}
	return out
}

// PopulateCatalog pins all currently connected servers' tools into an EpochCatalog for tool_search.
func (m *Manager) PopulateCatalog(cat *EpochCatalog) {
	if cat == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, ms := range m.servers {
		cat.PinServer(name, ms.tools)
	}
}

// CallTool invokes toolName on serverName and returns text + image blocks strictly tagged untrusted_external.
func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (ToolCallOutput, error) {
	serverName = strings.TrimSpace(serverName)
	toolName = strings.TrimSpace(toolName)
	if serverName == "" || toolName == "" {
		return ToolCallOutput{}, errors.New("mcp call_tool: server and tool are required")
	}

	m.mu.RLock()
	ms, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return ToolCallOutput{}, fmt.Errorf("mcp call_tool: server %q is not connected (connected: %v)", serverName, m.ListServers())
	}
	if args == nil {
		args = map[string]any{}
	}

	if ms.session == nil {
		return m.httpClient.CallTool(ctx, ms.ep, toolName, args)
	}

	res, err := ms.session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return ToolCallOutput{}, fmt.Errorf("mcp stdio call %s.%s: %w", serverName, toolName, err)
	}

	var textParts []string
	var images []ImageAttachment
	for _, block := range res.Content {
		switch c := block.(type) {
		case *sdkmcp.TextContent:
			if c != nil && c.Text != "" {
				textParts = append(textParts, c.Text)
			}
		case *sdkmcp.ImageContent:
			if c != nil && len(c.Data) > 0 {
				raw := c.Data
				// go-sdk ImageContent.Data may be raw bytes or base64 depending on unmarshaler; normalize cleanly.
				if decoded, dErr := base64.StdEncoding.DecodeString(string(c.Data)); dErr == nil && len(decoded) > 0 && isLikelyImageBytes(decoded) {
					raw = decoded
				}
				mime := c.MIMEType
				if mime == "" {
					mime = "image/png"
				}
				images = append(images, ImageAttachment{MIMEType: mime, Data: raw})
			}
		}
	}
	content := strings.Join(textParts, "\n")
	if res.IsError {
		if content == "" {
			content = "MCP tool returned error"
		}
		return ToolCallOutput{
			Server:    serverName,
			Tool:      toolName,
			Content:   "MCP ERROR: " + content,
			Images:    images,
			TrustTier: "untrusted_external",
		}, nil
	}
	return ToolCallOutput{
		Server:    serverName,
		Tool:      toolName,
		Content:   content,
		Images:    images,
		TrustTier: "untrusted_external",
	}, nil
}

// Close shuts down all active stdio MCP sessions cleanly.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []string
	for name, ms := range m.servers {
		if ms.session != nil {
			if err := ms.session.Close(); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			}
		}
		delete(m.servers, name)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func isLikelyImageBytes(b []byte) bool {
	if len(b) >= 4 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' {
		return true
	}
	if len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff {
		return true
	}
	if len(b) >= 4 && string(b[:4]) == "GIF8" {
		return true
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return true
	}
	return false
}
