package shadow

import (
	"encoding/json"
	"fmt"
	"os"
)

// BaselineMetrics is optional, operator-supplied telemetry from some other harness. The gauntlet
// neither measures nor verifies it and never uses it in a score; it is only echoed into the report,
// labelled as external, when the operator passes one explicitly.
type BaselineMetrics struct {
	SourceLabel            string `json:"source_label"`
	Sessions               int    `json:"sessions"`
	Compactions            int    `json:"compactions"`
	RateLimitDeaths        int    `json:"rate_limit_deaths"`
	RateLimitTotal         int    `json:"rate_limit_total"`
	OrphanReaps            int    `json:"orphan_reaps"`
	OrphanTotal            int    `json:"orphan_total"`
	CuratorMutations       int    `json:"curator_mutations"`
	BlockedUnattendedCalls int    `json:"blocked_unattended_calls"`
}

// LoadBaselineJSON loads operator-supplied external telemetry from path.
func LoadBaselineJSON(path string) (*BaselineMetrics, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baseline %s: %w", path, err)
	}
	var base BaselineMetrics
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	if base.SourceLabel == "" {
		base.SourceLabel = path
	}
	return &base, nil
}

// TaskResult records one conformance scenario: the same scripted model run twice, once with the
// kernel mechanism under test enabled and once with it disabled, each checked against ground truth
// read back from the workspace or the fake host, never from the model's own claim of completion.
type TaskResult struct {
	ID               string `json:"id"`
	Category         string `json:"category"` // coding | ssh_ops | ml_rd | quota_chaos
	Title            string `json:"title"`
	Mechanism        string `json:"mechanism"`
	RunID            string `json:"run_id"`
	WinningBranchID  string `json:"winning_branch_id"`
	HeadStepID       string `json:"head_step_id"`
	BranchesExplored int    `json:"branches_explored"`
	GraveyardEntries int    `json:"graveyard_entries"`
	TurnsExecuted    int    `json:"turns_executed"`

	// KernelVerified is the ground-truth check on the run with the mechanism enabled.
	KernelVerified bool   `json:"kernel_verified"`
	KernelEvidence string `json:"kernel_evidence"`
	// AblationVerified is the same ground-truth check with the mechanism disabled.
	AblationVerified bool   `json:"ablation_verified"`
	AblationOutcome  string `json:"ablation_outcome"`

	ExactReplayVerified bool  `json:"exact_replay_verified"`
	ReplayStepsChecked  int   `json:"replay_steps_checked"`
	DurationMS          int64 `json:"duration_ms"`
}

// LoadBearing reports whether disabling the mechanism changed the verified outcome.
func (t TaskResult) LoadBearing() bool { return t.KernelVerified && !t.AblationVerified }

// Scorecard aggregates the Suite B6 conformance gate. Every field is a count of measured outcomes.
type Scorecard struct {
	Tasks                 int     `json:"tasks"`
	KernelVerified        int     `json:"kernel_verified"`
	AblationVerified      int     `json:"ablation_verified"`
	MechanismsLoadBearing int     `json:"mechanisms_load_bearing"`
	ExactReplayPassRate   float64 `json:"exact_replay_pass_rate"`
	GatePassed            bool    `json:"gate_passed"`
}

// GauntletReport bundles metadata, task results, the conformance scorecard, and any external
// telemetry the operator chose to attach.
type GauntletReport struct {
	GeneratedAt string           `json:"generated_at"`
	SomaVersion string           `json:"soma_version"`
	Baseline    *BaselineMetrics `json:"external_baseline,omitempty"`
	Tasks       []TaskResult     `json:"tasks"`
	Scorecard   Scorecard        `json:"scorecard"`
}
