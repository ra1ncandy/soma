package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
)

// ExportOptions configures SFT and contrastive DPO/GRPO trajectory exports.
type ExportOptions struct {
	RunID           string // empty = export across all runs in store
	VerifiedOnly    bool   // only include branches that completed (and passed all attached contracts)
	RequireContract bool   // require at least one attached external verification contract to pass
}

// TrajectoryTurn represents a single reconstructed turn (reasoning, tool calls, and observations).
type TrajectoryTurn struct {
	TurnIndex         int                `json:"turn_index"`
	StepID            string             `json:"step_id"`
	CompiledFrameHash string             `json:"compiled_frame_hash"`
	Reasoning         string             `json:"reasoning,omitempty"`
	ToolCalls         []dag.ToolCallSpec `json:"tool_calls,omitempty"`
	Observations      []string           `json:"observations,omitempty"`
	Done              bool               `json:"done"`
	FinalSummary      string             `json:"final_summary,omitempty"`
}

// ShareGPTMessage represents a single turn in ShareGPT / OpenAI tool-calling training format.
type ShareGPTMessage struct {
	Role      string             `json:"role"` // system | user | assistant | tool
	Content   string             `json:"content"`
	ToolCalls []dag.ToolCallSpec `json:"tool_calls,omitempty"`
}

// SFTExample represents a supervised fine-tuning trajectory reconstructed from a soma branch.
type SFTExample struct {
	RunID          string            `json:"run_id"`
	BranchID       string            `json:"branch_id"`
	BranchLabel    string            `json:"branch_label"`
	GoalSpec       string            `json:"goal_spec"`
	ModelID        string            `json:"model_id"`
	Status         string            `json:"status"`
	OracleVerified bool              `json:"oracle_verified"`
	ContractRuns   int               `json:"contract_runs"`
	TotalTokens    int               `json:"total_tokens"`
	CachedTokens   int               `json:"cached_tokens"`
	Messages       []ShareGPTMessage `json:"messages"`
	Turns          []TrajectoryTurn  `json:"turns"`
}

// DPOPair represents a contrastive preference pair (Winning Branch vs. Graveyard Abandoned Branch)
// sharing the same Run and fork ancestor for DPO / GRPO reinforcement learning.
type DPOPair struct {
	RunID            string           `json:"run_id"`
	GoalSpec         string           `json:"goal_spec"`
	ForkStepID       string           `json:"fork_step_id,omitempty"`
	PromptAtFork     string           `json:"prompt_at_fork"`
	ChosenBranchID   string           `json:"chosen_branch_id"`
	ChosenLabel      string           `json:"chosen_label"`
	RejectedBranchID string           `json:"rejected_branch_id"`
	RejectedLabel    string           `json:"rejected_label"`
	GraveyardReason  string           `json:"graveyard_reason,omitempty"`
	OracleVerified   bool             `json:"oracle_verified"`
	ChosenTurns      []TrajectoryTurn `json:"chosen_turns"`
	RejectedTurns    []TrajectoryTurn `json:"rejected_turns"`
}

// ExportSFT reconstructs and exports SFT trajectories from stored Merkle DAG runs.
func (r *Replayer) ExportSFT(ctx context.Context, opts ExportOptions) ([]SFTExample, error) {
	runs, err := r.selectRuns(ctx, opts.RunID)
	if err != nil {
		return nil, err
	}

	prof := compiler.WorkerProfile()
	var out []SFTExample

	for _, run := range runs {
		contracts, _ := r.Store.ListRunContracts(ctx, run.ID)
		branches, err := r.Store.ListBranches(ctx, run.ID)
		if err != nil {
			continue
		}

		for _, br := range branches {
			if opts.VerifiedOnly && br.Status != string(dag.StatusCompleted) {
				continue
			}
			steps, err := r.Store.ListBranchSteps(ctx, br.ID)
			if err != nil || len(steps) == 0 {
				continue
			}

			cruns, _ := r.Store.ListBranchContractRuns(ctx, br.ID)
			oracleVerified := evaluateBranchOraclePass(contracts, cruns, br.Status == string(dag.StatusCompleted), opts.RequireContract)
			if opts.VerifiedOnly && !oracleVerified {
				continue
			}

			turns, firstFrame, totalToks, cachedToks, modelID, err := r.reconstructBranchTurns(ctx, steps, prof)
			if err != nil || len(turns) == 0 {
				continue
			}

			msgs := []ShareGPTMessage{
				{
					Role:    "system",
					Content: strings.TrimSpace(firstFrame.Layer0) + "\n\n" + strings.TrimSpace(firstFrame.Layer1),
				},
				{
					Role:    "user",
					Content: run.GoalSpec,
				},
			}
			for _, t := range turns {
				content := t.Reasoning
				if t.Done && t.FinalSummary != "" {
					if content != "" && content != t.FinalSummary {
						content = content + "\n" + t.FinalSummary
					} else {
						content = t.FinalSummary
					}
				}
				msgs = append(msgs, ShareGPTMessage{
					Role:      "assistant",
					Content:   content,
					ToolCalls: t.ToolCalls,
				})
				for _, obs := range t.Observations {
					msgs = append(msgs, ShareGPTMessage{
						Role:    "tool",
						Content: obs,
					})
				}
			}

			out = append(out, SFTExample{
				RunID:          run.ID,
				BranchID:       br.ID,
				BranchLabel:    br.Label,
				GoalSpec:       run.GoalSpec,
				ModelID:        modelID,
				Status:         br.Status,
				OracleVerified: oracleVerified,
				ContractRuns:   len(cruns),
				TotalTokens:    totalToks,
				CachedTokens:   cachedToks,
				Messages:       msgs,
				Turns:          turns,
			})
		}
	}
	return out, nil
}

// ExportDPO reconstructs contrastive preference pairs (winning branch vs. abandoned/failed sibling branches)
// for Direct Preference Optimization (DPO) and GRPO reinforcement learning.
func (r *Replayer) ExportDPO(ctx context.Context, opts ExportOptions) ([]DPOPair, error) {
	runs, err := r.selectRuns(ctx, opts.RunID)
	if err != nil {
		return nil, err
	}

	prof := compiler.WorkerProfile()
	var pairs []DPOPair

	for _, run := range runs {
		contracts, _ := r.Store.ListRunContracts(ctx, run.ID)
		branches, err := r.Store.ListBranches(ctx, run.ID)
		if err != nil || len(branches) < 2 {
			continue
		}

		// Map Graveyard negative_path reasons by branch label.
		graveByLabel := make(map[string]string)
		if graveNodes, gErr := r.Store.ListActiveMemoryByScopes(ctx, []string{"repo:" + run.ID}, []string{"negative_path"}); gErr == nil {
			for _, gn := range graveNodes {
				for _, br := range branches {
					if strings.Contains(gn.Content, fmt.Sprintf("approach=%q", br.Label)) {
						graveByLabel[br.Label] = gn.Content
					}
				}
			}
		}

		var winners []store.Branch
		var rejected []store.Branch
		for _, br := range branches {
			switch br.Status {
			case string(dag.StatusCompleted):
				cruns, _ := r.Store.ListBranchContractRuns(ctx, br.ID)
				if !opts.VerifiedOnly || evaluateBranchOraclePass(contracts, cruns, true, opts.RequireContract) {
					winners = append(winners, br)
				}
			case string(dag.StatusAbandoned), string(dag.StatusFailed), string(dag.StatusStalled):
				rejected = append(rejected, br)
			}
		}
		if len(winners) == 0 || len(rejected) == 0 {
			continue
		}

		winBr := winners[0]
		winSteps, err := r.Store.ListBranchSteps(ctx, winBr.ID)
		if err != nil || len(winSteps) == 0 {
			continue
		}
		winTurns, winFirstFrame, _, _, _, err := r.reconstructBranchTurns(ctx, winSteps, prof)
		if err != nil || len(winTurns) == 0 {
			continue
		}
		winCRuns, _ := r.Store.ListBranchContractRuns(ctx, winBr.ID)
		oracleOK := evaluateBranchOraclePass(contracts, winCRuns, true, opts.RequireContract)

		for _, rejBr := range rejected {
			rejSteps, rErr := r.Store.ListBranchSteps(ctx, rejBr.ID)
			if rErr != nil || len(rejSteps) == 0 {
				continue
			}
			rejTurns, _, _, _, _, rErr := r.reconstructBranchTurns(ctx, rejSteps, prof)
			if rErr != nil || len(rejTurns) == 0 {
				continue
			}

			forkStepID := winBr.ForkedFromStepID
			if forkStepID == "" {
				forkStepID = rejBr.ForkedFromStepID
			}
			promptAtFork := strings.TrimSpace(winFirstFrame.Layer0) + "\n\n" + strings.TrimSpace(winFirstFrame.Layer1)
			if forkStepID != "" {
				if forkFrame, fErr := compiler.ReconstructFrame(ctx, r.Store, forkStepID, prof); fErr == nil {
					promptAtFork = strings.TrimSpace(forkFrame.Layer0) + "\n\n" +
						strings.TrimSpace(forkFrame.Layer1) + "\n\n" +
						strings.TrimSpace(forkFrame.Layer2)
				}
			}

			pairs = append(pairs, DPOPair{
				RunID:            run.ID,
				GoalSpec:         run.GoalSpec,
				ForkStepID:       forkStepID,
				PromptAtFork:     promptAtFork,
				ChosenBranchID:   winBr.ID,
				ChosenLabel:      winBr.Label,
				RejectedBranchID: rejBr.ID,
				RejectedLabel:    rejBr.Label,
				GraveyardReason:  graveByLabel[rejBr.Label],
				OracleVerified:   oracleOK,
				ChosenTurns:      winTurns,
				RejectedTurns:    rejTurns,
			})
		}
	}
	return pairs, nil
}

// EncodeJSONL serializes a slice of SFTExamples or DPOPairs into newline-delimited JSON bytes.
func EncodeJSONL[T any](items []T) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, item := range items {
		if err := enc.Encode(item); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// WriteJSONL writes items as newline-delimited JSON to path.
func WriteJSONL[T any](path string, items []T) error {
	raw, err := EncodeJSONL(items)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func (r *Replayer) selectRuns(ctx context.Context, runID string) ([]store.Run, error) {
	if strings.TrimSpace(runID) != "" {
		run, err := r.Store.GetRun(ctx, strings.TrimSpace(runID))
		if err != nil {
			return nil, err
		}
		return []store.Run{run}, nil
	}
	return r.Store.ListRuns(ctx)
}

func (r *Replayer) reconstructBranchTurns(
	ctx context.Context,
	steps []store.Step,
	prof compiler.ModelProfile,
) ([]TrajectoryTurn, compiler.CompiledFrame, int, int, string, error) {
	var turns []TrajectoryTurn
	var firstFrame compiler.CompiledFrame
	var totalToks, cachedToks int
	var modelID string

	for i, s := range steps {
		frame, err := compiler.ReconstructFrame(ctx, r.Store, s.ID, prof)
		if err != nil {
			return nil, compiler.CompiledFrame{}, 0, 0, "", err
		}
		if i == 0 {
			firstFrame = frame
		}
		if modelID == "" && s.ModelID != "" {
			modelID = s.ModelID
		}
		totalToks += s.InputTokens + s.CacheReadTokens + s.OutputTokens
		cachedToks += s.CacheReadTokens

		emBytes, err := r.Store.Blobs().Get(s.EmittedBlobHash)
		if err != nil {
			return nil, compiler.CompiledFrame{}, 0, 0, "", err
		}
		var ep dag.EmittedPayload
		if err := json.Unmarshal(emBytes, &ep); err != nil {
			return nil, compiler.CompiledFrame{}, 0, 0, "", err
		}

		calls := ep.Response.ToolCalls
		if len(calls) == 0 && ep.Response.ToolCall != nil {
			calls = []dag.ToolCallSpec{*ep.Response.ToolCall}
		}
		var obs []string
		for _, le := range ep.ResultingLogEntries {
			if le.Kind == "turn" && le.Action != "done" && le.Observation != "" {
				obs = append(obs, le.Observation)
			}
		}

		turns = append(turns, TrajectoryTurn{
			TurnIndex:         s.TurnIndex,
			StepID:            s.ID,
			CompiledFrameHash: s.CompiledFrameHash,
			Reasoning:         ep.Response.Reasoning,
			ToolCalls:         calls,
			Observations:      obs,
			Done:              ep.Response.Done,
			FinalSummary:      ep.Response.FinalSummary,
		})
	}
	return turns, firstFrame, totalToks, cachedToks, modelID, nil
}

func evaluateBranchOraclePass(contracts []store.Contract, cruns []store.ContractRun, branchCompleted, requireContract bool) bool {
	if !branchCompleted {
		return false
	}
	if len(contracts) == 0 {
		return !requireContract
	}
	passedByContract := make(map[string]bool, len(cruns))
	for _, cr := range cruns {
		if cr.Passed {
			passedByContract[cr.ContractID] = true
		}
	}
	for _, c := range contracts {
		if c.Required && !passedByContract[c.ID] {
			return false
		}
	}
	return true
}
