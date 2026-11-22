package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".soma")
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	return st
}

func TestSchemaFKAndBlobs(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	err := st.CreateBranch(ctx, Branch{
		ID:     "b_orphan",
		RunID:  "does_not_exist",
		Label:  "orphan",
		Status: "queued",
	})
	if err == nil {
		t.Fatal("expected foreign key error for orphan branch, got nil")
	}

	payload := []byte("hello world log payload repeated data repeated data")
	h1, rel1, err := st.Blobs().Put(payload)
	if err != nil {
		t.Fatalf("Blobs.Put: %v", err)
	}
	h2, rel2, err := st.Blobs().Put(payload)
	if err != nil {
		t.Fatalf("Blobs.Put duplicate: %v", err)
	}
	if h1 != h2 || rel1 != rel2 {
		t.Fatalf("expected deterministic dedup hash/path, got (%s,%s) vs (%s,%s)", h1, rel1, h2, rel2)
	}
	got, err := st.Blobs().Get(h1)
	if err != nil {
		t.Fatalf("Blobs.Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("blob content mismatch: %q", string(got))
	}

	absBlob := filepath.Join(st.SomaDir(), rel1)
	origBytes, err := os.ReadFile(absBlob)
	if err != nil {
		t.Fatalf("read raw blob: %v", err)
	}
	origBytes[len(origBytes)-1] ^= 0xff
	if err := os.WriteFile(absBlob, origBytes, 0o644); err != nil {
		t.Fatalf("corrupt raw blob: %v", err)
	}
	if _, err := st.Blobs().Get(h1); err == nil {
		t.Fatal("expected blob decode or checksum error on corrupted file")
	}
}

func TestAncestrySnapshotsJournalDeclassLeases(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.CreateRun(ctx, Run{
		ID:            "run_1",
		Title:         "Test Run",
		WorkspacePath: "/tmp/ws",
		WorkspaceKind: "git",
		GoalSpec:      "fix bug",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.CreateBranch(ctx, Branch{
		ID:                "b_main",
		RunID:             "run_1",
		Label:             "main",
		WorkspaceDir:      "/tmp/ws",
		WorkspaceStrategy: "git_worktree",
	}); err != nil {
		t.Fatalf("CreateBranch main: %v", err)
	}

	hL0, _, _ := st.Blobs().Put([]byte("layer0"))
	hL1, _, _ := st.Blobs().Put([]byte("layer1"))
	hDelta, _, _ := st.Blobs().Put([]byte("delta"))
	if err := st.PutEpochSnapshot(ctx, EpochSnapshot{
		ID:             "snap_1",
		RunID:          "run_1",
		BranchID:       "b_main",
		Epoch:          1,
		Layer0BlobHash: hL0,
		Layer1BlobHash: hL1,
		PrefixHashL0L1: "pref_1",
		MetadataJSON:   `{"epoch":1}`,
	}); err != nil {
		t.Fatalf("PutEpochSnapshot: %v", err)
	}

	if err := st.PutPage(ctx, Page{
		ID:          "page_1",
		RunID:       "run_1",
		ContentHash: hDelta,
		BlobPath:    "blobs/x",
		ContentType: "log",
		TrustTier:   "untrusted_external",
		ByteSize:    100,
		LineCount:   5,
		PreviewText: "preview",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}

	if err := st.PrepareAction(ctx, ActionJournalEntry{
		ActionID:            "act_1",
		RunID:               "run_1",
		BranchID:            "b_main",
		Epoch:               1,
		TurnIndex:           1,
		ToolName:            "fs_read",
		ArgsJSON:            `{"path":"a.go"}`,
		ArgsHash:            "ah_1",
		EffectClass:         "read_only",
		IdempotencyKey:      "idem_1",
		InputProvenanceJSON: `["page_1"]`,
	}); err != nil {
		t.Fatalf("PrepareAction: %v", err)
	}
	if err := st.MarkActionDispatched(ctx, "act_1"); err != nil {
		t.Fatalf("MarkActionDispatched: %v", err)
	}
	if err := st.CommitActionAndAppendStep(ctx, "act_1", "page_1", "ok", Step{
		ID:                  "s1",
		RunID:               "run_1",
		BranchID:            "b_main",
		Epoch:               1,
		TurnIndex:           1,
		AccountID:           "acc_1",
		ModelID:             "m_1",
		StateHash:           "sh_s1",
		EpochSnapshotID:     "snap_1",
		FrameDeltaBlobHash:  hDelta,
		CompiledFrameHash:   "cfh_1",
		EmittedBlobHash:     hDelta,
		ObservationPageID:   "page_1",
		InputProvenanceJSON: `["page_1"]`,
	}, BranchUpdate{Epoch: 1, Status: "running"}); err != nil {
		t.Fatalf("CommitActionAndAppendStep: %v", err)
	}

	pending, err := st.ListPendingActions(ctx, "b_main")
	if err != nil {
		t.Fatalf("ListPendingActions: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending actions after commit, got %d", len(pending))
	}

	// Fork b_alt from s1 and append s2.
	if err := st.CreateBranch(ctx, Branch{
		ID:               "b_alt",
		RunID:            "run_1",
		ParentBranchID:   "b_main",
		ForkedFromStepID: "s1",
		Label:            "alt",
	}); err != nil {
		t.Fatalf("CreateBranch alt: %v", err)
	}
	if err := st.AppendStep(ctx, Step{
		ID:                 "s2",
		RunID:              "run_1",
		BranchID:           "b_alt",
		ParentStepID:       "s1",
		Epoch:              1,
		TurnIndex:          2,
		AccountID:          "acc_1",
		ModelID:            "m_1",
		StateHash:          "sh_s2",
		EpochSnapshotID:    "snap_1",
		FrameDeltaBlobHash: hDelta,
		CompiledFrameHash:  "cfh_2",
		EmittedBlobHash:    hDelta,
	}, BranchUpdate{Epoch: 1, Status: "completed"}); err != nil {
		t.Fatalf("AppendStep s2: %v", err)
	}

	anc, err := st.GetAncestry(ctx, "s2")
	if err != nil {
		t.Fatalf("GetAncestry: %v", err)
	}
	if len(anc) != 2 || anc[0].ID != "s1" || anc[1].ID != "s2" {
		t.Fatalf("unexpected ancestry: %+v", anc)
	}

	// Declassification scoping check.
	ok, err := st.HasDeclassification(ctx, "page_1", "http:github.com")
	if err != nil || ok {
		t.Fatalf("expected false before declassification, got (%v, %v)", ok, err)
	}
	if err := st.AddDeclassification(ctx, Declassification{
		ID:              "dec_1",
		PageID:          "page_1",
		CapabilityScope: "http:github.com",
		Method:          "schema_parser",
		ApprovedBy:      "metrics_v1",
		SchemaName:      "metrics",
	}); err != nil {
		t.Fatalf("AddDeclassification: %v", err)
	}
	ok, err = st.HasDeclassification(ctx, "page_1", "http:github.com")
	if err != nil || !ok {
		t.Fatalf("expected true for matching scope, got (%v, %v)", ok, err)
	}
	okOther, err := st.HasDeclassification(ctx, "page_1", "ssh:prod")
	if err != nil || okOther {
		t.Fatalf("expected false for unrelated scope, got (%v, %v)", okOther, err)
	}

	// Remote lease mutual exclusion.
	if _, err := st.AcquireRemoteLease(ctx, "gpu:pchan:0", "b_main", "runs/run_1/b_main", 60*time.Second); err != nil {
		t.Fatalf("AcquireRemoteLease b_main: %v", err)
	}
	if _, err := st.AcquireRemoteLease(ctx, "gpu:pchan:0", "b_alt", "runs/run_1/b_alt", 60*time.Second); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("expected ErrLeaseBusy for b_alt, got %v", err)
	}
	if err := st.ReleaseRemoteLease(ctx, "gpu:pchan:0", "b_main"); err != nil {
		t.Fatalf("ReleaseRemoteLease: %v", err)
	}
	if _, err := st.AcquireRemoteLease(ctx, "gpu:pchan:0", "b_alt", "runs/run_1/b_alt", 60*time.Second); err != nil {
		t.Fatalf("AcquireRemoteLease b_alt after release: %v", err)
	}

	// Bi-temporal memory deterministic sorting + edge creation.
	now := time.Now().Unix()
	vOld := now - 5
	for _, n := range []MemoryNode{
		{ID: "M-002", Scope: "global", Kind: "rule", Content: "second", Enabled: true, Status: "active"},
		{ID: "M-001", Scope: "global", Kind: "rule", Content: "first", Enabled: true, Status: "active"},
		{ID: "M-000", Scope: "global", Kind: "rule", Content: "expired", Enabled: true, Status: "active", ValidTo: &vOld},
	} {
		if err := st.InsertMemoryNode(ctx, n); err != nil {
			t.Fatalf("InsertMemoryNode %s: %v", n.ID, err)
		}
	}
	if err := st.AddMemoryEdge(ctx, MemoryEdge{
		FromNodeID: "M-002",
		ToNodeID:   "M-000",
		Relation:   "SUPERSEDES",
	}); err != nil {
		t.Fatalf("AddMemoryEdge: %v", err)
	}
	active, err := st.ListActiveMemory(ctx, "global", []string{"rule"})
	if err != nil {
		t.Fatalf("ListActiveMemory: %v", err)
	}
	if len(active) != 2 || active[0].ID != "M-001" || active[1].ID != "M-002" {
		t.Fatalf("unexpected active memory order/filter: %+v", active)
	}

	// Inbox item round-trip.
	if err := st.InsertInboxItem(ctx, InboxItem{
		ID:       "inbox_1",
		RunID:    "run_1",
		BranchID: "b_main",
		ItemKind: "reconcile",
		Title:    "Verify action",
		Summary:  "Check remote result",
		Urgency:  "urgent",
	}); err != nil {
		t.Fatalf("InsertInboxItem: %v", err)
	}
	items, err := st.ListInboxItems(ctx, "pending")
	if err != nil || len(items) != 1 || items[0].ID != "inbox_1" {
		t.Fatalf("ListInboxItems: %+v, err=%v", items, err)
	}

	// Bi-temporal supersession, N-to-1 merge, AsOf query, and Contracts.
	if err := st.InsertMemoryNode(ctx, MemoryNode{
		ID: "M-010", Scope: "global", Kind: "rule", Content: "v1 rule", Status: "active", ValidFrom: 100, IngestedAt: 100, Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode M-010: %v", err)
	}
	if err := st.SupersedeMemoryNode(ctx, "M-010", MemoryNode{
		ID: "M-011", Scope: "global", Kind: "rule", Content: "v2 rule", Status: "active", ValidFrom: 200, IngestedAt: 200, Enabled: true,
	}, "s1"); err != nil {
		t.Fatalf("SupersedeMemoryNode: %v", err)
	}
	if err := st.InsertMemoryNode(ctx, MemoryNode{
		ID: "M-012", Scope: "global", Kind: "rule", Content: "extra rule", Status: "active", ValidFrom: 210, IngestedAt: 210, Enabled: true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode M-012: %v", err)
	}
	if err := st.MergeMemoryNodes(ctx, []string{"M-011", "M-012"}, MemoryNode{
		ID: "M-013", Scope: "global", Kind: "rule", Content: "merged rule", Status: "active", ValidFrom: 300, IngestedAt: 300, Enabled: true,
	}, "s2"); err != nil {
		t.Fatalf("MergeMemoryNodes: %v", err)
	}

	asOf150, err := st.QueryMemoryAsOf(ctx, []string{"global"}, []string{"rule"}, 150)
	if err != nil {
		t.Fatalf("QueryMemoryAsOf 150: %v", err)
	}
	var got150 []string
	for _, m := range asOf150 {
		got150 = append(got150, m.ID)
	}
	if len(got150) != 1 || got150[0] != "M-010" {
		t.Fatalf("expected [M-010] at t=150, got %v", got150)
	}
	edges, err := st.ListMemoryEdges(ctx, "M-013")
	if err != nil || len(edges) != 2 {
		t.Fatalf("expected 2 MERGES edges on M-013, got %+v err=%v", edges, err)
	}

	if err := st.CreateContract(ctx, Contract{
		ID: "ctr_1", RunID: "run_1", Kind: "command_oracle", SpecJSON: `{}`, PinnedHash: "ph1", Required: true,
	}); err != nil {
		t.Fatalf("CreateContract: %v", err)
	}
	if err := st.RecordContractRun(ctx, ContractRun{
		ID: "crun_1", ContractID: "ctr_1", BranchID: "b_main", StepID: "s1", Passed: true, ArtifactSHA256: "abc", DurationMS: 12,
	}); err != nil {
		t.Fatalf("RecordContractRun: %v", err)
	}
	cruns, err := st.ListBranchContractRuns(ctx, "b_main")
	if err != nil || len(cruns) != 1 || !cruns[0].Passed {
		t.Fatalf("ListBranchContractRuns: %+v err=%v", cruns, err)
	}
}

func TestSplitPoolConcurrencyNoSQLiteBusy(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.CreateRun(ctx, Run{
		ID:            "run_conc",
		Title:         "Concurrency",
		WorkspacePath: "/tmp",
		GoalSpec:      "stress",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	const writers = 8
	const readers = 16
	const iters = 25

	hBlob, _, err := st.Blobs().Put([]byte("snap"))
	if err != nil {
		t.Fatalf("Put blob: %v", err)
	}

	for w := 0; w < writers; w++ {
		bID := fmt.Sprintf("b_%d", w)
		if err := st.CreateBranch(ctx, Branch{
			ID:    bID,
			RunID: "run_conc",
			Label: "worker",
		}); err != nil {
			t.Fatalf("CreateBranch: %v", err)
		}
		if err := st.PutEpochSnapshot(ctx, EpochSnapshot{
			ID:             fmt.Sprintf("snap_%d", w),
			RunID:          "run_conc",
			BranchID:       bID,
			Epoch:          1,
			Layer0BlobHash: hBlob,
			Layer1BlobHash: hBlob,
			PrefixHashL0L1: "pref",
			MetadataJSON:   "{}",
		}); err != nil {
			t.Fatalf("PutEpochSnapshot: %v", err)
		}
	}

	errCh := make(chan error, writers+readers)
	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			bID := fmt.Sprintf("b_%d", wid)
			snapID := fmt.Sprintf("snap_%d", wid)
			prev := ""
			for i := 0; i < iters; i++ {
				payload := []byte(fmt.Sprintf("writer=%d step=%d data=abc", wid, i))
				h, _, err := st.Blobs().Put(payload)
				if err != nil {
					errCh <- err
					return
				}
				sid := fmt.Sprintf("s_%d_%d", wid, i)
				if err := st.AppendStep(ctx, Step{
					ID:                 sid,
					RunID:              "run_conc",
					BranchID:           bID,
					ParentStepID:       prev,
					Epoch:              1,
					TurnIndex:          i + 1,
					AccountID:          "acc",
					ModelID:            "mod",
					StateHash:          sid,
					EpochSnapshotID:    snapID,
					FrameDeltaBlobHash: h,
					CompiledFrameHash:  h,
					EmittedBlobHash:    h,
				}, BranchUpdate{Epoch: 1, Status: "running"}); err != nil {
					errCh <- err
					return
				}
				prev = sid
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(rid int) {
			defer wg.Done()
			bID := fmt.Sprintf("b_%d", rid%writers)
			for i := 0; i < iters*2; i++ {
				if _, err := st.ListBranches(ctx, "run_conc"); err != nil {
					errCh <- err
					return
				}
				if _, err := st.ListBranchSteps(ctx, bID); err != nil {
					errCh <- err
					return
				}
			}
		}(r)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent store failure: %v", err)
		}
	}

	if err := st.UpsertAccount(ctx, Account{
		ID:            "sub1",
		Provider:      "openai-codex",
		ModelID:       "gpt-6.2-sol",
		BillingMode:   "subscription",
		MaxConcurrent: 2,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	nowMs := time.Now().UnixMilli()
	if err := st.AcquireLease(ctx, "sub1", nowMs); err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	if err := st.AcquireLease(ctx, "sub1", nowMs); err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	if err := st.AcquireLease(ctx, "sub1", nowMs); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity on 3rd lease, got %v", err)
	}
	if err := st.ReleaseLease(ctx, "sub1", 0); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if err := st.AcquireLease(ctx, "sub1", nowMs); err != nil {
		t.Fatalf("lease after release: %v", err)
	}
}

func TestV3StoreExtensions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// TriggerRun claim + duplicate rejection + latest hash + TTL GC.
	now := time.Now().Unix()
	claimed, gotTR, err := st.TryClaimTriggerRun(ctx, TriggerRun{
		IdempotencyKey:   "idem_1",
		TriggerID:        "sentinel_1",
		LastSentinelHash: "hash_a",
		Status:           "running",
		CreatedAt:        now - 10,
		ExpiresAt:        now + 300,
	})
	if err != nil || !claimed || gotTR.IdempotencyKey != "idem_1" {
		t.Fatalf("expected claim=true, got (%v, %+v, %v)", claimed, gotTR, err)
	}
	claimed2, existing, err := st.TryClaimTriggerRun(ctx, TriggerRun{
		IdempotencyKey: "idem_1",
		TriggerID:      "sentinel_1",
		Status:         "running",
	})
	if err != nil || claimed2 || existing.LastSentinelHash != "hash_a" {
		t.Fatalf("expected duplicate rejection, got (%v, %+v, %v)", claimed2, existing, err)
	}
	if err := st.UpdateTriggerRun(ctx, "idem_1", "done", "", "hash_b", 0.01); err != nil {
		t.Fatalf("UpdateTriggerRun: %v", err)
	}
	h, err := st.GetLatestTriggerHash(ctx, "sentinel_1")
	if err != nil || h != "hash_b" {
		t.Fatalf("GetLatestTriggerHash: want hash_b, got %q err=%v", h, err)
	}
	if _, _, err := st.TryClaimTriggerRun(ctx, TriggerRun{
		IdempotencyKey:   "idem_old",
		TriggerID:        "sentinel_1",
		LastSentinelHash: "hash_old",
		Status:           "skipped",
		CreatedAt:        now - 200,
		ExpiresAt:        now - 50,
	}); err != nil {
		t.Fatalf("TryClaimTriggerRun expired: %v", err)
	}
	purged, err := st.PurgeExpiredTriggerRuns(ctx, now)
	if err != nil || purged != 1 {
		t.Fatalf("PurgeExpiredTriggerRuns: want 1, got %d err=%v", purged, err)
	}
	runsList, err := st.ListTriggerRuns(ctx, "sentinel_1", 10)
	if err != nil || len(runsList) != 1 || runsList[0].IdempotencyKey != "idem_1" {
		t.Fatalf("ListTriggerRuns: %+v err=%v", runsList, err)
	}

	// Memory helpers: ListAllMemoryNodes (proposed first), SetMemoryEnabled, UpdateMemoryContent.
	for _, m := range []MemoryNode{
		{ID: "M-act", Scope: "global", Kind: "rule", Content: "active rule", Status: "active", Enabled: true, ValidFrom: 10, IngestedAt: 10},
		{ID: "M-prop", Scope: "global", Kind: "rule", Content: "proposed rule", Status: "proposed", Enabled: true, ValidFrom: 5, IngestedAt: 5},
	} {
		if err := st.InsertMemoryNode(ctx, m); err != nil {
			t.Fatalf("InsertMemoryNode %s: %v", m.ID, err)
		}
	}
	allMem, err := st.ListAllMemoryNodes(ctx, "global")
	if err != nil || len(allMem) != 2 || allMem[0].ID != "M-prop" {
		t.Fatalf("ListAllMemoryNodes ordering: %+v err=%v", allMem, err)
	}
	if err := st.SetMemoryEnabled(ctx, "M-act", false); err != nil {
		t.Fatalf("SetMemoryEnabled: %v", err)
	}
	if err := st.UpdateMemoryContent(ctx, "M-act", "updated active rule"); err != nil {
		t.Fatalf("UpdateMemoryContent: %v", err)
	}
	updated, err := st.GetMemoryNode(ctx, "M-act")
	if err != nil || updated.Enabled || updated.Content != "updated active rule" {
		t.Fatalf("unexpected updated memory node: %+v err=%v", updated, err)
	}

	// Account helpers: ResetCooldown, TotalDailyMeteredSpend.
	if err := st.UpsertAccount(ctx, Account{
		ID:            "met1",
		Provider:      "openai",
		ModelID:       "gpt-6.2",
		BillingMode:   "metered",
		MaxConcurrent: 1,
		DailySpentUSD: 1.25,
	}); err != nil {
		t.Fatalf("UpsertAccount met1: %v", err)
	}
	if err := st.SetCooldown(ctx, "met1", time.Now().Add(time.Minute).UnixMilli(), "429"); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	if err := st.ResetCooldown(ctx, "met1"); err != nil {
		t.Fatalf("ResetCooldown: %v", err)
	}
	acc, err := st.GetAccount(ctx, "met1")
	if err != nil || acc.CooldownUntil != 0 || acc.LastError != "" {
		t.Fatalf("ResetCooldown did not clear cooldown: %+v err=%v", acc, err)
	}
	if err := st.ReleaseLease(ctx, "met1", 0.50); err != nil {
		t.Fatalf("ReleaseLease spend: %v", err)
	}
	totalSpend, err := st.TotalDailyMeteredSpend(ctx)
	if err != nil || totalSpend < 1.74 {
		t.Fatalf("TotalDailyMeteredSpend: want >=1.75, got %v err=%v", totalSpend, err)
	}

	// Pages & Inbox helpers.
	if err := st.CreateRun(ctx, Run{ID: "run_p", Title: "pages", WorkspacePath: "/tmp", GoalSpec: "test"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.PutPage(ctx, Page{
		ID: "pg_1", RunID: "run_p", ContentHash: "ch1", BlobPath: "blobs/ch1.zst",
		ContentType: "log", TrustTier: "trusted_repo", ByteSize: 100, LineCount: 4, PreviewText: "preview",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	pages, err := st.ListRunPages(ctx, "run_p", 10)
	if err != nil || len(pages) != 1 || pages[0].ID != "pg_1" {
		t.Fatalf("ListRunPages: %+v err=%v", pages, err)
	}
	if err := st.InsertInboxItem(ctx, InboxItem{
		ID: "inb_v3", ItemKind: "skill_patch", Title: "Review patch", Summary: "diff", ArtifactRef: "sk",
	}); err != nil {
		t.Fatalf("InsertInboxItem: %v", err)
	}
	gotInb, err := st.GetInboxItem(ctx, "inb_v3")
	if err != nil || gotInb.Title != "Review patch" {
		t.Fatalf("GetInboxItem: %+v err=%v", gotInb, err)
	}
	cnt, err := st.CountPendingInbox(ctx)
	if err != nil || cnt != 1 {
		t.Fatalf("CountPendingInbox: want 1, got %d err=%v", cnt, err)
	}
}
