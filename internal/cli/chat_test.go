package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

type echoHistoryProvider struct{}

func (echoHistoryProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	// Reports whether the first message is still in context, proving the chat is one conversation.
	remembers := strings.Contains(req.Frame.Layer0+req.Frame.Layer2, "my name is Ada")
	return &dag.GenerateResponse{Done: true, FinalSummary: "remembers=" + map[bool]string{true: "yes", false: "no"}[remembers], StateHash: "x"}, nil
}

func TestChatKeepsOneConversationAndSupportsCommands(t *testing.T) {
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertAccount(context.Background(), store.Account{ID: "a", Provider: "local", ModelID: "m", BillingMode: "local", MaxConcurrent: 2}); err != nil {
		t.Fatal(err)
	}
	ex := dag.NewExecutor(st, echoHistoryProvider{}, dag.NewLocalToolRunner(vmem.NewManager(st)), compiler.WorkerProfile())
	var out bytes.Buffer
	sess := &chatSession{st: st, ex: ex, ws: ws, accID: "a", modelID: "m", maxTurns: 4, out: &out}
	input := "my name is Ada\nwhat is my name?\n/status\n/new\nwho am I?\n/search Ada\n/exit\n"
	if err := sess.loop(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "remembers=yes") != 2 {
		t.Fatalf("follow-up did not continue the conversation:\n%s", got)
	}
	if !strings.Contains(got, "remembers=no") {
		t.Fatalf("/new did not start a fresh conversation:\n%s", got)
	}
	if !strings.Contains(got, "[Ada]") {
		t.Fatalf("/search did not find the earlier conversation:\n%s", got)
	}
	runs, _ := st.ListRuns(context.Background())
	if len(runs) != 2 {
		t.Fatalf("want 2 conversations (runs), got %d", len(runs))
	}
}
