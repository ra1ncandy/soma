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

// ServerEndpoint configures a remote HTTP or local stdio MCP server.
type ServerEndpoint struct {
	Name            string            `yaml:"name" json:"name"`
	Transport       string            `yaml:"transport,omitempty" json:"transport,omitempty"` // "http" (default if URL set) | "stdio" (default if Command set)
	URL             string            `yaml:"url,omitempty" json:"url,omitempty"`
	AuthToken       string            `yaml:"auth_token,omitempty" json:"auth_token,omitempty"`
	Command         string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args            []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env             map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
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

// EffectiveTransport resolves "stdio" vs "http".
func (ep ServerEndpoint) EffectiveTransport() string {
	t := strings.ToLower(strings.TrimSpace(ep.Transport))
	if t != "" {
		return t
	}
	if strings.TrimSpace(ep.Command) != "" {
		return "stdio"
	}
	return "http"
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
		req.Header.Set("Authorization", "Bearer "+ep.AuthToken)
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
