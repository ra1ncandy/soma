package inbox

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
)

func openTestInboxStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedRunAndBranch(t *testing.T, st *store.Store, ws string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateRun(ctx, store.Run{
		ID: "run_inb", Title: "inbox test", WorkspacePath: ws, GoalSpec: "test", Status: "running",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.CreateBranch(ctx, store.Branch{
		ID: "br_inb", RunID: "run_inb", Label: "main", Epoch: 1, Status: "suspended_approval", WorkspaceDir: ws,
	}); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	h, _, err := st.Blobs().Put([]byte("snap"))
	if err != nil {
		t.Fatalf("Blobs.Put: %v", err)
	}
	if err := st.PutEpochSnapshot(ctx, store.EpochSnapshot{
		ID: "snap_inb_e1", RunID: "run_inb", BranchID: "br_inb", Epoch: 1,
		Layer0BlobHash: h, Layer1BlobHash: h, PrefixHashL0L1: h,
	}); err != nil {
		t.Fatalf("PutEpochSnapshot: %v", err)
	}
}

func TestInboxApproveAndDismissAllKinds(t *testing.T) {
	ctx := context.Background()
	st := openTestInboxStore(t)
	seedRunAndBranch(t, st, t.TempDir())
	svc := NewService(st)

	// 1) memory_proposal approve + dismiss
	if err := st.InsertMemoryNode(ctx, store.MemoryNode{
		ID: "M-prop-1", Scope: "global", Kind: "rule", Content: "untrusted mem",
		Status: "proposed", TrustTier: "untrusted_external", Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode 1: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inbox_M-prop-1", ItemKind: "memory_proposal", Title: "Approve mem", Summary: "mem", ArtifactRef: "M-prop-1",
	}); err != nil {
		t.Fatalf("InsertInboxItem 1: %v", err)
	}
	if _, err := svc.Approve(ctx, "inbox_M-prop-1", "alice"); err != nil {
		t.Fatalf("Approve memory_proposal: %v", err)
	}
	m1, err := st.GetMemoryNode(ctx, "M-prop-1")
	if err != nil || m1.Status != "active" {
		t.Fatalf("expected active memory node, got %+v err=%v", m1, err)
	}

	if err := st.InsertMemoryNode(ctx, store.MemoryNode{
		ID: "M-prop-2", Scope: "global", Kind: "rule", Content: "bad mem",
		Status: "proposed", TrustTier: "untrusted_external", Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode 2: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inbox_M-prop-2", ItemKind: "memory_proposal", Title: "Reject mem", Summary: "mem", ArtifactRef: "M-prop-2",
	}); err != nil {
		t.Fatalf("InsertInboxItem 2: %v", err)
	}
	if _, err := svc.Dismiss(ctx, "inbox_M-prop-2", "alice"); err != nil {
		t.Fatalf("Dismiss memory_proposal: %v", err)
	}
	m2, err := st.GetMemoryNode(ctx, "M-prop-2")
	if err != nil || m2.Status != "archived" {
		t.Fatalf("expected archived memory node, got %+v err=%v", m2, err)
	}

	// 2) approval with untrusted_external @page declassification
	if err := st.PutPage(ctx, store.Page{
		ID: "page_untr_1", RunID: "run_inb", ContentHash: "ch", BlobPath: "blobs/ch.zst",
		ContentType: "http", TrustTier: "untrusted_external", ByteSize: 50, LineCount: 1, PreviewText: "ext",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	provJSON, _ := json.Marshal([]string{"page_untr_1"})
	if err := st.PrepareAction(ctx, store.ActionJournalEntry{
		ActionID: "act_inb_1", RunID: "run_inb", BranchID: "br_inb", Epoch: 1, TurnIndex: 1,
		ToolName: "http_post", ArgsJSON: `{}`, ArgsHash: "ah", EffectClass: "external_write",
		IdempotencyKey: "ik_1", InputProvenanceJSON: string(provJSON), Status: "prepared",
	}); err != nil {
		t.Fatalf("PrepareAction: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inbox_act_inb_1", RunID: "run_inb", BranchID: "br_inb", ItemKind: "approval",
		Title: "Approve http_post", Summary: "untrusted page", ArtifactRef: "act_inb_1", Urgency: "urgent",
	}); err != nil {
		t.Fatalf("InsertInboxItem approval: %v", err)
	}
	if _, err := svc.Approve(ctx, "inbox_act_inb_1", "alice"); err != nil {
		t.Fatalf("Approve approval: %v", err)
	}
	okDec, err := st.HasDeclassification(ctx, "page_untr_1", "http:api.example.com")
	if err != nil || !okDec {
		t.Fatalf("expected scoped declassification granted, got (%v, %v)", okDec, err)
	}
	br, err := st.GetBranch(ctx, "br_inb")
	if err != nil || br.Status != "queued" {
		t.Fatalf("expected branch queued after approval, got %+v err=%v", br, err)
	}

	// 3) reconcile approve
	if err := st.PrepareAction(ctx, store.ActionJournalEntry{
		ActionID: "act_rec_1", RunID: "run_inb", BranchID: "br_inb", Epoch: 1, TurnIndex: 2,
		ToolName: "deploy", ArgsJSON: `{}`, ArgsHash: "ah2", EffectClass: "irreversible_write",
		IdempotencyKey: "ik_2", Status: "ambiguous",
	}); err != nil {
		t.Fatalf("PrepareAction rec: %v", err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inbox_rec_1", RunID: "run_inb", BranchID: "br_inb", ItemKind: "reconcile",
		Title: "Reconcile deploy", Summary: "check", ArtifactRef: "act_rec_1", Urgency: "urgent",
	}); err != nil {
		t.Fatalf("InsertInboxItem rec: %v", err)
	}
	if _, err := svc.Approve(ctx, "inbox_rec_1", "alice"); err != nil {
		t.Fatalf("Approve reconcile: %v", err)
	}
	actRec, err := st.GetAction(ctx, "act_rec_1")
	if err != nil || actRec.Status != "reconciled" {
		t.Fatalf("expected reconciled action status, got %+v err=%v", actRec, err)
	}

	// 4) skill_patch approve + idempotency error on double approve
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID: "inbox_sk_1", ItemKind: "skill_patch", Title: "Patch skill", Summary: "diff", ArtifactRef: "my-skill",
	}); err != nil {
		t.Fatalf("InsertInboxItem sk: %v", err)
	}
	if _, err := svc.Approve(ctx, "inbox_sk_1", "alice"); err != nil {
		t.Fatalf("Approve skill_patch: %v", err)
	}
	if _, err := svc.Approve(ctx, "inbox_sk_1", "alice"); err == nil {
		t.Fatalf("expected error on double approve")
	}
}
