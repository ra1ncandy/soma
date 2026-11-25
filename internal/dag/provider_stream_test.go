package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
)

func testFrame() compiler.CompiledFrame {
	snap := compiler.NewEpochSnapshot(1, compiler.EpochInput{SystemPrompt: "sys", TaskGoal: "goal"})
	return compiler.Compile(snap, nil, compiler.LiveTailState{}, compiler.WorkerProfile())
}

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
		if fl != nil {
			fl.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func TestProviderStreamsToolCallsAndUsage(t *testing.T) {
	var sawStream atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, _ := body["stream"].(bool); s {
			sawStream.Store(true)
		}
		sse(w,
			`{"choices":[{"delta":{"content":"Let me "}}]}`,
			`{"choices":[{"delta":{"content":"look."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"fs_read","arguments":"{\"pa"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"fs_list","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":60}}}`,
		)
	}))
	defer ts.Close()

	var streamed strings.Builder
	p := NewOpenAICompatProvider(ts.URL, "k")
	p.OnDelta = func(s string) { streamed.WriteString(s) }
	resp, err := p.Generate(context.Background(), GenerateRequest{ModelID: "m", TurnIndex: 1, Frame: testFrame()})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !sawStream.Load() {
		t.Fatal("request did not ask for a streamed response")
	}
	if streamed.String() != "Let me look." || resp.AssistantContent != "Let me look." {
		t.Fatalf("content: streamed=%q assistant=%q", streamed.String(), resp.AssistantContent)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "fs_read" || resp.ToolCalls[0].ArgsJSON != `{"path":"a.txt"}` ||
		resp.ToolCalls[0].CallID != "call_1" || resp.ToolCalls[1].Name != "fs_list" {
		t.Fatalf("tool calls not reassembled: %+v", resp.ToolCalls)
	}
	if resp.CacheReadTokens != 60 || resp.InputTokens != 40 || resp.OutputTokens != 20 {
		t.Fatalf("usage: %+v", resp)
	}
}

func TestProviderStreamStallTimesOut(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)

	p := NewOpenAICompatProvider(ts.URL, "k")
	p.StreamIdleTimeout = 150 * time.Millisecond
	start := time.Now()
	_, err := p.Generate(context.Background(), GenerateRequest{ModelID: "m", TurnIndex: 1, Frame: testFrame()})
	if err == nil {
		t.Fatal("expected an error from a stalled stream")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("stalled stream took %s to fail", time.Since(start))
	}
}

func TestProviderWaitsOutShortRateLimit(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"done"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer ts.Close()

	p := NewOpenAICompatProvider(ts.URL, "k")
	resp, err := p.Generate(context.Background(), GenerateRequest{ModelID: "m", TurnIndex: 1, Frame: testFrame()})
	if err != nil {
		t.Fatalf("a 1s rate limit should be retried in place, got %v", err)
	}
	if !resp.Done || resp.FinalSummary != "done" || calls.Load() != 2 {
		t.Fatalf("resp=%+v calls=%d", resp, calls.Load())
	}
}

func TestCacheControlOnlyForAnthropicFamily(t *testing.T) {
	var bodies []map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cache_write_tokens":4}}}`)
	}))
	defer ts.Close()
	p := NewOpenAICompatProvider(ts.URL, "k")
	for _, model := range []string{"gpt-4.1", "anthropic/claude-sonnet-4.5"} {
		resp, err := p.Generate(context.Background(), GenerateRequest{ModelID: model, TurnIndex: 1, Frame: testFrame()})
		if err != nil {
			t.Fatal(err)
		}
		if resp.CacheWriteTokens != 4 {
			t.Fatalf("cache write tokens not parsed: %+v", resp)
		}
	}
	sysContent := func(b map[string]any) any {
		return b["messages"].([]any)[0].(map[string]any)["content"]
	}
	if _, isString := sysContent(bodies[0]).(string); !isString {
		t.Fatal("non-Anthropic models must keep a plain string system prompt")
	}
	parts, ok := sysContent(bodies[1]).([]any)
	if !ok || parts[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("Anthropic-family system prompt should carry cache_control, got %#v", sysContent(bodies[1]))
	}
}
