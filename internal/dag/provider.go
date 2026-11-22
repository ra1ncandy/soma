package dag

import (
	"bytes"
	"context"
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
	Reasoning        string              `json:"reasoning"`
	Done             bool                `json:"done"`
	FinalSummary     string              `json:"final_summary,omitempty"`
	ToolCall         *ToolCallSpec       `json:"tool_call,omitempty"`
	DynamicSkill     *compiler.SkillBody `json:"dynamic_skill,omitempty"`
	DynamicMCP       *compiler.ToolSchema `json:"dynamic_mcp,omitempty"`
	StateHash        string              `json:"state_hash"`
	InputTokens      int                 `json:"input_tokens"`
	CacheReadTokens  int                 `json:"cache_read_tokens"`
	CacheWriteTokens int                 `json:"cache_write_tokens"`
	OutputTokens     int                 `json:"output_tokens"`
	CostUSD          float64             `json:"cost_usd"`
	LatencyMS        int64               `json:"latency_ms"`
	RawEmitted       []byte              `json:"-"`
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

// OpenAICompatProvider talks to any OpenAI-compatible /v1/chat/completions endpoint.
type OpenAICompatProvider struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// NewOpenAICompatProvider creates a client targeting baseURL (e.g. http://127.0.0.1:11434/v1).
func NewOpenAICompatProvider(baseURL, apiKey string) *OpenAICompatProvider {
	return &OpenAICompatProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

func (p *OpenAICompatProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	if p.BaseURL == "" {
		return nil, errors.New("openai-compat provider: empty BaseURL")
	}
	payload := map[string]any{
		"model": req.ModelID,
		"messages": []map[string]string{
			{"role": "system", "content": req.Frame.Layer0 + "\n" + req.Frame.Layer1},
			{"role": "user", "content": req.Frame.Layer2 + "\n" + req.Frame.Layer3},
		},
		"temperature": 0.0,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	start := time.Now()
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, &TransientError{AccountID: req.AccountID, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &TransientError{AccountID: req.AccountID, Err: err}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return nil, &RateLimitError{
			AccountID:  req.AccountID,
			RetryAfter: retryAfter,
			Err:        fmt.Errorf("HTTP 429: %s", string(respBytes)),
		}
	}
	if resp.StatusCode >= 500 {
		return nil, &TransientError{
			AccountID:  req.AccountID,
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBytes)),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("decode chat completion: %w", err)
	}
	content := ""
	if len(parsed.Choices) > 0 {
		content = parsed.Choices[0].Message.Content
	}
	uncached := parsed.Usage.PromptTokens - parsed.Usage.PromptTokensDetails.CachedTokens
	if uncached < 0 {
		uncached = parsed.Usage.PromptTokens
	}
	return &GenerateResponse{
		Reasoning:       content,
		Done:            true,
		FinalSummary:    content,
		StateHash:       fmt.Sprintf("http_t%d", req.TurnIndex),
		InputTokens:     uncached,
		CacheReadTokens: parsed.Usage.PromptTokensDetails.CachedTokens,
		OutputTokens:    parsed.Usage.CompletionTokens,
		LatencyMS:       time.Since(start).Milliseconds(),
		RawEmitted:      respBytes,
	}, nil
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
