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
	"sync/atomic"
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
	// ReasoningEffort overrides SOMA_REASONING_EFFORT and the default (see anthropicEffort).
	ReasoningEffort string

	// NoThinkingReplay stops the adapter from sending the model's thinking blocks back on later
	// turns (see thinkingBindingBeta). The model then re-plans every turn.
	NoThinkingReplay bool

	effortOmitted     atomic.Bool // the model rejected output_config.effort once; stop sending it
	bindingControlsNo atomic.Bool // the model rejected thinking.block_binding once; stop sending it
}

// thinkingBindingBeta lets a request ask the API to drop, instead of rejecting, thinking blocks
// whose earlier conversation has changed. Current models sign each thinking block against the
// system prompt, tool set and messages before it; soma replays the blocks verbatim so the model
// keeps its reasoning across tool calls, and anything soma rewrites in an earlier turn (the live
// tail, for one) invalidates the blocks after it. With "drop_block" that costs the affected
// reasoning, not the request.
const thinkingBindingBeta = "thinking-binding-controls-2026-08-01"

// anthropicEffort maps soma's effort setting to the levels the Messages API accepts
// (low, medium, high, xhigh, max). Thinking cannot be turned off on current models, so soma's
// two lowest settings both become "low". An empty result means "send nothing".
func anthropicEffort(explicit string) string {
	switch e := effectiveReasoningEffort(explicit); e {
	case "":
		return ""
	case "minimal", "none", "low":
		return "low"
	case "medium", "high", "xhigh", "max":
		return e
	default:
		return ""
	}
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
// merging consecutive same-role messages (the API requires alternation). An assistant turn that
// carries verbatim provider content (thinking blocks and what followed them) is sent back exactly
// as it was received; replayed reports whether any turn was.
func toAnthropicMessages(msgs []map[string]any) (system []map[string]any, out []map[string]any, replayed bool) {
	appendBlocks := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1]["role"] == role {
			out[n-1]["content"] = append(out[n-1]["content"].([]any), blocks...)
			return
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	anyBlocks := func(in []map[string]any) []any {
		res := make([]any, 0, len(in))
		for _, b := range in {
			res = append(res, b)
		}
		return res
	}
	for _, m := range msgs {
		role, _ := m["role"].(string)
		switch role {
		case "system":
			if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
				system = append(system, map[string]any{"type": "text", "text": s, "cache_control": ephemeral()})
			}
		case "user":
			appendBlocks("user", anyBlocks(contentBlocks(m["content"])))
		case "assistant":
			if verbatim, _ := m[providerContentKey].(string); verbatim != "" {
				var raw []json.RawMessage
				if json.Unmarshal([]byte(verbatim), &raw) == nil && len(raw) > 0 {
					blocks := make([]any, 0, len(raw))
					for _, r := range raw {
						blocks = append(blocks, r)
					}
					appendBlocks("assistant", blocks)
					replayed = true
					continue
				}
			}
			var blocks []any
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
			appendBlocks("user", []any{map[string]any{"type": "tool_result", "tool_use_id": m["tool_call_id"], "content": text}})
		}
	}
	// Rolling cache breakpoint on the newest block: the whole conversation prefix is reused next
	// turn. (Markers may move freely; they are not part of what thinking blocks are signed against.)
	if n := len(out); n > 0 {
		blocks := out[n-1]["content"].([]any)
		if len(blocks) > 0 {
			if last, ok := blocks[len(blocks)-1].(map[string]any); ok {
				last["cache_control"] = ephemeral()
			}
		}
	}
	return system, out, replayed
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
	maxToks := p.MaxTokens
	if maxToks <= 0 {
		maxToks = 16384
	}
	// build assembles the request. replay sends earlier thinking blocks back verbatim; controls
	// asks the API to drop blocks it can no longer accept instead of failing the request.
	var controlsSent bool
	build := func(replay, controls bool) ([]byte, error) {
		system, msgs, replayed := toAnthropicMessages(buildMessages(req.Frame, false, replay))
		if len(msgs) == 0 {
			msgs = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Continue."}}}}
		}
		payload := map[string]any{
			"model":      req.ModelID,
			"max_tokens": maxToks,
			"system":     system,
			"messages":   msgs,
			"tools":      anthropicTools(),
			"stream":     true,
		}
		if t, ok := configuredTemperature(); ok {
			payload["temperature"] = t
		}
		// Thinking is adaptive (and on by default) on current models; effort is the control for
		// how much the model thinks and writes. Models without the setting get one retry without it.
		if effort := anthropicEffort(p.ReasoningEffort); effort != "" && !p.effortOmitted.Load() {
			payload["output_config"] = map[string]any{"effort": effort}
		}
		controlsSent = replayed && controls
		if controlsSent {
			payload["thinking"] = map[string]any{"type": "adaptive", "block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"}}
		}
		return json.Marshal(payload)
	}
	replay := !p.NoThinkingReplay
	body, err := build(replay, !p.bindingControlsNo.Load())
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
		if controlsSent {
			hreq.Header.Set("anthropic-beta", thinkingBindingBeta)
		}
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
		if resp.StatusCode == http.StatusBadRequest {
			lower := bytes.ToLower(errBody)
			retry := false
			switch {
			case replay && bytes.Contains(lower, []byte("signature")) && bytes.Contains(lower, []byte("thinking")):
				// A replayed thinking block was refused outright. Answer this turn without the
				// earlier reasoning; later turns replay only what the model produces from here.
				replay, retry = false, true
			case controlsSent && (bytes.Contains(lower, []byte("block_binding")) || bytes.Contains(lower, []byte("prefix_mismatch_behavior")) ||
				bytes.Contains(lower, []byte(thinkingBindingBeta)) || bytes.Contains(lower, []byte("anthropic-beta"))):
				p.bindingControlsNo.Store(true)
				retry = true
			case !p.effortOmitted.Load() && (bytes.Contains(lower, []byte("effort")) || bytes.Contains(lower, []byte("output_config"))):
				p.effortOmitted.Store(true)
				retry = true
			}
			if retry {
				if body, err = build(replay, !p.bindingControlsNo.Load()); err != nil {
					return nil, err
				}
				attempt--
				continue
			}
		}
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
		data string          // redacted_thinking payload
		text strings.Builder // text, or thinking text
		json strings.Builder // tool_use input
		sig  strings.Builder // thinking signature
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
				Usage                anthropicUsage        `json:"usage"`
				InputTransformations []inputTransformation `json:"input_transformations"`
			} `json:"message"`
			InputTransformations []inputTransformation `json:"input_transformations"`
			ContentBlock         struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				Text      string `json:"text"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
				Data      string `json:"data"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
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
		for _, tr := range append(ev.Message.InputTransformations, ev.InputTransformations...) {
			if tr.Type == "thinking_dropped" {
				gen.ThinkingDropped++
			}
		}
		switch ev.Type {
		case "message_start":
			ev.Message.Usage.apply(gen)
		case "content_block_start":
			for len(blocks) <= ev.Index {
				blocks = append(blocks, &block{})
			}
			b := blocks[ev.Index]
			b.kind, b.id, b.name, b.data = ev.ContentBlock.Type, ev.ContentBlock.ID, ev.ContentBlock.Name, ev.ContentBlock.Data
			b.text.WriteString(ev.ContentBlock.Text)
			b.text.WriteString(ev.ContentBlock.Thinking)
			b.sig.WriteString(ev.ContentBlock.Signature)
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
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
			case "signature_delta":
				b.sig.WriteString(ev.Delta.Signature)
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
	// verbatim is the content array to send back unchanged on later turns. It is only worth
	// keeping when the turn has thinking, and only safe when every block is one we can reproduce.
	var verbatim []any
	hasThinking, reproducible := false, true
	for _, b := range blocks {
		switch b.kind {
		case "text":
			text.WriteString(b.text.String())
			verbatim = append(verbatim, map[string]any{"type": "text", "text": b.text.String()})
		case "tool_use":
			args := strings.TrimSpace(b.json.String())
			if args == "" {
				args = "{}"
			}
			calls = append(calls, wireToolCall{ID: b.id, Name: b.name, Arguments: args})
			if !json.Valid([]byte(args)) {
				reproducible = false
			}
			verbatim = append(verbatim, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": json.RawMessage(args)})
		case "thinking":
			// Kept even when the text is empty (the default): the signature is what counts.
			hasThinking = true
			verbatim = append(verbatim, map[string]any{"type": "thinking", "thinking": b.text.String(), "signature": b.sig.String()})
		case "redacted_thinking":
			hasThinking = true
			verbatim = append(verbatim, map[string]any{"type": "redacted_thinking", "data": b.data})
		default:
			reproducible = false
		}
	}
	if hasThinking && reproducible && len(calls) > 0 {
		if raw, err := json.Marshal(verbatim); err == nil {
			gen.ProviderContent = string(raw)
		}
	}
	if stopReason == "refusal" {
		// A safety classifier declined the request (HTTP 200). There is nothing to act on, and
		// treating the empty reply as "done" would end the run as if it had succeeded.
		return nil, errors.New("anthropic: the model declined this request (stop_reason=refusal)")
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" && stopReason == "max_tokens" {
		return nil, &OutputTruncatedError{MaxTokens: p.MaxTokens}
	}
	return finishGenerateResponse(gen, req, text.String(), "", calls), nil
}

type inputTransformation struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
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
