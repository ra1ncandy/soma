package dag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

// delegateScript plays a parent that delegates two file-writing subtasks, and children that
// each write their file and finish. It tells roles apart by the frozen Layer 0.
type delegateScript struct{}

func (delegateScript) Generate(_ context.Context, req GenerateRequest) (*GenerateResponse, error) {
	l0 := req.Frame.Layer0
	l2 := req.Frame.Layer2
	if i := strings.Index(l0, "SUBTASK: write "); i >= 0 {
		name := strings.Fields(l0[i+len("SUBTASK: write "):])[0]
		if strings.Contains(l2, "fs_write") {
			return &GenerateResponse{Done: true, FinalSummary: "wrote " + name, StateHash: "c-done-" + name}, nil
		}
		return &GenerateResponse{ToolCall: &ToolCallSpec{Name: "fs_write", ArgsJSON: `{"path":"` + name + `","content":"from child\n"}`}, StateHash: "c-" + name}, nil
	}
	if strings.Contains(l2, "delegate_task") {
		return &GenerateResponse{Done: true, FinalSummary: "parent done", StateHash: "p-done"}, nil
	}
	return &GenerateResponse{ToolCall: &ToolCallSpec{Name: "delegate_task", ArgsJSON: `{"tasks":[{"goal":"write a.txt"},{"goal":"write b.txt"}],"max_turns":5}`}, StateHash: "p1"}, nil
}

func TestDelegateTaskRunsChildrenInIsolationAndMergesBack(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(project, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	setupAccount(t, st, "acc_dlg")
	runner := NewLocalToolRunner(vmem.NewManager(st))
	ex := NewExecutor(st, delegateScript{}, runner, compiler.WorkerProfile())
	ex.OnStep = func(s store.Step) {}
	runID, brID, err := ex.StartRun(ctx, RunRequest{WorkspacePath: project, GoalSpec: "split work", AccountIDs: []string{"acc_dlg"}, WorkspaceStrategy: "copy_snapshot"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	res, err := ex.ExecuteBranch(ctx, brID, 6)
	if err != nil || res.Status != StatusCompleted {
		t.Fatalf("parent: res=%+v err=%v", res, err)
	}
	parent, _ := st.GetBranch(ctx, brID)
	for _, f := range []string{"a.txt", "b.txt"} {
		if b, err := os.ReadFile(filepath.Join(parent.WorkspaceDir, f)); err != nil || string(b) != "from child\n" {
			t.Fatalf("%s not merged into parent workspace: %q err=%v", f, b, err)
		}
		if _, err := os.Stat(filepath.Join(project, f)); err == nil {
			t.Fatalf("%s leaked into the project before the operator applied the branch", f)
		}
	}
	branches, err := st.ListBranches(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	for _, b := range branches {
		if b.ParentBranchID == brID && strings.HasPrefix(b.Label, "delegate-") {
			children++
			if b.Status != string(StatusCompleted) {
				t.Fatalf("child %s status %s", b.ID, b.Status)
			}
		}
	}
	if children != 2 {
		t.Fatalf("want 2 durable child branches, got %d", children)
	}
	// Children cannot delegate further or ask the user.
	child := *runner
	child.subagentDepth = 1
	for _, name := range []string{"delegate_task", "clarify"} {
		if _, err := child.Execute(ctx, ToolCall{Name: name, ArgsJSON: `{"goal":"x","question":"y"}`}); err == nil {
			t.Fatalf("subagent was allowed to call %s", name)
		}
	}
}

// clarify suspends the branch durably until a follow-up answers; todo_write keeps a validated list.
func TestClarifySuspendsUntilAnsweredAndTodoList(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	setupAccount(t, st, "acc_cl")
	prov := &scriptedProvider{fn: func(_ int, req GenerateRequest) (*GenerateResponse, error) {
		switch {
		case strings.Contains(req.Frame.Layer2, "use postgres"):
			return &GenerateResponse{Done: true, FinalSummary: "configured postgres", StateHash: "done"}, nil
		case strings.Contains(req.Frame.Layer2, "todo_write"):
			return &GenerateResponse{ToolCall: &ToolCallSpec{Name: "clarify", ArgsJSON: `{"question":"Which database?","choices":["postgres","sqlite"]}`}, StateHash: "q"}, nil
		default:
			return &GenerateResponse{ToolCall: &ToolCallSpec{Name: "todo_write", ArgsJSON: `{"items":[{"content":"pick db","status":"in_progress"},{"content":"configure","status":"pending"}]}`}, StateHash: "t"}, nil
		}
	}}
	runner := NewLocalToolRunner(vmem.NewManager(st))
	ex := NewExecutor(st, prov, runner, compiler.WorkerProfile())
	_, brID, err := ex.StartRun(ctx, RunRequest{WorkspacePath: ws, GoalSpec: "set up db", AccountIDs: []string{"acc_cl"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.ExecuteBranch(ctx, brID, 10)
	if err != nil || res.Status != StatusAwaitingInput || !strings.Contains(res.FinalSummary, "Which database?") {
		t.Fatalf("want awaiting_input with the question, got res=%+v err=%v", res, err)
	}
	if br, _ := st.GetBranch(ctx, brID); br.Status != string(StatusAwaitingInput) {
		t.Fatalf("stored status %s", br.Status)
	}
	todos, err := LoadTodos(st.SomaDir(), brID)
	if err != nil || len(todos) != 2 || todos[0]["status"] != "in_progress" {
		t.Fatalf("todos = %v err=%v", todos, err)
	}
	if err := ex.AppendFollowUpUserTurn(ctx, brID, "use postgres"); err != nil {
		t.Fatal(err)
	}
	res, err = ex.ExecuteBranch(ctx, brID, 10)
	if err != nil || res.Status != StatusCompleted {
		t.Fatalf("after answer: res=%+v err=%v", res, err)
	}
	if _, err := runner.Execute(ctx, ToolCall{BranchID: brID, Name: "todo_write", ArgsJSON: `{"items":[{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"}]}`, WorkspaceDir: ws}); err == nil {
		t.Fatal("two in_progress items accepted")
	}
}
