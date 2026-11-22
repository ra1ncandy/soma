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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/taint"
)

// ToolCallSpec describes a tool/syscall invocation requested by a model turn.
type ToolCallSpec struct {
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
	BaseURL     string
	APIKey      string
	HTTPClient  *http.Client
	MaxTokens   int
	EnableTools bool
}

// NewOpenAICompatProvider creates a client targeting baseURL (e.g. https://opencode.ai/zen/go/v1).
func NewOpenAICompatProvider(baseURL, apiKey string) *OpenAICompatProvider {
	return &OpenAICompatProvider{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		APIKey:      apiKey,
		MaxTokens:   2048,
		EnableTools: true,
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func canonicalOpenAITools() []map[string]any {
	// Lexicographically ordered by function name to keep prompt/tool prefixes byte-stable across turns.
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "exec",
				"description": "Execute a shell command inside the isolated branch workspace.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string", "description": "Shell command to execute inside workspace"},
					},
					"required": []string{"command"},
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
				"name":        "fs_read",
				"description": "Read a file inside the workspace (large files spill to @page handles).",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Relative path inside workspace"},
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
				"name":        "page_grep",
				"description": "Search a spilled @page virtual memory handle using a regex or substring pattern.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"page_id": map[string]any{"type": "string", "description": "Page handle ID (e.g. page_0001)"},
						"pattern": map[string]any{"type": "string", "description": "Regex pattern to match lines"},
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

func (p *OpenAICompatProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	if p.BaseURL == "" {
		return nil, errors.New("openai-compat provider: empty BaseURL")
	}
	maxToks := p.MaxTokens
	if maxToks <= 0 {
		maxToks = 2048
	}

	payload := map[string]any{
		"model": req.ModelID,
		"messages": []map[string]string{
			{"role": "system", "content": req.Frame.Layer0 + "\n" + req.Frame.Layer1},
			{"role": "user", "content": req.Frame.Layer2 + "\n" + req.Frame.Layer3},
		},
		"temperature": 0.0,
		"max_tokens":  maxToks,
	}
	if p.EnableTools {
		payload["tools"] = canonicalOpenAITools()
		payload["tool_choice"] = "auto"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	const maxAttempts = 3
	var respBytes []byte
	var statusCode int
	var retryAfterHeader string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("User-Agent", "soma/0.4")
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
			tErr := &TransientError{AccountID: req.AccountID, Err: err}
			if attempt < maxAttempts && ctx.Err() == nil {
				if sleepErr := sleepWithContext(ctx, time.Duration(attempt)*300*time.Millisecond); sleepErr != nil {
					return nil, tErr
				}
				continue
			}
			return nil, tErr
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			tErr := &TransientError{AccountID: req.AccountID, Err: readErr}
			if attempt < maxAttempts && ctx.Err() == nil {
				if sleepErr := sleepWithContext(ctx, time.Duration(attempt)*300*time.Millisecond); sleepErr != nil {
					return nil, tErr
				}
				continue
			}
			return nil, tErr
		}
		respBytes = b
		statusCode = resp.StatusCode
		retryAfterHeader = resp.Header.Get("Retry-After")

		if statusCode >= 500 && attempt < maxAttempts && ctx.Err() == nil {
			if sleepErr := sleepWithContext(ctx, time.Duration(attempt)*300*time.Millisecond); sleepErr != nil {
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

	var parsed struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int     `json:"prompt_tokens"`
			CompletionTokens    int     `json:"completion_tokens"`
			Cost                float64 `json:"cost"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			OpenCode struct {
				Tokens struct {
					CacheRead int `json:"cache_read"`
				} `json:"tokens"`
			} `json:"opencode"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("decode chat completion: %w", err)
	}

	cached := parsed.Usage.PromptTokensDetails.CachedTokens
	if parsed.Usage.OpenCode.Tokens.CacheRead > cached {
		cached = parsed.Usage.OpenCode.Tokens.CacheRead
	}
	uncached := parsed.Usage.PromptTokens - cached
	if uncached < 0 {
		uncached = parsed.Usage.PromptTokens
	}

	gen := &GenerateResponse{
		InputTokens:     uncached,
		CacheReadTokens: cached,
		OutputTokens:    parsed.Usage.CompletionTokens,
		CostUSD:         parsed.Usage.Cost,
		LatencyMS:       time.Since(start).Milliseconds(),
		RawEmitted:      respBytes,
	}

	if len(parsed.Choices) == 0 {
		gen.Done = true
		gen.StateHash = shortHashHex([]byte("empty"))
		return gen, nil
	}

	msg := parsed.Choices[0].Message
	content := strings.TrimSpace(msg.Content)
	reasoning := content
	if reasoning == "" {
		reasoning = strings.TrimSpace(msg.ReasoningContent)
	}
	if len(reasoning) > 280 {
		reasoning = reasoning[:277] + "..."
	}

	if len(msg.ToolCalls) > 0 {
		var specs []ToolCallSpec
		var hashParts []string
		var names []string
		for _, rawTC := range msg.ToolCalls {
			fn := rawTC.Function
			argsJSON := strings.TrimSpace(fn.Arguments)
			if argsJSON == "" {
				argsJSON = "{}"
			}
			var refs []string
			var argMap map[string]any
			if json.Unmarshal([]byte(argsJSON), &argMap) == nil {
				if pid, ok := argMap["page_id"].(string); ok && strings.TrimSpace(pid) != "" {
					refs = append(refs, strings.TrimPrefix(strings.TrimSpace(pid), "@"))
				}
			}
			specs = append(specs, ToolCallSpec{
				Name:              fn.Name,
				ArgsJSON:          argsJSON,
				EffectClass:       inferToolEffectClass(fn.Name),
				ReferencedPageIDs: refs,
			})
			hashParts = append(hashParts, fn.Name+"|"+argsJSON)
			names = append(names, fn.Name)
		}
		if reasoning == "" {
			reasoning = fmt.Sprintf("invoke %s", strings.Join(names, ", "))
		}
		gen.Reasoning = reasoning
		gen.Done = false
		gen.ToolCalls = specs
		gen.ToolCall = &gen.ToolCalls[0]
		gen.StateHash = shortHashHex([]byte(strings.Join(hashParts, ";")))
		return gen, nil
	}

	gen.Reasoning = reasoning
	gen.Done = true
	gen.FinalSummary = content
	gen.StateHash = shortHashHex([]byte("done|" + content))
	return gen, nil
}

func inferToolEffectClass(toolName string) taint.EffectClass {
	switch toolName {
	case "fs_read", "fs_list", "page_slice", "page_grep", "page_search", "memory_search", "publish_finding":
		return taint.EffectReadOnly
	case "fs_write", "fs_patch", "exec":
		return taint.EffectWorkspaceWrite
	default:
		return taint.EffectReadOnly
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
