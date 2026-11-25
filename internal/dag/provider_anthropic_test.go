package dag

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
)

func anthropicTestFrame() compiler.CompiledFrame {
	return compiler.CompiledFrame{
		Layer0:     "L0 kernel",
		Layer1:     "L1 registers",
		Structured: true,
		Layer2Entries: []compiler.LogEntry{
			{Kind: "turn", TurnIndex: 1, ToolName: "fs_read", ToolCallID: "toolu_1", ArgsJSON: `{"path":"a.go"}`, AssistantContent: "reading", Observation: "package a"},
		},
	}
}

func TestAnthropicProviderRequestShapeAndStreamedToolUse(t *testing.T) {
	var got map[string]any
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":900,"cache_creation_input_tokens":40}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me "}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"grep."}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"fs_grep"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"TODO\"}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":33}}`,
			`{"type":"message_stop"}`,
		}
		for _, e := range events {
			_, _ = io.WriteString(w, "event: x\ndata: "+e+"\n\n")
		}
	}))
	defer srv.Close()

	var streamed strings.Builder
	p := NewAnthropicProvider(srv.URL, "sk-test")
	p.OnDelta = func(s string) { streamed.WriteString(s) }
	resp, err := p.Generate(context.Background(), GenerateRequest{ModelID: "claude-x", TurnIndex: 2, Frame: anthropicTestFrame()})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if headers.Get("x-api-key") != "sk-test" || headers.Get("anthropic-version") == "" {
		t.Fatalf("auth headers wrong: %v", headers)
	}
	sys := got["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("system prompt must be a cached block: %v", got["system"])
	}
	tools := got["tools"].([]any)
	if tools[0].(map[string]any)["input_schema"] == nil || tools[len(tools)-1].(map[string]any)["cache_control"] == nil {
		t.Fatal("tools must use input_schema and end with a cache breakpoint")
	}
	msgs := got["messages"].([]any)
	for i := 1; i < len(msgs); i++ {
		if msgs[i].(map[string]any)["role"] == msgs[i-1].(map[string]any)["role"] {
			t.Fatalf("roles must alternate: %v", msgs)
		}
	}
	raw, _ := json.Marshal(msgs)
	for _, want := range []string{`"type":"tool_use"`, `"id":"toolu_1"`, `"type":"tool_result"`, `"tool_use_id":"toolu_1"`, `"path":"a.go"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("messages missing %s: %s", want, raw)
		}
	}
	if resp.Done || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "fs_grep" || resp.ToolCalls[0].ArgsJSON != `{"pattern":"TODO"}` || resp.ToolCalls[0].CallID != "toolu_2" {
		t.Fatalf("tool call parsed wrong: %+v", resp.ToolCalls)
	}
	if resp.InputTokens != 12 || resp.CacheReadTokens != 900 || resp.CacheWriteTokens != 40 || resp.OutputTokens != 33 {
		t.Fatalf("usage wrong: %+v", resp)
	}
	if streamed.String() != "Let me grep." || resp.AssistantContent != "Let me grep." {
		t.Fatalf("text: streamed=%q content=%q", streamed.String(), resp.AssistantContent)
	}
}

func TestAnthropicProviderRateLimitAndOverload(t *testing.T) {
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"type":"error"}`)
	}))
	defer srv.Close()
	p := NewAnthropicProvider(srv.URL, "k")
	_, err := p.Generate(context.Background(), GenerateRequest{ModelID: "m", Frame: anthropicTestFrame()})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter.Seconds() != 120 {
		t.Fatalf("want RateLimitError(120s), got %v", err)
	}
	status = 529
	_, err = p.Generate(context.Background(), GenerateRequest{ModelID: "m", Frame: anthropicTestFrame()})
	var tr *TransientError
	if !errors.As(err, &tr) {
		t.Fatalf("want TransientError for 529, got %v", err)
	}
}
