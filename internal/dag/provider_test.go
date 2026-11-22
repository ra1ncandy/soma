package dag_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/replay"
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
	if gotSess := lastReqHeaders.Get("x-opencode-session"); gotSess != "soma-run_abc" {
		t.Fatalf("expected sticky run-scoped x-opencode-session header, got %q", gotSess)
	}
	rawTools, ok := lastReqBody["tools"].([]any)
	if !ok || len(rawTools) != 16 {
		t.Fatalf("expected 16 canonical tools in request body, got %+v", lastReqBody["tools"])
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

	runner := dag.NewUnsandboxedToolRunner(vm)

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

	pageSearchOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "page_search",
		ArgsJSON:     `{"query":"NEEDLE_991"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(pageSearchOut.Content), "NEEDLE_991") {
		t.Fatalf("page_search failed: %q err=%v", string(pageSearchOut.Content), err)
	}

	memSvc := memory.NewService(st)
	for i := 1; i <= 15; i++ {
		_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
			ID:      "M-noise-" + string(rune('a'+i)),
			Scope:   "global",
			Kind:    "rule",
			Content: "Unrelated frontend CSS padding convention rule",
		})
	}
	_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
		ID:      "M-needle-rule",
		Scope:   "global",
		Kind:    "rule",
		Content: "When NEEDLE_991 appears in big.log, set timeout_ms=4250 in config/timeout.yaml",
	})

	memSearchOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "memory_search",
		ArgsJSON:     `{"query":"NEEDLE_991 timeout_ms config"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(memSearchOut.Content), "M-needle-rule") {
		t.Fatalf("memory_search failed: %q err=%v", string(memSearchOut.Content), err)
	}
}

func TestOpenAICompatProvider_MultiToolBatchAndTransientRetry(t *testing.T) {
	ctx := context.Background()

	var attempts int
	var capturedMsgs []any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var reqBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		if msgs, ok := reqBody["messages"].([]any); ok {
			capturedMsgs = msgs
		}

		switch attempts {
		case 1:
			// Simulate transient upstream 502 gateway hiccup on first attempt.
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"bad gateway"}`))
		case 2:
			// Retry succeeds and emits TWO tool calls in a single turn.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": "Write both a.txt and b.txt in one batched turn.",
						"tool_calls": []map[string]any{
							{
								"id":   "call_a",
								"type": "function",
								"function": map[string]any{
									"name":      "fs_write",
									"arguments": `{"path":"a.txt","content":"alpha"}`,
								},
							},
							{
								"id":   "call_b",
								"type": "function",
								"function": map[string]any{
									"name":      "fs_write",
									"arguments": `{"path":"b.txt","content":"beta"}`,
								},
							},
						},
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     1200,
					"completion_tokens": 50,
					"prompt_tokens_details": map[string]any{
						"cached_tokens": 1000,
					},
				},
			})
		default:
			// Turn 2: finish run.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": "Both files written and verified in 2 turns.",
					},
				}},
				"usage": map[string]any{
					"prompt_tokens":     1350,
					"completion_tokens": 25,
					"prompt_tokens_details": map[string]any{
						"cached_tokens": 1200,
					},
				},
			})
		}
	}))
	defer ts.Close()

	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	_ = st.UpsertAccount(ctx, store.Account{
		ID: "batch_acc", Provider: "openai", ModelID: "beam-free", BillingMode: "subscription", MaxConcurrent: 1,
	})

	prov := dag.NewOpenAICompatProvider(ts.URL, "secret")
	execDag := dag.NewExecutor(st, prov, nil, compiler.WorkerProfile())
	execDag.AccountIDs = []string{"batch_acc"}

	_, brID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         "batch-test",
		WorkspacePath: ws,
		GoalSpec:      "Create a.txt and b.txt",
		AccountIDs:    []string{"batch_acc"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	res, err := execDag.ExecuteBranch(ctx, brID, 5)
	if err != nil {
		t.Fatalf("ExecuteBranch: %v", err)
	}
	if res.Status != dag.StatusCompleted || res.TurnsExecuted != 2 {
		t.Fatalf("expected completion in 2 turns, got status=%s turns=%d", res.Status, res.TurnsExecuted)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 HTTP requests (1 transient 502 + 2 successful turns), got %d", attempts)
	}
	if len(capturedMsgs) != 5 {
		t.Fatalf("expected 5 multi-turn messages (system, user, assistant[2 tool_calls], tool[a], tool[b]), got %d messages", len(capturedMsgs))
	}

	br, err := st.GetBranch(ctx, brID)
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	aBytes, aErr := os.ReadFile(filepath.Join(br.WorkspaceDir, "a.txt"))
	bBytes, bErr := os.ReadFile(filepath.Join(br.WorkspaceDir, "b.txt"))
	if aErr != nil || bErr != nil || string(aBytes) != "alpha" || string(bBytes) != "beta" {
		t.Fatalf("expected both batched files written in workspace, got a=%q (%v) b=%q (%v)", string(aBytes), aErr, string(bBytes), bErr)
	}
}

func TestEpochPinnedHybridLayer1MemorySelection(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_ = st.UpsertAccount(ctx, store.Account{
		ID: "mem_acc", Provider: "local", ModelID: "mock", BillingMode: "local", MaxConcurrent: 1,
	})

	memSvc := memory.NewService(st)
	// Seed 20 unrelated rules and 1 highly relevant rule.
	for i := 1; i <= 20; i++ {
		_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
			ID:      "M-unrel-" + string(rune('a'+i)),
			Scope:   "global",
			Kind:    "rule",
			Content: "Unrelated CSS animation keyframe styling rule",
		})
	}
	_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
		ID:      "M-watchdog-target",
		Scope:   "global",
		Kind:    "rule",
		Content: "For SOMA_CRC_7741 rpc_watchdog timeout failures, set timeout_ms=4250 in config/timeout.yaml",
	})
	_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
		ID:      "M-watchdog-rel1",
		Scope:   "global",
		Kind:    "rule",
		Content: "Always validate config/timeout.yaml YAML syntax after updating timeout_ms",
	})
	_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
		ID:      "M-watchdog-rel2",
		Scope:   "global",
		Kind:    "rule",
		Content: "Check rpc_watchdog logs when diagnosing timeout_ms errors",
	})
	_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
		ID:      "M-watchdog-rel3",
		Scope:   "global",
		Kind:    "rule",
		Content: "Timeout configuration lives in config/timeout.yaml",
	})

	execDag := dag.NewExecutor(st, &dag.MockProvider{ModelName: "mock"}, nil, compiler.WorkerProfile())
	execDag.AccountIDs = []string{"mem_acc"}
	execDag.MaxLayer1Memories = 3

	_, brID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         "epoch-mem-budget",
		WorkspacePath: ws,
		GoalSpec:      "Diagnose SOMA_CRC_7741 rpc_watchdog timeout_ms in config/timeout.yaml",
		AccountIDs:    []string{"mem_acc"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	res, err := execDag.ExecuteBranch(ctx, brID, 5)
	if err != nil || res.Status != dag.StatusCompleted {
		t.Fatalf("ExecuteBranch: res=%+v err=%v", res, err)
	}

	steps, err := st.ListBranchSteps(ctx, brID)
	if err != nil || len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %d err=%v", len(steps), err)
	}

	var prevPrefix string
	for _, s := range steps {
		frame, rErr := compiler.ReconstructFrame(ctx, st, s.ID, compiler.WorkerProfile())
		if rErr != nil {
			t.Fatalf("ReconstructFrame(%s): %v", s.ID, rErr)
		}
		if !strings.Contains(frame.Layer1, "M-watchdog-target") {
			t.Fatalf("expected M-watchdog-target selected into Layer 1, got:\n%s", frame.Layer1)
		}
		if strings.Count(frame.Layer1, "[M-") != 3 {
			t.Fatalf("expected exactly MaxLayer1Memories=3 rules in Layer 1, got:\n%s", frame.Layer1)
		}
		if prevPrefix != "" && frame.PrefixHashL0L1 != prevPrefix {
			t.Fatalf("PrefixHashL0L1 mutated mid-epoch!")
		}
		prevPrefix = frame.PrefixHashL0L1
	}
}

func TestFSPatchBgProcsAndWebMultimodalTools(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	_ = st.CreateRun(ctx, store.Run{ID: "run_tools", Title: "tools", WorkspacePath: ws, GoalSpec: "test tools"})

	vm := vmem.NewManager(st)
	runner := dag.NewUnsandboxedToolRunner(vm)

	// 1. Multi-hunk fs_patch with fuzzy indentation tolerance + unified diff + atomic rollback.
	initialPy := "def alpha():\n    x = 1\n    return x\n\ndef beta():\n    y = 2\n    return y\n"
	pyPath := filepath.Join(ws, "app.py")
	if err := os.WriteFile(pyPath, []byte(initialPy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	patchBlock := "<<<<<<< SEARCH\nx = 1\nreturn x\n=======\nx = 10\nreturn x * 2\n>>>>>>> REPLACE\n<<<<<<< SEARCH\n    y = 2\n=======\n    y = 42\n>>>>>>> REPLACE"
	_, err = runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "fs_patch",
		ArgsJSON:     `{"path":"app.py","patch":` + mustJSONQuote(patchBlock) + `}`,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("fs_patch multi-hunk fuzzy: %v", err)
	}
	patchedPy, _ := os.ReadFile(pyPath)
	if !strings.Contains(string(patchedPy), "    x = 10\n    return x * 2") || !strings.Contains(string(patchedPy), "    y = 42") {
		t.Fatalf("unexpected patched app.py:\n%s", string(patchedPy))
	}

	// Verify atomic rollback when 2nd hunk fails.
	badPatch := "<<<<<<< SEARCH\n    y = 42\n=======\n    y = 999\n>>>>>>> REPLACE\n<<<<<<< SEARCH\nNON_EXISTENT_LINE\n=======\nfoo\n>>>>>>> REPLACE"
	if _, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "fs_patch",
		ArgsJSON:     `{"path":"app.py","patch":` + mustJSONQuote(badPatch) + `}`,
		WorkspaceDir: ws,
	}); err == nil {
		t.Fatal("expected error when 2nd hunk fails")
	}
	afterFailPy, _ := os.ReadFile(pyPath)
	if strings.Contains(string(afterFailPy), "999") {
		t.Fatalf("fs_patch failed to roll back atomically! got:\n%s", string(afterFailPy))
	}

	// 2. Background process management (bg_exec -> bg_poll -> bg_kill).
	startOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "bg_exec",
		ArgsJSON:     `{"command":"echo BG_READY_77 && sleep 10","label":"daemon-sim"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(startOut.Content), "bg_0001") {
		t.Fatalf("bg_exec failed: %s err=%v", string(startOut.Content), err)
	}
	time.Sleep(120 * time.Millisecond)

	pollOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "bg_poll",
		ArgsJSON:     `{"job_id":"bg_0001"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(pollOut.Content), "BG_READY_77") {
		t.Fatalf("bg_poll failed: %s err=%v", string(pollOut.Content), err)
	}

	killOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "bg_kill",
		ArgsJSON:     `{"job_id":"bg_0001"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(killOut.Content), "exited") {
		t.Fatalf("bg_kill failed: %s err=%v", string(killOut.Content), err)
	}

	// 3. Web search, Web extract (with untrusted_external provenance taint), and Vision analyze.
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]string{
					{"title": "Soma Kernel Docs", "url": "https://example.org/soma", "snippet": "Monotonic 4-layer KV cache kernel"},
				},
			})
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><script>alert(1)</script></head><body><h1>Architecture</h1><p>Content-addressed @page virtual memory.</p></body></html>`))
		case "/v1/chat/completions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]string{"content": "Detected 32x16 test chart PNG with blue header."},
				}},
			})
		}
	}))
	defer webSrv.Close()

	runner.HTTPClient = webSrv.Client()
	runner.SearchEndpoint = webSrv.URL + "/search"
	runner.VisionBaseURL = webSrv.URL + "/v1"

	wsOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "web_search",
		ArgsJSON:     `{"query":"soma kernel"}`,
		WorkspaceDir: ws,
	})
	if err != nil || wsOut.TrustTier != "untrusted_external" || !strings.Contains(string(wsOut.Content), "Soma Kernel Docs") {
		t.Fatalf("web_search unexpected output: %+v err=%v", wsOut, err)
	}

	weOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "web_extract",
		ArgsJSON:     `{"url":"` + webSrv.URL + `/page"}`,
		WorkspaceDir: ws,
	})
	if err != nil || weOut.TrustTier != "untrusted_external" || strings.Contains(string(weOut.Content), "alert(1)") || !strings.Contains(string(weOut.Content), "Content-addressed @page") {
		t.Fatalf("web_extract unexpected output: %+v err=%v", weOut, err)
	}

	// Create a real 32x16 PNG image in workspace and run vision_analyze.
	img := image.NewRGBA(image.Rect(0, 0, 32, 16))
	img.Set(0, 0, color.RGBA{R: 10, G: 20, B: 200, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "chart.png"), pngBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile chart.png: %v", err)
	}

	visOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_tools",
		Name:         "vision_analyze",
		ArgsJSON:     `{"image_path":"chart.png","prompt":"Inspect chart dimensions and content"}`,
		WorkspaceDir: ws,
	})
	if err != nil || !strings.Contains(string(visOut.Content), "dimensions=32x16") || !strings.Contains(string(visOut.Content), "Detected 32x16 test chart PNG") {
		t.Fatalf("vision_analyze unexpected output: %s err=%v", string(visOut.Content), err)
	}
}

func mustJSONQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestOpenAICompatProvider_MultiTurnPrefixMonotonicity(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("alpha_content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("beta_content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(ctx, store.Account{
		ID: "acc_mono", Provider: "openai", ModelID: "gpt-mono", BillingMode: "subscription", MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	var ex *dag.Executor
	var branchID string
	var recorded [][]json.RawMessage
	var turnNum int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		recorded = append(recorded, body.Messages)
		turnNum++

		switch turnNum {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": "",
						"tool_calls": []map[string]any{{
							"id":   "call_t1_0",
							"type": "function",
							"function": map[string]any{
								"name":      "fs_list",
								"arguments": `{}`,
							},
						}},
					},
				}},
			})
		case 2:
			// Inject a non-tool blackboard rule_notice so Turn 3 tests non-tool user message alignment too.
			_, _ = ex.Blackboard.Publish(context.Background(), st, dag.PeerFinding{
				ID:         "notice_t2",
				RunID:      "run_mono",
				FromBranch: "coordinator",
				FromLabel:  "coord",
				TargetBranch: branchID,
				Kind:       "rule_notice",
				Summary:    "Check both alpha and beta contents",
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": "Reading a.txt and b.txt in parallel.",
						"tool_calls": []map[string]any{
							{
								"id":   "call_t2_a",
								"type": "function",
								"function": map[string]any{
									"name":      "fs_read",
									"arguments": `{"path":"a.txt"}`,
								},
							},
							{
								"id":   "call_t2_b",
								"type": "function",
								"function": map[string]any{
									"name":      "fs_read",
									"arguments": `{"path":"b.txt"}`,
								},
							},
						},
					},
				}},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"content": "Verified alpha_content and beta_content.",
					},
				}},
			})
		}
	}))
	defer srv.Close()

	prov := dag.NewOpenAICompatProvider(srv.URL+"/v1", "test-key")
	ex = dag.NewExecutor(st, prov, nil, compiler.WorkerProfile())
	ex.AccountIDs = []string{"acc_mono"}

	runID, brID, err := ex.StartRun(ctx, dag.RunRequest{
		RunID:         "run_mono",
		WorkspacePath: ws,
		GoalSpec:      "Verify multi-turn wire prefix monotonicity",
		AccountIDs:    []string{"acc_mono"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	branchID = brID

	res, err := ex.ExecuteBranch(ctx, branchID, 6)
	if err != nil || res.Status != dag.StatusCompleted {
		t.Fatalf("ExecuteBranch failed: res=%+v err=%v", res, err)
	}
	if len(recorded) != 3 {
		t.Fatalf("expected 3 HTTP turns, got %d", len(recorded))
	}

	extractRole := func(raw json.RawMessage) string {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		r, _ := m["role"].(string)
		return r
	}

	// Assert message counts, roles, and zero consecutive user messages on every turn.
	if len(recorded[0]) != 2 || extractRole(recorded[0][0]) != "system" || extractRole(recorded[0][1]) != "user" {
		t.Fatalf("turn 1 unexpected roles: len=%d", len(recorded[0]))
	}
	if len(recorded[1]) != 4 {
		t.Fatalf("turn 2 expected 4 messages (system, user, assistant, tool), got %d", len(recorded[1]))
	}
	if len(recorded[2]) != 8 {
		t.Fatalf("turn 3 expected 8 messages (system, user, asst, tool, asst, tool, tool, user notice), got %d", len(recorded[2]))
	}
	for tIdx, msgs := range recorded {
		for i := 1; i < len(msgs); i++ {
			if extractRole(msgs[i-1]) == "user" && extractRole(msgs[i]) == "user" {
				t.Fatalf("turn %d produced consecutive user messages at indices %d,%d", tIdx+1, i-1, i)
			}
		}
	}

	// Assert exact byte-for-byte prefix monotonicity across turns.
	for i := 0; i < len(recorded[0]); i++ {
		if !bytes.Equal(recorded[0][i], recorded[1][i]) {
			t.Fatalf("turn 2 mutated message[%d] prefix:\nt1=%s\nt2=%s", i, recorded[0][i], recorded[1][i])
		}
	}
	for i := 0; i < len(recorded[1]); i++ {
		if !bytes.Equal(recorded[1][i], recorded[2][i]) {
			t.Fatalf("turn 3 mutated message[%d] prefix:\nt2=%s\nt3=%s", i, recorded[1][i], recorded[2][i])
		}
	}

	// Verify ModeExact replay still passes 100% after structured wire fields.
	rep, err := replay.NewReplayer(st).Replay(ctx, branchID, replay.ReplayOptions{Mode: replay.ModeExact})
	if err != nil || rep.Diverged || rep.StepsChecked == 0 || rep.HashVerifiedCount != rep.StepsChecked {
		t.Fatalf("ModeExact replay failed on run %s branch %s: rep=%+v err=%v", runID, branchID, rep, err)
	}
}
