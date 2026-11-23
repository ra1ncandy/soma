package dag_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/vmem"
)

func TestResearchTools_FSGrepRangedReadTimeoutPartialAndBgPollWait(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "seq.fasta"), []byte(">gene1\nACGTACGT\n>gene2\nTTTTGCTA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "binary.bin"), []byte{0x00, 0x01, 'A', 'C', 'G', 'T'}, 0o644); err != nil {
		t.Fatal(err)
	}
	var manyLines strings.Builder
	for i := 1; i <= 25; i++ {
		manyLines.WriteString("line content ")
		manyLines.WriteString(string(rune('A' + (i-1)%26)))
		manyLines.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "data.txt"), []byte(manyLines.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	vm := vmem.NewManager(st)
	runner := dag.NewUnsandboxedToolRunner(vm)

	// 1. fs_grep with ignore_case, context_lines, glob, and binary skip.
	out, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "fs_grep",
		ArgsJSON:     `{"pattern":"ttttgcta","ignore_case":true,"context_lines":1,"glob":"*.fasta"}`,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("fs_grep failed: %v", err)
	}
	got := string(out.Content)
	if !strings.Contains(got, "src/seq.fasta:L4: TTTTGCTA") || !strings.Contains(got, "src/seq.fasta:L3- >gene2") {
		t.Fatalf("unexpected fs_grep output:\n%s", got)
	}
	if strings.Contains(got, "binary.bin") {
		t.Fatalf("fs_grep should skip binary files, got:\n%s", got)
	}

	// 2. Line-ranged fs_read.
	rOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "fs_read",
		ArgsJSON:     `{"path":"src/data.txt","start_line":5,"end_line":8}`,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("ranged fs_read failed: %v", err)
	}
	rText := string(rOut.Content)
	if !strings.Contains(rText, "lines 5..8 of 25") || !strings.Contains(rText, "L5: line content E") || !strings.Contains(rText, "L8: line content H") {
		t.Fatalf("unexpected ranged fs_read output:\n%s", rText)
	}

	// 3. exec with timeout_sec=1 returns partial stdout instead of discarding it.
	eOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "exec",
		ArgsJSON:     `{"command":"echo PARTIAL_MARKER; sleep 5","timeout_sec":1}`,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("exec timeout should return partial output, got err=%v", err)
	}
	eText := string(eOut.Content)
	if !strings.Contains(eText, "TIMEOUT") || !strings.Contains(eText, "PARTIAL_MARKER") {
		t.Fatalf("expected TIMEOUT with PARTIAL_MARKER, got:\n%s", eText)
	}

	// 4. bg_exec + bg_poll with wait_seconds returns promptly once output arrives.
	bgStart, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_bgwait",
		Name:         "bg_exec",
		ArgsJSON:     `{"command":"sleep 0.15; echo BG_READY_42","label":"wait-test"}`,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("bg_exec failed: %v", err)
	}
	startStr := string(bgStart.Content)
	jobIdx := strings.Index(startStr, "bg_")
	if jobIdx < 0 {
		t.Fatalf("missing job id in %q", startStr)
	}
	jobID := strings.Fields(startStr[jobIdx:])[0]

	t0 := time.Now()
	pollOut, err := runner.Execute(ctx, dag.ToolCall{
		RunID:        "run_bgwait",
		Name:         "bg_poll",
		ArgsJSON:     `{"job_id":"` + jobID + `","wait_seconds":3}`,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("bg_poll wait failed: %v", err)
	}
	if !strings.Contains(string(pollOut.Content), "BG_READY_42") {
		t.Fatalf("expected BG_READY_42 after wait_seconds, got:\n%s", string(pollOut.Content))
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("bg_poll wait_seconds did not return early on output/exit (took %s)", time.Since(t0))
	}

	_ = st.CreateRun(ctx, store.Run{ID: "run_bgwait", Title: "t", WorkspacePath: ws, WorkspaceKind: "generic", GoalSpec: "g", Status: "running"})
	// 5. page_grep with context_lines and ignore_case.
	obs, err := vm.Observe(ctx, vmem.ObserveRequest{
		RunID:                "run_bgwait",
		NameHint:             "metrics.jsonl",
		Content:              []byte("{\"step\":1,\"loss\":0.9}\n{\"step\":2,\"loss\":0.4,\"flag\":\"NEEDLE\"}\n{\"step\":3,\"loss\":0.1}\n"),
		InlineThresholdBytes: 10,
	})
	if err != nil || !obs.Paged {
		t.Fatalf("expected paged jsonl observation, got obs=%+v err=%v", obs, err)
	}
	if !strings.Contains(obs.Manifest, "jsonl summary: 3 records") {
		t.Fatalf("expected jsonl structural preview, got:\n%s", obs.Manifest)
	}
	pgOut, err := runner.Execute(ctx, dag.ToolCall{
		Name:         "page_grep",
		ArgsJSON:     `{"page_id":"` + obs.PageID + `","pattern":"needle","ignore_case":true,"context_lines":1}`,
		WorkspaceDir: ws,
	})
	if err != nil {
		t.Fatalf("page_grep failed: %v", err)
	}
	pgText := string(pgOut.Content)
	if !strings.Contains(pgText, "L1-") || !strings.Contains(pgText, "L2:") || !strings.Contains(pgText, "L3-") {
		t.Fatalf("expected context lines L1..L3 in page_grep, got:\n%s", pgText)
	}
}

func TestInterleavedReasoningPassthroughAndExactReplay(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "note.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	_ = st.UpsertAccount(ctx, store.Account{
		ID: "acc_rc", Provider: "opencode-go", ModelID: "deepseek-v4.1-flash", BillingMode: "subscription", MaxConcurrent: 2,
	})

	var turnCount int
	var sawReasoningOnTurn2 bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turnCount++
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if turnCount == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"finish_reason": "tool_calls",
					"message": map[string]any{
						"content":           "",
						"reasoning_content": "Hypothesis H1: inspect note.txt before concluding.",
						"tool_calls": []map[string]any{{
							"id":   "call_rc_1",
							"type": "function",
							"function": map[string]any{
								"name":      "fs_read",
								"arguments": `{"path":"note.txt"}`,
							},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 500, "completion_tokens": 30},
			})
			return
		}
		for _, m := range body.Messages {
			if m["role"] == "assistant" {
				if rc, _ := m["reasoning_content"].(string); strings.Contains(rc, "Hypothesis H1") {
					sawReasoningOnTurn2 = true
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message": map[string]any{
					"content": "Done after reading note.txt.",
				},
			}},
			"usage": map[string]any{"prompt_tokens": 600, "completion_tokens": 15},
		})
	}))
	defer srv.Close()

	prov := dag.NewOpenAICompatProvider(srv.URL+"/v1", "test-key")
	prov.PassReasoningContent = true
	tools := dag.NewLocalToolRunner(vmem.NewManager(st))
	execDag := dag.NewExecutor(st, prov, tools, compiler.WorkerProfile())
	execDag.AccountIDs = []string{"acc_rc"}

	runID, brID, err := execDag.StartRun(ctx, dag.RunRequest{
		Title:         "rc-passthrough",
		WorkspacePath: ws,
		GoalSpec:      "Read note.txt and summarize",
		AccountIDs:    []string{"acc_rc"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	res, err := execDag.ExecuteBranch(ctx, brID, 5)
	if err != nil || res.Status != dag.StatusCompleted {
		t.Fatalf("ExecuteBranch failed: res=%+v err=%v", res, err)
	}
	if !sawReasoningOnTurn2 {
		t.Fatalf("expected turn 1 reasoning_content to be passed through on turn 2 assistant message")
	}

	_ = runID
	rep, err := replay.NewReplayer(st).Replay(ctx, brID, replay.ReplayOptions{Mode: replay.ModeExact})
	if err != nil || rep.Diverged || rep.HashVerifiedCount != rep.StepsChecked {
		t.Fatalf("ModeExact replay failed with reasoning_content: rep=%+v err=%v", rep, err)
	}
}

type slowReadToolRunner struct {
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (s *slowReadToolRunner) Execute(_ context.Context, call dag.ToolCall) (dag.ToolOutput, error) {
	cur := s.inFlight.Add(1)
	for {
		prev := s.maxInFlight.Load()
		if cur <= prev || s.maxInFlight.CompareAndSwap(prev, cur) {
			break
		}
	}
	time.Sleep(60 * time.Millisecond)
	s.inFlight.Add(-1)
	return dag.ToolOutput{NameHint: call.Name + ".txt", Content: []byte("ok:" + call.ArgsJSON), TrustTier: "trusted_repo"}, nil
}

type batchReadProvider struct {
	turn int
}

func (p *batchReadProvider) Generate(_ context.Context, _ dag.GenerateRequest) (*dag.GenerateResponse, error) {
	p.turn++
	if p.turn == 1 {
		return &dag.GenerateResponse{
			Reasoning: "Read 4 files in parallel",
			ToolCalls: []dag.ToolCallSpec{
				{CallID: "c1", Name: "fs_read", ArgsJSON: `{"path":"a"}`, EffectClass: taint.EffectReadOnly},
				{CallID: "c2", Name: "fs_read", ArgsJSON: `{"path":"b"}`, EffectClass: taint.EffectReadOnly},
				{CallID: "c3", Name: "fs_read", ArgsJSON: `{"path":"c"}`, EffectClass: taint.EffectReadOnly},
				{CallID: "c4", Name: "fs_read", ArgsJSON: `{"path":"d"}`, EffectClass: taint.EffectReadOnly},
			},
			StateHash: "batch1",
		}, nil
	}
	return &dag.GenerateResponse{Done: true, FinalSummary: "done", StateHash: "done"}, nil
}

func TestParallelReadOnlyToolBatchExecution(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	_ = st.UpsertAccount(ctx, store.Account{
		ID: "acc_par", Provider: "local", ModelID: "m", BillingMode: "local", MaxConcurrent: 2,
	})

	runner := &slowReadToolRunner{}
	execDag := dag.NewExecutor(st, &batchReadProvider{}, runner, compiler.WorkerProfile())
	execDag.AccountIDs = []string{"acc_par"}

	_, brID, err := execDag.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "parallel read batch",
		AccountIDs:    []string{"acc_par"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	res, err := execDag.ExecuteBranch(ctx, brID, 4)
	if err != nil || res.Status != dag.StatusCompleted {
		t.Fatalf("ExecuteBranch: res=%+v err=%v", res, err)
	}
	if runner.maxInFlight.Load() < 2 {
		t.Fatalf("expected concurrent execution of read-only batch, maxInFlight=%d", runner.maxInFlight.Load())
	}
}
