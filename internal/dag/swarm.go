package dag

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/workspace"
)

// SwarmMode selects cooperative sub-goal decomposition (fan-out + 3-way merge + root oracle)
// or competitive best-of-K speculation (parallel branches + live Graveyard broadcasts).
type SwarmMode string

const (
	SwarmModeCooperative SwarmMode = "cooperative"
	SwarmModeCompetitive SwarmMode = "competitive"
)

// SwarmTask defines one concurrent agent worker's assignment in a SwarmRequest.
type SwarmTask struct {
	Label     string
	SubGoal   string
	AccountID string
	Provider  ModelProvider
	Verifier  ContractVerifier
	MaxTurns  int
}

// SwarmRequest configures a concurrent multi-agent swarm run.
type SwarmRequest struct {
	RunRequest       RunRequest
	Mode             SwarmMode
	Tasks            []SwarmTask
	DefaultMaxTurns  int
	CancelOnFirstWin bool
}

// SwarmWorkerResult captures the outcome of a single concurrent worker branch.
type SwarmWorkerResult struct {
	BranchID     string       `json:"branch_id"`
	Label        string       `json:"label"`
	SubGoal      string       `json:"sub_goal,omitempty"`
	WorkspaceDir string       `json:"workspace_dir"`
	Status       BranchStatus `json:"status"`
	Turns        int          `json:"turns"`
	Rollovers    int          `json:"rollovers"`
	TotalTokens  int          `json:"total_tokens"`
	CachedTokens int          `json:"cached_tokens"`
	Passed       bool         `json:"passed"`
	ErrorText    string       `json:"error_text,omitempty"`
}

// SwarmResult summarizes the outcome of a concurrent multi-agent SwarmCoordinator execution.
type SwarmResult struct {
	RunID             string                `json:"run_id"`
	RootBranchID      string                `json:"root_branch_id"`
	WinningBranchID   string                `json:"winning_branch_id,omitempty"`
	Mode              SwarmMode             `json:"mode"`
	Completed         bool                  `json:"completed"`
	Workers           []SwarmWorkerResult   `json:"workers"`
	MergeReport       workspace.MergeReport `json:"merge_report"`
	BlackboardNotices int                   `json:"blackboard_notices"`
	GraveyardCount    int                   `json:"graveyard_count"`
	TotalTurns        int                   `json:"total_turns"`
	OracleSummary     VerifySummary         `json:"oracle_summary"`
}

// SwarmCoordinator orchestrates concurrent worker branches sharing a Blackboard, @page store,
// and QuotaScheduler across isolated per-branch workspaces.
type SwarmCoordinator struct {
	Executor *Executor
}

// NewSwarmCoordinator creates a multi-agent coordinator backed by exec.
func NewSwarmCoordinator(exec *Executor) *SwarmCoordinator {
	if exec != nil && exec.Blackboard == nil {
		exec.Blackboard = NewBlackboard()
	}
	return &SwarmCoordinator{Executor: exec}
}

// Run starts a new Run and executes req.Tasks concurrently.
func (c *SwarmCoordinator) Run(ctx context.Context, req SwarmRequest) (SwarmResult, error) {
	if c.Executor == nil {
		return SwarmResult{}, errors.New("dag: SwarmCoordinator requires non-nil Executor")
	}
	runID, rootBranchID, err := c.Executor.StartRun(ctx, req.RunRequest)
	if err != nil {
		return SwarmResult{}, err
	}
	return c.RunExisting(ctx, runID, rootBranchID, req)
}

// RunExisting executes req.Tasks concurrently against an existing runID and rootBranchID.
func (c *SwarmCoordinator) RunExisting(ctx context.Context, runID, rootBranchID string, req SwarmRequest) (SwarmResult, error) {
	if c.Executor == nil {
		return SwarmResult{}, errors.New("dag: SwarmCoordinator requires non-nil Executor")
	}
	if len(req.Tasks) == 0 {
		return SwarmResult{}, errors.New("dag: SwarmRequest requires at least one SwarmTask")
	}
	mode := req.Mode
	if mode == "" {
		mode = SwarmModeCooperative
	}
	defaultMaxTurns := req.DefaultMaxTurns
	if defaultMaxTurns <= 0 {
		defaultMaxTurns = 10
	}
	if c.Executor.Blackboard == nil {
		c.Executor.Blackboard = NewBlackboard()
	}

	run, err := c.Executor.Store.GetRun(ctx, runID)
	if err != nil {
		return SwarmResult{}, err
	}
	rootBr, err := c.Executor.Store.GetBranch(ctx, rootBranchID)
	if err != nil {
		return SwarmResult{}, err
	}

	// Provision isolated branch + workspace for each concurrent worker before launching goroutines.
	type spawnedWorker struct {
		idx      int
		task     SwarmTask
		branch   store.Branch
		maxTurns int
	}
	spawned := make([]spawnedWorker, len(req.Tasks))
	for i, task := range req.Tasks {
		label := task.Label
		if label == "" {
			label = fmt.Sprintf("worker-%d", i+1)
		}
		task.Label = label
		maxT := task.MaxTurns
		if maxT <= 0 {
			maxT = defaultMaxTurns
		}

		bID := fmt.Sprintf("br_%x_%04d", time.Now().UnixNano(), c.Executor.seq.Add(1))
		wBr := store.Branch{
			ID:              bID,
			RunID:           runID,
			ParentBranchID:  rootBranchID,
			Label:           label,
			Epoch:           1,
			Status:          string(StatusQueued),
			ActiveAccountID: task.AccountID,
		}
		if err := c.Executor.Store.CreateBranch(ctx, wBr); err != nil {
			return SwarmResult{}, fmt.Errorf("create swarm branch %s: %w", label, err)
		}
		wsDir, strat, err := c.Executor.Workspace.PrepareBranchWorkspace(ctx, c.Executor.Store, run, wBr)
		if err != nil {
			return SwarmResult{}, fmt.Errorf("prepare swarm workspace %s: %w", label, err)
		}
		wBr.WorkspaceDir = wsDir
		wBr.WorkspaceStrategy = strat

		// Queue the worker's specific sub-goal into its Layer 2 Blackboard inbox so all workers
		// share the exact same frozen Layer 0 + Layer 1 KV-cache prefix.
		if task.SubGoal != "" {
			_, _ = c.Executor.Blackboard.Publish(ctx, c.Executor.Store, PeerFinding{
				ID:              fmt.Sprintf("subgoal-%s-%s", bID, label),
				RunID:           runID,
				FromBranch:      rootBranchID,
				FromLabel:       "coordinator",
				TargetBranch:    bID,
				Kind:            "rule_notice",
				Summary:         fmt.Sprintf("Assigned worker sub-goal [%s]: %s", label, task.SubGoal),
				PersistToMemory: false,
			})
		}

		spawned[i] = spawnedWorker{
			idx:      i,
			task:     task,
			branch:   wBr,
			maxTurns: maxT,
		}
	}

	out := SwarmResult{
		RunID:        runID,
		RootBranchID: rootBranchID,
		Mode:         mode,
		Workers:      make([]SwarmWorkerResult, len(spawned)),
	}

	swarmCtx, cancelSwarm := context.WithCancel(ctx)
	defer cancelSwarm()

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, sw := range spawned {
		wg.Add(1)
		go func(w spawnedWorker) {
			defer wg.Done()

			wExec := c.cloneWorkerExecutor(w.task, mode)
			res, execErr := wExec.ExecuteBranch(swarmCtx, w.branch.ID, w.maxTurns)

			wRes := SwarmWorkerResult{
				BranchID:     w.branch.ID,
				Label:        w.task.Label,
				SubGoal:      w.task.SubGoal,
				WorkspaceDir: w.branch.WorkspaceDir,
			}
			if res != nil {
				wRes.Status = res.Status
				wRes.Turns = res.TurnsExecuted
				wRes.Rollovers = res.Rollovers
				wRes.TotalTokens = res.TotalTokens
				wRes.CachedTokens = res.CacheReadToks
			}
			if execErr != nil {
				wRes.ErrorText = execErr.Error()
			}

			if execErr == nil && res != nil && res.Status == StatusCompleted {
				wRes.Passed = true
				mu.Lock()
				out.Workers[w.idx] = wRes
				out.TotalTurns += wRes.Turns
				if mode == SwarmModeCompetitive && out.WinningBranchID == "" {
					out.WinningBranchID = w.branch.ID
					out.Completed = true
					if req.CancelOnFirstWin {
						cancelSwarm()
					}
				}
				mu.Unlock()
				return
			}

			// In Competitive mode, failed or stalled hypotheses immediately broadcast a Graveyard warning
			// to all still-running sibling workers via AbandonBranch.
			if mode == SwarmModeCompetitive && !errors.Is(execErr, context.Canceled) {
				reason := wRes.ErrorText
				if reason == "" {
					reason = fmt.Sprintf("worker %s stopped with status %s", w.task.Label, wRes.Status)
				}
				_ = wExec.AbandonBranch(ctx, w.branch.ID, reason, "")
				wRes.Status = StatusAbandoned
			}

			mu.Lock()
			out.Workers[w.idx] = wRes
			out.TotalTurns += wRes.Turns
			mu.Unlock()
		}(sw)
	}

	wg.Wait()

	out.BlackboardNotices = len(c.Executor.Blackboard.Snapshot(runID))
	if grave, err := c.Executor.Store.ListActiveMemoryByScopes(ctx, []string{"repo:" + runID}, []string{"negative_path"}); err == nil {
		out.GraveyardCount = len(grave)
	}

	if mode == SwarmModeCompetitive {
		if out.Completed {
			_ = c.Executor.Store.UpdateRunStatus(ctx, runID, string(StatusCompleted))
		}
		return out, nil
	}

	// Cooperative Mode: verify all workers succeeded, merge their isolated workspaces into rootBr.WorkspaceDir,
	// and evaluate the root ContractVerifier oracle against the merged integration workspace.
	var workerDirs []string
	var lastStepID string
	for _, w := range out.Workers {
		if !w.Passed {
			return out, fmt.Errorf("cooperative worker %s (%s) did not complete cleanly: status=%s err=%s",
				w.Label, w.BranchID, w.Status, w.ErrorText)
		}
		workerDirs = append(workerDirs, w.WorkspaceDir)
		if br, bErr := c.Executor.Store.GetBranch(ctx, w.BranchID); bErr == nil && br.HeadStepID != "" {
			lastStepID = br.HeadStepID
		}
	}

	mergeRep, mergeErr := c.Executor.Workspace.MergeBranchWorkspaces(ctx, rootBr.WorkspaceDir, workerDirs, rootBr.WorkspaceDir)
	out.MergeReport = mergeRep
	if mergeErr != nil {
		_ = c.Executor.Store.UpdateBranchStatus(ctx, rootBranchID, string(StatusFailed), rootBr.ActiveAccountID, 0)
		_ = c.Executor.Store.UpdateRunStatus(ctx, runID, string(StatusFailed))
		return out, mergeErr
	}

	if c.Executor.Verifier != nil && lastStepID != "" {
		vSum, vErr := c.Executor.Verifier.VerifyBranch(ctx, run, rootBr, lastStepID)
		out.OracleSummary = vSum
		if vErr != nil {
			_ = c.Executor.Store.UpdateBranchStatus(ctx, rootBranchID, string(StatusFailed), rootBr.ActiveAccountID, 0)
			_ = c.Executor.Store.UpdateRunStatus(ctx, runID, string(StatusFailed))
			return out, vErr
		}
		if !vSum.AllPassed {
			_ = c.Executor.Store.UpdateBranchStatus(ctx, rootBranchID, string(StatusFailed), rootBr.ActiveAccountID, 0)
			_ = c.Executor.Store.UpdateRunStatus(ctx, runID, string(StatusFailed))
			return out, &VerificationFailedError{BranchID: rootBranchID, Counterexample: vSum.Counterexample}
		}
	} else {
		out.OracleSummary = VerifySummary{AllPassed: true}
	}

	_ = c.Executor.Store.UpdateBranchStatus(ctx, rootBranchID, string(StatusCompleted), rootBr.ActiveAccountID, 0)
	_ = c.Executor.Store.UpdateRunStatus(ctx, runID, string(StatusCompleted))
	out.WinningBranchID = rootBranchID
	out.Completed = true
	return out, nil
}

func (c *SwarmCoordinator) cloneWorkerExecutor(task SwarmTask, mode SwarmMode) *Executor {
	base := c.Executor
	prov := base.Provider
	if task.Provider != nil {
		prov = task.Provider
	}
	accs := append([]string(nil), base.AccountIDs...)
	if task.AccountID != "" {
		accs = append([]string{task.AccountID}, accs...)
	}
	var verifier ContractVerifier
	if mode == SwarmModeCompetitive {
		verifier = base.Verifier
	}
	if task.Verifier != nil {
		verifier = task.Verifier
	}

	tools := base.Tools
	if lt, ok := tools.(*LocalToolRunner); ok {
		clonedLT := *lt
		clonedLT.Blackboard = base.Blackboard
		tools = &clonedLT
	}

	return &Executor{
		Store:               base.Store,
		VM:                  base.VM,
		Memory:              base.Memory,
		Blackboard:          base.Blackboard,
		Workspace:           base.Workspace,
		Scheduler:           base.Scheduler,
		Provider:            prov,
		Tools:               tools,
		Reconciler:          base.Reconciler,
		Verifier:            verifier,
		Profile:             base.Profile,
		SystemPrompt:        base.SystemPrompt,
		MaxLayer1Memories:   base.MaxLayer1Memories,
		AccountIDs:          accs,
		AllowFailover:       true,
		EnforceStallBreaker: mode == SwarmModeCompetitive || base.EnforceStallBreaker,
	}
}
