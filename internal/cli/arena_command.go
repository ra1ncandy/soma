package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/arena"
	"github.com/ra1ncandy/soma/internal/dag"
)

func newArenaCmd() *cobra.Command {
	root := &cobra.Command{Use: "arena", Short: "Measured head-to-head runs of soma against other agent harnesses"}
	var (
		taskIDs, tier, contestants, baseURL, model, keyEnv, somaBin, hermesBin, hermesHome, outDir string
		repeats, wallMinutes                                                                       int
		capUSD, priceIn, priceCached, priceOut, priceCacheWrite                                    float64
		keep                                                                                       bool
		somaEnv                                                                                    []string
		hermesEffort                                                                               string
		osworldDir                                                                                 string
		hermesUncontained                                                                          bool
		memoryMax                                                                                  string
		limit                                                                                      int
	)
	run := &cobra.Command{
		Use:   "run",
		Short: "Run every contestant on the same tasks, same model and same budgets; write a measured report",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if baseURL == "" || model == "" {
				return errors.New("--base-url and --model are required")
			}
			if capUSD <= 0 {
				return errors.New("--spend-cap-usd is required and must be positive")
			}
			key := os.Getenv(keyEnv)
			if key == "" {
				return fmt.Errorf("set the API key in $%s (it is injected by the proxy; contestants never see it)", keyEnv)
			}
			memoryNote := "Each contestant's process tree is limited to " + memoryMax + " of memory (no swap)."
			if strings.TrimSpace(memoryMax) == "" {
				memoryNote = "No memory cap was applied to contestants."
			} else if !arena.MemoryCapAvailable() {
				memoryMax = ""
				memoryNote = "No memory cap was applied to contestants: this machine has no systemd user session that accepts transient scopes."
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: --contestant-memory-max cannot be enforced here (no usable systemd user session); contestants run without a memory cap")
			}
			tasks, err := selectArenaTasks(tier, taskIDs, osworldDir, limit)
			if err != nil {
				return err
			}
			if somaBin == "" {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				somaBin = exe
			}
			var cs []arena.Contestant
			for _, name := range strings.Split(contestants, ",") {
				switch strings.TrimSpace(name) {
				case "soma":
					for _, kv := range somaEnv {
						k, _, ok := strings.Cut(kv, "=")
						if u := strings.ToUpper(k); !ok || !strings.HasPrefix(u, "SOMA_") || strings.Contains(u, "KEY") {
							return fmt.Errorf("--soma-env %q: want SOMA_NAME=value, and never a credential", kv)
						}
					}
					cs = append(cs, arena.SomaContestant{Bin: somaBin, Env: somaEnv})
				case "hermes":
					if hermesBin == "" {
						return errors.New("--hermes-bin is required for the hermes contestant")
					}
					cs = append(cs, arena.HermesContestant{Bin: hermesBin, BaseHome: hermesHome, ReasoningEffort: hermesEffort, Uncontained: hermesUncontained})
				default:
					return fmt.Errorf("unknown contestant %q (soma, hermes)", name)
				}
			}
			proxy, err := arena.NewProxy(baseURL, key, arena.Price{Input: priceIn, Cached: priceCached, CacheWrite: priceCacheWrite, Output: priceOut}, capUSD)
			if err != nil {
				return err
			}
			defer func() { _ = proxy.Close() }()
			out := cmd.OutOrStdout()
			runner := &arena.Runner{
				Proxy:       proxy,
				Config:      arena.RunConfig{ProxyBaseURL: proxy.BaseURL(), Model: model, WallClock: time.Duration(wallMinutes) * time.Minute, MemoryMax: memoryMax, Anthropic: proxy.AnthropicUpstream()},
				Contestants: cs,
				Tasks:       tasks,
				Repeats:     repeats,
				Log:         func(s string) { fmt.Fprintln(out, s) },
				KeepDir:     keepDir(keep, outDir),
			}
			results, runErr := runner.Run(cmd.Context())
			versions := map[string]string{}
			for _, c := range cs {
				versions[c.Name()] = c.Version()
			}
			host := ""
			if u, err := url.Parse(baseURL); err == nil {
				host = u.Host
			}
			ids := make([]string, len(tasks))
			for i, t := range tasks {
				ids[i] = t.ID
			}
			rep := arena.Report{
				GeneratedAt: time.Now(), Model: model, Upstream: host, Tasks: ids, Repeats: repeats,
				Summaries: arena.Summarize(results, versions), Results: results,
				Notes: []string{
					"Both contestants reach the model only through the arena proxy, which counts calls, tokens and cost and enforces each task's call budget and the run's spend cap. Provider credentials are stripped from their environments.",
					"Both receive the same goal text; graders are held out and run after the contestant exits.",
					"soma runs `soma ask` (OS-sandboxed shell without network, isolated workspace applied to the project on completion). Hermes runs `hermes -z` one-shot with --yolo (its hardline blocklist still applies) on its default local terminal backend. " + hermesContainmentNote(hermesUncontained),
					"Hermes' turn limit (agent.max_turns) and soma's --max-turns are both set to the task's call budget.",
					"Each Hermes attempt gets a fresh HERMES_HOME (no memory or history from other attempts). Before the clock starts, Hermes answers one trivial prompt served locally by the proxy, so its first-run setup and gateway start-up are neither timed nor billed. soma has no such setup; its start-up is inside its timed run.",
					hermesEffortNote(hermesEffort),
					memoryNote,
					fmt.Sprintf("Prices used for the spend cap (USD per 1M tokens): input %.4g, cached input %.4g, output %.4g.", priceIn, priceCached, priceOut),
				},
			}
			for _, t := range tasks {
				if t.Tier == "osworld" {
					rep.Notes = append(rep.Notes, "Tasks named osw-* are OSWorld tasks replicated on plain files: OSWorld's instruction (plus a note that there is no GUI and that the working directory is the home directory), its starting files and its own checker functions, but no desktop VM. Only tasks whose checker reads files are included, and only those where the checker rejects the starting files and was shown to be passable (OSWorld's gold file, the values a settings rule asks for, or a held-out reference solution). LibreOffice is not involved: its task domains are left out and nothing runs it. Tasks that need the network are left out. An attempt passes when the checker's score is at least 0.9 (most checkers answer 0 or 1; a few return a similarity); the raw score is in the per-attempt grader output. These scores are not comparable with the OSWorld leaderboard.")
					break
				}
			}
			if runErr != nil {
				rep.Notes = append(rep.Notes, "Run stopped early: "+runErr.Error())
			}
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			raw, _ := json.MarshalIndent(rep, "", "  ")
			if err := os.WriteFile(filepath.Join(outDir, "arena-report.json"), raw, 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(outDir, "arena-report.md"), []byte(rep.Markdown()), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(out, "\n%s\nReport: %s\n", rep.Markdown(), filepath.Join(outDir, "arena-report.md"))
			return runErr
		},
	}
	run.Flags().StringVar(&taskIDs, "tasks", "", "Comma-separated task ids (overrides --tier)")
	run.Flags().StringVar(&tier, "tier", "starter", "Task tier: starter, hard, all, or osworld (OSWorld tasks replicated on plain files, no VM)")
	run.Flags().StringVar(&osworldDir, "osworld-dir", "", "Prepared OSWorld directory for --tier osworld (default ~/.local/share/soma-arena/osworld)")
	run.Flags().IntVar(&limit, "limit", 0, "Run at most this many tasks, spread evenly over the selected ones (0 = all)")
	run.Flags().BoolVar(&keep, "keep", false, "Keep every attempt's workspace, agent output and grader output under <out>/attempts")
	run.Flags().StringVar(&contestants, "contestants", "soma,hermes", "Contestants to run")
	run.Flags().StringVar(&baseURL, "base-url", "", "Upstream OpenAI-compatible base URL (e.g. https://openrouter.ai/api/v1)")
	run.Flags().StringVar(&model, "model", "", "Model id both contestants use")
	run.Flags().StringVar(&keyEnv, "api-key-env", "SOMA_ARENA_API_KEY", "Environment variable holding the upstream API key")
	run.Flags().IntVar(&repeats, "repeats", 1, "Attempts per task per contestant")
	run.Flags().IntVar(&wallMinutes, "wall-clock-minutes", 20, "Wall-clock limit per attempt")
	run.Flags().Float64Var(&capUSD, "spend-cap-usd", 0, "Hard spend cap for the whole run (required)")
	run.Flags().Float64Var(&priceIn, "price-in", 0, "USD per 1M uncached input tokens (for the spend cap)")
	run.Flags().Float64Var(&priceCached, "price-cached", 0, "USD per 1M cached input tokens")
	run.Flags().Float64Var(&priceCacheWrite, "price-cache-write", 0, "USD per 1M tokens written to the cache (default 1.25x --price-in)")
	run.Flags().Float64Var(&priceOut, "price-out", 0, "USD per 1M output tokens")
	run.Flags().StringVar(&somaBin, "soma-bin", "", "soma binary (default: this binary)")
	run.Flags().StringArrayVar(&somaEnv, "soma-env", nil, "Extra SOMA_NAME=value setting for the soma contestant (repeatable), for comparing configurations")
	run.Flags().StringVar(&hermesBin, "hermes-bin", "", "Installed hermes launcher")
	run.Flags().StringVar(&memoryMax, "contestant-memory-max", "8G", "Memory cap for each contestant's process tree (systemd scope); empty disables it")
	run.Flags().BoolVar(&hermesUncontained, "hermes-uncontained", false, "Run Hermes directly on this machine instead of in a mount namespace that hides your home directory (it runs with --yolo: it can then write anywhere you can)")
	run.Flags().StringVar(&hermesEffort, "hermes-reasoning-effort", "", "Set Hermes' agent.reasoning_effort (default: leave Hermes' own default)")
	run.Flags().StringVar(&hermesHome, "hermes-home", "", "Installed HERMES_HOME (for --version)")
	run.Flags().StringVar(&outDir, "out", "arena-results", "Output directory for arena-report.{json,md}")
	root.AddCommand(run)
	return root
}

// selectArenaTasks resolves --tier / --tasks (and --limit) to a task list. Tasks whose id starts
// with "osw-" and the osworld tier come from the prepared OSWorld directory.
func selectArenaTasks(tier, taskIDs, osworldDir string, limit int) ([]arena.Task, error) {
	var osworld []arena.Task
	loadOSWorld := func() error {
		if osworld != nil {
			return nil
		}
		dir := osworldDir
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			dir = filepath.Join(home, ".local", "share", "soma-arena", "osworld")
		}
		var err error
		osworld, err = arena.OSWorldTasks(dir)
		return err
	}
	var tasks []arena.Task
	switch {
	case taskIDs != "":
		for _, id := range strings.Split(taskIDs, ",") {
			id = strings.TrimSpace(id)
			if t, ok := arena.TaskByID(id); ok {
				tasks = append(tasks, t)
				continue
			}
			found := false
			if strings.HasPrefix(id, "osw-") {
				if err := loadOSWorld(); err != nil {
					return nil, err
				}
				for _, t := range osworld {
					if t.ID == id {
						tasks, found = append(tasks, t), true
					}
				}
			}
			if !found {
				return nil, fmt.Errorf("unknown task %q", id)
			}
		}
	case tier == "osworld":
		if err := loadOSWorld(); err != nil {
			return nil, err
		}
		tasks = osworld
	default:
		tasks = arena.TasksForTier(tier)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks for tier %q (starter, hard, all, osworld)", tier)
	}
	if limit > 0 && limit < len(tasks) {
		// Evenly spaced, so a limited run still covers every part of the list.
		picked := make([]arena.Task, 0, limit)
		for i := 0; i < limit; i++ {
			picked = append(picked, tasks[i*len(tasks)/limit])
		}
		tasks = picked
	}
	return tasks, nil
}

func hermesContainmentNote(uncontained bool) string {
	if uncontained {
		return "Hermes ran directly on the host, with the operator's files and network."
	}
	return "Hermes ran in a mount namespace: it sees the host's programs read-only and has network access, but the operator's home directory is hidden and it can write only to the task workspace, its own state directory and a private /tmp."
}

func hermesEffortNote(effort string) string {
	if strings.TrimSpace(effort) == "" {
		return "Reasoning effort: each contestant uses its own default (soma: " + dag.DefaultReasoningEffort + "; Hermes: its built-in default)."
	}
	return "Reasoning effort: soma uses its default (" + dag.DefaultReasoningEffort + "); Hermes was set to " + strings.TrimSpace(effort) + " with --hermes-reasoning-effort."
}

func keepDir(keep bool, outDir string) string {
	if !keep {
		return ""
	}
	return filepath.Join(outDir, "attempts")
}
