package dag

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

type funcToolRunner struct {
	fn func(ToolCall) (ToolOutput, error)
}

func (r *funcToolRunner) Execute(_ context.Context, call ToolCall) (ToolOutput, error) {
	return r.fn(call)
}

func TestObservationFailed(t *testing.T) {
	for obs, want := range map[string]bool{
		"exit_code=0\nall good":           false,
		"exit_code=0":                     false,
		"exit_code=1\nboom":               true,
		"exit_code=-1 (TIMEOUT after 5s)": true,
		"TOOL ERROR: no such file":        true,
		"POLICY DENY: tainted":            true,
		"wrote 12 bytes":                  false,
		"":                                false,
	} {
		if got := observationFailed(obs); got != want {
			t.Errorf("observationFailed(%q) = %v, want %v", obs, got, want)
		}
	}
}

// After an edit, two turns in a row that only run commands without a failure put a notice in the
// log telling the model to finish. Exploration before any edit, failing commands and new
// edits do not count.
func TestRepeatingChecksNotice(t *testing.T) {
	ctx := context.Background()
	const notice = "CHECKS REPEATING"
	run := func(t *testing.T, script []string, execOut func(n int) string) []bool {
		st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		setupAccount(t, st, "acc_chk")
		execs := 0
		tools := &funcToolRunner{fn: func(c ToolCall) (ToolOutput, error) {
			out := "ok"
			if c.Name == "exec" {
				execs++
				out = execOut(execs)
			}
			return ToolOutput{NameHint: "out.txt", Content: []byte(out), TrustTier: "trusted_repo"}, nil
		}}
		var sawNotice []bool
		calls := 0
		ex := NewExecutor(st, &scriptedProvider{fn: func(_ int, req GenerateRequest) (*GenerateResponse, error) {
			// The notice for this turn is the newest log entry; earlier ones stay in the log.
			newest := ""
			if n := len(req.Frame.Layer2Entries); n > 0 {
				newest = req.Frame.Layer2Entries[n-1].Observation
			}
			if strings.Contains(req.Frame.Layer3, notice) {
				t.Errorf("the notice must live in the log, not the live tail: %q", req.Frame.Layer3)
			}
			sawNotice = append(sawNotice, strings.Contains(newest, notice))
			if calls >= len(script) {
				return &GenerateResponse{Done: true, FinalSummary: "ok", StateHash: "done"}, nil
			}
			name := script[calls]
			calls++
			spec := ToolCallSpec{CallID: "c", Name: name, ArgsJSON: `{"n":` + string(rune('0'+calls)) + `}`, EffectClass: inferToolEffectClass(name)}
			return &GenerateResponse{ToolCalls: []ToolCallSpec{spec}, ToolCall: &spec, StateHash: name + string(rune('0'+calls))}, nil
		}}, tools, compiler.WorkerProfile())
		_, br, err := ex.StartRun(ctx, RunRequest{WorkspacePath: t.TempDir(), GoalSpec: "x", AccountIDs: []string{"acc_chk"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ex.ExecuteBranch(ctx, br, 40); err != nil {
			t.Fatal(err)
		}
		return sawNotice
	}
	pass := func(int) string { return "exit_code=0\nok" }

	t.Run("fires after two passing check turns that follow an edit", func(t *testing.T) {
		got := run(t, []string{"fs_write", "exec", "exec", "exec"}, pass)
		want := []bool{false, false, false, true, true}
		if len(got) != len(want) {
			t.Fatalf("calls=%d", len(got))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("notice per call = %v, want %v", got, want)
			}
		}
	})
	t.Run("exploring before any edit does not count", func(t *testing.T) {
		for _, saw := range run(t, []string{"exec", "exec", "exec", "exec"}, pass) {
			if saw {
				t.Fatal("notice must not fire before the first edit")
			}
		}
	})
	t.Run("a failing command or a new edit resets it", func(t *testing.T) {
		got := run(t, []string{"fs_write", "exec", "exec", "exec", "fs_patch", "exec"}, func(n int) string {
			if n == 2 {
				return "exit_code=1\n1 failed"
			}
			return "exit_code=0\nok"
		})
		for i, saw := range got {
			if saw {
				t.Fatalf("notice fired at call %d: %v", i+1, got)
			}
		}
	})
}
