package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ResponsesProvider speaks the OpenAI Responses API (POST {base}/responses), used by OpenAI
// reasoning models, the Codex backend and xAI. It sends stateless requests (store=false): soma's
// DAG, not the provider, is the source of truth for history.
type ResponsesProvider struct {
	BaseURL         string
	APIKey          string
	HTTPClient      *http.Client
	MaxOutputTokens int
	ReasoningEffort string
	// ExtraHeaders are sent on every request (e.g. account routing headers of a subscription backend).
	ExtraHeaders map[string]string
}

// NewResponsesProvider creates a Responses API client.
func NewResponsesProvider(baseURL, apiKey string) *ResponsesProvider {
	return &ResponsesProvider{
		BaseURL:         strings.TrimRight(baseURL, "/"),
		APIKey:          apiKey,
		MaxOutputTokens: 16384,
		HTTPClient:      &http.Client{Transport: providerTransport()},
	}
}

func responsesTools() []map[string]any {
	src := openAITools()
	out := make([]map[string]any, 0, len(src))
	for _, t := range src {
		fn, _ := t["function"].(map[string]any)
		out = append(out, map[string]any{"type": "function", "name": fn["name"], "description": fn["description"], "parameters": fn["parameters"]})
	}
	return out
}

// toResponsesInput converts the canonical message list into Responses input items.
func toResponsesInput(msgs []map[string]any) (instructions string, items []map[string]any) {
	for _, m := range msgs {
		role, _ := m["role"].(string)
		switch role {
		case "system":
			s, _ := m["content"].(string)
			instructions += s
		case "user":
			var parts []map[string]any
			switch c := m["content"].(type) {
			case string:
				parts = append(parts, map[string]any{"type": "input_text", "text": c})
			case []map[string]any:
				for _, p := range c {
					switch p["type"] {
					case "text":
						parts = append(parts, map[string]any{"type": "input_text", "text": p["text"]})
					case "image_url":
						iu, _ := p["image_url"].(map[string]string)
						parts = append(parts, map[string]any{"type": "input_image", "image_url": iu["url"]})
					}
				}
			}
			if len(parts) > 0 {
				items = append(items, map[string]any{"role": "user", "content": parts})
			}
		case "assistant":
			if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
				items = append(items, map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": s}}})
			}
			if tcs, ok := m["tool_calls"].([]map[string]any); ok {
				for _, tc := range tcs {
					fn, _ := tc["function"].(map[string]any)
					items = append(items, map[string]any{"type": "function_call", "call_id": tc["id"], "name": fn["name"], "arguments": fn["arguments"]})
				}
			}
		case "tool":
			items = append(items, map[string]any{"type": "function_call_output", "call_id": m["tool_call_id"], "output": m["content"]})
		}
	}
	return instructions, items
}

// Generate implements ModelProvider.
func (p *ResponsesProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	instructions, input := toResponsesInput(buildOpenAIMessages(req.Frame, false))
	payload := map[string]any{
		"model":        req.ModelID,
		"instructions": instructions,
		"input":        input,
		"tools":        responsesTools(),
		"store":        false,
	}
	if p.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = p.MaxOutputTokens
	}
	if e := strings.TrimSpace(p.ReasoningEffort); e != "" {
		payload["reasoning"] = map[string]any{"effort": e}
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
	var raw []byte
	for attempt := 1; ; attempt++ {
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		if p.APIKey != "" {
			hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
		for k, v := range p.ExtraHeaders {
			hreq.Header.Set(k, v)
		}
		resp, err := client.Do(hreq)
		if err != nil {
			if ctx.Err() == nil && attempt < 3 {
				if sErr := sleepWithContext(ctx, backoff(attempt)); sErr != nil {
					return nil, sErr
				}
				continue
			}
			return nil, &TransientError{AccountID: req.AccountID, Err: err}
		}
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, &RateLimitError{AccountID: req.AccountID, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), Err: fmt.Errorf("responses 429: %s", strings.TrimSpace(string(raw)))}
		case resp.StatusCode >= 500 && attempt < 3:
			if sErr := sleepWithContext(ctx, backoff(attempt)); sErr != nil {
				return nil, sErr
			}
			continue
		case resp.StatusCode >= 500:
			return nil, &TransientError{AccountID: req.AccountID, StatusCode: resp.StatusCode, Err: fmt.Errorf("responses %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
		default:
			return nil, fmt.Errorf("responses HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
	}
	var parsed struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("responses: decode: %w", err)
	}
	cached := parsed.Usage.InputTokensDetails.CachedTokens
	gen := &GenerateResponse{
		InputTokens:     parsed.Usage.InputTokens - cached,
		CacheReadTokens: cached,
		OutputTokens:    parsed.Usage.OutputTokens,
		LatencyMS:       time.Since(start).Milliseconds(),
		RawEmitted:      raw,
	}
	var text, reasoning strings.Builder
	var calls []wireToolCall
	for _, item := range parsed.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
		case "function_call":
			calls = append(calls, wireToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		case "reasoning":
			for _, s := range item.Summary {
				reasoning.WriteString(s.Text)
			}
		}
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" && parsed.IncompleteDetails != nil {
		return nil, &OutputTruncatedError{MaxTokens: p.MaxOutputTokens, Reason: parsed.IncompleteDetails.Reason}
	}
	if onDelta := deltaSink(ctx, nil); onDelta != nil && text.Len() > 0 {
		onDelta(text.String())
	}
	return finishGenerateResponse(gen, req, text.String(), reasoning.String(), calls), nil
}
