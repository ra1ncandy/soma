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
	"syscall"
	"time"
)

// RunConfig is shared by every contestant in an arena run: same model, same endpoint (through
// the metering proxy), same call budget per task, same wall-clock limit.
type RunConfig struct {
	ProxyBaseURL string
	Model        string
	WallClock    time.Duration
}

// preparer is implemented by contestants that need untimed setup before an attempt (for example
// first-run initialisation that a normal installation has already done). Prepare runs while the
// proxy is in warm-up mode; the returned run is what gets timed and metered.
type preparer interface {
	Prepare(ctx context.Context, cfg RunConfig, task Task) (run func(ctx context.Context, dir string) (string, error), cleanup func(), err error)
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

// somaToolsPythonPath is where a task's Python libraries appear inside soma's sandbox.
const somaToolsPythonPath = "/opt/arena-pylib"

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
	if t := task.Tools; t != nil {
		// soma's sandbox sees only /usr by default; the task's tooling is mounted read-only.
		binds := append([]string(nil), t.ROPaths...)
		if t.PythonPath != "" {
			binds = append(binds, t.PythonPath+":"+somaToolsPythonPath)
			cmd.Env = append(cmd.Env, "SOMA_SANDBOX_ENV=PYTHONPATH="+somaToolsPythonPath)
		}
		cmd.Env = append(cmd.Env, "SOMA_SANDBOX_RO_BINDS="+strings.Join(binds, ","))
	}
	if task.HomeIsWorkspace {
		// Inside soma's sandbox the workspace is always mounted at /workspace.
		sandboxEnv := "HOME=/workspace"
		for i, kv := range cmd.Env {
			if rest, ok := strings.CutPrefix(kv, "SOMA_SANDBOX_ENV="); ok {
				sandboxEnv = rest + ";" + sandboxEnv
				cmd.Env = append(cmd.Env[:i], cmd.Env[i+1:]...)
				break
			}
		}
		cmd.Env = append(cmd.Env, "SOMA_SANDBOX_ENV="+sandboxEnv)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// HermesContestant runs an installed Hermes Agent in one-shot mode with an isolated
// HERMES_HOME per run whose only provider is the metering proxy.
type HermesContestant struct {
	Bin string
	// BaseHome is an installed HERMES_HOME; its code is reused, its state is not.
	BaseHome string
	// ReasoningEffort, when set, is written to Hermes' agent.reasoning_effort; empty keeps
	// Hermes' own default.
	ReasoningEffort string
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

func (c HermesContestant) config(cfg RunConfig, maxTurns int) string {
	// Same budget as soma: Hermes' turn limit equals the task's call budget (the proxy also
	// enforces it). Its default local terminal backend runs on the host.
	effortLine := ""
	if e := strings.TrimSpace(c.ReasoningEffort); e != "" {
		effortLine = fmt.Sprintf("  reasoning_effort: %q\n", e)
	}
	return fmt.Sprintf(`model:
  default: %q
  provider: "custom"
  base_url: %q
  api_key: "arena"
agent:
  max_turns: %d
%sterminal:
  backend: "local"
`, cfg.Model, cfg.ProxyBaseURL, maxTurns, effortLine)
}

// Prepare gives the attempt its own fresh HERMES_HOME and lets Hermes finish its first-run setup
// in it (installing bundled tools, starting its gateway daemon: about 13 seconds) by answering one
// trivial prompt. The runner serves that prompt from the proxy's warm-up mode, so it costs nothing
// and is not timed. A fresh home per attempt keeps Hermes' memory and session history from
// carrying over between attempts; warming it keeps that isolation from being billed to Hermes as
// wall time, which an installed Hermes never pays.
func (c HermesContestant) Prepare(ctx context.Context, cfg RunConfig, task Task) (func(context.Context, string) (string, error), func(), error) {
	home, err := os.MkdirTemp("", "arena-hermes-home-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		stopHermesGateway(home)
		_ = os.RemoveAll(home)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(c.config(cfg, task.MaxCalls)), 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}
	scratch, err := os.MkdirTemp("", "arena-hermes-warmup-")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	warmCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	warm := exec.CommandContext(warmCtx, c.Bin, "-z", "Reply with the single word: ok", "--yolo")
	warm.Dir = scratch
	warm.Env = append(cleanEnv(), "HERMES_HOME="+home)
	_ = warm.Run() // a failed warm-up only means the attempt starts cold
	cancel()
	_ = os.RemoveAll(scratch)

	run := func(ctx context.Context, dir string) (string, error) {
		// --yolo because one-shot mode denies prompts that nobody can answer; Hermes' hardline
		// blocklist still applies.
		cmd := exec.CommandContext(ctx, c.Bin, "-z", task.Goal, "--yolo")
		cmd.Dir = dir
		cmd.Env = append(cleanEnv(), "HERMES_HOME="+home)
		if t := task.Tools; t != nil && t.PythonPath != "" {
			// Hermes runs commands on the host, so the libraries only need to be importable.
			cmd.Env = append(cmd.Env, "PYTHONPATH="+t.PythonPath)
		}
		if task.HomeIsWorkspace {
			cmd.Env = append(cmd.Env, "HOME="+dir)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return run, cleanup, nil
}

// Run runs one attempt without the untimed warm-up (used when the caller does not prepare).
func (c HermesContestant) Run(ctx context.Context, cfg RunConfig, task Task, dir string) (string, error) {
	run, cleanup, err := c.Prepare(ctx, cfg, task)
	if err != nil {
		return "", err
	}
	defer cleanup()
	return run(ctx, dir)
}

// stopHermesGateway stops the gateway daemon Hermes started for this home, so attempts do not
// leave daemons behind.
func stopHermesGateway(home string) {
	raw, err := os.ReadFile(filepath.Join(home, "gateway.pid"))
	if err != nil {
		return
	}
	var info struct {
		PID  int    `json:"pid"`
		Home string `json:"hermes_home"`
	}
	if json.Unmarshal(raw, &info) != nil || info.PID <= 1 || info.Home != home {
		return
	}
	proc, err := os.FindProcess(info.PID)
	if err != nil || proc.Signal(syscall.SIGTERM) != nil {
		return
	}
	// Give it a moment to exit, so it does not write into the home while it is being removed.
	for i := 0; i < 30 && proc.Signal(syscall.Signal(0)) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
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
	run := func(ctx context.Context, dir string) (string, error) { return c.Run(ctx, r.Config, task, dir) }
	if p, ok := c.(preparer); ok {
		r.Proxy.BeginWarmup()
		prepared, cleanup, err := p.Prepare(ctx, r.Config, task)
		if err != nil {
			return Result{}, fmt.Errorf("prepare %s: %w", c.Name(), err)
		}
		defer cleanup()
		run = prepared
	}
	session := fmt.Sprintf("%s/%s/%d", c.Name(), task.ID, rep)
	r.Proxy.Begin(session, task.MaxCalls)
	wall := r.Config.WallClock
	if wall <= 0 {
		wall = 20 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, wall)
	start := time.Now()
	agentOut, runErr := run(runCtx, dir)
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
	if task.GradeFunc != nil {
		return task.GradeFunc(ctx, dir)
	}
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
