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

	runner := NewRunner(st, nil, "v0.4.0-test")
	rep, err := runner.RunGauntlet(ctx, nil)
	if err != nil {
		t.Fatalf("RunGauntlet: %v", err)
	}

	if len(rep.Tasks) != 4 {
		t.Fatalf("expected 4 gauntlet tasks, got %d", len(rep.Tasks))
	}
	sc := rep.Scorecard
	if !sc.GatePassed || sc.KernelVerified != 4 || sc.ExactReplayPassRate != 100.0 {
		t.Fatalf("expected gate to pass with 4/4 verified and exact replay, got %+v", sc)
	}
	// Every scenario must actually fail its ground-truth check with the mechanism switched off;
	// otherwise the scenario proves nothing about the mechanism.
	for _, task := range rep.Tasks {
		if !task.KernelVerified || task.AblationVerified {
			t.Errorf("%s: kernel_verified=%v ablation_verified=%v (want true/false)\n  enabled: %s\n  disabled: %s",
				task.ID, task.KernelVerified, task.AblationVerified, task.KernelEvidence, task.AblationOutcome)
		}
	}
	if rep.Baseline != nil {
		t.Fatalf("no external baseline was supplied, report must not invent one: %+v", rep.Baseline)
	}

	// A scenario that passes equally well without its mechanism must fail the gate.
	weak := append([]TaskResult(nil), rep.Tasks...)
	weak[0].AblationVerified = true
	if computeScorecard(weak).GatePassed {
		t.Fatal("gate must fail when a mechanism is not load-bearing")
	}

	// External telemetry is opt-in, echoed verbatim and labelled as unverified.
	outDir := t.TempDir()
	baseJSON := filepath.Join(outDir, "baseline.json")
	rawBase, _ := json.Marshal(BaselineMetrics{SourceLabel: "someone else's logs", Compactions: 567, Sessions: 324})
	if err := os.WriteFile(baseJSON, rawBase, 0o644); err != nil {
		t.Fatalf("WriteFile baseline.json: %v", err)
	}
	loadedBase, err := LoadBaselineJSON(baseJSON)
	if err != nil || loadedBase.Compactions != 567 {
		t.Fatalf("LoadBaselineJSON mismatch: %+v err=%v", loadedBase, err)
	}
	if md := RenderMarkdown(rep); strings.Contains(md, "External Reference Telemetry") || strings.Contains(md, "567") {
		t.Fatalf("report without a baseline must not contain external telemetry:\n%s", md)
	}
	rep.Baseline = loadedBase

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
	if !strings.Contains(md, "PASS") || !strings.Contains(md, "shadow-ml-lala-01") || strings.Contains(md, "/tmp/") ||
		!strings.Contains(md, "Not measured, reproduced, or verified by this suite") || !strings.Contains(md, "No LLM is called") {
		t.Fatalf("unexpected Markdown report content:\n%s", md)
	}
}
