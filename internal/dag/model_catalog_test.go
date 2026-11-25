package dag

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

func TestPricingYAMLAccruesCostAndCapsEpochCeiling(t *testing.T) {
	ctx := context.Background()
	somaDir := filepath.Join(t.TempDir(), ".soma")
	st, err := store.Open(somaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pricing := "models:\n  - match: \"priced-*\"\n    input: 2\n    output: 10\n    cache_read: 0.5\n    context_window: 10000\n"
	if err := os.WriteFile(filepath.Join(somaDir, "pricing.yaml"), []byte(pricing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(ctx, store.Account{ID: "acc", Provider: "local", ModelID: "priced-model-1", BillingMode: "metered", MaxConcurrent: 2, DailyBudgetUSD: 100}); err != nil {
		t.Fatal(err)
	}
	prov := &scriptedProvider{fn: func(int, GenerateRequest) (*GenerateResponse, error) {
		return &GenerateResponse{Done: true, FinalSummary: "ok", StateHash: "x", InputTokens: 100_000, CacheReadTokens: 200_000, OutputTokens: 10_000}, nil
	}}
	ex := NewExecutor(st, prov, &recordingToolRunner{}, compiler.WorkerProfile())
	_, br, err := ex.StartRun(ctx, RunRequest{WorkspacePath: t.TempDir(), GoalSpec: "x", AccountIDs: []string{"acc"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecuteBranch(ctx, br, 2); err != nil {
		t.Fatal(err)
	}
	acc, err := st.GetAccount(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	// 0.1M*2 + 0.2M*0.5 + 0.01M*10 = 0.2 + 0.1 + 0.1
	if math.Abs(acc.DailySpentUSD-0.4) > 1e-9 {
		t.Fatalf("daily spend = %v, want 0.4", acc.DailySpentUSD)
	}
	info, ok := modelCatalogFor(somaDir).Lookup("PRICED-model-1")
	if !ok || epochCeilingFor(48000, info, ok) != 7000 || epochCeilingFor(5000, info, ok) != 5000 {
		t.Fatalf("context window cap wrong: %+v %v", info, ok)
	}
}
