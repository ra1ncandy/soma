package shadow

import (
	"encoding/json"
	"fmt"
	"os"
)

// BaselineMetrics captures empirical failure and completion metrics from the competitor baseline
// (defaulting to verified November 2026 Hermes production audit logs).
type BaselineMetrics struct {
	SourceLabel            string  `json:"source_label"`
	Sessions               int     `json:"sessions"`
	Compactions            int     `json:"compactions"`
	RateLimitDeaths        int     `json:"rate_limit_deaths"`
	RateLimitTotal         int     `json:"rate_limit_total"`
	OrphanReaps            int     `json:"orphan_reaps"`
	OrphanTotal            int     `json:"orphan_total"`
	CuratorMutations       int     `json:"curator_mutations"`
	BlockedUnattendedCalls int     `json:"blocked_unattended_calls"`
	LinearCompletionRate   float64 `json:"linear_completion_rate"`
}

// DefaultHermesBaseline returns the empirical Hermes forensic baseline measured across multi-host production runs.
func DefaultHermesBaseline() BaselineMetrics {
	return BaselineMetrics{
		SourceLabel:            "Hermes Multi-Host Production Audit (Nov 2026)",
		Sessions:               324,
		Compactions:            567,
		RateLimitDeaths:        417,
		RateLimitTotal:         781,
		OrphanReaps:            739,
		OrphanTotal:            1662,
		CuratorMutations:       150,
		BlockedUnattendedCalls: 547,
		LinearCompletionRate:   0.25,
	}
}

// LoadBaselineJSON loads custom competitor baseline metrics from path.
func LoadBaselineJSON(path string) (BaselineMetrics, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return BaselineMetrics{}, fmt.Errorf("read baseline %s: %w", path, err)
	}
	base := DefaultHermesBaseline()
	if err := json.Unmarshal(raw, &base); err != nil {
		return BaselineMetrics{}, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	return base, nil
}

// TaskResult records side-by-side shadow gauntlet telemetry for one scenario.
type TaskResult struct {
	ID                  string  `json:"id"`
	Category            string  `json:"category"` // coding | ssh_ops | ml_rd | quota_chaos
	Title               string  `json:"title"`
	RunID               string  `json:"run_id"`
	WinningBranchID     string  `json:"winning_branch_id"`
	HeadStepID          string  `json:"head_step_id"`
	BranchesExplored    int     `json:"branches_explored"`
	GraveyardEntries    int     `json:"graveyard_entries"`
	TurnsExecuted       int     `json:"turns_executed"`
	Compactions         int     `json:"compactions"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	SomaTokens          int     `json:"soma_tokens"`
	LinearTokens        int     `json:"linear_tokens"`
	SomaCompleted       bool    `json:"soma_completed"`
	LinearCompleted     bool    `json:"linear_completed"`
	OraclesPassed       string  `json:"oracles_passed"`
	ExactReplayVerified bool    `json:"exact_replay_verified"`
	ReplayStepsChecked  int     `json:"replay_steps_checked"`
	DurationMS          int64   `json:"duration_ms"`
	Notes               string  `json:"notes"`
}

// Scorecard aggregates top-level RFC-0001 Suite B6 acceptance metrics.
type Scorecard struct {
	SomaCompletionRate   float64 `json:"soma_completion_rate"`
	LinearCompletionRate float64 `json:"linear_completion_rate"`
	CompletionRatio      float64 `json:"completion_ratio"`
	AvgCacheHitRate      float64 `json:"avg_cache_hit_rate"`
	TotalCompactions     int     `json:"total_compactions"`
	ExactReplayPassRate  float64 `json:"exact_replay_pass_rate"`
	TokenSavingsPct      float64 `json:"token_savings_pct"`
	GatePassed           bool    `json:"gate_passed"`
}

// GauntletReport bundles metadata, baseline comparison, task results, and acceptance scorecard.
type GauntletReport struct {
	GeneratedAt string          `json:"generated_at"`
	SomaVersion string          `json:"soma_version"`
	Baseline    BaselineMetrics `json:"baseline"`
	Tasks       []TaskResult    `json:"tasks"`
	Scorecard   Scorecard       `json:"scorecard"`
}
