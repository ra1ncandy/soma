package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
)

// Mode defines one of the three unambiguous replay modes (RFC-0001 Section 3.4).
type Mode string

const (
	ModeExact          Mode = "exact"
	ModeCounterfactual Mode = "counterfactual"
	ModeSandboxBranch  Mode = "sandbox_branch"
)

// ReplayOptions configures how Replay operates on a recorded branch.
type ReplayOptions struct {
	Mode                 Mode
	FromStepID           string
	OverrideModel        string
	OverrideSystemPrompt string
	OverrideProfile      *compiler.ModelProfile
	Provider             dag.ModelProvider
	Tools                dag.ToolRunner
	MaxTurns             int
}

// StepComparison summarizes verification or counterfactual divergence for one step.
type StepComparison struct {
	TurnIndex         int    `json:"turn_index"`
	StepID            string `json:"step_id"`
	FrameHashVerified bool   `json:"frame_hash_verified"`
	OriginalAction    string `json:"original_action"`
	ReplayAction      string `json:"replay_action,omitempty"`
	Diverged          bool   `json:"diverged"`
	OriginalTokens    int    `json:"original_tokens"`
	ReplayTokens      int    `json:"replay_tokens"`
}

// ReplayReport captures the outcome and comparisons of a Replay run.
type ReplayReport struct {
	Mode                Mode             `json:"mode"`
	SourceBranchID      string           `json:"source_branch_id"`
	SandboxBranchID     string           `json:"sandbox_branch_id,omitempty"`
	StepsChecked        int              `json:"steps_checked"`
	HashVerifiedCount   int              `json:"hash_verified_count"`
	Diverged            bool             `json:"diverged"`
	DivergedAtStep      string           `json:"diverged_at_step,omitempty"`
	DivergenceReason    string           `json:"divergence_reason,omitempty"`
	Comparisons         []StepComparison `json:"comparisons"`
	OriginalTotalTokens int              `json:"original_total_tokens"`
	ReplayTotalTokens   int              `json:"replay_total_tokens"`
}

// Replayer executes exact, counterfactual, and sandboxed branch replays over stored Merkle DAGs.
type Replayer struct {
	Store *store.Store
}

// NewReplayer builds a Replayer backed by st.
func NewReplayer(st *store.Store) *Replayer {
	return &Replayer{Store: st}
}

// Replay runs the specified ReplayOptions against branchID.
func (r *Replayer) Replay(ctx context.Context, branchID string, opts ReplayOptions) (*ReplayReport, error) {
	if opts.Mode == "" {
		opts.Mode = ModeExact
	}
	branch, err := r.Store.GetBranch(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if branch.HeadStepID == "" {
		return &ReplayReport{Mode: opts.Mode, SourceBranchID: branchID}, nil
	}
	ancestry, err := r.Store.GetAncestry(ctx, branch.HeadStepID)
	if err != nil {
		return nil, err
	}

	prof := compiler.WorkerProfile()
	if opts.OverrideProfile != nil {
		prof = *opts.OverrideProfile
	}

	report := &ReplayReport{
		Mode:           opts.Mode,
		SourceBranchID: branchID,
	}

	switch opts.Mode {
	case ModeExact:
		for _, s := range ancestry {
			origAct := loadActionKey(r.Store, s)
			toks := s.InputTokens + s.CacheReadTokens + s.OutputTokens
			// Kernel-synthesised steps (user follow-ups, crash recovery, manual reconcile) never sent a
			// compiled frame to a model, so they carry no frame hash to reproduce. Their deltas are
			// still part of every later step's reconstruction, which is verified normally.
			synthetic := s.ModelID == "user" || s.ModelID == "recovery" || s.ModelID == "reconciler"
			var frame compiler.CompiledFrame
			if !synthetic {
				var err error
				frame, err = compiler.ReconstructFrame(ctx, r.Store, s.ID, prof)
				if err != nil && !errors.Is(err, compiler.ErrFrameHashMismatch) {
					return nil, fmt.Errorf("exact replay step %s: %w", s.ID, err)
				}
			}
			verified := synthetic || frame.CompiledFrameHash == s.CompiledFrameHash
			report.StepsChecked++
			if verified {
				report.HashVerifiedCount++
			} else if !report.Diverged {
				report.Diverged = true
				report.DivergedAtStep = s.ID
			}
			report.OriginalTotalTokens += toks
			report.ReplayTotalTokens += frame.TotalTokens
			report.Comparisons = append(report.Comparisons, StepComparison{
				TurnIndex:         s.TurnIndex,
				StepID:            s.ID,
				FrameHashVerified: verified,
				Diverged:          !verified,
				OriginalAction:    origAct,
				ReplayAction:      origAct,
				OriginalTokens:    toks,
				ReplayTokens:      frame.TotalTokens,
			})
		}
		return report, nil

	case ModeCounterfactual:
		if opts.Provider == nil {
			return nil, errors.New("counterfactual replay requires Provider")
		}
		for _, s := range ancestry {
			frame, err := compiler.ReconstructFrame(ctx, r.Store, s.ID, prof)
			if err != nil {
				return nil, fmt.Errorf("counterfactual reconstruct %s: %w", s.ID, err)
			}
			if opts.OverrideSystemPrompt != "" {
				frame.Layer0 = opts.OverrideSystemPrompt + "\n" + frame.Layer0
			}

			modelID := s.ModelID
			if opts.OverrideModel != "" {
				modelID = opts.OverrideModel
			}
			resp, err := opts.Provider.Generate(ctx, dag.GenerateRequest{
				RunID:     s.RunID,
				BranchID:  s.BranchID,
				AccountID: s.AccountID,
				ModelID:   modelID,
				TurnIndex: s.TurnIndex,
				Frame:     frame,
			})
			if err != nil {
				return nil, fmt.Errorf("counterfactual generate at %s: %w", s.ID, err)
			}

			origAct := loadActionKey(r.Store, s)
			replayAct := responseActionKey(resp)
			origToks := s.InputTokens + s.CacheReadTokens + s.OutputTokens
			repToks := resp.InputTokens + resp.CacheReadTokens + resp.OutputTokens

			report.StepsChecked++
			report.HashVerifiedCount++
			report.OriginalTotalTokens += origToks
			report.ReplayTotalTokens += repToks

			diverged := origAct != replayAct
			report.Comparisons = append(report.Comparisons, StepComparison{
				TurnIndex:         s.TurnIndex,
				StepID:            s.ID,
				FrameHashVerified: true,
				OriginalAction:    origAct,
				ReplayAction:      replayAct,
				Diverged:          diverged,
				OriginalTokens:    origToks,
				ReplayTokens:      repToks,
			})

			if diverged {
				report.Diverged = true
				report.DivergedAtStep = s.ID
				report.DivergenceReason = fmt.Sprintf(
					"halted at turn %d (%s): original emitted %s while counterfactual emitted %s (recorded observation invalid beyond divergence)",
					s.TurnIndex, s.ID, origAct, replayAct,
				)
				return report, nil
			}
		}
		return report, nil

	case ModeSandboxBranch:
		if opts.Provider == nil {
			return nil, errors.New("sandbox_branch replay requires Provider")
		}
		fromStep := opts.FromStepID
		if fromStep == "" {
			fromStep = ancestry[0].ID
		}
		exec := dag.NewExecutor(r.Store, opts.Provider, opts.Tools, prof)
		if opts.OverrideSystemPrompt != "" {
			exec.SystemPrompt = opts.OverrideSystemPrompt
		}
		if opts.OverrideModel != "" {
			// Register/reuse a dedicated replay account pointing to OverrideModel if configured,
			// or inherit accounts.
			_ = r.Store.UpsertAccount(ctx, store.Account{
				ID:            "replay_" + opts.OverrideModel,
				Provider:      "replay",
				ModelID:       opts.OverrideModel,
				BillingMode:   "local",
				MaxConcurrent: 4,
			})
			exec.AccountIDs = []string{"replay_" + opts.OverrideModel}
		}

		newBranchID, err := exec.ForkBranch(ctx, branch.RunID, fromStep, "replay-"+string(ModeSandboxBranch))
		if err != nil {
			return nil, fmt.Errorf("fork sandbox branch: %w", err)
		}
		maxTurns := opts.MaxTurns
		if maxTurns <= 0 {
			maxTurns = len(ancestry) + 10
		}
		res, err := exec.ExecuteBranch(ctx, newBranchID, maxTurns)
		if err != nil {
			return nil, fmt.Errorf("execute sandbox branch %s: %w", newBranchID, err)
		}

		for _, s := range ancestry {
			report.OriginalTotalTokens += s.InputTokens + s.CacheReadTokens + s.OutputTokens
		}
		report.SandboxBranchID = newBranchID
		report.StepsChecked = res.TurnsExecuted
		report.ReplayTotalTokens = res.TotalTokens
		return report, nil

	default:
		return nil, fmt.Errorf("unknown replay mode %q", opts.Mode)
	}
}

func loadActionKey(st *store.Store, s store.Step) string {
	b, err := st.Blobs().Get(s.EmittedBlobHash)
	if err != nil {
		return "unknown"
	}
	var ep dag.EmittedPayload
	if err := json.Unmarshal(b, &ep); err != nil {
		return "unknown"
	}
	return responseActionKey(&ep.Response)
}

func responseActionKey(resp *dag.GenerateResponse) string {
	if resp == nil {
		return "nil"
	}
	if resp.Done || resp.ToolCall == nil {
		return "DONE"
	}
	// Canonicalize ArgsJSON when valid JSON so key ordering differences don't cause false divergence.
	normArgs := resp.ToolCall.ArgsJSON
	var v any
	if json.Unmarshal([]byte(resp.ToolCall.ArgsJSON), &v) == nil {
		if cb, err := json.Marshal(v); err == nil {
			normArgs = string(cb)
		}
	}
	sum := sha256.Sum256([]byte(normArgs))
	return fmt.Sprintf("%s:%s", resp.ToolCall.Name, hex.EncodeToString(sum[:8]))
}
