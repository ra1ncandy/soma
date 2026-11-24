package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/skills"
	"github.com/ra1ncandy/soma/internal/store"
)

// ServeOptions configures `soma mcp serve` (reverse MCP server mode).
type ServeOptions struct {
	WorkspaceDir string
	Transport    string // "stdio" | "http"
	Addr         string // e.g. "127.0.0.1:8765" when Transport == "http"
	ToolGroups   []string
}

// NewSomaMCPServer constructs an MCP server exposing soma's memory, skills, and status tools.
func NewSomaMCPServer(st *store.Store, opts ServeOptions) *sdkmcp.Server {
	wsDir := strings.TrimSpace(opts.WorkspaceDir)
	if wsDir == "" {
		wsDir = "."
	}
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "soma",
		Version: "0.5.0",
	}, nil)

	allowGroup := func(group string) bool {
		if len(opts.ToolGroups) == 0 {
			return true
		}
		for _, g := range opts.ToolGroups {
			if strings.EqualFold(strings.TrimSpace(g), group) {
				return true
			}
		}
		return false
	}

	if allowGroup("status") {
		type statusArgs struct{}
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "soma_status",
			Description: "Return soma workspace path and configured MCP server summaries.",
		}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ statusArgs) (*sdkmcp.CallToolResult, any, error) {
			eps, _ := LoadServerEndpoints(wsDir)
			names := make([]string, 0, len(eps))
			for _, ep := range eps {
				names = append(names, ep.Name)
			}
			b, _ := json.MarshalIndent(map[string]any{
				"workspace_dir": wsDir,
				"mcp_servers":   names,
			}, "", "  ")
			return textToolResult(string(b)), nil, nil
		})
	}

	if allowGroup("memory") && st != nil {
		memSvc := memory.NewService(st)
		type memSearchArgs struct {
			Query string   `json:"query"`
			Scope string   `json:"scope,omitempty"`
			Kinds []string `json:"kinds,omitempty"`
			TopK  int      `json:"top_k,omitempty"`
		}
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "soma_memory_search",
			Description: "Search soma's bi-temporal memory graph using hybrid BM25 + semantic RRF.",
		}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, args memSearchArgs) (*sdkmcp.CallToolResult, any, error) {
			var scopes []string
			if strings.TrimSpace(args.Scope) != "" {
				scopes = []string{strings.TrimSpace(args.Scope)}
			}
			topK := args.TopK
			if topK <= 0 {
				topK = 5
			}
			results, err := memSvc.Search(ctx, scopes, args.Kinds, args.Query, topK)
			if err != nil {
				return nil, nil, err
			}
			b, _ := json.MarshalIndent(results, "", "  ")
			return textToolResult(string(b)), nil, nil
		})

		type memProposeArgs struct {
			Scope   string `json:"scope,omitempty"`
			Kind    string `json:"kind"`
			Content string `json:"content"`
		}
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "soma_memory_propose",
			Description: "Propose a memory entry (fact, rule, preference) into soma's quarantined memory graph.",
		}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, args memProposeArgs) (*sdkmcp.CallToolResult, any, error) {
			scope := strings.TrimSpace(args.Scope)
			if scope == "" {
				scope = "global"
			}
			kind := strings.TrimSpace(args.Kind)
			if kind == "" {
				kind = "fact"
			}
			node, err := memSvc.Propose(ctx, memory.ProposeRequest{
				Scope:     scope,
				Kind:      kind,
				Content:   args.Content,
				TrustTier: "untrusted_external",
				Status:    "proposed",
			})
			if err != nil {
				return nil, nil, err
			}
			b, _ := json.MarshalIndent(node, "", "  ")
			return textToolResult(string(b)), nil, nil
		})
	}

	if allowGroup("skills") {
		skillRoots := []string{
			filepath.Join(wsDir, ".soma", "skills"),
			filepath.Join(wsDir, ".agents", "skills"),
		}
		type skillListArgs struct{}
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "soma_skill_list",
			Description: "List available AAIF SKILL.md packages in the soma workspace.",
		}, func(_ context.Context, _ *sdkmcp.CallToolRequest, _ skillListArgs) (*sdkmcp.CallToolResult, any, error) {
			metas, err := skills.LoadIndex(skillRoots)
			if err != nil {
				return nil, nil, err
			}
			b, _ := json.MarshalIndent(metas, "", "  ")
			return textToolResult(string(b)), nil, nil
		})

		type skillReadArgs struct {
			Name string `json:"name"`
		}
		sdkmcp.AddTool(srv, &sdkmcp.Tool{
			Name:        "soma_skill_read",
			Description: "Read and security-lint a SKILL.md package by name.",
		}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args skillReadArgs) (*sdkmcp.CallToolResult, any, error) {
			name := strings.TrimSpace(args.Name)
			if name == "" {
				return nil, nil, errors.New("skill name is required")
			}
			for _, root := range skillRoots {
				dir := filepath.Join(root, name)
				body, findings, err := skills.ActivateBody(dir)
				if err == nil {
					b, _ := json.MarshalIndent(map[string]any{
						"skill":         body,
						"lint_findings": findings,
					}, "", "  ")
					return textToolResult(string(b)), nil, nil
				}
			}
			return nil, nil, fmt.Errorf("skill %q not found", name)
		})
	}

	return srv
}

// RunServer starts soma in reverse MCP server mode over stdio or Streamable HTTP.
func RunServer(ctx context.Context, st *store.Store, opts ServeOptions) error {
	srv := NewSomaMCPServer(st, opts)
	tr := strings.ToLower(strings.TrimSpace(opts.Transport))
	if tr == "" || tr == "stdio" {
		return srv.Run(ctx, &sdkmcp.StdioTransport{})
	}
	if tr == "http" || tr == "streamable_http" {
		addr := strings.TrimSpace(opts.Addr)
		if addr == "" {
			addr = "127.0.0.1:8765"
		}
		handler := sdkmcp.NewStreamableHTTPHandler(func(_ *http.Request) *sdkmcp.Server {
			return srv
		}, nil)
		httpSrv := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutCtx)
		}()
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	return fmt.Errorf("unsupported serve transport %q (expected stdio or http)", opts.Transport)
}

func textToolResult(text string) *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.TextContent{Text: text},
		},
	}
}
