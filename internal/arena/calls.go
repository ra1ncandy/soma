package arena

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// CallRecord summarises one metered model call: what the contestant asked for and where the
// output tokens went. It holds sizes and settings only, never prompt or completion text.
type CallRecord struct {
	Seq       int   `json:"seq"`
	Status    int   `json:"status"`
	StartMS   int64 `json:"start_ms"` // when the request arrived, since the attempt began
	LatencyMS int64 `json:"latency_ms"`

	// Request.
	Params                 map[string]any `json:"params,omitempty"` // every top-level field except the conversation and tool schemas
	Messages               int            `json:"messages"`
	Tools                  int            `json:"tools"`
	RequestBytes           int            `json:"request_bytes"`
	SystemChars            int            `json:"system_chars"`
	ToolSchemaChars        int            `json:"tool_schema_chars"`
	ReplayedReasoningChars int            `json:"replayed_reasoning_chars"`    // reasoning the contestant sent back from earlier turns
	CacheBreakpoints       int            `json:"cache_breakpoints,omitempty"` // explicit cache_control markers in the request

	// Response.
	InputTokens     int      `json:"input_tokens"`
	CachedTokens    int      `json:"cached_tokens"`
	OutputTokens    int      `json:"output_tokens"`
	ReasoningTokens int      `json:"reasoning_tokens,omitempty"` // as reported by the provider, when it reports them
	ReasoningChars  int      `json:"reasoning_chars"`
	ContentChars    int      `json:"content_chars"`
	ToolArgChars    int      `json:"tool_arg_chars"`
	ToolNames       []string `json:"tool_names,omitempty"`
	FinishReason    string   `json:"finish_reason,omitempty"`
	// ThinkingDropped counts replayed thinking blocks the API reported discarding on this call.
	ThinkingDropped int `json:"thinking_dropped,omitempty"`
}

func summarizeRequest(body []byte) *CallRecord {
	rec := &CallRecord{RequestBytes: len(body), CacheBreakpoints: bytes.Count(body, []byte(`"cache_control"`))}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return rec
	}
	params := map[string]any{}
	for k, raw := range top {
		switch k {
		case "messages", "input":
			var msgs []map[string]json.RawMessage
			if json.Unmarshal(raw, &msgs) != nil {
				continue
			}
			rec.Messages = len(msgs)
			for _, m := range msgs {
				var role string
				_ = json.Unmarshal(m["role"], &role)
				if role == "system" || role == "developer" {
					rec.SystemChars += len(m["content"])
				}
				for _, key := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
					if v := m[key]; len(v) > 2 && string(v) != "null" {
						rec.ReplayedReasoningChars += len(v)
					}
				}
				// Anthropic Messages: reasoning comes back as thinking blocks inside content.
				var blocks []map[string]json.RawMessage
				if json.Unmarshal(m["content"], &blocks) == nil {
					for _, b := range blocks {
						var typ string
						_ = json.Unmarshal(b["type"], &typ)
						if typ == "thinking" || typ == "redacted_thinking" {
							rec.ReplayedReasoningChars += len(b["thinking"]) + len(b["signature"]) + len(b["data"])
						}
					}
				}
			}
		case "tools":
			var tools []json.RawMessage
			if json.Unmarshal(raw, &tools) == nil {
				rec.Tools = len(tools)
			}
			rec.ToolSchemaChars = len(raw)
		case "system", "instructions":
			rec.SystemChars += len(raw)
		default:
			var v any
			if json.Unmarshal(raw, &v) == nil {
				params[k] = v
			}
		}
	}
	if len(params) > 0 {
		rec.Params = params
	}
	return rec
}

type callMessage struct {
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	ToolCalls        []struct {
		Index    int `json:"index"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type callChunk struct {
	// Anthropic Messages stream events.
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	Choices []struct {
		Message      *callMessage `json:"message"`
		Delta        *callMessage `json:"delta"`
		FinishReason string       `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// summarizeResponse fills the response half of rec from a chat-completions body (JSON or SSE).
func summarizeResponse(rec *CallRecord, body []byte, sse bool) {
	rec.ThinkingDropped = bytes.Count(body, []byte(`"thinking_dropped"`))
	names := map[int]string{}
	apply := func(c callChunk) {
		if c.Usage != nil && c.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
			rec.ReasoningTokens = c.Usage.CompletionTokensDetails.ReasoningTokens
		}
		switch c.Type {
		case "content_block_start":
			if c.ContentBlock != nil && c.ContentBlock.Type == "tool_use" {
				names[c.Index] = c.ContentBlock.Name
			}
		case "content_block_delta":
			if c.Delta != nil {
				rec.ContentChars += len(c.Delta.Text)
				rec.ReasoningChars += len(c.Delta.Thinking)
				rec.ToolArgChars += len(c.Delta.PartialJSON)
			}
		case "message_delta":
			if c.Delta != nil && c.Delta.StopReason != "" {
				rec.FinishReason = c.Delta.StopReason
			}
		}
		for _, ch := range c.Choices {
			if ch.FinishReason != "" {
				rec.FinishReason = ch.FinishReason
			}
			m := ch.Message
			if m == nil {
				m = ch.Delta
			}
			if m == nil {
				continue
			}
			var text string
			if json.Unmarshal(m.Content, &text) == nil {
				rec.ContentChars += len(text)
			}
			rec.ReasoningChars += len(m.ReasoningContent)
			if m.ReasoningContent == "" {
				rec.ReasoningChars += len(m.Reasoning)
			}
			for i, tc := range m.ToolCalls {
				idx := tc.Index
				if ch.Message != nil {
					idx = i
				}
				if tc.Function.Name != "" {
					names[idx] = tc.Function.Name
				}
				rec.ToolArgChars += len(tc.Function.Arguments)
			}
		}
	}
	if !sse {
		var c callChunk
		if json.Unmarshal(body, &c) == nil {
			apply(c)
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var c callChunk
			if json.Unmarshal([]byte(data), &c) == nil {
				apply(c)
			}
		}
	}
	idxs := make([]int, 0, len(names))
	for i := range names {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		rec.ToolNames = append(rec.ToolNames, names[i])
	}
}
