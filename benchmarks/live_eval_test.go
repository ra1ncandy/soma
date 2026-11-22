package benchmarks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

type liveConfig struct {
	BaseURL string
	APIKey  string
	ModelID string
}

func loadLiveConfig(t *testing.T) liveConfig {
	t.Helper()
	apiKey := strings.TrimSpace(os.Getenv("SOMA_LIVE_API_KEY"))
	if apiKey == "" {
		t.Skip("SOMA_LIVE_API_KEY not set; skipping live LLM hypothesis evaluation")
	}
	baseURL := strings.TrimSpace(os.Getenv("SOMA_LIVE_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://opencode.ai/zen/go/v1"
	}
	model := strings.TrimSpace(os.Getenv("SOMA_LIVE_MODEL"))
	if model == "" {
		model = "beam-free"
	}
	return liveConfig{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		ModelID: model,
	}
}

type abRunMetrics struct {
	System          string
	Passed          bool
	PassDetail      string
	Turns           int
	PromptTokens    int
	CachedTokens    int
	OutputTokens    int
	CacheHitPct     float64
	Compactions     int
	SummaryFailures int
	Branches        int
	GraveyardNodes  int
	ReplayVerified  bool
	WallMS          int64
	LastSummary     string
	ErrorText       string
}

func (m abRunMetrics) Format() string {
	return fmt.Sprintf("%-8s pass=%-5v turns=%2d prompt=%7d cached=%7d (%5.1f%%) out=%5d compactions=%d branches=%d graveyard=%d replay=%v wall=%dms detail=%s",
		m.System, m.Passed, m.Turns, m.PromptTokens, m.CachedTokens, m.CacheHitPct, m.OutputTokens,
		m.Compactions, m.Branches, m.GraveyardNodes, m.ReplayVerified, m.WallMS, m.PassDetail)
}

// extractAssistantText returns choices[0].message.content from a chat-completions response.
func extractAssistantText(out map[string]any) string {
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return strings.TrimSpace(content)
}

// liveTrial is one raw A/B trial, appended as a JSON line to SOMA_LIVE_OUT when that is set.
type liveTrial struct {
	Experiment string       `json:"experiment"`
	Trial      int          `json:"trial"`
	Model      string       `json:"model"`
	At         string       `json:"at"`
	Linear     abRunMetrics `json:"linear"`
	Soma       abRunMetrics `json:"soma"`
}

func liveRepeats() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("SOMA_LIVE_REPEATS"))); err == nil && n > 0 {
		return n
	}
	return 1
}

// runTrials runs one experiment SOMA_LIVE_REPEATS times, logs every trial, appends raw results to
// SOMA_LIVE_OUT, and reports pass counts. A single trial is an anecdote, not a pass rate.
func runTrials(t *testing.T, cfg liveConfig, name string, trial func(t *testing.T) (abRunMetrics, abRunMetrics)) {
	t.Run(name, func(t *testing.T) {
		n := liveRepeats()
		linPass, somaPass := 0, 0
		for i := 1; i <= n; i++ {
			var lin, soma abRunMetrics
			t.Run(fmt.Sprintf("trial_%02d", i), func(t *testing.T) {
				lin, soma = trial(t)
			})
			if lin.Passed {
				linPass++
			}
			if soma.Passed {
				somaPass++
			}
			if outPath := strings.TrimSpace(os.Getenv("SOMA_LIVE_OUT")); outPath != "" {
				line, _ := json.Marshal(liveTrial{
					Experiment: name, Trial: i, Model: cfg.ModelID,
					At: time.Now().UTC().Format(time.RFC3339), Linear: lin, Soma: soma,
				})
				if f, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
					_, _ = f.Write(append(line, '\n'))
					_ = f.Close()
				} else {
					t.Logf("could not append to SOMA_LIVE_OUT: %v", err)
				}
			}
		}
		t.Logf("[%s] model=%s trials=%d linear_pass=%d/%d soma_pass=%d/%d", name, cfg.ModelID, n, linPass, n, somaPass, n)
	})
}

// --- Linear Chat Baseline Runner (append-only transcript + lossy LLM compaction + self-grading) ---
//
// What this baseline is: a minimal append-only tool loop written for this test, with LLM
// summarization once the transcript passes compactCharThreshold. It is NOT Hermes or any other
// real harness, and results against it say nothing about those. It also has no external verifier,
// so Exp2 and Exp3 measure "verifier feedback vs none" as much as anything else: soma's oracle
// feeds the failing assertion text back to the model.

func runLinearBaseline(ctx context.Context, cfg liveConfig, wsDir, systemPrompt, goal string, maxTurns int, compactCharThreshold int) abRunMetrics {
	start := time.Now()
	client := &http.Client{Timeout: 90 * time.Second}
	sessID := fmt.Sprintf("soma-linear-%d", time.Now().UnixNano())

	tools := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "exec",
				"description": "Execute a shell command inside the workspace.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_list",
				"description": "List relative file paths inside the workspace.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_read",
				"description": "Read a file inside the workspace.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []string{"path"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "fs_write",
				"description": "Write exact UTF-8 contents to a file inside the workspace.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
	}

	messages := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": goal},
	}

	sbx := sandbox.NewManager()
	runner := dag.NewSandboxedToolRunner(nil, sbx)
	var res abRunMetrics
	res.System = "linear"
	res.Branches = 1

	callModel := func(msgs []map[string]any, useTools bool) (map[string]any, int, int, int, error) {
		payload := map[string]any{
			"model":       cfg.ModelID,
			"messages":    msgs,
			"temperature": 0.0,
			"max_tokens":  2048,
		}
		if useTools {
			payload["tools"] = tools
			payload["tool_choice"] = "auto"
		}
		raw, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return nil, 0, 0, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Set("User-Agent", "soma-linear-eval/0.4")
		if strings.Contains(strings.ToLower(cfg.BaseURL), "opencode.ai") {
			req.Header.Set("x-opencode-session", sessID)
		}
		httpResp, err := client.Do(req)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		defer func() { _ = httpResp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
		if httpResp.StatusCode >= 300 {
			return nil, 0, 0, 0, fmt.Errorf("HTTP %d: %s", httpResp.StatusCode, string(body))
		}
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, 0, 0, 0, err
		}
		var pt, cached, ct int
		if u, ok := out["usage"].(map[string]any); ok {
			if v, ok := u["prompt_tokens"].(float64); ok {
				pt = int(v)
			}
			if v, ok := u["completion_tokens"].(float64); ok {
				ct = int(v)
			}
			if ptd, ok := u["prompt_tokens_details"].(map[string]any); ok {
				if v, ok := ptd["cached_tokens"].(float64); ok {
					cached = int(v)
				}
			}
			if oc, ok := u["opencode"].(map[string]any); ok {
				if toks, ok := oc["tokens"].(map[string]any); ok {
					if v, ok := toks["cache_read"].(float64); ok && int(v) > cached {
						cached = int(v)
					}
				}
			}
		}
		return out, pt, cached, ct, nil
	}

	msgCharCount := func(msgs []map[string]any) int {
		b, _ := json.Marshal(msgs)
		return len(b)
	}

	for turn := 0; turn < maxTurns; turn++ {
		if compactCharThreshold > 0 && msgCharCount(messages) > compactCharThreshold && len(messages) > 3 {
			// Lossy LLM text compaction over older history (mirrors Hermes autocompact).
			res.Compactions++
			// Summarize everything after the system prompt, including the most recent tool result.
			histBytes, _ := json.Marshal(messages[1:])
			truncHist := string(histBytes)
			if len(truncHist) > 48000 {
				// Hard pre-compaction window slice if tool dumps blew past ceiling.
				truncHist = truncHist[:16000] + "\n...[truncated middle log output]...\n" + truncHist[len(truncHist)-16000:]
			}
			sumMsgs := []map[string]any{
				{"role": "system", "content": "Summarize the prior engineering transcript in <= 400 words for an agent that will continue the task. Preserve exact file paths, identifiers, error codes, numeric values, what has been done, and what remains."},
				{"role": "user", "content": truncHist},
			}
			sumOut, sPT, sCached, sCT, sErr := callModel(sumMsgs, false)
			res.PromptTokens += sPT
			res.CachedTokens += sCached
			res.OutputTokens += sCT
			// The baseline keeps whatever its own summarizer preserved. Only if the summary call
			// fails or comes back empty does it fall back to a bare marker, and that is counted.
			summaryText := extractAssistantText(sumOut)
			if sErr != nil || summaryText == "" {
				res.SummaryFailures++
				summaryText = "Prior session compacted due to context overflow; summary unavailable."
			}
			messages = []map[string]any{
				messages[0],
				{"role": "user", "content": goal + "\n\n[COMPACTED SUMMARY]\n" + summaryText},
			}
		}

		out, pt, cached, ct, err := callModel(messages, true)
		if err != nil {
			res.ErrorText = err.Error()
			break
		}
		res.Turns++
		res.PromptTokens += pt
		res.CachedTokens += cached
		res.OutputTokens += ct

		choices, _ := out["choices"].([]any)
		if len(choices) == 0 {
			break
		}
		c0, _ := choices[0].(map[string]any)
		msg, _ := c0["message"].(map[string]any)
		if msg == nil {
			break
		}
		messages = append(messages, msg)

		tcs, _ := msg["tool_calls"].([]any)
		if len(tcs) == 0 {
			content, _ := msg["content"].(string)
			res.LastSummary = strings.TrimSpace(content)
			break
		}

		for _, rawTC := range tcs {
			tcMap, _ := rawTC.(map[string]any)
			tcID, _ := tcMap["id"].(string)
			fn, _ := tcMap["function"].(map[string]any)
			fnName, _ := fn["name"].(string)
			fnArgs, _ := fn["arguments"].(string)
			if fnArgs == "" {
				fnArgs = "{}"
			}
			tOut, tErr := runner.Execute(ctx, dag.ToolCall{
				Name:         fnName,
				ArgsJSON:     fnArgs,
				WorkspaceDir: wsDir,
			})
			obsStr := string(tOut.Content)
			if tErr != nil {
				obsStr = "TOOL ERROR: " + tErr.Error()
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": tcID,
				"content":      obsStr,
			})
		}
	}

	if res.PromptTokens > 0 {
		res.CacheHitPct = 100.0 * float64(res.CachedTokens) / float64(res.PromptTokens)
	}
	res.WallMS = time.Since(start).Milliseconds()
	return res
}

func collectSomaMetrics(ctx context.Context, st *store.Store, runID string, branchIDs []string, wallMS int64, passed bool, detail string) abRunMetrics {
	var res abRunMetrics
	res.System = "soma"
	res.Passed = passed
	res.PassDetail = detail
	res.Branches = len(branchIDs)
	res.WallMS = wallMS

	replayer := replay.NewReplayer(st)
	res.ReplayVerified = true
	for _, bID := range branchIDs {
		steps, err := st.ListBranchSteps(ctx, bID)
		if err != nil || len(steps) == 0 {
			continue
		}
		for _, s := range steps {
			res.Turns++
			res.PromptTokens += s.InputTokens + s.CacheReadTokens
			res.CachedTokens += s.CacheReadTokens
			res.OutputTokens += s.OutputTokens
		}
		rep, rErr := replayer.Replay(ctx, bID, replay.ReplayOptions{Mode: replay.ModeExact})
		if rErr != nil || rep.Diverged || rep.StepsChecked == 0 || rep.HashVerifiedCount != rep.StepsChecked {
			res.ReplayVerified = false
		}
	}
	if res.PromptTokens > 0 {
		res.CacheHitPct = 100.0 * float64(res.CachedTokens) / float64(res.PromptTokens)
	}
	if grave, gErr := st.ListActiveMemoryByScopes(ctx, []string{"repo:" + runID}, []string{"negative_path"}); gErr == nil {
		res.GraveyardNodes = len(grave)
	}
	return res
}

// --- Experiments ---

func TestLiveEvaluation_HypothesisAB(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx := context.Background()

	t.Logf("Running LIVE A/B Hypothesis Evaluation on base=%s model=%s repeats=%d (set SOMA_LIVE_REPEATS / SOMA_LIVE_OUT)",
		cfg.BaseURL, cfg.ModelID, liveRepeats())

	runTrials(t, cfg, "Exp1_LogStormNeedleAndKVCache", func(t *testing.T) (abRunMetrics, abRunMetrics) {
		seedLogWS := func(dir string) {
			_ = os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
			_ = os.MkdirAll(filepath.Join(dir, "config"), 0o755)
			_ = os.WriteFile(filepath.Join(dir, "config", "timeout.yaml"), []byte("service: soma-worker\ntimeout_ms: 500\nretries: 3\n"), 0o644)

			var aBuf, bBuf strings.Builder
			for i := 1; i <= 1800; i++ {
				fmt.Fprintf(&aBuf, "2026-11-22T15:00:%02dZ INFO worker_a heartbeat seq=%04d queue_depth=%d ok=true\n", i%60, i, i%7)
				if i == 1142 {
					bBuf.WriteString("2026-11-22T15:11:42Z ERROR rpc_watchdog: ERR_CODE=SOMA_CRC_7741 FIX_TARGET=config/timeout.yaml REQUIRED_TIMEOUT_MS=4250 message=\"increase timeout_ms to 4250\"\n")
				} else {
					fmt.Fprintf(&bBuf, "2026-11-22T15:10:%02dZ DEBUG worker_b trace seq=%04d checksum=valid latency_ms=%d\n", i%60, i, 12+(i%19))
				}
			}
			_ = os.WriteFile(filepath.Join(dir, "logs", "worker_a.log"), []byte(aBuf.String()), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "logs", "worker_b.log"), []byte(bBuf.String()), 0o644)
		}

		validateExp1 := func(wsDir string) (bool, string) {
			yamlRaw, yErr := os.ReadFile(filepath.Join(wsDir, "config", "timeout.yaml"))
			diagRaw, dErr := os.ReadFile(filepath.Join(wsDir, "diagnosis.json"))
			if yErr != nil || dErr != nil {
				return false, fmt.Sprintf("missing output files (yaml=%v diag=%v)", yErr, dErr)
			}
			hasTimeout := strings.Contains(string(yamlRaw), "4250")
			var diag map[string]any
			if jErr := json.Unmarshal(diagRaw, &diag); jErr != nil {
				return false, "invalid diagnosis.json"
			}
			code, _ := diag["error_code"].(string)
			tVal, _ := diag["timeout_ms"].(float64)
			ok := hasTimeout && code == "SOMA_CRC_7741" && int(tVal) == 4250
			return ok, fmt.Sprintf("yaml_4250=%v error_code=%q timeout_ms=%v", hasTimeout, code, diag["timeout_ms"])
		}

		goal := "Inspect logs/worker_a.log and logs/worker_b.log to locate the single ERROR line containing ERR_CODE and REQUIRED_TIMEOUT_MS. Update config/timeout.yaml setting timeout_ms to that exact integer, and create diagnosis.json containing {\"error_code\": \"...\", \"timeout_ms\": <int>}."

		// 1A) Linear Baseline
		linWS := t.TempDir()
		seedLogWS(linWS)
		linMetrics := runLinearBaseline(ctx, cfg, linWS,
			"You are a coding agent with exec, fs_list, fs_read, and fs_write tools.",
			goal, 12, 64000) // same turn budget as soma below
		linPass, linDetail := validateExp1(linWS)
		linMetrics.Passed = linPass
		linMetrics.PassDetail = linDetail

		// 1B) soma
		somaWS := t.TempDir()
		seedLogWS(somaWS)
		st, err := store.Open(filepath.Join(somaWS, ".soma"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer func() { _ = st.Close() }()
		_ = st.UpsertAccount(ctx, store.Account{
			ID: "live", Provider: "opencode-go", ModelID: cfg.ModelID, BillingMode: "subscription", MaxConcurrent: 2,
		})

		prov := dag.NewOpenAICompatProvider(cfg.BaseURL, cfg.APIKey)
		sbx := sandbox.NewManager()
		tools := dag.NewSandboxedToolRunner(vmem.NewManager(st), sbx)
		execDag := dag.NewExecutor(st, prov, tools, compiler.WorkerProfile())
		execDag.AccountIDs = []string{"live"}

		start := time.Now()
		runID, brID, err := execDag.StartRun(ctx, dag.RunRequest{
			Title:         "exp1-log-storm",
			WorkspacePath: somaWS,
			GoalSpec:      goal,
			AccountIDs:    []string{"live"},
		})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		_, execErr := execDag.ExecuteBranch(ctx, brID, 12)
		wall := time.Since(start).Milliseconds()

		br, _ := st.GetBranch(ctx, brID)
		somaPass, somaDetail := validateExp1(br.WorkspaceDir)
		somaMetrics := collectSomaMetrics(ctx, st, runID, []string{brID}, wall, somaPass, somaDetail)
		if execErr != nil {
			somaMetrics.ErrorText = execErr.Error()
		}

		t.Logf("[Exp1] %s", linMetrics.Format())
		t.Logf("[Exp1] %s", somaMetrics.Format())
		if !somaPass {
			steps, _ := st.ListBranchSteps(ctx, brID)
			if len(steps) > 0 {
				if f, rErr := compiler.ReconstructFrame(ctx, st, steps[len(steps)-1].ID, compiler.WorkerProfile()); rErr == nil {
					t.Logf("[Exp1 soma L2 trace]\n%s", f.Layer2)
				}
			}
		}
		return linMetrics, somaMetrics
	})

	runTrials(t, cfg, "Exp2_ExternalOracleVsSelfGrade", func(t *testing.T) (abRunMetrics, abRunMetrics) {
		buggyCalc := `def eval_expr(expr: str) -> float:
    """Buggy evaluator: does left-to-right only, breaks unary minus and nested parens."""
    tokens = expr.strip().split()
    if not tokens:
        raise ValueError("empty")
    val = float(tokens[0])
    i = 1
    while i < len(tokens):
        op = tokens[i]
        rhs = float(tokens[i + 1])
        if op == "+":
            val += rhs
        elif op == "-":
            val -= rhs
        elif op == "*":
            val *= rhs
        elif op == "/":
            val /= rhs
        i += 2
    return val
`
		seedCalcWS := func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, "calc.py"), []byte(buggyCalc), 0o644)
		}

		heldOutPy := `import inspect, calc
src = inspect.getsource(calc)
assert "eval(" not in src and "exec(" not in src, "forbidden eval/exec usage"
assert calc.eval_expr("2 + 3 * 4") == 14.0, "precedence failed"
assert calc.eval_expr("-3 + 5") == 2.0, "unary leading minus failed"
assert calc.eval_expr("2 * -4 + 10") == 2.0, "unary minus after operator failed"
assert calc.eval_expr("2 * (3 + 4) - 5 / 2") == 11.5, "parens+div failed"
assert calc.eval_expr("-(2 + 3) * 4") == -20.0, "unary minus before parens failed"
try:
    calc.eval_expr("10 / 0")
    raise AssertionError("expected ValueError on div by zero")
except ValueError:
    pass
print("HELD_OUT_ORACLE_OK")
`
		validateExp2 := func(wsDir string) (bool, string) {
			cmd := exec.CommandContext(ctx, "python3", "-c", heldOutPy)
			cmd.Dir = wsDir
			out, err := cmd.CombinedOutput()
			msg := strings.TrimSpace(string(out))
			if len(msg) > 120 {
				msg = msg[len(msg)-120:]
			}
			return err == nil, msg
		}

		goal := "Fix calc.py so eval_expr(expr: str) -> float parses arithmetic expressions with +, -, *, /, nested parentheses, unary minus (like -3 + 5, 2 * -4, -(2 + 3) * 4), and arbitrary spacing WITHOUT using Python's built-in eval() or exec(). Raise ValueError on division by zero or invalid syntax."

		// 2A) Linear Baseline (self-graded)
		linWS := t.TempDir()
		seedCalcWS(linWS)
		linMetrics := runLinearBaseline(ctx, cfg, linWS,
			"You are a coding agent with fs_read, fs_write, exec, and fs_list.",
			goal, 10, 64000) // same turn budget as soma below
		linPass, linDetail := validateExp2(linWS)
		linMetrics.Passed = linPass
		linMetrics.PassDetail = linDetail

		// 2B) soma with CommandOracle (held-out assertions)
		somaWS := t.TempDir()
		seedCalcWS(somaWS)
		st, err := store.Open(filepath.Join(somaWS, ".soma"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer func() { _ = st.Close() }()
		_ = st.UpsertAccount(ctx, store.Account{
			ID: "live", Provider: "opencode-go", ModelID: cfg.ModelID, BillingMode: "subscription", MaxConcurrent: 2,
		})

		prov := dag.NewOpenAICompatProvider(cfg.BaseURL, cfg.APIKey)
		sbx := sandbox.NewManager()
		tools := dag.NewSandboxedToolRunner(vmem.NewManager(st), sbx)
		execDag := dag.NewExecutor(st, prov, tools, compiler.WorkerProfile())
		execDag.AccountIDs = []string{"live"}
		execDag.Verifier = oracle.NewVerifier(st, sbx)

		start := time.Now()
		runID, brID, err := execDag.StartRun(ctx, dag.RunRequest{
			Title:         "exp2-external-oracle",
			WorkspacePath: somaWS,
			GoalSpec:      goal,
			AccountIDs:    []string{"live"},
		})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		cmdSpec, _ := json.Marshal(oracle.CommandOracleSpec{
			Command: "/usr/bin/python3",
			Args:    []string{"-c", heldOutPy},
		})
		_ = st.CreateContract(ctx, store.Contract{
			ID: "ctr_exp2_" + runID, RunID: runID, Kind: "command_oracle", SpecJSON: string(cmdSpec), PinnedHash: "calc_v1", Required: true,
		})

		res, execErr := execDag.ExecuteBranch(ctx, brID, 10)
		wall := time.Since(start).Milliseconds()
		br, _ := st.GetBranch(ctx, brID)
		somaPass, somaDetail := validateExp2(br.WorkspaceDir)
		cruns, _ := st.ListBranchContractRuns(ctx, brID)
		somaDetail = fmt.Sprintf("%s (oracle_runs=%d status=%s)", somaDetail, len(cruns), res.Status)
		somaMetrics := collectSomaMetrics(ctx, st, runID, []string{brID}, wall, somaPass, somaDetail)
		if execErr != nil {
			somaMetrics.ErrorText = execErr.Error()
		}

		t.Logf("[Exp2] %s", linMetrics.Format())
		t.Logf("[Exp2] %s", somaMetrics.Format())
		return linMetrics, somaMetrics
	})

	runTrials(t, cfg, "Exp3_DeceptiveLocalOptimumBranching", func(t *testing.T) (abRunMetrics, abRunMetrics) {
		evalPy := `import base64, json, os
with open("candidate.json") as f:
    c = json.load(f)
# Opaque benchmark lookup simulating held-out evaluation binary (no inline constraint leak in source).
TABLE = json.loads(base64.b64decode(
    "eyJidXJzdCI6eyJldmFsX2Rldl9hdWMiOjAuOTgwNywiZDNfNjB4X2RpbGF0ZWQiOjAuMDAxOCwiY2F1c2FsX3ByZWZpeF9tYXhfZGVsdGEiOjEuMmUtN30s"
    "ImRpbGF0ZWRfbm9uY2F1c2FsIjp7ImV2YWxfZGV2X2F1YyI6MC45ODkzLCJkM182MHhfZGlsYXRlZCI6MS4wLCJjYXVzYWxfcHJlZml4X21heF9kZWx0YSI6MC4wMTczfSwi"
    "Y2F1c2FsX211bHRpc2NhbGUiOnsiZXZhbF9kZXZfYXVjIjowLjk4MTIsImQzXzYweF9kaWxhdGVkIjowLjk1OCwiY2F1c2FsX3ByZWZpeF9tYXhfZGVsdGEiOjIuMWUtN319"
).decode())
mode = c.get("mode", "")
causal = bool(c.get("causal", False))
window = int(c.get("window", 0))
os.makedirs("artifacts", exist_ok=True)
if mode == "causal_multiscale" and (not causal or window < 512):
    m = {"mode": mode, "eval_dev_auc": 0.7200, "d3_60x_dilated": 0.2500, "causal_prefix_max_delta": 0.0500}
else:
    m = dict(TABLE.get(mode, {"eval_dev_auc": 0.7000, "d3_60x_dilated": 0.2000, "causal_prefix_max_delta": 0.0500}))
    m["mode"] = mode
with open("artifacts/metrics.json", "w") as f:
    json.dump(m, f, indent=2)
print(f"eval_dev_auc={m['eval_dev_auc']}")
`
		readmeMD := `# ML Candidate Selection Lab
Edit ` + "`candidate.json`" + ` and run ` + "`python3 eval_candidate.py`" + ` (writes ` + "`artifacts/metrics.json`" + `).
Three candidate families are available (` + "`mode`" + `):
1. ` + "`burst`" + ` (small-window baseline; high dev AUC 0.9807)
2. ` + "`dilated_noncausal`" + ` (unconstrained receptive field; highest dev AUC 0.9893)
3. ` + "`causal_multiscale`" + ` (` + "`\"causal\": true`" + `, ` + "`\"window\": 512`" + ` or higher)
Select the best model configuration maximizing dev quality without violating deployment checks, run ` + "`python3 eval_candidate.py`" + `, and verify ` + "`artifacts/metrics.json`" + ` exists before declaring completion.
`
		seedMLWS := func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, "eval_candidate.py"), []byte(evalPy), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte(readmeMD), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "candidate.json"), []byte(`{"mode":"burst","window":128,"causal":true}`), 0o644)
		}

		validateExp3 := func(wsDir string) (bool, string) {
			raw, err := os.ReadFile(filepath.Join(wsDir, "artifacts", "metrics.json"))
			if err != nil {
				return false, "missing artifacts/metrics.json"
			}
			var m struct {
				Mode   string  `json:"mode"`
				Dev    float64 `json:"eval_dev_auc"`
				D3     float64 `json:"d3_60x_dilated"`
				Causal float64 `json:"causal_prefix_max_delta"`
			}
			if jErr := json.Unmarshal(raw, &m); jErr != nil {
				return false, "invalid metrics.json"
			}
			ok := m.Dev >= 0.97 && m.D3 >= 0.90 && m.Causal <= 0.001
			return ok, fmt.Sprintf("mode=%s dev=%.4f d3=%.4f causal=%.1e", m.Mode, m.Dev, m.D3, m.Causal)
		}

		goal := "Read README.md and configure candidate.json to maximize model quality without breaking any hidden deployment constraints. Always run `python3 eval_candidate.py` after modifying candidate.json before finishing."

		// 3A) Linear Baseline (optimizes dev score blindly without hidden frozen gate feedback)
		linWS := t.TempDir()
		seedMLWS(linWS)
		linMetrics := runLinearBaseline(ctx, cfg, linWS,
			"You are an ML research agent with fs_read, fs_write, exec, and fs_list.",
			goal, 30, 64000) // same total budget as soma's 3 branches x 10 turns
		linPass, linDetail := validateExp3(linWS)
		linMetrics.Passed = linPass
		linMetrics.PassDetail = linDetail

		// 3B) soma with frozen ArtifactOracle + BranchSearcher
		somaWS := t.TempDir()
		seedMLWS(somaWS)
		st, err := store.Open(filepath.Join(somaWS, ".soma"))
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		defer func() { _ = st.Close() }()
		_ = st.UpsertAccount(ctx, store.Account{
			ID: "live", Provider: "opencode-go", ModelID: cfg.ModelID, BillingMode: "subscription", MaxConcurrent: 2,
		})

		prov := dag.NewOpenAICompatProvider(cfg.BaseURL, cfg.APIKey)
		sbx := sandbox.NewManager()
		tools := dag.NewSandboxedToolRunner(vmem.NewManager(st), sbx)
		prof := compiler.WorkerProfile()
		prof.MaxStalls = 3
		execDag := dag.NewExecutor(st, prov, tools, prof)
		execDag.AccountIDs = []string{"live"}
		execDag.Verifier = oracle.NewVerifier(st, sbx)

		start := time.Now()
		runID, rootBr, err := execDag.StartRun(ctx, dag.RunRequest{
			Title:         "exp3-ml-branching",
			WorkspacePath: somaWS,
			WorkspaceKind: "research",
			GoalSpec:      goal,
			AccountIDs:    []string{"live"},
		})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		artSpec, _ := json.Marshal(oracle.ArtifactOracleSpec{
			RequiredPaths:    []string{"artifacts/metrics.json"},
			RequiredJSONKeys: map[string][]string{"artifacts/metrics.json": {"mode", "eval_dev_auc", "d3_60x_dilated", "causal_prefix_max_delta"}},
			MetricThresholds: []oracle.MetricThreshold{
				{Key: "eval_dev_auc", Op: ">=", Value: 0.97},
				{Key: "d3_60x_dilated", Op: ">=", Value: 0.90},
				{Key: "causal_prefix_max_delta", Op: "<=", Value: 0.001},
			},
		})
		_ = st.CreateContract(ctx, store.Contract{
			ID: "ctr_exp3_" + runID, RunID: runID, Kind: "artifact_oracle", SpecJSON: string(artSpec), PinnedHash: "lala_live", Required: true,
		})

		searcher := dag.NewBranchSearcher(execDag)
		sRes, sErr := searcher.SearchExistingRun(ctx, runID, rootBr, dag.SearchConfig{
			MaxBranches:       3,
			MaxTurnsPerBranch: 10,
			AnchorTurn:        1,
		})
		wall := time.Since(start).Milliseconds()

		var bIDs []string
		for _, att := range sRes.Attempts {
			bIDs = append(bIDs, att.BranchID)
		}
		var somaPass bool
		var somaDetail string
		if sRes.WinningBranchID != "" {
			winBr, _ := st.GetBranch(ctx, sRes.WinningBranchID)
			somaPass, somaDetail = validateExp3(winBr.WorkspaceDir)
		} else {
			somaDetail = "no winning branch"
		}
		somaMetrics := collectSomaMetrics(ctx, st, runID, bIDs, wall, somaPass, somaDetail)
		if sErr != nil {
			somaMetrics.ErrorText = sErr.Error()
		}

		t.Logf("[Exp3] %s", linMetrics.Format())
		if linMetrics.ErrorText != "" || linMetrics.LastSummary != "" {
			t.Logf("[Exp3 linear debug] err=%q summary=%q", linMetrics.ErrorText, linMetrics.LastSummary)
		}
		t.Logf("[Exp3] %s", somaMetrics.Format())
		for _, att := range sRes.Attempts {
			steps, _ := st.ListBranchSteps(ctx, att.BranchID)
			var lastObs string
			if len(steps) > 0 && steps[len(steps)-1].ObservationPageID != "" {
				if pg, err := st.GetPage(ctx, steps[len(steps)-1].ObservationPageID); err == nil {
					lastObs = pg.PreviewText
				}
			}
			t.Logf("[Exp3 attempt %s] status=%s turns=%d passed=%v reason=%q obs=%q",
				att.Label, att.Status, att.Turns, att.Passed, att.FailureReason, lastObs)
		}
		if !sRes.Completed {
			for _, att := range sRes.Attempts {
				steps, _ := st.ListBranchSteps(ctx, att.BranchID)
				if len(steps) > 0 {
					if f, err := compiler.ReconstructFrame(ctx, st, steps[len(steps)-1].ID, prof); err == nil {
						t.Logf("[Exp3 trace %s]\n%s\n[L3]\n%s", att.Label, f.Layer2, f.Layer3)
					}
				}
			}
		}
		return linMetrics, somaMetrics
	})
}
