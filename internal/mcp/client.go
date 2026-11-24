package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ra1ncandy/soma/internal/compiler"
)

const ProtocolVersion20260728 = "2026-07-28"

// ServerEndpoint configures a remote HTTP/SSE/Streamable HTTP or local stdio MCP server.
type ServerEndpoint struct {
	Name            string            `yaml:"name" json:"name"`
	Transport       string            `yaml:"transport,omitempty" json:"transport,omitempty"` // "http" | "streamable_http" | "sse" | "stdio"
	URL             string            `yaml:"url,omitempty" json:"url,omitempty"`
	AuthToken       string            `yaml:"auth_token,omitempty" json:"auth_token,omitempty"`
	Headers         map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Command         string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args            []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env             map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	IncludeTools    []string          `yaml:"include_tools,omitempty" json:"include_tools,omitempty"`
	ExcludeTools    []string          `yaml:"exclude_tools,omitempty" json:"exclude_tools,omitempty"`
	TimeoutSeconds  int               `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	OAuth           *OAuthConfig      `yaml:"oauth,omitempty" json:"oauth,omitempty"`
	Enabled         *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	IsolatedDesktop bool              `yaml:"isolated_desktop,omitempty" json:"isolated_desktop,omitempty"`
}

// IsEnabled reports whether this MCP server endpoint is active (defaults to true).
func (ep ServerEndpoint) IsEnabled() bool {
	if ep.Enabled == nil {
		return true
	}
	return *ep.Enabled
}

// EffectiveTransport resolves "stdio", "sse", "streamable_http", or "http".
func (ep ServerEndpoint) EffectiveTransport() string {
	t := strings.ToLower(strings.TrimSpace(ep.Transport))
	switch t {
	case "streamable", "streamable_http", "streamable-http", "http-streamable":
		return "streamable_http"
	case "sse":
		return "sse"
	case "stdio":
		return "stdio"
	case "http":
		return "http"
	}
	if strings.TrimSpace(ep.Command) != "" {
		return "stdio"
	}
	if ep.OAuth != nil && ep.OAuth.Enabled {
		return "streamable_http"
	}
	return "http"
}

// EffectiveTimeout returns the configured per-server timeout, defaulting to 30s.
func (ep ServerEndpoint) EffectiveTimeout() time.Duration {
	if ep.TimeoutSeconds > 0 {
		return time.Duration(ep.TimeoutSeconds) * time.Second
	}
	return 30 * time.Second
}

// ImageAttachment carries a decoded image block returned by a multimodal MCP tool (e.g. screenshot).
type ImageAttachment struct {
	MIMEType string
	Data     []byte
}

// SkillEntry describes a skill served via io.modelcontextprotocol/skills (SEP-2640).
type SkillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content,omitempty"`
}

// ToolCallOutput represents a tool call result always marked untrusted_external (*MCPTox defense).
type ToolCallOutput struct {
	Server    string
	Tool      string
	Content   string
	Images    []ImageAttachment
	TrustTier string
}

// ResourceReadOutput represents an MCP resources/read result tagged untrusted_external.
type ResourceReadOutput struct {
	Server    string
	URI       string
	MIMEType  string
	Text      string
	Blob      []byte
	TrustTier string
}

// PromptMessage represents a single message returned by MCP prompts/get.
type PromptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// PromptGetOutput represents an MCP prompts/get result tagged untrusted_external.
type PromptGetOutput struct {
	Server      string
	Prompt      string
	Description string
	Messages    []PromptMessage
	TrustTier   string
}

// PinnedTool ties a qualified tool name (server.tool) to its canonical JSON schema.
type PinnedTool struct {
	QualifiedName string
	Server        string
	Tool          sdkmcp.Tool
	SchemaJSON    string
	TrustTier     string
}

// Client speaks stateless MCP 2026-07-28 over HTTP using MCP-Protocol-Version, Mcp-Method, and Mcp-Name headers.
type Client struct {
	HTTPClient *http.Client
	reqSeq     atomic.Uint64
}

// NewClient creates a stateless MCP 2026-07-28 client.
func NewClient() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) rpcCall(ctx context.Context, ep ServerEndpoint, method, mcpName string, params any, out any) error {
	id := c.reqSeq.Add(1)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion20260728)
	req.Header.Set("Mcp-Method", method)
	if mcpName != "" {
		req.Header.Set("Mcp-Name", mcpName)
	}
	if ep.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+expandEnvVars(ep.AuthToken))
	}
	for k, v := range ResolveHeaders(ep) {
		if strings.EqualFold(k, "Authorization") && ep.AuthToken != "" {
			continue
		}
		req.Header.Set(k, v)
	}

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp server %s HTTP %d: %s", ep.Name, resp.StatusCode, string(respBytes))
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return fmt.Errorf("decode mcp response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("mcp error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if out != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("decode mcp result: %w", err)
		}
	}
	return nil
}

// ListTools queries tools/list on ep.
func (c *Client) ListTools(ctx context.Context, ep ServerEndpoint) ([]sdkmcp.Tool, error) {
	var res struct {
		Tools []sdkmcp.Tool `json:"tools"`
	}
	if err := c.rpcCall(ctx, ep, "tools/list", "", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool invokes tools/call with Mcp-Name header and returns output strictly tainted untrusted_external.
func (c *Client) CallTool(ctx context.Context, ep ServerEndpoint, toolName string, args map[string]any) (ToolCallOutput, error) {
	if args == nil {
		args = map[string]any{}
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	err := c.rpcCall(ctx, ep, "tools/call", toolName, map[string]any{
		"name":      toolName,
		"arguments": args,
	}, &res)
	if err != nil {
		return ToolCallOutput{}, err
	}
	var parts []string
	for _, item := range res.Content {
		if item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	return ToolCallOutput{
		Server:    ep.Name,
		Tool:      toolName,
		Content:   strings.Join(parts, "\n"),
		TrustTier: "untrusted_external",
	}, nil
}

// ListSkills fetches skills via io.modelcontextprotocol/skills (skills/list).
func (c *Client) ListSkills(ctx context.Context, ep ServerEndpoint) ([]SkillEntry, error) {
	var res struct {
		Skills []SkillEntry `json:"skills"`
	}
	if err := c.rpcCall(ctx, ep, "skills/list", "", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Skills, nil
}

// GetSkill fetches a specific skill package via skills/get.
func (c *Client) GetSkill(ctx context.Context, ep ServerEndpoint, name string) (SkillEntry, error) {
	var res SkillEntry
	if err := c.rpcCall(ctx, ep, "skills/get", name, map[string]any{"name": name}, &res); err != nil {
		return SkillEntry{}, err
	}
	return res, nil
}

// ListResources queries resources/list on ep.
func (c *Client) ListResources(ctx context.Context, ep ServerEndpoint) ([]sdkmcp.Resource, error) {
	var res struct {
		Resources []sdkmcp.Resource `json:"resources"`
	}
	if err := c.rpcCall(ctx, ep, "resources/list", "", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Resources, nil
}

// ReadResource reads a resource URI via resources/read on ep, tagged untrusted_external.
func (c *Client) ReadResource(ctx context.Context, ep ServerEndpoint, uri string) (ResourceReadOutput, error) {
	var res struct {
		Contents []struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType,omitempty"`
			Text     string `json:"text,omitempty"`
			Blob     string `json:"blob,omitempty"`
		} `json:"contents"`
	}
	if err := c.rpcCall(ctx, ep, "resources/read", uri, map[string]any{"uri": uri}, &res); err != nil {
		return ResourceReadOutput{}, err
	}
	out := ResourceReadOutput{
		Server:    ep.Name,
		URI:       uri,
		TrustTier: "untrusted_external",
	}
	var texts []string
	for _, item := range res.Contents {
		if out.MIMEType == "" && item.MIMEType != "" {
			out.MIMEType = item.MIMEType
		}
		if item.Text != "" {
			texts = append(texts, item.Text)
		}
		if len(out.Blob) == 0 && item.Blob != "" {
			out.Blob = []byte(item.Blob)
		}
	}
	out.Text = strings.Join(texts, "\n")
	return out, nil
}

// ListPrompts queries prompts/list on ep.
func (c *Client) ListPrompts(ctx context.Context, ep ServerEndpoint) ([]sdkmcp.Prompt, error) {
	var res struct {
		Prompts []sdkmcp.Prompt `json:"prompts"`
	}
	if err := c.rpcCall(ctx, ep, "prompts/list", "", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Prompts, nil
}

// GetPrompt fetches a prompt via prompts/get on ep, tagged untrusted_external.
func (c *Client) GetPrompt(ctx context.Context, ep ServerEndpoint, name string, args map[string]string) (PromptGetOutput, error) {
	if args == nil {
		args = map[string]string{}
	}
	var res struct {
		Description string `json:"description,omitempty"`
		Messages    []struct {
			Role    string `json:"role"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := c.rpcCall(ctx, ep, "prompts/get", name, map[string]any{
		"name":      name,
		"arguments": args,
	}, &res); err != nil {
		return PromptGetOutput{}, err
	}
	out := PromptGetOutput{
		Server:      ep.Name,
		Prompt:      name,
		Description: res.Description,
		TrustTier:   "untrusted_external",
	}
	for _, m := range res.Messages {
		out.Messages = append(out.Messages, PromptMessage{
			Role:    m.Role,
			Content: m.Content.Text,
		})
	}
	return out, nil
}

// EpochCatalog holds an alphabetically sorted, epoch-pinned snapshot of MCP tools for deterministic prefix caching.
type EpochCatalog struct {
	mu      sync.RWMutex
	servers []ServerEndpoint
	tools   []PinnedTool
}

// SnapshotCatalog queries tools/list across endpoints and freezes a lexicographically sorted catalog.
func SnapshotCatalog(ctx context.Context, client *Client, endpoints []ServerEndpoint) (*EpochCatalog, error) {
	sortedEps := append([]ServerEndpoint(nil), endpoints...)
	sort.Slice(sortedEps, func(i, j int) bool { return sortedEps[i].Name < sortedEps[j].Name })

	var pinned []PinnedTool
	for _, ep := range sortedEps {
		tools, err := client.ListTools(ctx, ep)
		if err != nil {
			return nil, fmt.Errorf("snapshot server %s: %w", ep.Name, err)
		}
		for _, t := range tools {
			schemaBytes, _ := json.Marshal(t.InputSchema)
			pinned = append(pinned, PinnedTool{
				QualifiedName: ep.Name + "." + t.Name,
				Server:        ep.Name,
				Tool:          t,
				SchemaJSON:    string(schemaBytes),
				TrustTier:     "untrusted_external",
			})
		}
	}
	sort.Slice(pinned, func(i, j int) bool { return pinned[i].QualifiedName < pinned[j].QualifiedName })

	return &EpochCatalog{
		servers: sortedEps,
		tools:   pinned,
	}, nil
}

// NewEpochCatalog creates an empty EpochCatalog ready for PinServer calls.
func NewEpochCatalog() *EpochCatalog {
	return &EpochCatalog{}
}

// PinServer adds or replaces a server's tools in the catalog, maintaining lexicographical order.
func (ec *EpochCatalog) PinServer(serverName string, tools []sdkmcp.Tool) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	kept := ec.tools[:0]
	for _, pt := range ec.tools {
		if pt.Server != serverName {
			kept = append(kept, pt)
		}
	}
	ec.tools = kept
	foundSrv := false
	for _, s := range ec.servers {
		if s.Name == serverName {
			foundSrv = true
			break
		}
	}
	if !foundSrv {
		ec.servers = append(ec.servers, ServerEndpoint{Name: serverName})
		sort.Slice(ec.servers, func(i, j int) bool { return ec.servers[i].Name < ec.servers[j].Name })
	}
	for _, t := range tools {
		schemaBytes, _ := json.Marshal(t.InputSchema)
		ec.tools = append(ec.tools, PinnedTool{
			QualifiedName: serverName + "." + t.Name,
			Server:        serverName,
			Tool:          t,
			SchemaJSON:    string(schemaBytes),
			TrustTier:     "untrusted_external",
		})
	}
	sort.Slice(ec.tools, func(i, j int) bool { return ec.tools[i].QualifiedName < ec.tools[j].QualifiedName })
}

// Layer0Summaries returns sorted 1-line summaries per MCP server for Layer 0.
func (ec *EpochCatalog) Layer0Summaries() []compiler.MCPSummary {
	ec.mu.RLock()
	defer ec.mu.RUnlock()

	counts := make(map[string]int, len(ec.servers))
	for _, t := range ec.tools {
		counts[t.Server]++
	}
	out := make([]compiler.MCPSummary, 0, len(ec.servers))
	for _, s := range ec.servers {
		out = append(out, compiler.MCPSummary{
			Server:  s.Name,
			Summary: fmt.Sprintf("%d deferred untrusted tool(s); use tool_search to inspect schemas", counts[s.Name]),
		})
	}
	return out
}

// Search matches query against qualified tool names and descriptions, returning sorted schemas for Layer 2 append.
func (ec *EpochCatalog) Search(query string) []compiler.ToolSchema {
	ec.mu.RLock()
	defer ec.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(query))
	var out []compiler.ToolSchema
	for _, pt := range ec.tools {
		if q == "" || strings.Contains(strings.ToLower(pt.QualifiedName), q) ||
			strings.Contains(strings.ToLower(pt.Tool.Description), q) {
			out = append(out, compiler.ToolSchema{
				Name:       pt.QualifiedName,
				JSONSchema: pt.SchemaJSON,
			})
		}
	}
	return out
}
