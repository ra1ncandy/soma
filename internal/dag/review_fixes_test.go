package dag

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

type cancelMidGenerateProvider struct{ cancel context.CancelFunc }

func (b *cancelMidGenerateProvider) Generate(ctx context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	b.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

// A run cancelled mid-generation (operator Stop, run timeout) must give its account lease back.
func TestLeaseReleasedWhenRunCancelled(t *testing.T) {
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	bg := context.Background()
	if err := st.UpsertAccount(bg, store.Account{ID: "acc", Provider: "t", ModelID: "m", BillingMode: "subscription", MaxConcurrent: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	ex := NewExecutor(st, &cancelMidGenerateProvider{cancel: cancel}, &recordingToolRunner{}, compiler.WorkerProfile())
	ex.AccountIDs = []string{"acc"}
	_, brID, err := ex.StartRun(bg, RunRequest{GoalSpec: "g", WorkspacePath: ws, WorkspaceStrategy: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecuteBranch(ctx, brID, 5); err == nil {
		t.Fatal("expected cancellation error")
	}
	acc, err := st.GetAccount(bg, "acc")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ActiveLeases != 0 {
		t.Fatalf("active_leases = %d after cancelled run, want 0", acc.ActiveLeases)
	}
}
