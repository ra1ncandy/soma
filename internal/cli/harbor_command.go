package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
	"github.com/ra1ncandy/soma/internal/webui"
	"github.com/ra1ncandy/soma/internal/workspace"
)

func newHarborRunCmd() *cobra.Command {
	var dir, harnessDir, execURL, goal, model string
	var maxTurns int
	var jsonOut bool

	cmd := &cobra.Command{
		Use:    "harbor-run",
		Short:  "Run a durable soma DAG task against a Harbor container exec callback (Terminal-Bench adapter)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(execURL) == "" {
				return errors.New("--exec-url is required")
			}
			if strings.TrimSpace(goal) == "" {
				return errors.New("--goal is required")
			}
			if strings.TrimSpace(dir) == "" {
				tmpDir, err := os.MkdirTemp("", "soma-harbor-*")
				if err != nil {
					return err
				}
				dir = tmpDir
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			cfgDir := absDir
			if strings.TrimSpace(harnessDir) != "" {
				cfgDir = harnessDir
			}
			prov, resolvedModel, err := selectProvider(cfgDir, false)
			if err != nil {
				var ok bool
				prov, resolvedModel, ok = webui.ResolveDefaultProviderFromHarness(cmd.Context(), st, cfgDir)
				if !ok || prov == nil {
					return err
				}
			}
			if strings.TrimSpace(model) != "" {
				resolvedModel = strings.TrimSpace(model)
			}
			if resolvedModel == "" {
				resolvedModel = "deepseek-v4.1-flash"
			}
			_ = st.UpsertAccount(cmd.Context(), store.Account{
				ID:            "harbor-default",
				Provider:      "openai_compat",
				ModelID:       resolvedModel,
				BillingMode:   "local",
				MaxConcurrent: 4,
			})

			vm := vmem.NewManager(st)
			tr := &harborToolRunner{
				execURL: execURL,
				httpClient: &http.Client{
					Timeout: 320 * time.Second,
				},
				fallback: dag.NewLocalToolRunner(vm),
			}

			exec := dag.NewExecutor(st, prov, tr, compiler.WorkerProfile())
			exec.VM = vm
			exec.AllowFailover = true
			exec.AutonomyMode = "autonomous"
			exec.SystemPrompt = "You are soma, solving a benchmark task inside an isolated Linux container. " +
				"Available tools: exec(command, timeout_sec?), fs_list, fs_grep, fs_read(path, start_line?, end_line?), fs_write, fs_patch, page_grep, page_slice, page_search. " +
				"Rules: (1) Inspect the current directory and task requirements first; batch read-only calls when possible. " +
				"(2) Verify your changes by running tests or checking outputs before finishing. " +
				"(3) Summarize concisely when done."

			runID, branchID, err := exec.StartRun(cmd.Context(), dag.RunRequest{
				WorkspacePath:     absDir,
				WorkspaceStrategy: workspace.StrategyDirect,
				GoalSpec:          goal,
				AutonomyMode:      "autonomous",
			})
			if err != nil {
				return err
			}
			if maxTurns <= 0 {
				maxTurns = 30
			}
			res, execErr := exec.ExecuteBranch(cmd.Context(), branchID, maxTurns)

			steps, _ := st.ListBranchSteps(cmd.Context(), branchID)
			var inToks, cacheToks, outToks int
			var costUSD float64
			for _, s := range steps {
				inToks += s.InputTokens + s.CacheReadTokens
				cacheToks += s.CacheReadTokens
				outToks += s.OutputTokens
				costUSD += s.CostUSD
			}

			if jsonOut {
				payload := map[string]any{
					"run_id":            runID,
					"branch_id":         branchID,
					"status":            "",
					"turns_executed":    0,
					"final_summary":     "",
					"input_tokens":      inToks,
					"cache_read_tokens": cacheToks,
					"output_tokens":     outToks,
					"cost_usd":          costUSD,
				}
				if res != nil {
					payload["status"] = string(res.Status)
					payload["turns_executed"] = res.TurnsExecuted
					payload["final_summary"] = res.FinalSummary
				}
				if execErr != nil {
					payload["error"] = execErr.Error()
				}
				raw, _ := json.Marshal(payload)
				fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return execErr
			}
			if execErr != nil {
				return execErr
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.FinalSummary)
			return nil
		},
	}

	cmd.Flags().StringVar(&dir, "dir", "", "Temporary local store directory for soma state")
	cmd.Flags().StringVar(&harnessDir, "harness-dir", "", "Optional workspace directory containing .soma/harness.json")
	cmd.Flags().StringVar(&execURL, "exec-url", "", "Harbor BaseAgent HTTP /exec callback URL")
	cmd.Flags().StringVar(&goal, "goal", "", "Task instruction")
	cmd.Flags().StringVar(&model, "model", "", "Override model ID")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 30, "Maximum agent turns")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit final JSON summary line")
	return cmd
}

type harborToolRunner struct {
	execURL    string
	httpClient *http.Client
	fallback   *dag.LocalToolRunner
}

type harborExecReq struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

type harborExecResp struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (r *harborToolRunner) remoteExec(ctx context.Context, command string, timeoutSec int) (harborExecResp, error) {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	raw, _ := json.Marshal(harborExecReq{Command: command, TimeoutSec: timeoutSec})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.execURL, bytes.NewReader(raw))
	if err != nil {
		return harborExecResp{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return harborExecResp{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return harborExecResp{}, fmt.Errorf("harbor exec callback HTTP %d", resp.StatusCode)
	}
	var out harborExecResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return harborExecResp{}, err
	}
	return out, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (r *harborToolRunner) Execute(ctx context.Context, call dag.ToolCall) (dag.ToolOutput, error) {
	var args map[string]any
	if strings.TrimSpace(call.ArgsJSON) != "" {
		if err := json.Unmarshal([]byte(call.ArgsJSON), &args); err != nil {
			return dag.ToolOutput{}, fmt.Errorf("parse args for %s: %w", call.Name, err)
		}
	}

	switch call.Name {
	case "exec":
		command, _ := args["command"].(string)
		if strings.TrimSpace(command) == "" {
			return dag.ToolOutput{}, errors.New("exec requires non-empty command")
		}
		timeoutSec := 60
		if tsF, ok := args["timeout_sec"].(float64); ok && tsF > 0 {
			timeoutSec = int(tsF)
			if timeoutSec > 300 {
				timeoutSec = 300
			}
		}
		res, err := r.remoteExec(ctx, command, timeoutSec)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		combined := res.Stdout
		if res.Stderr != "" {
			if combined != "" && !strings.HasSuffix(combined, "\n") {
				combined += "\n"
			}
			combined += res.Stderr
		}
		body := fmt.Sprintf("exit_code=%d\n%s", res.ExitCode, combined)
		return dag.ToolOutput{NameHint: "exec.log", Content: []byte(body), TrustTier: "trusted_repo"}, nil

	case "fs_list":
		rel, _ := args["path"].(string)
		if strings.TrimSpace(rel) == "" {
			rel = "."
		}
		cmd := fmt.Sprintf("find %s -maxdepth 5 -not -path '*/.git*' -not -path '*/.soma*' 2>/dev/null | sort | head -n 500", shellQuote(rel))
		res, err := r.remoteExec(ctx, cmd, 20)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		return dag.ToolOutput{NameHint: "fs_list.txt", Content: []byte(res.Stdout), TrustTier: "trusted_repo"}, nil

	case "fs_grep":
		pattern, _ := args["pattern"].(string)
		if strings.TrimSpace(pattern) == "" {
			return dag.ToolOutput{}, errors.New("fs_grep requires non-empty pattern")
		}
		rel, _ := args["path"].(string)
		if strings.TrimSpace(rel) == "" {
			rel = "."
		}
		flags := "-rn"
		if ic, _ := args["ignore_case"].(bool); ic {
			flags += "i"
		}
		cmd := fmt.Sprintf("grep %s -E %s %s 2>/dev/null | head -n 80", flags, shellQuote(pattern), shellQuote(rel))
		res, err := r.remoteExec(ctx, cmd, 20)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		out := strings.TrimSpace(res.Stdout)
		if out == "" {
			out = fmt.Sprintf("fs_grep: 0 matches for %q", pattern)
		}
		return dag.ToolOutput{NameHint: "fs_grep.txt", Content: []byte(out), TrustTier: "trusted_repo"}, nil

	case "fs_read":
		rel, _ := args["path"].(string)
		if strings.TrimSpace(rel) == "" {
			return dag.ToolOutput{}, errors.New("fs_read requires path")
		}
		startF, _ := args["start_line"].(float64)
		endF, _ := args["end_line"].(float64)
		start := int(startF)
		end := int(endF)
		var cmd string
		if start > 0 && end >= start {
			cmd = fmt.Sprintf("nl -ba %s | sed -n '%d,%dp'", shellQuote(rel), start, end)
		} else if start > 0 {
			cmd = fmt.Sprintf("nl -ba %s | sed -n '%d,%dp'", shellQuote(rel), start, start+200)
		} else {
			cmd = fmt.Sprintf("nl -ba %s | head -n 400", shellQuote(rel))
		}
		res, err := r.remoteExec(ctx, cmd, 20)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		if res.ExitCode != 0 && res.Stdout == "" {
			return dag.ToolOutput{}, fmt.Errorf("fs_read %s: %s", rel, strings.TrimSpace(res.Stderr))
		}
		return dag.ToolOutput{NameHint: filepath.Base(rel), Content: []byte(res.Stdout), TrustTier: "trusted_repo"}, nil

	case "fs_write":
		rel, _ := args["path"].(string)
		content, _ := args["content"].(string)
		if strings.TrimSpace(rel) == "" {
			return dag.ToolOutput{}, errors.New("fs_write requires path")
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(content))
		cmd := fmt.Sprintf("mkdir -p \"$(dirname %s)\" && printf '%%s' %s | base64 -d > %s", shellQuote(rel), shellQuote(b64), shellQuote(rel))
		res, err := r.remoteExec(ctx, cmd, 20)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		if res.ExitCode != 0 {
			return dag.ToolOutput{}, fmt.Errorf("fs_write %s failed (exit=%d): %s", rel, res.ExitCode, res.Stderr)
		}
		return dag.ToolOutput{
			NameHint:  "fs_write.txt",
			Content:   []byte("wrote " + strconv.Itoa(len(content)) + " bytes to " + rel),
			TrustTier: "trusted_repo",
		}, nil

	case "fs_patch":
		rel, _ := args["path"].(string)
		patchText, _ := args["patch"].(string)
		if strings.TrimSpace(rel) == "" {
			return dag.ToolOutput{}, errors.New("fs_patch requires path")
		}
		var explicitHunks []dag.HunkSpec
		if sStr, ok := args["search"].(string); ok && sStr != "" {
			rStr, _ := args["replace"].(string)
			repAll, _ := args["replace_all"].(bool)
			explicitHunks = append(explicitHunks, dag.HunkSpec{Search: sStr, Replace: rStr, ReplaceAll: repAll})
		}
		readRes, err := r.remoteExec(ctx, fmt.Sprintf("base64 -w0 %s 2>/dev/null || base64 %s", shellQuote(rel), shellQuote(rel)), 20)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		if readRes.ExitCode != 0 {
			return dag.ToolOutput{}, fmt.Errorf("fs_patch read %s failed: %s", rel, readRes.Stderr)
		}
		cleanB64 := strings.Map(func(c rune) rune {
			if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
				return -1
			}
			return c
		}, readRes.Stdout)
		origBytes, err := base64.StdEncoding.DecodeString(cleanB64)
		if err != nil {
			return dag.ToolOutput{}, fmt.Errorf("fs_patch decode %s: %w", rel, err)
		}
		patched, hunkCount, err := dag.ApplyFilePatch(string(origBytes), patchText, explicitHunks)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(patched))
		writeCmd := fmt.Sprintf("printf '%%s' %s | base64 -d > %s", shellQuote(b64), shellQuote(rel))
		wRes, err := r.remoteExec(ctx, writeCmd, 20)
		if err != nil {
			return dag.ToolOutput{}, err
		}
		if wRes.ExitCode != 0 {
			return dag.ToolOutput{}, fmt.Errorf("fs_patch write %s failed: %s", rel, wRes.Stderr)
		}
		return dag.ToolOutput{
			NameHint:  "fs_patch.txt",
			Content:   []byte(fmt.Sprintf("patched %s (%d hunk(s))", rel, hunkCount)),
			TrustTier: "trusted_repo",
		}, nil

	default:
		return r.fallback.Execute(ctx, call)
	}
}
