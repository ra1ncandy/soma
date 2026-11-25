package webui

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/dag"
)

type rateLimitOnceProvider struct{ calls atomic.Int32 }

func (p *rateLimitOnceProvider) Generate(_ context.Context, req dag.GenerateRequest) (*dag.GenerateResponse, error) {
	if p.calls.Add(1) == 1 {
		return nil, &dag.RateLimitError{AccountID: req.AccountID, RetryAfter: 200 * time.Millisecond, Err: errors.New("429")}
	}
	return &dag.GenerateResponse{Done: true, FinalSummary: "resumed", StateHash: "done"}, nil
}

// A branch suspended on a rate limit resumes by itself once the cooldown expires.
func TestServer_QuotaSuspendedBranchAutoResumes(t *testing.T) {
	prov := &rateLimitOnceProvider{}
	srv, st, ws := newTestWebServer(t, func(c *ServerConfig) {
		c.Provider = prov
		c.UseMock = false
	})
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	ctx := context.Background()
	exec, accs, err := srv.executorForRequest(ctx, "", "mock-acc")
	if err != nil {
		t.Fatalf("executorForRequest: %v", err)
	}
	_, brID, err := exec.StartRun(ctx, dag.RunRequest{WorkspacePath: ws, GoalSpec: "quota", AccountIDs: accs})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	srv.runBranchAsync(exec, brID, 5)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		br, err := st.GetBranch(ctx, brID)
		if err == nil && br.Status == string(dag.StatusCompleted) {
			if prov.calls.Load() < 2 {
				t.Fatalf("completed without a second model call")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	br, _ := st.GetBranch(ctx, brID)
	t.Fatalf("branch did not auto-resume after cooldown: status=%s calls=%d", br.Status, prov.calls.Load())
}
