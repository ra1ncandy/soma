package taint

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

func TestDataActionLevelProvenanceAndScopedDeclassification(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if err := st.CreateRun(ctx, store.Run{
		ID:            "run_taint",
		Title:         "Taint test",
		WorkspacePath: "/tmp",
		GoalSpec:      "verify provenance",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	vm := vmem.NewManager(st)

	// 1. Ingest untrusted external JSON metrics page.
	untrustedPayload := []byte(`{"pr_auc": 0.9807, "d3_dilated": 0.9502, "comment": "` + strings.Repeat("x", 4000) + `"}`)
	resUntrusted, err := vm.Observe(ctx, vmem.ObserveRequest{
		RunID:                "run_taint",
		NameHint:             "web_metrics.json",
		Content:              untrustedPayload,
		TrustTier:            "untrusted_external",
		InlineThresholdBytes: 512,
	})
	if err != nil || !resUntrusted.Paged {
		t.Fatalf("Observe untrusted: paged=%v err=%v", resUntrusted.Paged, err)
	}

	// 2. Workspace read/write remains autonomous even after reading untrusted data.
	decLocal, _, err := EvaluateAction(ctx, st, ActionPolicyInput{
		ToolName:          "fs_patch",
		EffectClass:       EffectWorkspaceWrite,
		ReferencedPageIDs: []string{resUntrusted.PageID},
	})
	if err != nil || decLocal != DecisionAllow {
		t.Fatalf("expected workspace_write allowed after reading untrusted page, got (%s, %v)", decLocal, err)
	}

	// 3. External write referencing untrusted page MUST ask.
	decExt, reason, err := EvaluateAction(ctx, st, ActionPolicyInput{
		ToolName:          "http_post",
		CapabilityScope:   "http:github.com:draft_pr",
		EffectClass:       EffectExternalWrite,
		ReferencedPageIDs: []string{resUntrusted.PageID},
	})
	if err != nil || decExt != DecisionAsk || !strings.Contains(reason, resUntrusted.PageID) {
		t.Fatalf("expected DecisionAsk referencing %s, got (%s, %q, %v)", resUntrusted.PageID, decExt, reason, err)
	}

	// 4. Deterministic schema_parser declassification unblocks ONLY the scoped capability.
	type metricsSchema struct {
		PRAUC     float64 `json:"pr_auc"`
		D3Dilated float64 `json:"d3_dilated"`
	}
	parsed, err := DeclassifyStructured(ctx, st, resUntrusted.PageID, "http:github.com:draft_pr", "metrics_v1", func(raw []byte) (metricsSchema, error) {
		var m metricsSchema
		if err := json.Unmarshal(raw, &m); err != nil {
			return m, err
		}
		if m.PRAUC <= 0 || m.PRAUC > 1 {
			return m, errors.New("out of range")
		}
		return m, nil
	})
	if err != nil || parsed.PRAUC != 0.9807 {
		t.Fatalf("DeclassifyStructured failed: %+v err=%v", parsed, err)
	}

	// Matching scope is now allowed.
	decAfter, _, err := EvaluateAction(ctx, st, ActionPolicyInput{
		ToolName:          "http_post",
		CapabilityScope:   "http:github.com:draft_pr",
		EffectClass:       EffectExternalWrite,
		ReferencedPageIDs: []string{resUntrusted.PageID},
	})
	if err != nil || decAfter != DecisionAllow {
		t.Fatalf("expected DecisionAllow after schema_parser declassification, got (%s, %v)", decAfter, err)
	}

	// Unrelated scope still asks!
	decOtherScope, _, err := EvaluateAction(ctx, st, ActionPolicyInput{
		ToolName:          "ssh_exec",
		CapabilityScope:   "ssh:prod:restart",
		EffectClass:       EffectExternalWrite,
		ReferencedPageIDs: []string{resUntrusted.PageID},
	})
	if err != nil || decOtherScope != DecisionAsk {
		t.Fatalf("expected DecisionAsk for unrelated scope, got (%s, %v)", decOtherScope, err)
	}

	// Operator scoped approval unlocks ssh scope.
	if err := ApproveDeclassification(ctx, st, resUntrusted.PageID, "ssh:prod:restart", "ame"); err != nil {
		t.Fatalf("ApproveDeclassification: %v", err)
	}
	decApproved, _, err := EvaluateAction(ctx, st, ActionPolicyInput{
		ToolName:          "ssh_exec",
		CapabilityScope:   "ssh:prod:restart",
		EffectClass:       EffectExternalWrite,
		ReferencedPageIDs: []string{resUntrusted.PageID},
	})
	if err != nil || decApproved != DecisionAllow {
		t.Fatalf("expected DecisionAllow after operator approval, got (%s, %v)", decApproved, err)
	}

	// 5. Irreversible writes ALWAYS require approval even when declassified or trusted.
	decIrrev, _, err := EvaluateAction(ctx, st, ActionPolicyInput{
		ToolName:          "git_push_force",
		CapabilityScope:   "http:github.com:draft_pr",
		EffectClass:       EffectIrreversible,
		ReferencedPageIDs: []string{resUntrusted.PageID},
	})
	if err != nil || decIrrev != DecisionAsk {
		t.Fatalf("expected DecisionAsk for irreversible action, got (%s, %v)", decIrrev, err)
	}

	// 6. Explicit deny overrides everything.
	decDeny, _, err := EvaluateAction(ctx, st, ActionPolicyInput{
		ToolName:     "rm_rf",
		EffectClass:  EffectWorkspaceWrite,
		ExplicitDeny: true,
	})
	if err != nil || decDeny != DecisionDeny {
		t.Fatalf("expected DecisionDeny, got (%s, %v)", decDeny, err)
	}
}
