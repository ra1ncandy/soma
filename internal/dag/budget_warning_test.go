package dag

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

// The model must be told it is running out of turns early enough to finish: from the last quarter
// of the budget (at least 3 turns), the wire messages carry a TURN BUDGET LOW notice.
func TestBudgetWarningReachesTheModelInTheLastQuarter(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	setupAccount(t, st, "acc_bw")
	warnedAt := map[int]bool{}
	prov := &scriptedProvider{fn: func(turn int, req GenerateRequest) (*GenerateResponse, error) {
		raw, _ := json.Marshal(buildOpenAIMessages(req.Frame, false))
		warnedAt[turn] = strings.Contains(string(raw), "TURN BUDGET LOW")
		return &GenerateResponse{ToolCall: &ToolCallSpec{Name: "fs_list", ArgsJSON: `{"path":"."}`}, StateHash: "s" + string(rune('a'+turn))}, nil
	}}
	ex := NewExecutor(st, prov, &recordingToolRunner{}, compiler.WorkerProfile())
	_, br, err := ex.StartRun(ctx, RunRequest{WorkspacePath: t.TempDir(), GoalSpec: "x", AccountIDs: []string{"acc_bw"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecuteBranch(ctx, br, 20); err != nil {
		t.Fatal(err)
	}
	// Budget 20 → warn when 5 or fewer turns remain: turns 16..20 (remaining 5..1).
	for turn := 1; turn <= 20; turn++ {
		want := turn >= 16
		if warnedAt[turn] != want {
			t.Errorf("turn %d: warned=%v, want %v", turn, warnedAt[turn], want)
		}
	}
}
