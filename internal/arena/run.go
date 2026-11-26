package arena

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RunConfig is shared by every contestant in an arena run: same model, same endpoint (through
// the metering proxy), same call budget per task, same wall-clock limit.
type RunConfig struct {
	ProxyBaseURL string
	Model        string
	WallClock    time.Duration
}

// Contestant runs one task in dir and returns when the agent stops.
type Contestant interface {
	Name() string
	Version() string
	Run(ctx context.Context, cfg RunConfig, task Task, dir string) (stdout string, err error)
}

// SomaContestant runs `soma ask` (isolated workspace, applied to the project on completion).
type SomaContestant struct {
	Bin      string
	MaxTurns int
	// Env is extra KEY=VALUE settings for soma (e.g. SOMA_REASONING_EFFORT=low), for comparing
	// configurations. It cannot carry credentials: the proxy address and model always win.
	Env []string
}

func (c SomaContestant) Name() string { return "soma" }

func (c SomaContestant) Version() string {
	out, err := exec.Command(c.Bin, "version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (c SomaContestant) Run(ctx context.Context, cfg RunConfig, task Task, dir string) (string, error) {
	turns := c.MaxTurns
	if turns <= 0 {
		turns = task.MaxCalls
	}
	cmd := exec.CommandContext(ctx, c.Bin, "ask", "--dir", dir, "--max-turns", fmt.Sprint(turns), task.Goal)
	cmd.Dir = dir
	cmd.Env = append(append(cleanEnv(), c.Env...), "SOMA_API_BASE="+cfg.ProxyBaseURL, "SOMA_MODEL="+cfg.Model, "SOMA_API_KEY=arena")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// HermesContestant runs an installed Hermes Agent in one-shot mode with an isolated
// HERMES_HOME per run whose only provider is the metering proxy.
type HermesContestant struct {
	Bin string
	// BaseHome is an installed HERMES_HOME; its code is reused, its state is not.
	BaseHome string
}

func (c HermesContestant) Name() string { return "hermes" }

func (c HermesContestant) Version() string {
	cmd := exec.Command(c.Bin, "--version")
	cmd.Env = append(cleanEnv(), "HERMES_HOME="+c.BaseHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func (c HermesContestant) Run(ctx context.Context, cfg RunConfig, task Task, dir string) (string, error) {
	home, err := os.MkdirTemp("", "arena-hermes-home-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(home) }()
	// Same budget as soma: Hermes' turn limit equals the task's call budget (the proxy also
	// enforces it). --yolo because one-shot mode denies prompts that nobody can answer; Hermes'
	// hardline blocklist still applies. Its default local terminal backend runs on the host.
	config := fmt.Sprintf(`model:
  default: %q
  provider: "custom"
  base_url: %q
  api_key: "arena"
agent:
  max_turns: %d
terminal:
  backend: "local"
`, cfg.Model, cfg.ProxyBaseURL, task.MaxCalls)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0o600); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, c.Bin, "-z", task.Goal, "--yolo")
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), "HERMES_HOME="+home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// cleanEnv passes the environment without any provider credentials, so a contestant can only
// reach the model through the metering proxy.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if strings.Contains(u, "KEY") || strings.Contains(u, "TOKEN") || strings.Contains(u, "SECRET") ||
			strings.HasPrefix(u, "SOMA_") || strings.HasPrefix(u, "HERMES_") || strings.HasPrefix(u, "OPENAI_") || strings.HasPrefix(u, "ANTHROPIC_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Result is one measured contestant attempt at one task.
type Result struct {
	Task       string  `json:"task"`
	Contestant string  `json:"contestant"`
	Repeat     int     `json:"repeat"`
	Passed     bool    `json:"passed"`
	GraderOut  string  `json:"grader_output"`
	Usage      Usage   `json:"usage"`
	WallSec    float64 `json:"wall_seconds"`
	ExitError  string  `json:"exit_error,omitempty"`
	TimedOut   bool    `json:"timed_out,omitempty"`
}

// Runner executes a task set for every contestant.
type Runner struct {
	Proxy       *Proxy
	Config      RunConfig
	Contestants []Contestant
	Tasks       []Task
	Repeats     int
	// Log receives progress lines.
	Log func(string)
	// KeepDir, when set, preserves every attempt's workspace and outputs under it.
	KeepDir string
}

// Run executes all attempts sequentially (one metered session at a time) and stops early when
// the proxy's spend cap is reached.
func (r *Runner) Run(ctx context.Context) ([]Result, error) {
	var results []Result
	repeats := r.Repeats
	if repeats <= 0 {
		repeats = 1
	}
	for rep := 1; rep <= repeats; rep++ {
		for _, task := range r.Tasks {
			// Alternate contestant order per repeat so neither always runs first.
			order := append([]Contestant(nil), r.Contestants...)
			if rep%2 == 0 {
				for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
					order[i], order[j] = order[j], order[i]
				}
			}
			for _, c := range order {
				if r.Proxy.capUSD > 0 && r.Proxy.SpentUSD() >= r.Proxy.capUSD {
					return results, fmt.Errorf("spend cap $%.2f reached after %d attempts", r.Proxy.capUSD, len(results))
				}
				res, err := r.attempt(ctx, c, task, rep)
				if err != nil {
					return results, err
				}
				results = append(results, res)
				if r.Log != nil {
					r.Log(fmt.Sprintf("%s/%s #%d pass=%v calls=%d tokens=%d cost=$%.4f wall=%.0fs",
						res.Contestant, res.Task, rep, res.Passed, res.Usage.Calls, res.Usage.InputTokens+res.Usage.OutputTokens, res.Usage.CostUSD, res.WallSec))
				}
			}
		}
	}
	return results, nil
}

func (r *Runner) attempt(ctx context.Context, c Contestant, task Task, rep int) (Result, error) {
	dir, err := os.MkdirTemp("", "arena-"+task.ID+"-")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := task.Seed(dir); err != nil {
		return Result{}, fmt.Errorf("seed %s: %w", task.ID, err)
	}
	session := fmt.Sprintf("%s/%s/%d", c.Name(), task.ID, rep)
	r.Proxy.Begin(session, task.MaxCalls)
	wall := r.Config.WallClock
	if wall <= 0 {
		wall = 20 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, wall)
	start := time.Now()
	agentOut, runErr := c.Run(runCtx, r.Config, task, dir)
	cancel()
	res := Result{Task: task.ID, Contestant: c.Name(), Repeat: rep, WallSec: time.Since(start).Seconds(), Usage: r.Proxy.End(session)}
	calls := r.Proxy.Calls(session)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	if runErr != nil {
		res.ExitError = runErr.Error()
	}
	res.Passed, res.GraderOut = grade(ctx, task, dir)
	if r.KeepDir != "" {
		keepAttempt(filepath.Join(r.KeepDir, task.ID, fmt.Sprintf("%s-%d", c.Name(), rep)), dir, agentOut, res.GraderOut, calls)
	}
	return res, nil
}

// keepAttempt preserves an attempt's final workspace (including any agent state such as .soma)
// plus the agent's output, the grader's verdict and a per-call summary, for failure analysis.
func keepAttempt(dest, dir, agentOut, graderOut string, calls []CallRecord) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return
	}
	_ = exec.Command("cp", "-a", dir+"/.", filepath.Join(dest, "workspace")).Run()
	_ = os.WriteFile(filepath.Join(dest, "agent-output.txt"), []byte(agentOut), 0o644)
	_ = os.WriteFile(filepath.Join(dest, "grader-output.txt"), []byte(graderOut), 0o644)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, c := range calls {
		_ = enc.Encode(c)
	}
	_ = os.WriteFile(filepath.Join(dest, "calls.jsonl"), buf.Bytes(), 0o644)
}

// grade runs the held-out grader from outside the workspace.
func grade(ctx context.Context, task Task, dir string) (bool, string) {
	f, err := os.CreateTemp("", "arena-grader-*.py")
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, _ = f.WriteString(task.Grader)
	_ = f.Close()
	gctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// A private bytecode cache: never trust __pycache__ left in the workspace (stale .pyc files
	// can survive same-size edits made within the same second).
	cache, err := os.MkdirTemp("", "arena-pycache-")
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = os.RemoveAll(cache) }()
	cmd := exec.CommandContext(gctx, "python3", f.Name(), dir)
	cmd.Env = append(os.Environ(), "PYTHONPYCACHEPREFIX="+cache)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 2000 {
		text = text[len(text)-2000:]
	}
	return err == nil && strings.HasSuffix(text, "PASS"), text
}
