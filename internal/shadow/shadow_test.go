package shadow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
)

func TestRunner_ShadowGauntletAndReportGeneration(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	base := DefaultHermesBaseline()
	runner := NewRunner(st, nil, "v0.4.0-test")
	rep, err := runner.RunGauntlet(ctx, base)
	if err != nil {
		t.Fatalf("RunGauntlet: %v", err)
	}

	if len(rep.Tasks) != 4 {
		t.Fatalf("expected 4 gauntlet tasks, got %d", len(rep.Tasks))
	}
	if !rep.Scorecard.GatePassed {
		t.Fatalf("expected Scorecard.GatePassed=true, got %+v", rep.Scorecard)
	}
	if rep.Scorecard.CompletionRatio < 2.0 {
		t.Fatalf("expected CompletionRatio >= 2.0, got %.2f", rep.Scorecard.CompletionRatio)
	}
	if rep.Scorecard.ExactReplayPassRate != 100.0 || rep.Scorecard.TotalCompactions != 0 {
		t.Fatalf("unexpected replay/compaction metrics: %+v", rep.Scorecard)
	}

	// Verify baseline JSON loading + Markdown/JSON artifact generation without temp path leakage.
	outDir := t.TempDir()
	baseJSON := filepath.Join(outDir, "baseline.json")
	rawBase, _ := json.Marshal(base)
	if err := os.WriteFile(baseJSON, rawBase, 0o644); err != nil {
		t.Fatalf("WriteFile baseline.json: %v", err)
	}
	loadedBase, err := LoadBaselineJSON(baseJSON)
	if err != nil || loadedBase.Compactions != 567 {
		t.Fatalf("LoadBaselineJSON mismatch: %+v err=%v", loadedBase, err)
	}

	mdPath := filepath.Join(outDir, "BENCHMARK_REPORT.md")
	jsonPath := filepath.Join(outDir, "gauntlet.json")
	if err := WriteReport(rep, mdPath, jsonPath); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	mdBytes, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("ReadFile md: %v", err)
	}
	md := string(mdBytes)
	if !strings.Contains(md, "PASS") || !strings.Contains(md, "shadow-ml-lala-01") || strings.Contains(md, "/tmp/") {
		t.Fatalf("unexpected Markdown report content:\n%s", md)
	}
}
