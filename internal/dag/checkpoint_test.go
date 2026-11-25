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

// Forking at a step restores the files as they were right after that step, not the parent's
// current files; checkpoint blobs survive garbage collection.
func TestForkAtStepRestoresThatStepsFiles(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "keep.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(project, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	setupAccount(t, st, "acc_cp")
	script := []string{
		`{"path":"notes.txt","content":"v1\n"}`,
		`{"path":"notes.txt","content":"v2\n"}`,
		`{"path":"keep.txt","content":"clobbered\n"}`,
	}
	prov := &scriptedProvider{fn: func(turn int, _ GenerateRequest) (*GenerateResponse, error) {
		if turn <= len(script) {
			return &GenerateResponse{ToolCall: &ToolCallSpec{Name: "fs_write", ArgsJSON: script[turn-1]}, StateHash: "w" + script[turn-1]}, nil
		}
		return &GenerateResponse{Done: true, FinalSummary: "done", StateHash: "done"}, nil
	}}
	ex := NewExecutor(st, prov, NewLocalToolRunner(vmem.NewManager(st)), compiler.WorkerProfile())
	runID, brID, err := ex.StartRun(ctx, RunRequest{WorkspacePath: project, GoalSpec: "edit", AccountIDs: []string{"acc_cp"}, WorkspaceStrategy: "copy_snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := ex.ExecuteBranch(ctx, brID, 10); err != nil || res.Status != StatusCompleted {
		t.Fatalf("run: %+v %v", res, err)
	}
	br, _ := st.GetBranch(ctx, brID)
	ancestry, err := st.GetAncestry(ctx, br.HeadStepID)
	if err != nil {
		t.Fatal(err)
	}
	var firstWrite string
	for _, s := range ancestry {
		if s.TurnIndex == 1 {
			firstWrite = s.ID
		}
	}
	if firstWrite == "" {
		t.Fatal("no step for turn 1")
	}
	if _, _, err := st.SweepUnreferencedBlobs(ctx, 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	forkID, err := ex.ForkBranch(ctx, runID, firstWrite, "rewind")
	if err != nil {
		t.Fatalf("ForkBranch: %v", err)
	}
	fork, _ := st.GetBranch(ctx, forkID)
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(fork.WorkspaceDir, name))
		return strings.TrimSpace(string(b))
	}
	if got := read("notes.txt"); got != "v1" {
		t.Fatalf("fork notes.txt = %q, want v1 (state after step 1)", got)
	}
	if got := read("keep.txt"); got != "original" {
		t.Fatalf("fork keep.txt = %q, want original", got)
	}
	parentNotes, _ := os.ReadFile(filepath.Join(br.WorkspaceDir, "notes.txt"))
	if strings.TrimSpace(string(parentNotes)) != "v2" {
		t.Fatal("restoring the fork changed the parent's workspace")
	}
}
