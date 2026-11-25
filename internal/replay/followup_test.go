package replay

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
)

type doneProvider struct{}

func (doneProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	return &dag.GenerateResponse{Done: true, FinalSummary: "ok", StateHash: "done"}, nil
}

// A multi-turn conversation (user follow-ups are kernel-synthesised steps) must replay exactly.
func TestExactReplayAcrossUserFollowUps(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertAccount(ctx, store.Account{ID: "acc", Provider: "local", ModelID: "m", BillingMode: "local", MaxConcurrent: 2}); err != nil {
		t.Fatal(err)
	}
	ex := dag.NewExecutor(st, doneProvider{}, nil, compiler.WorkerProfile())
	_, br, err := ex.StartRun(ctx, dag.RunRequest{WorkspacePath: ws, GoalSpec: "hello", AccountIDs: []string{"acc"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, msg := range []string{"", "and now this", "one more"} {
		if i > 0 {
			if err := ex.AppendFollowUpUserTurn(ctx, br, msg); err != nil {
				t.Fatalf("AppendFollowUpUserTurn: %v", err)
			}
		}
		if _, err := ex.ExecuteBranch(ctx, br, 4); err != nil {
			t.Fatalf("ExecuteBranch %d: %v", i, err)
		}
	}
	rep, err := NewReplayer(st).Replay(ctx, br, ReplayOptions{Mode: ModeExact})
	if err != nil {
		t.Fatalf("exact replay failed on a conversation: %v", err)
	}
	if rep.Diverged || rep.HashVerifiedCount != rep.StepsChecked || rep.StepsChecked < 5 {
		t.Fatalf("replay report: diverged=%v verified=%d checked=%d", rep.Diverged, rep.HashVerifiedCount, rep.StepsChecked)
	}
}
