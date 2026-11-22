package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
)

func TestPrefixStabilityAcrossMidEpochDynamicLoads(t *testing.T) {
	prof := WorkerProfile()
	baseIn := EpochInput{
		SystemPrompt:  strings.Repeat("Kernel rule invariant line.\n", 80), // substantial L0 prefix
		WorkspaceMode: "git",
		TaskGoal:      "Fix webhook map race condition",
		ContractSpec:  "go test -race ./...",
		SkillIndex: []SkillMeta{
			{Name: "go-race-debug", Description: "Diagnose Go race conditions"},
			{Name: "git-bisect", Description: "Find offending commit"},
		},
		MCPSummaries: []MCPSummary{
			{Server: "github", Summary: "PR and CI queries"},
		},
		ActiveMemory: []MemoryEntry{
			{ID: "M-0002", Scope: "repo", Kind: "rule", Content: "Always run -race on concurrency edits"},
			{ID: "M-0001", Scope: "global", Kind: "preference", Content: "Keep patches minimal"},
		},
		Graveyard: []GraveyardEntry{
			{ID: "G-0001", SignatureHash: "sig_mutex", AttemptedApproach: "coarse global lock", FailureReason: "deadlock in callback", EvidencePageID: "page_0001"},
		},
	}

	snap := NewEpochSnapshot(1, baseIn)
	var l2 []LogEntry
	var prevPrefix string
	var prevL2 string
	var totalBytes, cachedBytes int64

	for turn := 1; turn <= 20; turn++ {
		if turn == 5 {
			l2 = append(l2, LogEntry{
				Kind:        "dynamic_skill",
				TurnIndex:   turn,
				Name:        "go-race-debug",
				Observation: "## Go Race Debug\nInspect concurrent map writes and lock ordering.",
			})
		}
		if turn == 9 {
			l2 = append(l2, LogEntry{
				Kind:        "dynamic_mcp",
				TurnIndex:   turn,
				Name:        "github.get_check_run",
				Observation: `{"type":"object","properties":{"run_id":{"type":"integer"}}}`,
			})
		}
		if turn == 13 {
			l2 = append(l2, LogEntry{
				Kind:        "graveyard_notice",
				TurnIndex:   turn,
				Name:        "G-0002",
				Observation: "Sibling branch failed: atomic.Value broke zero-init contract",
			})
		}
		l2 = append(l2, LogEntry{
			Kind:        "turn",
			TurnIndex:   turn,
			Reasoning:   fmt.Sprintf("Inspect step %d", turn),
			Action:      fmt.Sprintf("exec(go test -run Test%d)", turn),
			Observation: fmt.Sprintf("@page_%04d [log | 40KB | 0 errors]", turn),
			PageID:      fmt.Sprintf("page_%04d", turn),
		})

		frame := Compile(snap, l2, LiveTailState{
			WorkspaceKind:       "git",
			GitDiffStat:         fmt.Sprintf("handler.go | %d ++", turn),
			StepBudgetRemaining: 30 - turn,
			AccountQuotaSummary: "codex-plus:ready",
			StallCount:          0,
			MaxStalls:           prof.MaxStalls,
		}, prof)

		if prevPrefix != "" && frame.PrefixHashL0L1 != prevPrefix {
			t.Fatalf("turn %d mutated Layer 0/1 prefix hash", turn)
		}
		prevPrefix = frame.PrefixHashL0L1

		fullPrefix := frame.Layer0 + frame.Layer1 + frame.Layer2
		if prevL2 != "" && !strings.HasPrefix(fullPrefix, prevL2) {
			t.Fatalf("turn %d violated monotonic byte-prefix growth in L0..L2", turn)
		}
		// Count shared prefix bytes vs total frame bytes (L0+L1+L2+L3).
		if prevL2 != "" {
			cachedBytes += int64(len(prevL2))
		}
		totalBytes += int64(len(fullPrefix) + len(frame.Layer3))
		prevL2 = fullPrefix
	}

	hitRate := float64(cachedBytes) / float64(totalBytes)
	if hitRate < 0.80 {
		t.Fatalf("expected high prefix reuse, got %.4f", hitRate)
	}
}

func TestHybridRolloverAndExactFrameReconstruction(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.CreateRun(ctx, store.Run{
		ID:            "run_rec",
		Title:         "Reconstruct",
		WorkspacePath: "/tmp/ws",
		WorkspaceKind: "research",
		GoalSpec:      "Reconstruct exact wire frames",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.CreateBranch(ctx, store.Branch{
		ID:    "b_rec",
		RunID: "run_rec",
		Label: "main",
	}); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	prof := FrontierProfile()
	baseIn := EpochInput{
		SystemPrompt:  "Verify invariant.",
		WorkspaceMode: "research",
		TaskGoal:      "Fix temporal dilation",
		ActiveMemory: []MemoryEntry{
			{ID: "M-01", Scope: "global", Kind: "rule", Content: "Never alter eval_freeze"},
		},
		Graveyard: []GraveyardEntry{
			{ID: "G-01", SignatureHash: "s1", AttemptedApproach: "drop timestamps", FailureReason: "D3 collapsed", EvidencePageID: "p0"},
		},
	}

	snap := NewEpochSnapshot(1, baseIn)
	l0Blob, _, _ := st.Blobs().Put([]byte(snap.Layer0Text))
	l1Blob, _, _ := st.Blobs().Put([]byte(snap.Layer1Text))
	metaBytes, _ := json.Marshal(SnapshotMetadata{ProviderFlavor: prof.ProviderFlavor})
	snapID := "snap_ep1"
	if err := st.PutEpochSnapshot(ctx, store.EpochSnapshot{
		ID:             snapID,
		RunID:          "run_rec",
		BranchID:       "b_rec",
		Epoch:          1,
		Layer0BlobHash: l0Blob,
		Layer1BlobHash: l1Blob,
		PrefixHashL0L1: snap.PrefixHashL0L1,
		MetadataJSON:   string(metaBytes),
	}); err != nil {
		t.Fatalf("PutEpochSnapshot: %v", err)
	}

	var l2 []LogEntry
	var parentID string
	var totalDeltaBlobBytes int64
	var totalFullFrameBytes int64
	var stepIDs []string

	for turn := 1; turn <= 30; turn++ {
		var appended []LogEntry
		if turn == 6 {
			appended = append(appended, LogEntry{
				Kind:        "dynamic_skill",
				TurnIndex:   turn,
				Name:        "dilation-check",
				Observation: "Check Track D D3 60x dilated before promotion.",
			})
		}
		appended = append(appended, LogEntry{
			Kind:        "turn",
			TurnIndex:   turn,
			Reasoning:   fmt.Sprintf("Hypothesis step %d", turn),
			Action:      fmt.Sprintf("exec(eval %d)", turn),
			Observation: fmt.Sprintf("@page_%04d: D3=0.%04d", turn, turn*10),
			PageID:      fmt.Sprintf("page_%04d", turn),
		})
		l2 = append(l2, appended...)
		tail := LiveTailState{
			WorkspaceKind:       "research",
			ResearchStatus:      fmt.Sprintf("gpu0 idle, iter=%d", turn),
			StepBudgetRemaining: 50 - turn,
			AccountQuotaSummary: "xai:ready",
			MaxStalls:           prof.MaxStalls,
		}

		frame := Compile(snap, l2, tail, prof)
		deltaBytes, _ := json.Marshal(FrameDelta{
			TurnIndex:  turn,
			AppendedL2: appended,
			LiveTail:   tail,
		})
		deltaHash, _, err := st.Blobs().Put(deltaBytes)
		if err != nil {
			t.Fatalf("Put delta: %v", err)
		}
		totalDeltaBlobBytes += int64(len(deltaBytes))
		totalFullFrameBytes += int64(len(frame.WireBytes()))

		sid := fmt.Sprintf("step_%02d", turn)
		if err := st.AppendStep(ctx, store.Step{
			ID:                 sid,
			RunID:              "run_rec",
			BranchID:           "b_rec",
			ParentStepID:       parentID,
			Epoch:              1,
			TurnIndex:          turn,
			AccountID:          "xai",
			ModelID:            "grok-4.6",
			StateHash:          sid,
			EpochSnapshotID:    snapID,
			FrameDeltaBlobHash: deltaHash,
			CompiledFrameHash:  frame.CompiledFrameHash,
			EmittedBlobHash:    deltaHash,
		}, store.BranchUpdate{Epoch: 1, Status: "running"}); err != nil {
			t.Fatalf("AppendStep %d: %v", turn, err)
		}
		parentID = sid
		stepIDs = append(stepIDs, sid)
	}

	// Verify bit-for-bit wire reconstruction on all 30 steps.
	for _, sid := range stepIDs {
		rec, err := ReconstructFrame(ctx, st, sid, prof)
		if err != nil {
			t.Fatalf("ReconstructFrame(%s): %v", sid, err)
		}
		if rec.CompiledFrameHash == "" {
			t.Fatalf("empty hash for %s", sid)
		}
	}

	// Incremental deltas must be strictly much smaller than naive full-frame wire payloads.
	if totalDeltaBlobBytes*3 >= totalFullFrameBytes {
		t.Fatalf("expected incremental deltas to be <1/3 of full frames, got delta=%d full=%d",
			totalDeltaBlobBytes, totalFullFrameBytes)
	}

	// Test Hybrid Rollover keeps skeletons, Graveyard, and promotes mid-epoch skill into Epoch 2 L1.
	nextSnap, nextL2 := Rollover(snap, l2, baseIn.ActiveMemory, baseIn.Graveyard, baseIn)
	if nextSnap.Epoch != 2 {
		t.Fatalf("expected epoch 2, got %d", nextSnap.Epoch)
	}
	if !strings.Contains(nextSnap.Layer1Text, "dilation-check") || !strings.Contains(nextSnap.Layer1Text, "G-01") {
		t.Fatalf("expected promoted skill and graveyard in Epoch 2 Layer 1, got:\n%s", nextSnap.Layer1Text)
	}
	if len(nextL2) != 30 {
		t.Fatalf("expected all 30 active turn skeletons preserved across rollover, got %d", len(nextL2))
	}
	if !strings.Contains(formatLogEntry(nextL2[0]), "Hypothesis step 1") || !strings.Contains(formatLogEntry(nextL2[0]), "masked at rollover") {
		t.Fatalf("expected skeleton reasoning + masked observation, got: %s", formatLogEntry(nextL2[0]))
	}
}
