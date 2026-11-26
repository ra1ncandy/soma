package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type managedServer struct {
	ep      ServerEndpoint
	session *sdkmcp.ClientSession
	tools   []sdkmcp.Tool
}

// Manager manages live stdio, SSE, Streamable HTTP, and stateless HTTP MCP server connections for soma runs,
// supporting progressive tool discovery, per-server tool filtering, resources/prompts, OAuth 2.1, and taint-tagged invocation.
type Manager struct {
	mu         sync.RWMutex
	httpClient *Client
	tokenStore *TokenStore
	servers    map[string]*managedServer
}

// NewManager creates a unified MCP Manager supporting stdio, SSE, Streamable HTTP, and stateless HTTP servers.
func NewManager() *Manager {
	return &Manager{
		httpClient: NewClient(),
		tokenStore: DefaultTokenStore(),
		servers:    make(map[string]*managedServer),
	}
}

// SetTokenStore configures the OAuth token store used by Streamable HTTP endpoints.
func (m *Manager) SetTokenStore(ts *TokenStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenStore = ts
}

// Connect connects to ep (via stdio, SSE, Streamable HTTP, or stateless HTTP), lists & filters its tools, and registers it.
func (m *Manager) Connect(ctx context.Context, ep ServerEndpoint) error {
	if strings.TrimSpace(ep.Name) == "" {
		return errors.New("mcp manager: empty server name")
	}
	if !ep.IsEnabled() {
		return nil
	}

	ms := &managedServer{ep: ep}
	transport := ep.EffectiveTransport()
	switch transport {
	case "stdio":
		if strings.TrimSpace(ep.Command) == "" {
			return fmt.Errorf("mcp manager: stdio server %q requires command", ep.Name)
		}
		cmd := exec.Command(ep.Command, ep.Args...)
		// Never hand a server the host's full environment (provider keys, cloud tokens): only
		// basic variables plus what the entry declares.
		cmd.Env = minimalServerEnv(ResolveEnv(ep.Env))
		client := sdkmcp.NewClient(&sdkmcp.Implementation{
			Name:    "soma",
			Version: "0.5.0",
		}, nil)
		sess, err := client.Connect(ctx, &sdkmcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			return fmt.Errorf("mcp stdio connect %s: %w", ep.Name, err)
		}
		ms.session = sess
		tools, err := listSessionTools(ctx, sess, ep)
		if err != nil {
			_ = sess.Close()
			return fmt.Errorf("mcp stdio list_tools %s: %w", ep.Name, err)
		}
		ms.tools = tools

	case "sse":
		if strings.TrimSpace(ep.URL) == "" {
			return fmt.Errorf("mcp manager: sse server %q requires url", ep.Name)
		}
		hc := NewStreamingEndpointHTTPClient(ep, m.httpClient.HTTPClient)
		client := sdkmcp.NewClient(&sdkmcp.Implementation{
			Name:    "soma",
			Version: "0.5.0",
		}, nil)
		sess, err := client.Connect(ctx, &sdkmcp.SSEClientTransport{
			Endpoint:   expandEnvVars(ep.URL),
			HTTPClient: hc,
		}, nil)
		if err != nil {
			return fmt.Errorf("mcp sse connect %s: %w", ep.Name, err)
		}
		ms.session = sess
		tools, err := listSessionTools(ctx, sess, ep)
		if err != nil {
			_ = sess.Close()
			return fmt.Errorf("mcp sse list_tools %s: %w", ep.Name, err)
		}
		ms.tools = tools

	case "streamable_http":
		if strings.TrimSpace(ep.URL) == "" {
			return fmt.Errorf("mcp manager: streamable_http server %q requires url", ep.Name)
		}
		hc := NewStreamingEndpointHTTPClient(ep, m.httpClient.HTTPClient)
		var oauthHandler *StoredOAuthHandler
		if ep.OAuth != nil && ep.OAuth.Enabled {
			m.mu.RLock()
			ts := m.tokenStore
			m.mu.RUnlock()
			oauthHandler = &StoredOAuthHandler{
				Endpoint:   ep,
				Store:      ts,
				HTTPClient: hc,
			}
		}
		tr := &sdkmcp.StreamableClientTransport{
			Endpoint:   expandEnvVars(ep.URL),
			HTTPClient: hc,
		}
		if oauthHandler != nil {
			tr.OAuthHandler = oauthHandler
		}
		client := sdkmcp.NewClient(&sdkmcp.Implementation{
			Name:    "soma",
			Version: "0.5.0",
		}, nil)
		sess, err := client.Connect(ctx, tr, nil)
		if err != nil {
			return fmt.Errorf("mcp streamable_http connect %s: %w", ep.Name, err)
		}
		ms.session = sess
		tools, err := listSessionTools(ctx, sess, ep)
		if err != nil {
			_ = sess.Close()
			return fmt.Errorf("mcp streamable_http list_tools %s: %w", ep.Name, err)
		}
		ms.tools = tools

	default:
		if strings.TrimSpace(ep.URL) == "" {
			return fmt.Errorf("mcp manager: http server %q requires url", ep.Name)
		}
		tools, err := m.httpClient.ListTools(ctx, ep)
		if err != nil {
			return fmt.Errorf("mcp http list_tools %s: %w", ep.Name, err)
		}
		ms.tools = FilterTools(tools, ep.IncludeTools, ep.ExcludeTools)
	}

	m.mu.Lock()
	if prev, ok := m.servers[ep.Name]; ok && prev.session != nil {
		_ = prev.session.Close()
	}
	m.servers[ep.Name] = ms
	m.mu.Unlock()
	return nil
}

// connectDeadline bounds how long one server may take to start and answer initialize/tools/list
// (first-run `npx -y` / `uvx` downloads are slow, but a server that never answers must not hang a run).
const connectDeadline = 60 * time.Second

// connectWithDeadline runs Connect without letting a hung server block the caller. The connect
// itself keeps using ctx (transports tie the session's lifetime to it), so on timeout the attempt
// is abandoned and, if it completes later, the stray session is closed.
func (m *Manager) connectWithDeadline(ctx context.Context, ep ServerEndpoint, deadline time.Duration) error {
	done := make(chan error, 1)
	var abandoned atomic.Bool
	go func() {
		err := m.Connect(ctx, ep)
		if err == nil && abandoned.Load() {
			m.mu.Lock()
			if ms, ok := m.servers[ep.Name]; ok {
				if ms.session != nil {
					_ = ms.session.Close()
				}
				delete(m.servers, ep.Name)
			}
			m.mu.Unlock()
		}
		done <- err
	}()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		abandoned.Store(true)
		return fmt.Errorf("mcp connect %s: no response within %s", ep.Name, deadline)
	case <-ctx.Done():
		abandoned.Store(true)
		return ctx.Err()
	}
}

// ConnectAll connects all enabled endpoints concurrently, each bounded by connectDeadline, and
// returns any per-server errors aggregated in endpoint order.
func (m *Manager) ConnectAll(ctx context.Context, endpoints []ServerEndpoint) error {
	errsByIdx := make([]error, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		if !ep.IsEnabled() {
			continue
		}
		wg.Add(1)
		go func(i int, ep ServerEndpoint) {
			defer wg.Done()
			errsByIdx[i] = m.connectWithDeadline(ctx, ep, connectDeadline)
		}(i, ep)
	}
	wg.Wait()
	var errs []string
	for _, err := range errsByIdx {
		if err != nil {
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

// ToolAnnotations returns the behaviour hints a server declared for one of its tools.
// Hints are untrusted server metadata: callers may use them only to tighten policy.
func (m *Manager) ToolAnnotations(serverName, toolName string) *sdkmcp.ToolAnnotations {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ms, ok := m.servers[strings.TrimSpace(serverName)]
	if !ok {
		return nil
	}
	for _, t := range ms.tools {
		if t.Name == toolName {
			return t.Annotations
		}
	}
	return nil
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
	if !IsToolAllowed(toolName, ms.ep.IncludeTools, ms.ep.ExcludeTools) {
		return ToolCallOutput{}, fmt.Errorf("mcp call_tool: tool %q on server %q is blocked by server tool filter", toolName, serverName)
	}
	if args == nil {
		args = map[string]any{}
	}

	if ms.session == nil {
		return m.httpClient.CallTool(ctx, ms.ep, toolName, args)
	}

	// Bound the call by context rather than by the HTTP client, so a slow call fails on its own
	// without closing the session. Stdio servers are only bounded when a timeout was configured
	// (GUI and browser tools can legitimately run long).
	if tr := ms.ep.EffectiveTransport(); ms.ep.TimeoutSeconds > 0 || tr == "sse" || tr == "streamable_http" {
		callTimeout := ms.ep.EffectiveTimeout()
		if ms.ep.TimeoutSeconds == 0 && callTimeout < 120*time.Second {
			callTimeout = 120 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
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

func listSessionTools(ctx context.Context, sess *sdkmcp.ClientSession, ep ServerEndpoint) ([]sdkmcp.Tool, error) {
	res, err := sess.ListTools(ctx, &sdkmcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}
	rawTools := make([]sdkmcp.Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		if t != nil {
			rawTools = append(rawTools, *t)
		}
	}
	return FilterTools(rawTools, ep.IncludeTools, ep.ExcludeTools), nil
}

// Ping verifies connectivity to serverName.
func (m *Manager) Ping(ctx context.Context, serverName string) error {
	m.mu.RLock()
	ms, ok := m.servers[strings.TrimSpace(serverName)]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("mcp ping: server %q is not connected", serverName)
	}
	if ms.session != nil {
		return ms.session.Ping(ctx, &sdkmcp.PingParams{})
	}
	_, err := m.httpClient.ListTools(ctx, ms.ep)
	return err
}

// RefreshTools re-queries tools/list for serverName and updates its filtered tool cache.
func (m *Manager) RefreshTools(ctx context.Context, serverName string) error {
	serverName = strings.TrimSpace(serverName)
	m.mu.RLock()
	ms, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("mcp refresh_tools: server %q is not connected", serverName)
	}
	var tools []sdkmcp.Tool
	var err error
	if ms.session != nil {
		tools, err = listSessionTools(ctx, ms.session, ms.ep)
	} else {
		var raw []sdkmcp.Tool
		raw, err = m.httpClient.ListTools(ctx, ms.ep)
		if err == nil {
			tools = FilterTools(raw, ms.ep.IncludeTools, ms.ep.ExcludeTools)
		}
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	if cur, exists := m.servers[serverName]; exists {
		cur.tools = tools
	}
	m.mu.Unlock()
	return nil
}

// ListResources queries resources/list on serverName (or all connected servers when serverName == "").
func (m *Manager) ListResources(ctx context.Context, serverName string) (map[string][]sdkmcp.Resource, error) {
	m.mu.RLock()
	targets := make(map[string]*managedServer, len(m.servers))
	for name, ms := range m.servers {
		if serverName != "" && name != serverName {
			continue
		}
		targets[name] = ms
	}
	m.mu.RUnlock()

	out := make(map[string][]sdkmcp.Resource, len(targets))
	for name, ms := range targets {
		if ms.session != nil {
			res, err := ms.session.ListResources(ctx, &sdkmcp.ListResourcesParams{})
			if err != nil {
				return nil, fmt.Errorf("mcp list_resources %s: %w", name, err)
			}
			var items []sdkmcp.Resource
			for _, r := range res.Resources {
				if r != nil {
					items = append(items, *r)
				}
			}
			out[name] = items
		} else {
			items, err := m.httpClient.ListResources(ctx, ms.ep)
			if err != nil {
				return nil, fmt.Errorf("mcp list_resources %s: %w", name, err)
			}
			out[name] = items
		}
	}
	return out, nil
}

// ReadResource fetches uri from serverName and returns it strictly tagged untrusted_external.
func (m *Manager) ReadResource(ctx context.Context, serverName, uri string) (ResourceReadOutput, error) {
	serverName = strings.TrimSpace(serverName)
	uri = strings.TrimSpace(uri)
	if serverName == "" || uri == "" {
		return ResourceReadOutput{}, errors.New("mcp read_resource: server and uri are required")
	}
	m.mu.RLock()
	ms, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return ResourceReadOutput{}, fmt.Errorf("mcp read_resource: server %q is not connected", serverName)
	}
	if ms.session == nil {
		return m.httpClient.ReadResource(ctx, ms.ep, uri)
	}
	res, err := ms.session.ReadResource(ctx, &sdkmcp.ReadResourceParams{URI: uri})
	if err != nil {
		return ResourceReadOutput{}, fmt.Errorf("mcp read_resource %s %s: %w", serverName, uri, err)
	}
	out := ResourceReadOutput{
		Server:    serverName,
		URI:       uri,
		TrustTier: "untrusted_external",
	}
	var texts []string
	for _, c := range res.Contents {
		if c == nil {
			continue
		}
		if out.MIMEType == "" && c.MIMEType != "" {
			out.MIMEType = c.MIMEType
		}
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
		if len(out.Blob) == 0 && len(c.Blob) > 0 {
			out.Blob = c.Blob
		}
	}
	out.Text = strings.Join(texts, "\n")
	return out, nil
}

// ListPrompts queries prompts/list on serverName (or all connected servers when serverName == "").
func (m *Manager) ListPrompts(ctx context.Context, serverName string) (map[string][]sdkmcp.Prompt, error) {
	m.mu.RLock()
	targets := make(map[string]*managedServer, len(m.servers))
	for name, ms := range m.servers {
		if serverName != "" && name != serverName {
			continue
		}
		targets[name] = ms
	}
	m.mu.RUnlock()

	out := make(map[string][]sdkmcp.Prompt, len(targets))
	for name, ms := range targets {
		if ms.session != nil {
			res, err := ms.session.ListPrompts(ctx, &sdkmcp.ListPromptsParams{})
			if err != nil {
				return nil, fmt.Errorf("mcp list_prompts %s: %w", name, err)
			}
			var items []sdkmcp.Prompt
			for _, p := range res.Prompts {
				if p != nil {
					items = append(items, *p)
				}
			}
			out[name] = items
		} else {
			items, err := m.httpClient.ListPrompts(ctx, ms.ep)
			if err != nil {
				return nil, fmt.Errorf("mcp list_prompts %s: %w", name, err)
			}
			out[name] = items
		}
	}
	return out, nil
}

// GetPrompt fetches promptName with args from serverName, tagged untrusted_external.
func (m *Manager) GetPrompt(ctx context.Context, serverName, promptName string, args map[string]string) (PromptGetOutput, error) {
	serverName = strings.TrimSpace(serverName)
	promptName = strings.TrimSpace(promptName)
	if serverName == "" || promptName == "" {
		return PromptGetOutput{}, errors.New("mcp get_prompt: server and prompt are required")
	}
	m.mu.RLock()
	ms, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return PromptGetOutput{}, fmt.Errorf("mcp get_prompt: server %q is not connected", serverName)
	}
	if ms.session == nil {
		return m.httpClient.GetPrompt(ctx, ms.ep, promptName, args)
	}
	res, err := ms.session.GetPrompt(ctx, &sdkmcp.GetPromptParams{
		Name:      promptName,
		Arguments: args,
	})
	if err != nil {
		return PromptGetOutput{}, fmt.Errorf("mcp get_prompt %s %s: %w", serverName, promptName, err)
	}
	out := PromptGetOutput{
		Server:      serverName,
		Prompt:      promptName,
		Description: res.Description,
		TrustTier:   "untrusted_external",
	}
	for _, msg := range res.Messages {
		if msg == nil {
			continue
		}
		var text string
		if tc, ok := msg.Content.(*sdkmcp.TextContent); ok && tc != nil {
			text = tc.Text
		}
		out.Messages = append(out.Messages, PromptMessage{
			Role:    string(msg.Role),
			Content: text,
		})
	}
	return out, nil
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
