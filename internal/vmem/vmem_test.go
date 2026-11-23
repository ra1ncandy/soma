package vmem

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/store"
)

func setupVMTest(t *testing.T) (*store.Store, *Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateRun(context.Background(), store.Run{
		ID:            "run_vm",
		Title:         "VM test",
		WorkspacePath: "/tmp",
		GoalSpec:      "test vmem",
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return st, NewManager(st)
}

func TestObserveThresholdsAndPreviewers(t *testing.T) {
	ctx := context.Background()
	_, mgr := setupVMTest(t)

	// Below 3 KiB -> inline.
	small := []byte("small output under threshold")
	resSmall, err := mgr.Observe(ctx, ObserveRequest{
		RunID:                "run_vm",
		NameHint:             "test.log",
		Content:              small,
		InlineThresholdBytes: 3 * 1024,
	})
	if err != nil {
		t.Fatalf("Observe small: %v", err)
	}
	if resSmall.Paged || resSmall.InlineText != string(small) {
		t.Fatalf("expected inline return for small payload, got Paged=%v", resSmall.Paged)
	}

	// Same payload of 8 KiB: paged under 3 KiB Worker threshold, inline under 16 KiB Frontier threshold.
	med := []byte(strings.Repeat("INFO ok line\n", 650)) // ~8.4 KB
	resWorker, err := mgr.Observe(ctx, ObserveRequest{
		RunID:                "run_vm",
		NameHint:             "build.log",
		Content:              med,
		InlineThresholdBytes: 3 * 1024,
	})
	if err != nil || !resWorker.Paged {
		t.Fatalf("expected worker spill for 8KB log, got paged=%v err=%v", resWorker.Paged, err)
	}
	resFrontier, err := mgr.Observe(ctx, ObserveRequest{
		RunID:                "run_vm",
		NameHint:             "build.log",
		Content:              med,
		InlineThresholdBytes: 16 * 1024,
	})
	if err != nil || resFrontier.Paged {
		t.Fatalf("expected frontier inline for 8KB log, got paged=%v err=%v", resFrontier.Paged, err)
	}

	// 200 KB log spill with errors -> compact preview + Slice + Grep.
	var bigLog strings.Builder
	for i := 0; i < 4000; i++ {
		if i == 2500 {
			bigLog.WriteString("FAIL: DATA RACE detected at handler.go:88\n")
		} else {
			fmt.Fprintf(&bigLog, "PASS check step %d nominal padding padding padding\n", i)
		}
	}
	bigLog.WriteString("exit status 1\n")

	resBig, err := mgr.Observe(ctx, ObserveRequest{
		RunID:                "run_vm",
		NameHint:             "go_test.log",
		Content:              []byte(bigLog.String()),
		TrustTier:            "trusted_repo",
		InlineThresholdBytes: 16 * 1024,
	})
	if err != nil || !resBig.Paged {
		t.Fatalf("Observe 200KB log: paged=%v err=%v", resBig.Paged, err)
	}
	if !strings.Contains(resBig.Manifest, "DATA RACE") || !strings.Contains(resBig.Manifest, "exit=1") {
		t.Fatalf("expected structural log preview to highlight error and exit code, got:\n%s", resBig.Manifest)
	}

	// Slice capped and 1-indexed.
	sl, err := mgr.Slice(ctx, resBig.PageID, 2500, 2503)
	if err != nil {
		t.Fatalf("Slice: %v", err)
	}
	if !strings.Contains(sl, "L2501: FAIL: DATA RACE") {
		t.Fatalf("expected L2501 DATA RACE in slice, got:\n%s", sl)
	}
	slCap, err := mgr.Slice(ctx, resBig.PageID, 1, 1000)
	if err != nil {
		t.Fatalf("Slice cap: %v", err)
	}
	if !strings.Contains(slCap, "lines 1..300") {
		t.Fatalf("expected 300-line cap in slice header, got:\n%s", slCap)
	}

	// Grep finds exact line.
	gr, err := mgr.Grep(ctx, resBig.PageID, `DATA RACE`, 10)
	if err != nil {
		t.Fatalf("Grep: %v", err)
	}
	if !strings.Contains(gr, "1 match(es)") || !strings.Contains(gr, "L2501:") {
		t.Fatalf("unexpected grep result:\n%s", gr)
	}

	// Previewers for code, json, tabular, diff.
	codeSample := "package main\n\nfunc Alpha() {}\ntype Beta struct{}\n"
	if p := GeneratePreview(TypeCode, []byte(codeSample), 15); !strings.Contains(p, "func Alpha") || !strings.Contains(p, "type Beta") {
		t.Fatalf("unexpected code preview:\n%s", p)
	}

	jsonSample := `{"status":"ok","items":[{"id":1,"name":"x"},{"id":2,"name":"y"}]}`
	if p := GeneratePreview(TypeJSON, []byte(jsonSample), 15); !strings.Contains(p, "array[2]") {
		t.Fatalf("unexpected json preview:\n%s", p)
	}

	csvSample := "id,metric,val\n1,pr_auc,0.98\n2,fpr,0.001\n"
	if p := GeneratePreview(TypeTabular, []byte(csvSample), 15); !strings.Contains(p, "3 rows (2 data)") {
		t.Fatalf("unexpected tabular preview:\n%s", p)
	}

	diffSample := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -10,2 +10,3 @@\n-old\n+new\n+extra\n"
	if p := GeneratePreview(TypeDiff, []byte(diffSample), 15); !strings.Contains(p, "+2 -1 lines") {
		t.Fatalf("unexpected diff preview:\n%s", p)
	}

	// Hybrid BM25 + Matryoshka int8 Vector RRF chunk search across spilled @page.
	searchRes, err := mgr.Search(ctx, "run_vm", "DATA RACE handler.go:88", resBig.PageID, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(searchRes, "L2501: FAIL: DATA RACE") || !strings.Contains(searchRes, resBig.PageID) {
		t.Fatalf("expected page_search to rank chunk containing L2501 #1, got:\n%s", searchRes)
	}
}
