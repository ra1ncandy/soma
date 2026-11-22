package dag_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/vmem"
)

func TestOpenAICompatProvider_ToolsSessionCacheAnd429(t *testing.T) {
	ctx := context.Background()

	var callNum int
	var lastReqHeaders http.Header
	var lastReqBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNum++
		lastReqHeaders = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&lastReqBody)

		switch callNum {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"reasoning_content": "Search spilled log page for CRC error.",
						"tool_calls": []map[string]any{{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "page_grep",
								"arguments": `{"page_id":"@page_0007","pattern":"SOMA_CRC"}`,
							},
						}},
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     2000,
					"completion_tokens": 45,
					"cost":              0.0012,
					"prompt_tokens_details": map[string]any{
						"cached_tokens": 1500,
					},
					"opencode": map[string]any{
						"tokens": map[string]any{
							"cache_read": 1750,
						},
					},
				},
			})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": "Verified timeout updated cleanly.",
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     2100,
					"completion_tokens": 20,
					"prompt_tokens_details": map[string]any{
						"cached_tokens": 1950,
					},
				},
			})
		default:
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
		}
	}))
	defer ts.Close()

	// Verify tool calling, OpenCode session header detection, and max cache_read extraction.
	prov := dag.NewOpenAICompatProvider(ts.URL+"/opencode.ai/zen/go/v1", "test-secret")
	frame := compiler.CompiledFrame{
		Epoch:  1,
		Layer0: "sys",
		Layer1: "mem",
		Layer2: "log",
		Layer3: "tail",
	}

	resp1, err := prov.Generate(ctx, dag.GenerateRequest{
		RunID:     "run_abc",
		BranchID:  "br_1",
		AccountID: "acc_1",
		ModelID:   "beam-free",
		TurnIndex: 1,
		Frame:     frame,
	})
	if err != nil {
		t.Fatalf("Generate call 1: %v", err)
	}
	if resp1.Done || resp1.ToolCall == nil || resp1.ToolCall.Name != "page_grep" {
		t.Fatalf("expected page_grep tool call, got %+v", resp1)
	}
	if resp1.ToolCall.EffectClass != taint.EffectReadOnly || len(resp1.ToolCall.ReferencedPageIDs) != 1 || resp1.ToolCall.ReferencedPageIDs[0] != "page_0007" {
		t.Fatalf("unexpected tool call metadata: %+v", resp1.ToolCall)
	}
	if resp1.CacheReadTokens != 1750 || resp1.InputTokens != 250 || resp1.CostUSD != 0.0012 {
		t.Fatalf("unexpected usage telemetry: %+v", resp1)
	}
	if gotSess := lastReqHeaders.Get("x-opencode-session"); gotSess != "soma-run_abc-br_1" {
		t.Fatalf("expected sticky x-opencode-session header, got %q", gotSess)
	}
	rawTools, ok := lastReqBody["tools"].([]any)
	if !ok || len(rawTools) != 6 {
		t.Fatalf("expected 6 canonical tools in request body, got %+v", lastReqBody["tools"])
	}

	// Call 2: normal completion without tool_calls -> Done=true.
	resp2, err := prov.Generate(ctx, dag.GenerateRequest{
		RunID:     "run_abc",
		BranchID:  "br_1",
		AccountID: "acc_1",
		ModelID:   "beam-free",
		TurnIndex: 2,
		Frame:     frame,
	})
	if err != nil {
		t.Fatalf("Generate call 2: %v", err)
	}
	if !resp2.Done || resp2.ToolCall != nil || resp2.CacheReadTokens != 1950 {
		t.Fatalf("unexpected completion response: %+v", resp2)
	}

	// Call 3: HTTP 429 -> *RateLimitError with Retry-After=3s.
	_, err = prov.Generate(ctx, dag.GenerateRequest{
		RunID:     "run_abc",
		BranchID:  "br_1",
		AccountID: "acc_1",
		ModelID:   "beam-free",
		TurnIndex: 3,
		Frame:     frame,
	})
	var rlErr *dag.RateLimitError
	if !errors.As(err, &rlErr) || rlErr.RetryAfter != 3*time.Second {
		t.Fatalf("expected RateLimitError with 3s RetryAfter, got %v", err)
	}
}

func TestLocalToolRunner_FSListExecAndAtPagePrefix(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello_soma\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.CreateRun(ctx, store.Run{ID: "run_lt", Title: "lt", WorkspacePath: ws, GoalSpec: "test"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	vm := vmem.NewManager(st)
	obs, err := vm.Observe(ctx, vmem.ObserveRequest{
		RunID:                "run_lt",
		NameHint:             "big.log",
		Content:              []byte(strings.Repeat("ok\n", 100) + "NEEDLE_991\n"),
		TrustTier:            "trusted_repo",
		InlineThresholdBytes: 50,
	})
	if err != nil || !obs.Paged {
		t.Fatalf("Observe failed: %+v err=%v", obs, err)
	}

	runner := dag.NewLocalToolRunner(vm)

	listOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "fs_list",
		ArgsJSON:     `{}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(listOut.Content), "hello.txt") {
		t.Fatalf("fs_list failed: %q err=%v", string(listOut.Content), err)
	}

	execOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "exec",
		ArgsJSON:     `{"command":"cat hello.txt"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(execOut.Content), "hello_soma") {
		t.Fatalf("exec failed: %q err=%v", string(execOut.Content), err)
	}

	grepOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "page_grep",
		ArgsJSON:     `{"page_id":"@` + obs.PageID + `","pattern":"NEEDLE_991"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(grepOut.Content), "NEEDLE_991") {
		t.Fatalf("page_grep with @ prefix failed: %q err=%v", string(grepOut.Content), err)
	}
}
