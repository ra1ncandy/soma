package dag

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicProvider speaks the native Anthropic Messages API (instead of its OpenAI-compatibility
// layer): system prompt and tools as first-class fields, tool_use / tool_result blocks, and
// explicit prompt-cache breakpoints on the frozen Layer 0/1 prefix, the tool list and the
// newest message, so a whole epoch is served from cache.
type AnthropicProvider struct {
	BaseURL    string // default https://api.anthropic.com/v1
	APIKey     string
	HTTPClient *http.Client
	MaxTokens  int
	// OnDelta receives streamed assistant text.
	OnDelta func(text string)
	// StreamIdleTimeout bounds silence on the stream (default 180s).
	StreamIdleTimeout time.Duration
}

// NewAnthropicProvider creates a native Anthropic Messages client.
func NewAnthropicProvider(baseURL, apiKey string) *AnthropicProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	return &AnthropicProvider{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		MaxTokens:  16384,
		HTTPClient: &http.Client{Transport: providerTransport()},
	}
}

const anthropicVersion = "2023-06-01"

// backoff is the retry delay before attempt+1: 400ms, 800ms, 1.6s, ...
func backoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt-1)) * 400 * time.Millisecond
}

func ephemeral() map[string]any { return map[string]any{"type": "ephemeral"} }

// anthropicTools converts the canonical OpenAI tool schemas; the last tool carries a cache
// breakpoint so the (epoch-stable) tool list is cached with the system prompt.
func anthropicTools() []map[string]any {
	src := openAITools()
	out := make([]map[string]any, 0, len(src))
	for _, t := range src {
		fn, _ := t["function"].(map[string]any)
		out = append(out, map[string]any{
			"name":         fn["name"],
			"description":  fn["description"],
			"input_schema": fn["parameters"],
		})
	}
	if len(out) > 0 {
		out[len(out)-1]["cache_control"] = ephemeral()
	}
	return out
}

// toAnthropicMessages converts the canonical OpenAI-shaped message list into Anthropic blocks,
// merging consecutive same-role messages (the API requires alternation).
func toAnthropicMessages(msgs []map[string]any) (system []map[string]any, out []map[string]any) {
	appendBlocks := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1]["role"] == role {
			out[n-1]["content"] = append(out[n-1]["content"].([]map[string]any), blocks...)
			return
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	for _, m := range msgs {
		role, _ := m["role"].(string)
		switch role {
		case "system":
			if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
				system = append(system, map[string]any{"type": "text", "text": s, "cache_control": ephemeral()})
			}
		case "user":
			appendBlocks("user", contentBlocks(m["content"]))
		case "assistant":
			var blocks []map[string]any
			if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
			if tcs, ok := m["tool_calls"].([]map[string]any); ok {
				for _, tc := range tcs {
					fn, _ := tc["function"].(map[string]any)
					var input any = map[string]any{}
					if a, _ := fn["arguments"].(string); a != "" {
						_ = json.Unmarshal([]byte(a), &input)
					}
					blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": input})
				}
			}
			appendBlocks("assistant", blocks)
		case "tool":
			text, _ := m["content"].(string)
			appendBlocks("user", []map[string]any{{"type": "tool_result", "tool_use_id": m["tool_call_id"], "content": text}})
		}
	}
	// Rolling cache breakpoint on the newest block: the whole conversation prefix is reused next turn.
	if n := len(out); n > 0 {
		blocks := out[n-1]["content"].([]map[string]any)
		if len(blocks) > 0 {
			blocks[len(blocks)-1]["cache_control"] = ephemeral()
		}
	}
	return system, out
}

func contentBlocks(c any) []map[string]any {
	switch v := c.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": v}}
	case []map[string]any:
		var out []map[string]any
		for _, p := range v {
			switch p["type"] {
			case "text":
				out = append(out, map[string]any{"type": "text", "text": p["text"]})
			case "image_url":
				iu, _ := p["image_url"].(map[string]string)
				if mt, data, ok := parseDataURI(iu["url"]); ok {
					out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mt, "data": data}})
				}
			}
		}
		return out
	}
	return nil
}

func parseDataURI(u string) (mime, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	head, body, found := strings.Cut(strings.TrimPrefix(u, "data:"), ",")
	if !found || !strings.HasSuffix(head, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(head, ";base64"), body, true
}

// Generate implements ModelProvider.
func (p *AnthropicProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	system, msgs := toAnthropicMessages(buildOpenAIMessages(req.Frame, false))
	if len(msgs) == 0 {
		msgs = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Continue."}}}}
	}
	maxToks := p.MaxTokens
	if maxToks <= 0 {
		maxToks = 16384
	}
	payload := map[string]any{
		"model":       req.ModelID,
		"max_tokens":  maxToks,
		"system":      system,
		"messages":    msgs,
		"tools":       anthropicTools(),
		"temperature": 0.0,
		"stream":      true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	start := time.Now()
	waitedInline := false
	for attempt := 1; ; attempt++ {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("content-type", "application/json")
		hreq.Header.Set("anthropic-version", anthropicVersion)
		hreq.Header.Set("x-api-key", p.APIKey)
		resp, err := client.Do(hreq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < 3 {
				if sErr := sleepWithContext(ctx, backoff(attempt)); sErr != nil {
					return nil, sErr
				}
				continue
			}
			return nil, &TransientError{AccountID: req.AccountID, Err: err}
		}
		if resp.StatusCode == http.StatusOK {
			gen, sErr := p.readStream(ctx, resp.Body, req)
			_ = resp.Body.Close()
			if sErr != nil {
				return nil, sErr
			}
			gen.LatencyMS = time.Since(start).Milliseconds()
			return gen, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			ra := parseRetryAfter(resp.Header.Get("Retry-After"))
			if !waitedInline && ra <= 10*time.Second {
				waitedInline = true
				if sErr := sleepWithContext(ctx, ra); sErr != nil {
					return nil, sErr
				}
				continue
			}
			return nil, &RateLimitError{AccountID: req.AccountID, RetryAfter: ra, Err: fmt.Errorf("anthropic 429: %s", strings.TrimSpace(string(errBody)))}
		case resp.StatusCode >= 500: // includes 529 overloaded
			if attempt < 3 {
				if sErr := sleepWithContext(ctx, backoff(attempt)); sErr != nil {
					return nil, sErr
				}
				continue
			}
			return nil, &TransientError{AccountID: req.AccountID, StatusCode: resp.StatusCode, Err: fmt.Errorf("anthropic %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))}
		default:
			return nil, fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
		}
	}
}

// readStream assembles a streamed Messages response.
func (p *AnthropicProvider) readStream(ctx context.Context, r io.Reader, req GenerateRequest) (*GenerateResponse, error) {
	type block struct {
		kind string
		id   string
		name string
		text strings.Builder
		json strings.Builder
	}
	var blocks []*block
	gen := &GenerateResponse{}
	onDelta := deltaSink(ctx, p.OnDelta)
	idle := p.StreamIdleTimeout
	if idle <= 0 {
		idle = 180 * time.Second
	}
	lines := make(chan string)
	errc := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		errc <- sc.Err()
		close(lines)
	}()
	stopReason := ""
	for {
		var line string
		var ok bool
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(idle):
			return nil, &TransientError{AccountID: req.AccountID, Err: fmt.Errorf("anthropic stream idle for %s", idle)}
		case line, ok = <-lines:
		}
		if !ok {
			if err := <-errc; err != nil {
				return nil, &TransientError{AccountID: req.AccountID, Err: err}
			}
			break
		}
		data, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage anthropicUsage `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			ev.Message.Usage.apply(gen)
		case "content_block_start":
			for len(blocks) <= ev.Index {
				blocks = append(blocks, &block{})
			}
			b := blocks[ev.Index]
			b.kind, b.id, b.name = ev.ContentBlock.Type, ev.ContentBlock.ID, ev.ContentBlock.Name
			b.text.WriteString(ev.ContentBlock.Text)
		case "content_block_delta":
			if ev.Index >= len(blocks) {
				continue
			}
			b := blocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				if onDelta != nil {
					onDelta(ev.Delta.Text)
				}
			case "input_json_delta":
				b.json.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage.OutputTokens > 0 {
				gen.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			if ev.Error.Type == "overloaded_error" {
				return nil, &TransientError{AccountID: req.AccountID, StatusCode: 529, Err: errors.New(ev.Error.Message)}
			}
			return nil, fmt.Errorf("anthropic stream error %s: %s", ev.Error.Type, ev.Error.Message)
		}
	}
	var text strings.Builder
	var calls []wireToolCall
	for _, b := range blocks {
		switch b.kind {
		case "text":
			text.WriteString(b.text.String())
		case "tool_use":
			args := strings.TrimSpace(b.json.String())
			if args == "" {
				args = "{}"
			}
			calls = append(calls, wireToolCall{ID: b.id, Name: b.name, Arguments: args})
		}
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" && stopReason == "max_tokens" {
		return nil, fmt.Errorf("anthropic: response truncated at max_tokens before emitting content or tool calls")
	}
	return finishGenerateResponse(gen, req, text.String(), "", calls), nil
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u anthropicUsage) apply(gen *GenerateResponse) {
	gen.InputTokens = u.InputTokens
	gen.CacheReadTokens = u.CacheReadInputTokens
	gen.CacheWriteTokens = u.CacheCreationInputTokens
	if u.OutputTokens > 0 {
		gen.OutputTokens = u.OutputTokens
	}
}
