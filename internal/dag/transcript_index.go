package dag

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

// indexTranscript adds a step's Layer 2 entries to the full-text session index. Observations
// from tools that reach outside the workspace, or from a run that has seen untrusted content,
// are indexed as untrusted so that recalling them later taints the recalling run.
func (e *Executor) indexTranscript(ctx context.Context, runID, branchID, stepID string, entries []compiler.LogEntry) {
	if e.Store == nil || len(entries) == 0 {
		return
	}
	runTainted := false
	if ids, err := e.Store.ListUntrustedPageIDs(ctx, runID); err == nil && len(ids) > 0 {
		runTainted = true
	}
	var out []store.TranscriptEntry
	for _, le := range entries {
		base := store.TranscriptEntry{RunID: runID, BranchID: branchID, StepID: stepID}
		if le.Kind == "user" {
			t := base
			t.Kind, t.Text = "user", le.Observation+le.AssistantContent
			out = append(out, t)
			continue
		}
		if s := strings.TrimSpace(le.Reasoning + "\n" + le.AssistantContent); s != "" {
			t := base
			t.Kind, t.Text = "assistant", s
			out = append(out, t)
		}
		if le.ToolName != "" {
			t := base
			t.Kind, t.Text = "tool", le.ToolName+" "+le.ArgsJSON
			out = append(out, t)
		}
		if strings.TrimSpace(le.Observation) != "" {
			t := base
			t.Kind, t.Text = "observation", le.Observation
			cls := inferToolEffectClass(le.ToolName)
			if runTainted || le.ToolName == "mcp_call" || (cls != taint.EffectReadOnly && cls != taint.EffectWorkspaceWrite) {
				t.Trust = "untrusted_external"
			}
			out = append(out, t)
		}
	}
	_ = e.Store.IndexTranscript(context.WithoutCancel(ctx), out)
}

// searchSessions renders ranked transcript hits grouped by run, falling back to run metadata.
func searchSessions(ctx context.Context, st *store.Store, query string, limit int) (string, string, error) {
	hits, err := st.SearchTranscripts(ctx, query, limit*4)
	if err != nil {
		return "", "", err
	}
	tier := "trusted_repo"
	type group struct {
		runID string
		best  float64
		hits  []store.TranscriptHit
	}
	byRun := map[string]*group{}
	for _, h := range hits {
		if h.Trust == "untrusted_external" {
			tier = "untrusted_external"
		}
		g := byRun[h.RunID]
		if g == nil {
			g = &group{runID: h.RunID, best: h.Score}
			byRun[h.RunID] = g
		}
		if len(g.hits) < 3 {
			g.hits = append(g.hits, h)
		}
	}
	groups := make([]*group, 0, len(byRun))
	for _, g := range byRun {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].best < groups[j].best }) // bm25: lower is better
	if len(groups) > limit {
		groups = groups[:limit]
	}
	var sb strings.Builder
	if len(groups) == 0 {
		fmt.Fprintf(&sb, "session_search: no past conversation mentions %q", query)
		return sb.String(), tier, nil
	}
	fmt.Fprintf(&sb, "session_search: %d past run(s) matching %q (best first):\n", len(groups), query)
	for i, g := range groups {
		title, status := "", ""
		if rn, err := st.GetRun(ctx, g.runID); err == nil {
			title, status = rn.Title, rn.Status
		}
		fmt.Fprintf(&sb, "%d. run %s status=%s title=%q\n", i+1, g.runID, status, title)
		for _, h := range g.hits {
			fmt.Fprintf(&sb, "   - (%s, branch %s, step %s) %s\n", h.Kind, h.BranchID, h.StepID, strings.ReplaceAll(h.Snippet, "\n", " "))
		}
	}
	return strings.TrimRight(sb.String(), "\n"), tier, nil
}
