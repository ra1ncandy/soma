package dag

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
)

const thinkingTurn = `[{"signature":"SIG-1","thinking":"","type":"thinking"},{"text":"reading","type":"text"},{"id":"toolu_1","input":{"path":"a.go"},"name":"fs_read","type":"tool_use"}]`

func thinkingFrame() compiler.CompiledFrame {
	return compiler.CompiledFrame{
		Layer0: "L0", Layer1: "L1", Structured: true,
		Layer2Entries: []compiler.LogEntry{
			{Kind: "turn", TurnIndex: 1, ToolName: "fs_read", ToolCallID: "toolu_1", ArgsJSON: `{"path":"a.go"}`,
				AssistantContent: "reading", Observation: "package a", ProviderContent: thinkingTurn},
		},
	}
}

// A turn with thinking is captured exactly as received, so it can be sent back unchanged.
func TestAnthropicCapturesThinkingTurnVerbatim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		anthropicSSE(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG-"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"1"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"reading"}}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"fs_read"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\"}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		)
	}))
	defer srv.Close()
	resp, err := NewAnthropicProvider(srv.URL, "k").Generate(context.Background(), GenerateRequest{ModelID: "m", Frame: compiler.CompiledFrame{Layer0: "s", Layer2: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProviderContent != thinkingTurn {
		t.Fatalf("captured content:\n got %s\nwant %s", resp.ProviderContent, thinkingTurn)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "fs_read" || resp.AssistantContent != "reading" {
		t.Fatalf("the usual fields must still be filled: %+v", resp)
	}
}

// On the next turn the captured content goes back unchanged, with the request asking the API to
// drop (not reject) blocks whose earlier conversation changed. The fallbacks cover an API that
// refuses the control and one that refuses a block.
func TestAnthropicReplaysThinkingAndFallsBack(t *testing.T) {
	t.Setenv("SOMA_REASONING_EFFORT", "provider")
	type seen struct {
		beta        string
		thinking    any
		firstBlocks string
	}
	mode := ""
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Thinking any `json:"thinking"`
			Messages []struct {
				Role    string            `json:"role"`
				Content []json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		s := seen{beta: r.Header.Get("anthropic-beta"), thinking: body.Thinking}
		for _, m := range body.Messages {
			if m.Role == "assistant" {
				raw, _ := json.Marshal(m.Content)
				s.firstBlocks = string(raw)
				break
			}
		}
		got = append(got, s)
		hasThinkingBlock := strings.Contains(s.firstBlocks, `"type":"thinking"`)
		switch {
		case mode == "no-controls" && body.Thinking != nil:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.block_binding: Extra inputs are not permitted"}}`)
			return
		case mode == "bad-signature" && hasThinkingBlock:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages.1.content.0: Invalid `signature` in `thinking` block.\"}}")
			return
		}
		anthropicSSE(w,
			`{"type":"message_start","message":{"usage":{"input_tokens":5},"input_transformations":[{"type":"thinking_dropped","reason":"prefix_binding_mismatch"}]}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		)
	}))
	defer srv.Close()
	call := func(p *AnthropicProvider) *GenerateResponse {
		t.Helper()
		resp, err := p.Generate(context.Background(), GenerateRequest{ModelID: "m", Frame: thinkingFrame()})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		return resp
	}

	resp := call(NewAnthropicProvider(srv.URL, "k"))
	if len(got) != 1 || got[0].beta != thinkingBindingBeta || got[0].firstBlocks != thinkingTurn {
		t.Fatalf("replay request wrong: %+v", got)
	}
	if th, _ := got[0].thinking.(map[string]any); th == nil || th["type"] != "adaptive" {
		t.Fatalf("thinking controls missing: %+v", got[0].thinking)
	}
	if resp.ThinkingDropped != 1 {
		t.Fatalf("dropped blocks reported by the API must be counted, got %d", resp.ThinkingDropped)
	}

	mode, got = "no-controls", nil
	p := NewAnthropicProvider(srv.URL, "k")
	call(p)
	call(p)
	if len(got) != 3 || got[0].thinking == nil || got[1].thinking != nil || got[1].beta != "" || got[2].thinking != nil {
		t.Fatalf("an API without the control must be retried without it, and remembered: %+v", got)
	}
	if got[1].firstBlocks != thinkingTurn {
		t.Fatalf("the blocks are still replayed without the control: %s", got[1].firstBlocks)
	}

	mode, got = "bad-signature", nil
	call(NewAnthropicProvider(srv.URL, "k"))
	if len(got) != 2 || !strings.Contains(got[0].firstBlocks, `"thinking"`) || strings.Contains(got[1].firstBlocks, `"thinking"`) {
		t.Fatalf("a refused block must be retried without the thinking blocks: %+v", got)
	}
	if !strings.Contains(got[1].firstBlocks, `"tool_use"`) {
		t.Fatalf("the retry keeps the turn's text and tool calls: %s", got[1].firstBlocks)
	}

	mode, got = "", nil
	off := NewAnthropicProvider(srv.URL, "k")
	off.NoThinkingReplay = true
	call(off)
	if strings.Contains(got[0].firstBlocks, `"thinking"`) || got[0].beta != "" {
		t.Fatalf("NoThinkingReplay must send no thinking blocks: %+v", got[0])
	}
}

// Thinking blocks are only valid after the conversation that produced them, so an epoch rollover
// (new system prompt, masked history) must not carry them over.
func TestRolloverDropsProviderContent(t *testing.T) {
	prev := compiler.NewEpochSnapshot(1, compiler.EpochInput{TaskGoal: "x"})
	l2 := []compiler.LogEntry{{Kind: "turn", TurnIndex: 1, ToolName: "fs_read", ProviderContent: thinkingTurn, Observation: "o"}}
	_, skeleton := compiler.Rollover(prev, l2, nil, nil, compiler.EpochInput{TaskGoal: "x"})
	for _, e := range skeleton {
		if e.ProviderContent != "" {
			t.Fatalf("rollover kept provider content: %+v", e)
		}
	}
}
