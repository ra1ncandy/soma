package dag

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/taint"
)

// ToolCallSpec describes a tool/syscall invocation requested by a model turn.
type ToolCallSpec struct {
	CallID            string            `json:"call_id,omitempty"`
	Name              string            `json:"name"`
	ArgsJSON          string            `json:"args_json"`
	EffectClass       taint.EffectClass `json:"effect_class"`
	CapabilityScope   string            `json:"capability_scope,omitempty"`
	ReferencedPageIDs []string          `json:"referenced_page_ids,omitempty"`
	ExplicitDeny      bool              `json:"explicit_deny,omitempty"`
}

// GenerateRequest carries the compiled 4-layer prompt frame for one model turn.
type GenerateRequest struct {
	RunID     string
	BranchID  string
	AccountID string
	ModelID   string
	TurnIndex int
	Frame     compiler.CompiledFrame
}

// GenerateResponse represents the normalized model emission for one turn.
type GenerateResponse struct {
	Reasoning        string               `json:"reasoning"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	AssistantContent string               `json:"assistant_content,omitempty"`
	Done             bool                 `json:"done"`
	FinalSummary     string               `json:"final_summary,omitempty"`
	ToolCall         *ToolCallSpec        `json:"tool_call,omitempty"`
	ToolCalls        []ToolCallSpec       `json:"tool_calls,omitempty"`
	DynamicSkill     *compiler.SkillBody  `json:"dynamic_skill,omitempty"`
	DynamicMCP       *compiler.ToolSchema `json:"dynamic_mcp,omitempty"`
	StateHash        string               `json:"state_hash"`
	InputTokens      int                  `json:"input_tokens"`
	CacheReadTokens  int                  `json:"cache_read_tokens"`
	CacheWriteTokens int                  `json:"cache_write_tokens"`
	OutputTokens     int                  `json:"output_tokens"`
	CostUSD          float64              `json:"cost_usd"`
	LatencyMS        int64                `json:"latency_ms"`
	RawEmitted       []byte               `json:"-"`
}

// RateLimitError signals an HTTP 429 or subscription quota limit so the kernel can suspend cleanly.
type RateLimitError struct {
	AccountID  string
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit on account %s (retry after %s): %v", e.AccountID, e.RetryAfter, e.Err)
}

func (e *RateLimitError) Unwrap() error { return e.Err }

// TransientError signals a retryable upstream 5xx or transport failure.
type TransientError struct {
	AccountID  string
	StatusCode int
	Err        error
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient error on account %s (status %d): %v", e.AccountID, e.StatusCode, e.Err)
}

func (e *TransientError) Unwrap() error { return e.Err }

// ModelProvider abstracts LLM inference backends.
type ModelProvider interface {
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
}

// OpenAICompatProvider talks to any OpenAI-compatible /v1/chat/completions endpoint
// with deterministic tool schemas, KV-cache usage parsing, and OpenCode Go session routing.
type OpenAICompatProvider struct {
	BaseURL              string
	APIKey               string
	HTTPClient           *http.Client
	MaxTokens            int
	ReasoningEffort      string
	EnableTools          bool
	PassReasoningContent bool // Opt-in: echo full reasoning_content on historical assistant tool turns (increases prompt tokens).
	// DisableStream forces a single buffered response. Streaming is the default: it replaces the
	// whole-request timeout with an idle timeout and lets cancellation take effect immediately.
	DisableStream bool
	// OnDelta, when set, receives assistant text as it streams.
	OnDelta func(text string)
	// StreamIdleTimeout overrides how long a stream may stay silent (default 180s).
	StreamIdleTimeout time.Duration

	streamUnsupported atomic.Bool
}

// NewOpenAICompatProvider creates a client targeting baseURL (e.g. https://opencode.ai/zen/go/v1).
func NewOpenAICompatProvider(baseURL, apiKey string) *OpenAICompatProvider {
	return &OpenAICompatProvider{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		APIKey:      apiKey,
		MaxTokens:   16384,
		EnableTools: true,
		// No whole-request timeout: a long generation that is still streaming must not be cut off.
		// Waiting for the response to start is bounded here; a stalled stream by the idle timeout.
		HTTPClient: &http.Client{Transport: providerTransport()},
	}
}

func providerTransport() http.RoundTripper {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr := dt.Clone()
		tr.ResponseHeaderTimeout = 240 * time.Second
		return tr
	}
	return http.DefaultTransport
}

var (
	openAIToolsOnce  sync.Once
	openAIToolsCache []map[string]any
)

// openAITools returns the canonical tool schema list, built once (it is immutable).
func openAITools() []map[string]any {
	openAIToolsOnce.Do(func() {
		tools := append(canonicalOpenAITools(), registryWireSchemas()...)
		sort.SliceStable(tools, func(i, j int) bool { return wireToolName(tools[i]) < wireToolName(tools[j]) })
		openAIToolsCache = tools
	})
	return openAIToolsCache
}

func wireToolName(t map[string]any) string {
	if fn, ok := t["function"].(map[string]any); ok {
		n, _ := fn["name"].(string)
		return n
	}
	return ""
}

func simpleTool(name, description string, required []string, props map[string]any) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": description,
			"parameters": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func canonicalOpenAITools() []map[string]any {
	// Lexicographically ordered by function name to keep prompt/tool prefixes byte-stable across turns.
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bg_exec",
				"description": "Start a long-running background shell command inside the workspace and return a job_id.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string", "description": "Shell command to run in background"},
						"label":   map[string]any{"type": "string", "description": "Optional human-readable job label"},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bg_kill",
				"description": "Terminate a background job by job_id and return its final output/@page snapshot.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"job_id": map[string]any{"type": "string", "description": "Background job ID (e.g. bg_0001)"},
					},
					"required": []string{"job_id"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "bg_poll",
				"description": "Check status and output of a background job (large outputs spill to @page handles). Optionally wait up to wait_seconds (max 30) for output or completion.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"job_id":       map[string]any{"type": "string", "description": "Background job ID (e.g. bg_0001)"},
						"wait_seconds": map[string]any{"type": "number", "description": "Optional seconds to wait for job exit or new output before returning (0-30)"},
					},
					"required": []string{"job_id"},
				},
			},
		},
		simpleTool("branch_fork", "Fork a sibling branch from the current step to explore an alternative approach in its own isolated workspace.",
			[]string{"label"}, map[string]any{
				"label":  strProp("Short name for the alternative approach"),
				"reason": strProp("Why this fork is being created"),
			}),
		simpleTool("broker_call", "Call an external capability through a secretless broker (credentials are attached by the host, never visible here).",
			[]string{"capability", "op"}, map[string]any{
				"capability":  strProp("Configured broker capability name"),
				"op":          strProp("Operation, e.g. GET or POST"),
				"params_json": strProp("JSON object string of operation parameters"),
				"target":      strProp("Optional target path or resource"),
			}),
		{
			"type": "function",
			"function": map[string]any{
				"name":        "browser_task",
				"description": "Delegate a multi-step web browser task to a dedicated fast GUI subagent (Playwright/CDP) without polluting main context.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"goal":      map[string]any{"type": "string", "description": "Clear browser objective and completion condition"},
						"max_steps": map[string]any{"type": "integer", "description": "Optional max subagent turns (default 30, max 80)"},
						"start_url": map[string]any{"type": "string", "description": "Optional starting URL to navigate to first"},
					},
					"required": []string{"goal"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "cronjob_manage",
				"description": "List, add, or remove recurring Tier-0 daemon cron schedules in .soma/triggers.yaml.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":   map[string]any{"type": "string", "description": "Action: list | add | remove"},
						"id":       map[string]any{"type": "string", "description": "Trigger ID (required for add/remove)"},
						"prompt":   map[string]any{"type": "string", "description": "Task prompt to run on schedule (required for add)"},
						"schedule": map[string]any{"type": "string", "description": "Duration interval (e.g. '30m', '24h') or cron expression"},
					},
					"required": []string{"action"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "desktop_task",
				"description": "Delegate a GUI desktop task to a dedicated fast vision subagent operating inside soma's isolated virtual desktop.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"goal":      map[string]any{"type": "string", "description": "Clear desktop GUI objective and completion condition"},
						"max_steps": map[string]any{"type": "integer", "description": "Optional max subagent turns (default 40, max 100)"},
					},
					"required": []string{"goal"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "exec",
				"description": "Execute a shell command inside the isolated branch workspace. Returns exit_code and combined stdout/stderr (including partial output on timeout).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command":     map[string]any{"type": "string", "description": "Shell command to execute inside workspace"},
						"timeout_sec": map[string]any{"type": "integer", "description": "Optional timeout in seconds (default 20, max 300; use bg_exec for long-running jobs)"},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_grep",
				"description": "Fast regex search across workspace files (skips .git/.soma/binary files). Prefer this over reading many files when locating logs, symbols, configs, or motifs.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"context_lines": map[string]any{"type": "integer", "description": "Optional context lines before/after each match (0-5)"},
						"glob":          map[string]any{"type": "string", "description": "Optional filename glob filter (e.g. '*.py', '*.log')"},
						"ignore_case":   map[string]any{"type": "boolean", "description": "Optional case-insensitive regex matching"},
						"path":          map[string]any{"type": "string", "description": "Optional relative subdirectory or file path (default '.')"},
						"pattern":       map[string]any{"type": "string", "description": "Go regular expression pattern to match"},
					},
					"required": []string{"pattern"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_list",
				"description": "List relative file paths inside the workspace or a subdirectory.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Optional relative subdirectory (default '.')"},
					},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_patch",
				"description": "Apply single or multi-hunk SEARCH/REPLACE blocks or a unified diff to a workspace file with fuzzy indentation tolerance.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"patch":   map[string]any{"type": "string", "description": "<<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE block(s) or unified diff"},
						"path":    map[string]any{"type": "string", "description": "Relative target file path"},
						"replace": map[string]any{"type": "string", "description": "Optional explicit replacement snippet when not using patch field"},
						"search":  map[string]any{"type": "string", "description": "Optional explicit search snippet when not using patch field"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_read",
				"description": "Read a file inside the workspace (large files spill to @page handles). Pass start_line/end_line to read a specific 1-indexed line range inline.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"end_line":   map[string]any{"type": "integer", "description": "Optional 1-indexed inclusive end line"},
						"path":       map[string]any{"type": "string", "description": "Relative path inside workspace"},
						"start_line": map[string]any{"type": "integer", "description": "Optional 1-indexed start line"},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_write",
				"description": "Write complete file contents inside the workspace.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string", "description": "Exact UTF-8 file contents"},
						"path":    map[string]any{"type": "string", "description": "Relative target file path"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "mcp_call",
				"description": "Invoke a tool on a connected stdio or HTTP MCP server (use tool_search to discover available MCP tools).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"arguments_json": map[string]any{"type": "string", "description": "JSON object string of arguments for the MCP tool"},
						"server":         map[string]any{"type": "string", "description": "Connected MCP server name (e.g. 'playwright', 'desktop')"},
						"tool":           map[string]any{"type": "string", "description": "Tool name exposed by the MCP server"},
					},
					"required": []string{"server", "tool"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "memory_search",
				"description": "Search scoped bi-temporal memory rules, facts, and preferences using Hybrid BM25 + Matryoshka int8 Vector RRF.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string", "description": "Natural language or keyword search query"},
						"scope": map[string]any{"type": "string", "description": "Optional memory scope filter (e.g. 'global')"},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "memory_write",
				"description": "Persist a learned fact, rule, or user preference into soma's bi-temporal memory graph (quarantined automatically if the run has observed untrusted external data).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string", "description": "Concise fact, rule, or preference to remember"},
						"kind":    map[string]any{"type": "string", "description": "Optional kind: fact | preference | rule (default 'fact')"},
						"scope":   map[string]any{"type": "string", "description": "Optional scope (default 'global')"},
					},
					"required": []string{"content"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "page_grep",
				"description": "Search a spilled @page virtual memory handle using a regex or substring pattern.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"context_lines": map[string]any{"type": "integer", "description": "Optional context lines before/after each match (0-5)"},
						"ignore_case":   map[string]any{"type": "boolean", "description": "Optional case-insensitive matching"},
						"page_id":       map[string]any{"type": "string", "description": "Page handle ID (e.g. page_0001)"},
						"pattern":       map[string]any{"type": "string", "description": "Regex pattern to match lines"},
					},
					"required": []string{"page_id", "pattern"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "page_search",
				"description": "Hybrid BM25 + Matryoshka int8 Vector RRF search across 40-line chunks of spilled @page virtual memory handles.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"page_id": map[string]any{"type": "string", "description": "Optional specific page handle ID to search within"},
						"query":   map[string]any{"type": "string", "description": "Conceptual or keyword query to rank @page chunks"},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "page_slice",
				"description": "Read a specific 1-indexed line range from a spilled @page virtual memory handle.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"end_line":   map[string]any{"type": "integer", "description": "1-indexed end line inclusive"},
						"page_id":    map[string]any{"type": "string", "description": "Page handle ID (e.g. page_0001)"},
						"start_line": map[string]any{"type": "integer", "description": "1-indexed start line"},
					},
					"required": []string{"page_id", "start_line", "end_line"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "publish_finding",
				"description": "Broadcast a discovered fact, contract, or @page handle to concurrent sibling agents over the Blackboard.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"page_id": map[string]any{"type": "string", "description": "Optional @page handle ID with supporting evidence"},
						"summary": map[string]any{"type": "string", "description": "Concise finding summary to deliver to sibling agents"},
					},
					"required": []string{"summary"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "session_search",
				"description": "Search past soma runs and goals to recall previous sessions, decisions, and outcomes.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"limit": map[string]any{"type": "integer", "description": "Optional max results (default 5, max 20)"},
						"query": map[string]any{"type": "string", "description": "Keyword search query over past runs"},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "skill_manage",
				"description": "List, read, create, or patch reusable AAIF SKILL.md skill packages (security-linted before activation).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":  map[string]any{"type": "string", "description": "Action: list | read | create | patch"},
						"content": map[string]any{"type": "string", "description": "Full SKILL.md content (required for create/patch)"},
						"name":    map[string]any{"type": "string", "description": "Lowercase hyphenated skill name (required for read/create/patch)"},
					},
					"required": []string{"action"},
				},
			},
		},
		simpleTool("tool_search", "Search available core syscalls and connected MCP tools (with their JSON schemas) by keyword.",
			[]string{"query"}, map[string]any{
				"query": strProp("Keyword to match against tool names and descriptions (empty lists everything)"),
			}),
		{
			"type": "function",
			"function": map[string]any{
				"name":        "vision_analyze",
				"description": "Inspect a workspace image file (dimensions, format, and multimodal visual/OCR analysis).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"image_path": map[string]any{"type": "string", "description": "Relative path to image file inside workspace"},
						"prompt":     map[string]any{"type": "string", "description": "Visual question or analysis instruction"},
					},
					"required": []string{"image_path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "web_extract",
				"description": "Fetch an HTTP/HTTPS URL and extract clean readable text (marked untrusted_external for provenance taint).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{"type": "string", "description": "Target http:// or https:// URL"},
					},
					"required": []string{"url"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "web_search",
				"description": "Search the web for documentation, issues, or technical references (marked untrusted_external).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"max_results": map[string]any{"type": "integer", "description": "Optional max results (default 5)"},
						"query":       map[string]any{"type": "string", "description": "Web search query"},
					},
					"required": []string{"query"},
				},
			},
		},
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func appendUserMessage(msgs []map[string]any, text string) []map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1]["role"] == "user" {
		prev, _ := msgs[n-1]["content"].(string)
		if prev == "" {
			msgs[n-1]["content"] = text
		} else {
			msgs[n-1]["content"] = prev + "\n\n" + text
		}
		return msgs
	}
	return append(msgs, map[string]any{
		"role":    "user",
		"content": text,
	})
}

func buildOpenAIMessages(frame compiler.CompiledFrame, passReasoning bool) []map[string]any {
	if !frame.Structured {
		return []map[string]any{
			{"role": "system", "content": frame.Layer0 + "\n" + frame.Layer1},
			{"role": "user", "content": frame.Layer2 + "\n" + frame.Layer3},
		}
	}

	msgs := []map[string]any{
		{"role": "system", "content": frame.Layer0 + "\n" + frame.Layer1},
		{"role": "user", "content": "Execute the task_goal in Layer 0 using the available tools. When a large tool output spills to an @page handle, use page_grep or page_slice to inspect it before acting."},
	}

	entries := frame.Layer2Entries
	for i := 0; i < len(entries); {
		e := entries[i]
		if e.Kind == "turn" && !e.Masked && e.ToolName != "" {
			j := i + 1
			for j < len(entries) && entries[j].Kind == "turn" && !entries[j].Masked && entries[j].ToolName != "" && entries[j].TurnIndex == e.TurnIndex {
				j++
			}
			type toolResp struct {
				callID  string
				content string
			}
			var asstContent string
			var asstReasoning string
			toolCalls := make([]map[string]any, 0, j-i)
			toolResps := make([]toolResp, 0, j-i)
			for k := i; k < j; k++ {
				ent := entries[k]
				callID := strings.TrimSpace(ent.ToolCallID)
				if callID == "" {
					callID = fmt.Sprintf("call_e%d_t%d_%d", frame.Epoch, ent.TurnIndex, k-i)
				}
				argsJSON := strings.TrimSpace(ent.ArgsJSON)
				if argsJSON == "" {
					argsJSON = "{}"
				}
				if asstContent == "" {
					asstContent = ent.AssistantContent
				}
				if asstReasoning == "" {
					asstReasoning = ent.ReasoningContent
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      ent.ToolName,
						"arguments": argsJSON,
					},
				})
				obs := strings.TrimSpace(ent.Observation)
				if obs == "" {
					obs = "(empty)"
				}
				toolResps = append(toolResps, toolResp{callID: callID, content: obs})
			}
			asstMsg := map[string]any{
				"role":       "assistant",
				"content":    asstContent,
				"tool_calls": toolCalls,
			}
			if passReasoning && asstReasoning != "" {
				asstMsg["reasoning_content"] = asstReasoning
			}
			msgs = append(msgs, asstMsg)
			for _, tr := range toolResps {
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": tr.callID,
					"content":      tr.content,
				})
			}
			i = j
			continue
		}

		if e.Kind == "turn" && !e.Masked && e.Action == "done" {
			txt := strings.TrimSpace(e.Observation)
			if txt == "" {
				txt = strings.TrimSpace(e.Reasoning)
			}
			if txt == "" {
				txt = "Done."
			}
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": txt,
			})
			i++
			continue
		}

		msgs = appendUserMessage(msgs, compiler.FormatLogEntry(e))
		i++
	}

	lastRole, _ := msgs[len(msgs)-1]["role"].(string)
	hasViewport := frame.Tail.Viewport != nil && frame.Tail.Viewport.DataURI != ""
	needsTail := hasViewport || frame.Tail.StallCount > 0 || (frame.Tail.StepBudgetRemaining > 0 && frame.Tail.StepBudgetRemaining <= 2) || frame.Tail.GitDiffStat != ""
	if needsTail {
		if hasViewport {
			parts := []map[string]any{
				{"type": "text", "text": frame.Layer3},
				{"type": "image_url", "image_url": map[string]string{"url": frame.Tail.Viewport.DataURI}},
			}
			if lastRole == "user" {
				if prevText, ok := msgs[len(msgs)-1]["content"].(string); ok && strings.TrimSpace(prevText) != "" {
					parts[0]["text"] = prevText + "\n\n" + frame.Layer3
				}
				msgs[len(msgs)-1]["content"] = parts
			} else {
				msgs = append(msgs, map[string]any{
					"role":    "user",
					"content": parts,
				})
			}
		} else if lastRole != "user" {
			msgs = append(msgs, map[string]any{
				"role":    "user",
				"content": frame.Layer3,
			})
		}
	}
	return msgs
}

func (p *OpenAICompatProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	if p.BaseURL == "" {
		return nil, errors.New("openai-compat provider: empty BaseURL")
	}
	maxToks := p.MaxTokens
	if maxToks <= 0 {
		maxToks = 16384
	}

	msgs := buildOpenAIMessages(req.Frame, p.PassReasoningContent)
	if wantsExplicitCacheControl(req.ModelID) {
		markCacheBreakpoints(msgs)
	}
	payload := map[string]any{
		"model":       req.ModelID,
		"messages":    msgs,
		"temperature": 0.0,
		"max_tokens":  maxToks,
	}
	if effort := strings.TrimSpace(p.ReasoningEffort); effort != "" {
		payload["reasoning_effort"] = effort
	}
	if p.EnableTools {
		payload["tools"] = openAITools()
		payload["tool_choice"] = "auto"
	}

	marshal := func(stream bool) ([]byte, error) {
		if stream {
			payload["stream"] = true
			payload["stream_options"] = map[string]any{"include_usage": true}
		} else {
			delete(payload, "stream")
			delete(payload, "stream_options")
		}
		return json.Marshal(payload)
	}
	streaming := !p.DisableStream && !p.streamUnsupported.Load()
	body, err := marshal(streaming)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	const (
		maxAttempts = 4
		// A short rate limit is cheaper to wait out than to suspend the whole branch for.
		maxInlineRateLimitWait = 10 * time.Second
	)
	var respBytes []byte
	var statusCode int
	var retryAfterHeader string
	var parsed chatCompletion
	haveParsed := false
	waitedOnRateLimit := false // wait out at most one short rate limit per call
	backoff := func(attempt int) time.Duration {
		return time.Duration(1<<uint(attempt-1)) * 400 * time.Millisecond
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		reqCtx, cancelReq := context.WithCancel(ctx)
		httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			cancelReq()
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("User-Agent", "soma/0.4")
		if streaming {
			httpReq.Header.Set("Accept", "text/event-stream")
		}
		if p.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
		if strings.Contains(strings.ToLower(p.BaseURL), "opencode.ai") {
			sessRun := req.RunID
			if sessRun == "" {
				sessRun = "run"
			}
			// Route all branches of a run to the same upstream RadixAttention worker so forked
			// branches share the warm Layer 0 KV cache prefix across the search tree.
			httpReq.Header.Set("x-opencode-session", fmt.Sprintf("soma-%s", sessRun))
		}

		resp, err := client.Do(httpReq)
		if err != nil {
			cancelReq()
			tErr := &TransientError{AccountID: req.AccountID, Err: err}
			if attempt < maxAttempts && ctx.Err() == nil {
				if sleepErr := sleepWithContext(ctx, backoff(attempt)); sleepErr != nil {
					return nil, tErr
				}
				continue
			}
			return nil, tErr
		}
		statusCode = resp.StatusCode
		retryAfterHeader = resp.Header.Get("Retry-After")

		if statusCode == http.StatusOK && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			completion, streamErr := readChatStream(resp.Body, cancelReq, p.StreamIdleTimeout, deltaSink(ctx, p.OnDelta))
			_ = resp.Body.Close()
			cancelReq()
			if streamErr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				// A stream that broke part-way is retried once at most: the tokens generated so
				// far are already paid for, and repeating a multi-minute generation is expensive.
				tErr := &TransientError{AccountID: req.AccountID, Err: streamErr}
				if attempt < 2 {
					if sleepErr := sleepWithContext(ctx, backoff(attempt)); sleepErr != nil {
						return nil, tErr
					}
					continue
				}
				return nil, tErr
			}
			parsed = *completion
			haveParsed = true
			respBytes, _ = json.Marshal(completion)
			break
		}

		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		cancelReq()
		if readErr != nil {
			tErr := &TransientError{AccountID: req.AccountID, Err: readErr}
			if attempt < maxAttempts && ctx.Err() == nil {
				if sleepErr := sleepWithContext(ctx, backoff(attempt)); sleepErr != nil {
					return nil, tErr
				}
				continue
			}
			return nil, tErr
		}
		respBytes = b

		// Endpoints that reject streaming parameters get one buffered retry, remembered afterwards.
		if streaming && statusCode == http.StatusBadRequest && bytes.Contains(bytes.ToLower(b), []byte("stream")) {
			p.streamUnsupported.Store(true)
			streaming = false
			if body, err = marshal(false); err != nil {
				return nil, err
			}
			attempt--
			continue
		}
		if statusCode == http.StatusTooManyRequests && attempt < maxAttempts && ctx.Err() == nil {
			if wait := parseRetryAfter(retryAfterHeader); retryAfterHeader != "" && wait <= maxInlineRateLimitWait && !waitedOnRateLimit {
				waitedOnRateLimit = true
				if sleepErr := sleepWithContext(ctx, wait); sleepErr != nil {
					break
				}
				continue
			}
		}
		if statusCode >= 500 && attempt < maxAttempts && ctx.Err() == nil {
			if sleepErr := sleepWithContext(ctx, backoff(attempt)); sleepErr != nil {
				break
			}
			continue
		}
		break
	}

	if statusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(retryAfterHeader)
		return nil, &RateLimitError{
			AccountID:  req.AccountID,
			RetryAfter: retryAfter,
			Err:        fmt.Errorf("HTTP 429: %s", string(respBytes)),
		}
	}
	if statusCode >= 500 {
		return nil, &TransientError{
			AccountID:  req.AccountID,
			StatusCode: statusCode,
			Err:        fmt.Errorf("HTTP %d: %s", statusCode, string(respBytes)),
		}
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, fmt.Errorf("upstream HTTP %d: %s", statusCode, string(respBytes))
	}

	if !haveParsed {
		if err := json.Unmarshal(respBytes, &parsed); err != nil {
			return nil, fmt.Errorf("decode chat completion: %w", err)
		}
	}

	cached := parsed.Usage.PromptTokensDetails.CachedTokens
	if parsed.Usage.OpenCode.Tokens.CacheRead > cached {
		cached = parsed.Usage.OpenCode.Tokens.CacheRead
	}
	uncached := parsed.Usage.PromptTokens - cached
	if uncached < 0 {
		uncached = parsed.Usage.PromptTokens
	}

	cacheWrite := parsed.Usage.PromptTokensDetails.CacheWriteTokens
	if parsed.Usage.CacheCreationInputTokens > cacheWrite {
		cacheWrite = parsed.Usage.CacheCreationInputTokens
	}

	gen := &GenerateResponse{
		InputTokens:      uncached,
		CacheWriteTokens: cacheWrite,
		CacheReadTokens:  cached,
		OutputTokens:     parsed.Usage.CompletionTokens,
		CostUSD:          parsed.Usage.Cost,
		LatencyMS:        time.Since(start).Milliseconds(),
		RawEmitted:       respBytes,
	}

	if len(parsed.Choices) == 0 {
		gen.Done = true
		gen.StateHash = shortHashHex([]byte("empty"))
		return gen, nil
	}

	msg := parsed.Choices[0].Message
	content := strings.TrimSpace(msg.Content)
	if len(msg.ToolCalls) == 0 && content == "" && parsed.Choices[0].FinishReason == "length" {
		return nil, fmt.Errorf("openai-compat provider: response truncated (finish_reason=length at max_tokens=%d) before emitting content or tool_calls", maxToks)
	}
	calls := make([]wireToolCall, 0, len(msg.ToolCalls))
	for _, rawTC := range msg.ToolCalls {
		calls = append(calls, wireToolCall{ID: rawTC.ID, Name: rawTC.Function.Name, Arguments: rawTC.Function.Arguments})
	}
	return finishGenerateResponse(gen, req, content, msg.ReasoningContent, calls), nil
}

// inferToolEffectClass maps a tool name to its risk tier. Only the built-in workspace tools are
// known to be local; anything else (MCP tools, brokers, future tools) is assumed to reach outside
// the sandbox, so it is subject to taint checks instead of being waved through as read-only.
func inferToolEffectClass(toolName string) taint.EffectClass {
	if def, ok := lookupTool(toolName); ok {
		return def.Effect
	}
	switch toolName {
	case "fs_read", "fs_list", "fs_grep", "page_slice", "page_grep", "page_search", "memory_search", "session_search", "bg_poll", "tool_search":
		return taint.EffectReadOnly
	case "web_search", "web_extract", "vision_analyze":
		// Outbound requests: their arguments are an egress channel once the run is tainted.
		return taint.EffectExternalRead
	case "publish_finding":
		// Persists shared memory, so it goes through the same quarantine as memory_write.
		return taint.EffectWorkspaceWrite
	case "fs_write", "fs_patch", "exec", "bg_exec", "bg_kill", "branch_fork", "browser_task", "desktop_task", "memory_write", "skill_manage", "cronjob_manage":
		return taint.EffectWorkspaceWrite
	default:
		return taint.EffectExternalWrite
	}
}

func shortHashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func parseRetryAfter(hdr string) time.Duration {
	hdr = strings.TrimSpace(hdr)
	if hdr == "" {
		return 15 * time.Second
	}
	if secs, err := strconv.Atoi(hdr); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(hdr); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 15 * time.Second
}

// MockProvider is a deterministic zero-key simulation provider for CLI --mock demos and smoke tests.
type MockProvider struct {
	ModelName string
	calls     atomic.Int64
}

// Generate simulates a 3-turn inspect -> patch/write -> verify completion sequence.
func (m *MockProvider) Generate(_ context.Context, req GenerateRequest) (*GenerateResponse, error) {
	n := m.calls.Add(1)
	l01 := req.Frame.L0Tokens + req.Frame.L1Tokens
	cacheRead := l01
	if req.TurnIndex > 1 {
		cacheRead += req.Frame.L2Tokens / 2
	}
	inTok := req.Frame.TotalTokens - cacheRead
	if inTok < 1 {
		inTok = 1
	}

	switch req.TurnIndex {
	case 1:
		return &GenerateResponse{
			Reasoning: "Inspect workspace state before proposing changes.",
			ToolCall: &ToolCallSpec{
				Name:        "fs_read",
				ArgsJSON:    `{"path":"README.md"}`,
				EffectClass: taint.EffectReadOnly,
			},
			StateHash:       fmt.Sprintf("mock_s1_%d", n),
			InputTokens:     inTok,
			CacheReadTokens: cacheRead,
			OutputTokens:    42,
			LatencyMS:       12,
		}, nil
	case 2:
		return &GenerateResponse{
			Reasoning: "Write verification note into isolated branch workspace.",
			ToolCall: &ToolCallSpec{
				Name:        "fs_write",
				ArgsJSON:    `{"path":"artifacts/soma_mock_note.txt","content":"verified in isolated workspace\n"}`,
				EffectClass: taint.EffectWorkspaceWrite,
			},
			StateHash:       fmt.Sprintf("mock_s2_%d", n),
			InputTokens:     inTok,
			CacheReadTokens: cacheRead,
			OutputTokens:    58,
			LatencyMS:       15,
		}, nil
	default:
		return &GenerateResponse{
			Reasoning:       "Workspace note written cleanly; task complete.",
			Done:            true,
			FinalSummary:    fmt.Sprintf("Completed mock run on model %s in %d turns.", req.ModelID, req.TurnIndex),
			StateHash:       fmt.Sprintf("mock_done_%d", n),
			InputTokens:     inTok,
			CacheReadTokens: cacheRead,
			OutputTokens:    36,
			LatencyMS:       10,
		}, nil
	}
}
