package dag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// SearchConfig configures bounded speculative branching and Graveyard backtracking.
type SearchConfig struct {
	MaxBranches       int
	MaxTurnsPerBranch int
	AnchorTurn        int // 1-indexed step on the root branch to fork siblings from (default 1)
}

func (c SearchConfig) withDefaults() SearchConfig {
	if c.MaxBranches <= 0 {
		c.MaxBranches = 4
	}
	if c.MaxTurnsPerBranch <= 0 {
		c.MaxTurnsPerBranch = 8
	}
	if c.AnchorTurn <= 0 {
		c.AnchorTurn = 1
	}
	return c
}

// SearchAttempt summarizes one explored branch during BranchSearcher execution.
type SearchAttempt struct {
	BranchID      string `json:"branch_id"`
	Label         string `json:"label"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
	Turns         int    `json:"turns"`
	Rollovers     int    `json:"rollovers"`
	Passed        bool   `json:"passed"`
}

// SearchResult captures all explored hypotheses, Graveyard entries, and winning branch.
type SearchResult struct {
	RunID           string          `json:"run_id"`
	WinningBranchID string          `json:"winning_branch_id,omitempty"`
	Completed       bool            `json:"completed"`
	Attempts        []SearchAttempt `json:"attempts"`
	GraveyardCount  int             `json:"graveyard_count"`
	TotalTurns      int             `json:"total_turns"`
}

// BranchSearcher orchestrates bounded exploration over isolated branch workspaces,
// abandoning stalled or oracle-rejected paths to the Graveyard before forking siblings.
type BranchSearcher struct {
	Executor *Executor
}

// NewBranchSearcher wraps exec with Graveyard-guided sibling branching.
func NewBranchSearcher(exec *Executor) *BranchSearcher {
	return &BranchSearcher{Executor: exec}
}

// Search starts a new Run and explores up to cfg.MaxBranches branches until completion.
func (s *BranchSearcher) Search(ctx context.Context, req RunRequest, cfg SearchConfig) (SearchResult, error) {
	if s.Executor == nil {
		return SearchResult{}, errors.New("dag: BranchSearcher requires non-nil Executor")
	}
	runID, rootBranchID, err := s.Executor.StartRun(ctx, req)
	if err != nil {
		return SearchResult{}, err
	}
	return s.SearchExistingRun(ctx, runID, rootBranchID, cfg)
}

// SearchExistingRun executes and forks branches within an existing runID (useful when external Contracts
// are attached after StartRun).
func (s *BranchSearcher) SearchExistingRun(ctx context.Context, runID, rootBranchID string, cfg SearchConfig) (SearchResult, error) {
	if s.Executor == nil {
		return SearchResult{}, errors.New("dag: BranchSearcher requires non-nil Executor")
	}
	cfg = cfg.withDefaults()
	prevStallFlag := s.Executor.EnforceStallBreaker
	s.Executor.EnforceStallBreaker = true
	defer func() {
		s.Executor.EnforceStallBreaker = prevStallFlag
	}()

	out := SearchResult{RunID: runID}
	curBranchID := rootBranchID
	curLabel := "main"
	if br, err := s.Executor.Store.GetBranch(ctx, curBranchID); err == nil && br.Label != "" {
		curLabel = br.Label
	}

	for attempt := 1; attempt <= cfg.MaxBranches; attempt++ {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}

		res, execErr := s.Executor.ExecuteBranch(ctx, curBranchID, cfg.MaxTurnsPerBranch)
		att := SearchAttempt{
			BranchID: curBranchID,
			Label:    curLabel,
		}
		if res != nil {
			att.Status = string(res.Status)
			att.Turns = res.TurnsExecuted
			att.Rollovers = res.Rollovers
			out.TotalTurns += res.TurnsExecuted
		}

		if execErr == nil && res != nil && res.Status == StatusCompleted {
			att.Passed = true
			out.Attempts = append(out.Attempts, att)
			out.WinningBranchID = curBranchID
			out.Completed = true
			break
		}

		var reason string
		if execErr != nil {
			reason = execErr.Error()
		} else {
			reason = fmt.Sprintf("exhausted %d turn(s) with status %s", cfg.MaxTurnsPerBranch, att.Status)
		}
		att.FailureReason = reason
		out.Attempts = append(out.Attempts, att)

		// Suspend cleanly without abandoning when blocked by quota or operator gates.
		var sqErr *SuspendedQuotaError
		var saErr *SuspendedApprovalError
		var srErr *SuspendedReconcileError
		if errors.As(execErr, &sqErr) || errors.As(execErr, &saErr) || errors.As(execErr, &srErr) {
			break
		}

		_ = s.Executor.AbandonBranch(ctx, curBranchID, reason, "")

		if attempt == cfg.MaxBranches {
			break
		}

		nextLabel := fmt.Sprintf("branch-%d", attempt+1)
		anchorStepID := ""
		if rootSteps, err := s.Executor.Store.ListBranchSteps(ctx, rootBranchID); err == nil && len(rootSteps) > 0 {
			idx := cfg.AnchorTurn - 1
			if idx < 0 {
				idx = 0
			}
			if idx >= len(rootSteps) {
				idx = len(rootSteps) - 1
			}
			anchorStepID = rootSteps[idx].ID
		}

		var nextBranchID string
		var forkErr error
		if anchorStepID != "" {
			nextBranchID, forkErr = s.Executor.ForkBranch(ctx, runID, anchorStepID, nextLabel)
		} else {
			run, rErr := s.Executor.Store.GetRun(ctx, runID)
			if rErr != nil {
				forkErr = rErr
			} else {
				nextBranchID = fmt.Sprintf("br_%x_%04d", time.Now().UnixNano(), s.Executor.seq.Add(1))
				nextBr := store.Branch{
					ID:     nextBranchID,
					RunID:  runID,
					Label:  nextLabel,
					Epoch:  1,
					Status: string(StatusQueued),
				}
				if err := s.Executor.Store.CreateBranch(ctx, nextBr); err != nil {
					forkErr = err
				} else if _, _, err := s.Executor.Workspace.PrepareBranchWorkspace(ctx, s.Executor.Store, run, nextBr); err != nil {
					forkErr = err
				}
			}
		}
		if forkErr != nil {
			return out, fmt.Errorf("fork sibling %s: %w", nextLabel, forkErr)
		}
		curBranchID = nextBranchID
		curLabel = nextLabel
	}

	if grave, err := s.Executor.Store.ListActiveMemoryByScopes(ctx, []string{"repo:" + runID}, []string{"negative_path"}); err == nil {
		out.GraveyardCount = len(grave)
	}
	return out, nil
}
