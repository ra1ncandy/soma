package oracle

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/vmem"
)

// CommandOracleSpec configures an external sandboxed command/test oracle with hash-pinned invariant files.
type CommandOracleSpec struct {
	Command          string            `json:"command"`
	Args             []string          `json:"args,omitempty"`
	PinnedFiles      map[string]string `json:"pinned_files,omitempty"`
	MutableTestFiles []string          `json:"mutable_test_files,omitempty"`
	JSONMetricsFile  string            `json:"json_metrics_file,omitempty"`
	MetricThresholds []MetricThreshold `json:"metric_thresholds,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds,omitempty"`
	AllowNetwork     bool              `json:"allow_network,omitempty"`
}

func (v *Verifier) evalCommandOracle(ctx context.Context, runID, wsDir string, spec CommandOracleSpec) (bool, string, string, error) {
	if spec.Command == "" {
		return false, "", "command_oracle: empty command", nil
	}

	pinnedPaths := make([]string, 0, len(spec.PinnedFiles))
	for rel := range spec.PinnedFiles {
		pinnedPaths = append(pinnedPaths, rel)
	}
	sort.Strings(pinnedPaths)

	if ce := verifyPinnedHashes(wsDir, pinnedPaths, spec.PinnedFiles); ce != "" {
		return false, "", ce, nil
	}

	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	res, err := v.Sandbox.Exec(ctx, sandbox.ExecSpec{
		WorkspaceDir:        wsDir,
		PinnedReadOnlyFiles: pinnedPaths,
		Command:             spec.Command,
		Args:                spec.Args,
		AllowNetwork:        spec.AllowNetwork,
		Timeout:             timeout,
	})
	if err != nil {
		return false, "", fmt.Sprintf("sandbox exec error: %v", err), nil
	}

	var outPageID string
	obs, obsErr := v.VM.Observe(ctx, vmem.ObserveRequest{
		RunID:                runID,
		NameHint:             "oracle_cmd.log",
		Content:              res.Combined,
		TrustTier:            "trusted_repo",
		InlineThresholdBytes: 1024, // always keep compact preview/handle for oracle output when substantial
	})
	if obsErr == nil && obs.Paged {
		outPageID = obs.PageID
	}

	if ce := verifyPinnedHashes(wsDir, pinnedPaths, spec.PinnedFiles); ce != "" {
		return false, outPageID, ce, nil
	}

	if res.ExitCode != 0 {
		snippet := strings.TrimSpace(string(res.Combined))
		if len(snippet) > 600 {
			snippet = snippet[len(snippet)-600:]
		}
		return false, outPageID, fmt.Sprintf("command exited with status %d: %s", res.ExitCode, snippet), nil
	}

	if spec.JSONMetricsFile != "" {
		metricsPath, err := ResolveSafePath(wsDir, spec.JSONMetricsFile)
		if err != nil {
			return false, outPageID, err.Error(), nil
		}
		rawMetrics, err := os.ReadFile(metricsPath)
		if err != nil {
			return false, outPageID, fmt.Sprintf("read metrics file %s: %v", spec.JSONMetricsFile, err), nil
		}
		if err := CheckMetricThresholds(rawMetrics, spec.MetricThresholds); err != nil {
			return false, outPageID, err.Error(), nil
		}
	}

	return true, outPageID, "", nil
}

func verifyPinnedHashes(wsDir string, sortedRels []string, expected map[string]string) string {
	for _, rel := range sortedRels {
		want := expected[rel]
		full, err := ResolveSafePath(wsDir, rel)
		if err != nil {
			return fmt.Sprintf("%v: %s (%v)", ErrPinnedInvariantTampered, rel, err)
		}
		got, err := HashFileSHA256(full)
		if err != nil {
			return fmt.Sprintf("%v: %s missing or unreadable (%v)", ErrPinnedInvariantTampered, rel, err)
		}
		if want != "" && !strings.EqualFold(got, want) {
			return fmt.Sprintf("%v: %s sha256 mismatch (got %s want %s)", ErrPinnedInvariantTampered, rel, got, want)
		}
	}
	return ""
}
