package oracle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

// ErrPinnedInvariantTampered indicates a pinned file was deleted or modified vs its locked SHA-256.
var ErrPinnedInvariantTampered = errors.New("oracle: pinned invariant tampered")

// MetricThreshold defines an assertion against a numeric JSON metric field (supports dot-paths).
type MetricThreshold struct {
	Key   string  `json:"key"`
	Op    string  `json:"op"` // ">=" | "<=" | ">" | "<" | "=="
	Value float64 `json:"value"`
}

// Verifier implements dag.ContractVerifier for command_oracle, artifact_oracle, and advisory_audit.
type Verifier struct {
	Store            *store.Store
	VM               *vmem.Manager
	Sandbox          *sandbox.Manager
	AdvisoryProvider dag.ModelProvider
	AdvisoryModelID  string
	seq              atomic.Uint64
}

// NewVerifier builds a Verifier backed by st and sbx (defaulting to fail-closed sandbox.NewManager()).
func NewVerifier(st *store.Store, sbx *sandbox.Manager) *Verifier {
	if sbx == nil {
		sbx = sandbox.NewManager()
	}
	return &Verifier{
		Store:   st,
		VM:      vmem.NewManager(st),
		Sandbox: sbx,
	}
}

// VerifyBranch evaluates all Verification Contracts attached to run.ID and records ContractRuns.
func (v *Verifier) VerifyBranch(ctx context.Context, run store.Run, branch store.Branch, stepID string) (dag.VerifySummary, error) {
	contracts, err := v.Store.ListRunContracts(ctx, run.ID)
	if err != nil {
		return dag.VerifySummary{}, err
	}
	if len(contracts) == 0 {
		return dag.VerifySummary{AllPassed: true}, nil
	}

	wsDir := branch.WorkspaceDir
	if wsDir == "" {
		wsDir = run.WorkspacePath
	}

	allPassed := true
	var counterexamples []string
	var pageIDs []string

	for _, c := range contracts {
		start := time.Now()
		var passed bool
		var outPageID, artSHA, ceText string
		var evalErr error

		switch c.Kind {
		case "command_oracle":
			var spec CommandOracleSpec
			if err := json.Unmarshal([]byte(c.SpecJSON), &spec); err != nil {
				return dag.VerifySummary{}, fmt.Errorf("parse command_oracle %s: %w", c.ID, err)
			}
			passed, outPageID, ceText, evalErr = v.evalCommandOracle(ctx, run.ID, wsDir, spec)
		case "artifact_oracle":
			var spec ArtifactOracleSpec
			if err := json.Unmarshal([]byte(c.SpecJSON), &spec); err != nil {
				return dag.VerifySummary{}, fmt.Errorf("parse artifact_oracle %s: %w", c.ID, err)
			}
			passed, artSHA, ceText, evalErr = evalArtifactOracle(wsDir, spec)
		case "advisory_audit":
			if v.AdvisoryProvider == nil {
				passed = true
			} else {
				var spec AdvisorySpec
				_ = json.Unmarshal([]byte(c.SpecJSON), &spec)
				rep, err := RunAdvisoryAudit(ctx, v.AdvisoryProvider, "advisory", v.AdvisoryModelID, run.GoalSpec, spec.GraveyardSummary, spec.DiffText)
				if err != nil {
					evalErr = err
				} else {
					passed = rep.Passed
					ceText = rep.Counterexample
				}
			}
		default:
			return dag.VerifySummary{}, fmt.Errorf("unknown contract kind %q", c.Kind)
		}

		if evalErr != nil {
			return dag.VerifySummary{}, evalErr
		}
		if outPageID != "" {
			pageIDs = append(pageIDs, outPageID)
		}

		cr := store.ContractRun{
			ID:                 fmt.Sprintf("crun_%x_%04d", time.Now().UnixNano(), v.seq.Add(1)),
			ContractID:         c.ID,
			BranchID:           branch.ID,
			StepID:             stepID,
			Passed:             passed,
			OutputPageID:       outPageID,
			ArtifactSHA256:     artSHA,
			CounterexampleText: ceText,
			DurationMS:         time.Since(start).Milliseconds(),
		}
		if err := v.Store.RecordContractRun(ctx, cr); err != nil {
			return dag.VerifySummary{}, err
		}

		if !passed && c.Required {
			allPassed = false
			counterexamples = append(counterexamples, fmt.Sprintf("[%s:%s] %s", c.Kind, c.ID, ceText))
		}
	}

	return dag.VerifySummary{
		AllPassed:      allPassed,
		Counterexample: strings.Join(counterexamples, "\n"),
		OutputPageIDs:  pageIDs,
	}, nil
}

// HashFileSHA256 returns the lowercase hex SHA-256 of a file on disk.
func HashFileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ResolveSafePath verifies rel is relative and stays strictly inside wsDir.
func ResolveSafePath(wsDir, rel string) (string, error) {
	cleanRel := filepath.Clean(rel)
	if filepath.IsAbs(cleanRel) || cleanRel == "." || strings.HasPrefix(cleanRel, "..") {
		return "", fmt.Errorf("unsafe relative path %q", rel)
	}
	absWS, err := filepath.Abs(wsDir)
	if err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(absWS, cleanRel))
	if !strings.HasPrefix(target, absWS+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %q", rel)
	}
	return target, nil
}

// CheckMetricThresholds evaluates numeric thresholds against JSON bytes (supports dot-separated nested keys).
func CheckMetricThresholds(jsonBytes []byte, thresholds []MetricThreshold) error {
	if len(thresholds) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(jsonBytes, &root); err != nil {
		return fmt.Errorf("invalid metrics JSON: %w", err)
	}
	for _, th := range thresholds {
		val, ok := lookupDotFloat(root, th.Key)
		if !ok {
			return fmt.Errorf("missing numeric metric key %q", th.Key)
		}
		if !compareThreshold(val, th.Op, th.Value) {
			return fmt.Errorf("metric %s=%g failed assertion %s %g", th.Key, val, th.Op, th.Value)
		}
	}
	return nil
}

func lookupDotFloat(root map[string]any, dotKey string) (float64, bool) {
	parts := strings.Split(dotKey, ".")
	var cur any = root
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		v, exists := m[part]
		if !exists {
			return 0, false
		}
		cur = v
	}
	f, ok := cur.(float64)
	return f, ok
}

func compareThreshold(actual float64, op string, target float64) bool {
	switch op {
	case ">=":
		return actual >= target
	case "<=":
		return actual <= target
	case ">":
		return actual > target
	case "<":
		return actual < target
	case "==":
		return actual == target
	default:
		return false
	}
}
