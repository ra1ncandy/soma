package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
)

func keyChar(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func keySpecial(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code}
}

func applyCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if msg == nil {
		return m
	}
	nxt, _ := m.Update(msg)
	return nxt.(Model)
}

func seedTUIWorkspace(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	ctx := context.Background()
	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.UpsertAccount(ctx, store.Account{
		ID: "codex-plus", Provider: "openai-codex", ModelID: "gpt-6.2-sol", BillingMode: "subscription", MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	exec := dag.NewExecutor(st, &dag.MockProvider{ModelName: "gpt-6.2-sol"}, nil, compiler.WorkerProfile())
	runID, brID, err := exec.StartRun(ctx, dag.RunRequest{
		WorkspacePath: ws,
		GoalSpec:      "build interactive tui",
		AccountIDs:    []string{"codex-plus"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := exec.ExecuteBranch(ctx, brID, 10); err != nil {
		t.Fatalf("ExecuteBranch: %v", err)
	}

	// Seed proposed + active memory with SUPERSEDES edge.
	if err := st.InsertMemoryNode(ctx, store.MemoryNode{
		ID: "M-old", Scope: "global", Kind: "rule", Content: "legacy rule", Status: "active", Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode M-old: %v", err)
	}
	if err := st.SupersedeMemoryNode(ctx, "M-old", store.MemoryNode{
		ID: "M-new", Scope: "global", Kind: "rule", Content: "new active rule", Status: "active", Enabled: true,
	}, ""); err != nil {
		t.Fatalf("SupersedeMemoryNode: %v", err)
	}
	if err := st.InsertMemoryNode(ctx, store.MemoryNode{
		ID: "M-prop", Scope: "global", Kind: "rule", Content: "untrusted candidate", Status: "proposed", TrustTier: "untrusted_external", Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode M-prop: %v", err)
	}

	// Seed a clean AAIF skill directory.
	skDir := filepath.Join(ws, ".soma", "skills", "demo-skill")
	if err := os.MkdirAll(skDir, 0o755); err != nil {
		t.Fatalf("MkdirAll skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skDir, "SKILL.md"), []byte("---\nname: demo-skill\ndescription: Safe demo skill\n---\nInstructions.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile SKILL.md: %v", err)
	}

	// Seed trigger_runs & inbox items.
	if _, _, err := st.TryClaimTriggerRun(ctx, store.TriggerRun{
		IdempotencyKey: "t1", TriggerID: "watch-ci", Status: "skipped",
	}); err != nil {
		t.Fatalf("TryClaimTriggerRun: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_tui_1", RunID: runID, BranchID: brID, ItemKind: "memory_proposal",
		Title: "Approve M-prop", Summary: "Candidate rule from external log", ArtifactRef: "M-prop", Urgency: "normal",
	}); err != nil {
		t.Fatalf("InsertInboxItem 1: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_tui_2", RunID: runID, BranchID: brID, ItemKind: "draft_pr",
		Title: "Review draft PR", Summary: "Diff ready", Urgency: "low",
	}); err != nil {
		t.Fatalf("InsertInboxItem 2: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inb_tui_3", RunID: runID, BranchID: brID, ItemKind: "alert",
		Title: "Approve skill diff", Summary: "Safe skill patch", ArtifactRef: "demo-skill", Urgency: "normal",
	}); err != nil {
		t.Fatalf("InsertInboxItem 3: %v", err)
	}
	return st, ws, runID, brID
}

func TestTUI_AllFourTabsAndInteractiveActions(t *testing.T) {
	ctx := context.Background()
	st, ws, _, brID := seedTUIWorkspace(t)
	m := New(st, ws)

	// Tab 1: DAG & HUD + fork [f] + jump to X-Ray [x]
	v1 := m.View()
	if !v1.AltScreen || !strings.Contains(v1.Content, brID) || !strings.Contains(v1.Content, "KV-Cache Hit") {
		t.Fatalf("Tab 1 render missing expected branch HUD info:\n%s", v1.Content)
	}
	nxt, cmd := m.Update(keyChar('f'))
	m = applyCmd(t, nxt.(Model), cmd)
	if len(m.snap.Branches) != 2 {
		t.Fatalf("expected [f] fork to create 2nd branch, got %d", len(m.snap.Branches))
	}

	// Jump to Tab 3 X-Ray via [x], scrub left/right, run exact replay [r].
	nxt, _ = m.Update(keyChar('x'))
	m = nxt.(Model)
	if m.activeTab != TabXRay {
		t.Fatalf("expected [x] to switch to TabXRay, got %v", m.activeTab)
	}
	v3 := m.View().Content
	if !strings.Contains(v3, "[2] RECONSTRUCTED WIRE FRAME") || !strings.Contains(v3, "VERIFIED") {
		t.Fatalf("Tab 3 X-Ray missing SHA-256 verification:\n%s", v3)
	}
	nxt, _ = m.Update(keySpecial(tea.KeyLeft))
	m = nxt.(Model)
	nxt, cmd = m.Update(keyChar('r'))
	m = applyCmd(t, nxt.(Model), cmd)
	if !strings.Contains(m.statusMsg, "SHA-256 verified") {
		t.Fatalf("expected exact replay statusMsg, got %q", m.statusMsg)
	}

	// Switch to Tab 2 Memory & Skills: approve M-prop [a], toggle [Space], show edges [s], inspect Skills pane [l].
	nxt, _ = m.Update(keyChar('2'))
	m = nxt.(Model)
	if !strings.Contains(m.View().Content, "M-prop") {
		t.Fatalf("Tab 2 missing M-prop:\n%s", m.View().Content)
	}
	nxt, cmd = m.Update(keyChar('a'))
	m = applyCmd(t, nxt.(Model), cmd)
	if node, err := st.GetMemoryNode(ctx, "M-prop"); err != nil || node.Status != "active" {
		t.Fatalf("expected M-prop active after [a], got %+v err=%v", node, err)
	}
	toggleTargetID := m.snap.MemoryNodes[m.memoryCursor].ID
	nxt, cmd = m.Update(keySpecial(tea.KeySpace))
	m = applyCmd(t, nxt.(Model), cmd)
	if node, err := st.GetMemoryNode(ctx, toggleTargetID); err != nil || node.Enabled {
		t.Fatalf("expected [Space] to disable %s, got %+v err=%v", toggleTargetID, node, err)
	}
	nxt, _ = m.Update(keyChar('s'))
	m = nxt.(Model)
	if !strings.Contains(m.View().Content, "Provenance Edges") {
		t.Fatalf("expected [s] to reveal Provenance Edges view:\n%s", m.View().Content)
	}
	nxt, _ = m.Update(keyChar('l'))
	m = nxt.(Model)
	if !strings.Contains(m.View().Content, "demo-skill") || !strings.Contains(m.View().Content, "CLEAN") {
		t.Fatalf("expected Skills pane with demo-skill [CLEAN]:\n%s", m.View().Content)
	}

	// Test EditorDoneMsg persistence.
	tmpEdit := filepath.Join(t.TempDir(), "edit.md")
	_ = os.WriteFile(tmpEdit, []byte("edited content v2"), 0o644)
	nxt, cmd = m.Update(EditorDoneMsg{NodeID: "M-new", TempFile: tmpEdit, Original: "new active rule"})
	m = applyCmd(t, nxt.(Model), cmd)
	if node, err := st.GetMemoryNode(ctx, "M-new"); err != nil || node.Content != "edited content v2" {
		t.Fatalf("EditorDoneMsg did not persist update: %+v err=%v", node, err)
	}

	// Switch to Tab 4 Daemon, Quotas & Inbox: toggle drawer [v], dismiss [d].
	nxt, _ = m.Update(keyChar('4'))
	m = nxt.(Model)
	v4 := m.View().Content
	if !strings.Contains(v4, "codex-plus") || !strings.Contains(v4, "zero_token_skipped=1") {
		t.Fatalf("Tab 4 missing account/daemon stats:\n%s", v4)
	}
	nxt, _ = m.Update(keyChar('v'))
	m = nxt.(Model)
	if !strings.Contains(m.View().Content, "Inbox Detail Drawer") {
		t.Fatalf("expected [v] to show Inbox Detail Drawer:\n%s", m.View().Content)
	}
	// Navigate to pending inb_tui_2 (item 0 after inb_tui_1 was auto-approved when M-prop activated) and dismiss it.
	for i, it := range m.snap.InboxItems {
		if it.ID == "inb_tui_2" {
			m.inboxCursor = i
			break
		}
	}
	nxt, cmd = m.Update(keyChar('d'))
	m = applyCmd(t, nxt.(Model), cmd)
	it2, err := st.GetInboxItem(ctx, "inb_tui_2")
	if err != nil || it2.Status != "dismissed" {
		t.Fatalf("expected inb_tui_2 dismissed after [d], got %+v err=%v", it2, err)
	}

	// Approve inb_tui_3 via [a] on Tab 4.
	for i, it := range m.snap.InboxItems {
		if it.ID == "inb_tui_3" {
			m.inboxCursor = i
			break
		}
	}
	nxt, cmd = m.Update(keyChar('a'))
	m = applyCmd(t, nxt.(Model), cmd)
	it3, err := st.GetInboxItem(ctx, "inb_tui_3")
	if err != nil || it3.Status != "approved" {
		t.Fatalf("expected inb_tui_3 approved after [a], got %+v err=%v", it3, err)
	}
}
