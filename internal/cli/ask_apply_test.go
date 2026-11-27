package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/dag"
)

// A run that runs out of turns after editing files must not silently discard that work: the
// arena showed soma losing finished-but-unannounced fixes this way. The mock provider writes
// artifacts/soma_mock_note.txt on turn 2 and would finish on turn 3, so --max-turns 2 ends in
// budget_exhausted right after the write.
func TestAskAppliesPartialWorkWhenBudgetRunsOut(t *testing.T) {
	run := func(t *testing.T, extra ...string) string {
		t.Helper()
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# project\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := newAskCmd()
		cmd.SetArgs(append([]string{"--dir", ws, "--mock", "--max-turns", "2"}, append(extra, "make a note")...))
		cmd.SetOut(os.Stderr)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ask: %v", err)
		}
		return filepath.Join(ws, "artifacts", "soma_mock_note.txt")
	}
	t.Run("default applies on stop", func(t *testing.T) {
		if _, err := os.Stat(run(t)); err != nil {
			t.Fatalf("budget-exhausted run's edit was discarded: %v", err)
		}
	})
	t.Run("completed-only keeps it in the branch", func(t *testing.T) {
		if _, err := os.Stat(run(t, "--apply", "completed")); !os.IsNotExist(err) {
			t.Fatalf("--apply completed applied an unfinished run (err=%v)", err)
		}
	})
}

func TestShouldApplyAsk(t *testing.T) {
	for _, c := range []struct {
		mode   string
		status dag.BranchStatus
		want   bool
	}{
		{"on-stop", dag.StatusCompleted, true},
		{"on-stop", dag.StatusBudgetExhausted, true},
		{"on-stop", dag.StatusStalled, true},
		{"on-stop", dag.StatusFailed, true},
		{"on-stop", dag.StatusSuspendedApproval, false},
		{"on-stop", dag.StatusSuspendedReconcile, false},
		{"completed", dag.StatusBudgetExhausted, false},
		{"never", dag.StatusCompleted, false},
	} {
		if got := shouldApplyAsk(c.mode, c.status); got != c.want {
			t.Errorf("shouldApplyAsk(%s, %s) = %v, want %v", c.mode, c.status, got, c.want)
		}
	}
}
