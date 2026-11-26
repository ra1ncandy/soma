package dag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

func TestWorkspaceOverview(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, n int) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.py", 10)
	write("pkg/b.py", 2048)
	write(".git/config", 5)
	write(".soma/project.db", 5)
	write("node_modules/x/index.js", 5)

	got := workspaceOverview(dir)
	for _, want := range []string{"a.py (10 B)", "pkg/b.py (2.0 KB)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, hidden := range []string{".git", ".soma", "node_modules"} {
		if strings.Contains(got, hidden) {
			t.Errorf("%s must not be listed:\n%s", hidden, got)
		}
	}
	if workspaceOverview(t.TempDir()) != "" || workspaceOverview("") != "" {
		t.Error("an empty or unknown workspace has no overview")
	}

	big := t.TempDir()
	for i := 0; i < overviewMaxEntries+30; i++ {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("f%04d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got = workspaceOverview(big)
	if !strings.Contains(got, "and 30 more files not shown") || strings.Count(got, ".txt") != overviewMaxEntries {
		t.Errorf("large tree must be cut off with a pointer, got %d entries:\n%s", strings.Count(got, ".txt"), got[len(got)-120:])
	}
}

// A fresh run shows the model its workspace files before the first call, once.
func TestFreshRunStartsWithWorkspaceFiles(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	setupAccount(t, st, "acc_ws")
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var firstL2 []string
	ex := NewExecutor(st, &scriptedProvider{fn: func(_ int, req GenerateRequest) (*GenerateResponse, error) {
		firstL2 = append(firstL2, req.Frame.Layer2)
		return &GenerateResponse{Done: true, FinalSummary: "ok", StateHash: "d"}, nil
	}}, &recordingToolRunner{}, compiler.WorkerProfile())
	_, br, err := ex.StartRun(ctx, RunRequest{WorkspacePath: ws, GoalSpec: "x", AccountIDs: []string{"acc_ws"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecuteBranch(ctx, br, 3); err != nil {
		t.Fatal(err)
	}
	if len(firstL2) != 1 || strings.Count(firstL2[0], "main.py (9 B)") != 1 {
		t.Fatalf("first call must see the workspace files once, got %q", firstL2)
	}
}
