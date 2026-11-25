package dag

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

// session_search finds what was said and done inside past conversations, not just run titles,
// and recalled untrusted observations stay untrusted.
func TestSessionSearchFindsTranscriptContent(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	setupAccount(t, st, "acc_ss")
	prov := &scriptedProvider{fn: func(turn int, req GenerateRequest) (*GenerateResponse, error) {
		if strings.Contains(req.Frame.Layer2, "web_extract") {
			return &GenerateResponse{Done: true, FinalSummary: "done", StateHash: "d"}, nil
		}
		return &GenerateResponse{Reasoning: "The flaky test is caused by a race in the connection pool", ToolCall: &ToolCallSpec{Name: "web_extract", ArgsJSON: `{"url":"https://example.org"}`}, StateHash: "w"}, nil
	}}
	runner := &taintScenarioRunner{}
	ex := NewExecutor(st, prov, runner, compiler.WorkerProfile())
	_, br, err := ex.StartRun(ctx, RunRequest{WorkspacePath: ws, GoalSpec: "investigate CI flakiness", AccountIDs: []string{"acc_ss"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecuteBranch(ctx, br, 4); err != nil {
		t.Fatal(err)
	}
	if err := ex.AppendFollowUpUserTurn(ctx, br, "remember the pgbouncer setting"); err != nil {
		t.Fatal(err)
	}

	lt := NewLocalToolRunner(vmem.NewManager(st))
	for _, tc := range []struct{ q, want string }{
		{"connection pool race", "[race]"},
		{"pgbouncer", "[pgbouncer]"},
		{"flakiness", "[flakiness]"},
	} {
		out, err := lt.Execute(ctx, ToolCall{Name: "session_search", ArgsJSON: `{"query":"` + tc.q + `"}`})
		if err != nil {
			t.Fatalf("session_search %q: %v", tc.q, err)
		}
		if !strings.Contains(string(out.Content), tc.want) {
			t.Fatalf("session_search %q missing %s:\n%s", tc.q, tc.want, out.Content)
		}
	}
	out, _ := lt.Execute(ctx, ToolCall{Name: "session_search", ArgsJSON: `{"query":"IGNORE PREVIOUS INSTRUCTIONS"}`})
	if out.TrustTier != "untrusted_external" {
		t.Fatalf("recalled web observation should be untrusted, got %q:\n%s", out.TrustTier, out.Content)
	}
	if _, err := lt.Execute(ctx, ToolCall{Name: "session_search", ArgsJSON: `{"query":"text:* OR NEAR(a b) \" -"}`}); err != nil {
		t.Fatalf("FTS operators in user input must be neutralised: %v", err)
	}
}
