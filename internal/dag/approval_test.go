package dag

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

// An irreversible action must run exactly once after the operator approves it, and a second
// identical call must ask again.
func TestApprovedIrreversibleActionExecutesOnce(t *testing.T) {
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	bg := context.Background()
	setupAccount(t, st, "acc")
	tools := &recordingToolRunner{}
	prov := &scriptedProvider{fn: func(_ int, _ GenerateRequest) (*GenerateResponse, error) {
		return &GenerateResponse{Reasoning: "deploy", StateHash: "s",
			ToolCall: &ToolCallSpec{Name: "deploy_prod", ArgsJSON: `{}`, EffectClass: taint.EffectIrreversible}}, nil
	}}
	ex := NewExecutor(st, prov, tools, compiler.WorkerProfile())
	ex.AccountIDs = []string{"acc"}
	_, brID, err := ex.StartRun(bg, RunRequest{GoalSpec: "g", WorkspacePath: ws, WorkspaceStrategy: "direct"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = ex.ExecuteBranch(bg, brID, 5)
	var ap *SuspendedApprovalError
	if !errors.As(err, &ap) {
		t.Fatalf("first run: want SuspendedApprovalError, got %v", err)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("tool ran before approval: %d calls", len(tools.calls))
	}

	// What inbox.Service.Approve records for an approval item.
	if err := st.MarkActionStatus(bg, ap.ActionID, "approved", "", "approved by operator", ""); err != nil {
		t.Fatal(err)
	}

	_, err = ex.ExecuteBranch(bg, brID, 5)
	if !errors.As(err, &ap) {
		t.Fatalf("second run: want a fresh approval request for the repeated call, got %v", err)
	}
	if len(tools.calls) != 1 {
		t.Fatalf("approved action executed %d times, want exactly 1", len(tools.calls))
	}
}
