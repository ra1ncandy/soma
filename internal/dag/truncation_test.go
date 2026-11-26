package dag

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

// A response that burns the whole output budget without a tool call must not end the run: the
// model is told and gets another turn. Repeated truncation still fails eventually.
func TestOutputTruncationIsRecoverable(t *testing.T) {
	ctx := context.Background()
	newEx := func(t *testing.T, fn func(int, GenerateRequest) (*GenerateResponse, error)) (*Executor, string) {
		st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		setupAccount(t, st, "acc_tr")
		ex := NewExecutor(st, &scriptedProvider{fn: fn}, &recordingToolRunner{}, compiler.WorkerProfile())
		_, br, err := ex.StartRun(ctx, RunRequest{WorkspacePath: t.TempDir(), GoalSpec: "x", AccountIDs: []string{"acc_tr"}})
		if err != nil {
			t.Fatal(err)
		}
		return ex, br
	}

	t.Run("one truncation is recovered and the model is told", func(t *testing.T) {
		calls := 0
		ex, br := newEx(t, func(_ int, req GenerateRequest) (*GenerateResponse, error) {
			calls++
			if calls == 1 {
				return nil, &OutputTruncatedError{MaxTokens: 16384}
			}
			if !strings.Contains(req.Frame.Layer2, "hit the 16384-token output limit") {
				t.Errorf("model was not told about the truncation; L2=%q", req.Frame.Layer2)
			}
			return &GenerateResponse{Done: true, FinalSummary: "ok", StateHash: "d"}, nil
		})
		res, err := ex.ExecuteBranch(ctx, br, 5)
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("persistent truncation fails", func(t *testing.T) {
		ex, br := newEx(t, func(int, GenerateRequest) (*GenerateResponse, error) {
			return nil, &OutputTruncatedError{MaxTokens: 100}
		})
		_, err := ex.ExecuteBranch(ctx, br, 10)
		var tr *OutputTruncatedError
		if !errors.As(err, &tr) {
			t.Fatalf("want OutputTruncatedError after repeated truncation, got %v", err)
		}
	})
}
