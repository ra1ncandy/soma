package dag

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

// PeerFinding represents a cross-agent discovery, sub-goal assignment, or Graveyard warning
// broadcast over the kernel Blackboard without mutating Layer 0 or Layer 1 mid-epoch.
type PeerFinding struct {
	ID              string `json:"id"`
	RunID           string `json:"run_id"`
	FromBranch      string `json:"from_branch"`
	FromLabel       string `json:"from_label"`
	TargetBranch    string `json:"target_branch,omitempty"` // empty = broadcast to all sibling branches in RunID
	Kind            string `json:"kind"`                    // rule_notice | graveyard_notice
	Summary         string `json:"summary"`
	PageID          string `json:"page_id,omitempty"`
	PersistToMemory bool   `json:"persist_to_memory"`
	CreatedAt       int64  `json:"created_at"`
}

// Blackboard is a concurrency-safe kernel message bus that delivers cross-branch findings and
// @page pointers as append-only Layer 2 notices (and promotes them to Layer 1 on Epoch Rollover).
type Blackboard struct {
	mu       sync.Mutex
	seq      atomic.Uint64
	findings map[string][]PeerFinding       // runID -> findings in publication order
	seen     map[string]map[string]struct{} // branchID -> findingID -> struct{}
}

// NewBlackboard constructs an empty concurrency-safe Blackboard.
func NewBlackboard() *Blackboard {
	return &Blackboard{
		findings: make(map[string][]PeerFinding),
		seen:     make(map[string]map[string]struct{}),
	}
}

// Publish records a finding on the Blackboard and optionally persists it into repo:<runID> bi-temporal memory
// so subsequent Epoch Rollovers fold it into Layer 1.
func (b *Blackboard) Publish(ctx context.Context, st *store.Store, f PeerFinding) (PeerFinding, error) {
	now := time.Now().Unix()
	if f.CreatedAt == 0 {
		f.CreatedAt = now
	}
	if f.Kind == "" {
		f.Kind = "rule_notice"
	}
	f.PageID = strings.TrimPrefix(strings.TrimSpace(f.PageID), "@")
	if f.ID == "" {
		prefix := "P"
		if f.Kind == "graveyard_notice" {
			prefix = "PG"
		}
		f.ID = fmt.Sprintf("%s-%x-%04d", prefix, time.Now().UnixNano(), b.seq.Add(1))
	}

	b.mu.Lock()
	b.findings[f.RunID] = append(b.findings[f.RunID], f)
	if f.FromBranch != "" {
		if b.seen[f.FromBranch] == nil {
			b.seen[f.FromBranch] = make(map[string]struct{})
		}
		b.seen[f.FromBranch][f.ID] = struct{}{}
	}
	b.mu.Unlock()

	if st != nil && f.PersistToMemory && f.RunID != "" {
		memKind := "fact"
		if f.Kind == "graveyard_notice" {
			memKind = "negative_path"
		}
		content := f.Summary
		if f.FromLabel != "" {
			content = fmt.Sprintf("[peer:%s] %s", f.FromLabel, f.Summary)
		}
		_ = st.InsertMemoryNode(ctx, store.MemoryNode{
			ID:             f.ID,
			Scope:          "repo:" + f.RunID,
			Kind:           memKind,
			Content:        content,
			SignatureHash:  sha256Hex([]byte(f.FromLabel + "|" + f.Summary)),
			Status:         "active",
			TrustTier:      "trusted_repo",
			EvidencePageID: f.PageID,
			ValidFrom:      now,
			IngestedAt:     now,
			Enabled:        true,
		})
	}
	return f, nil
}

// DrainForBranch pops all unseen PeerFindings for branchID in runID and formats them as
// append-only Layer 2 LogEntries (preserving 100% Layer 0 + Layer 1 KV-cache prefix stability).
func (b *Blackboard) DrainForBranch(runID, branchID string, turnIndex int) []compiler.LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()

	list := b.findings[runID]
	if len(list) == 0 {
		return nil
	}
	if b.seen[branchID] == nil {
		b.seen[branchID] = make(map[string]struct{})
	}
	seenSet := b.seen[branchID]

	var out []compiler.LogEntry
	for _, f := range list {
		if f.FromBranch == branchID {
			continue
		}
		if f.TargetBranch != "" && f.TargetBranch != branchID {
			continue
		}
		if _, already := seenSet[f.ID]; already {
			continue
		}
		seenSet[f.ID] = struct{}{}

		kind := f.Kind
		if kind != "graveyard_notice" && kind != "rule_notice" {
			kind = "rule_notice"
		}
		obs := f.Summary
		if f.FromLabel != "" {
			obs = fmt.Sprintf("peer=%s: %s", f.FromLabel, f.Summary)
		}
		if f.PageID != "" {
			obs = fmt.Sprintf("%s (evidence=@%s)", obs, f.PageID)
		}
		out = append(out, compiler.LogEntry{
			Kind:        kind,
			TurnIndex:   turnIndex,
			Name:        f.ID,
			Observation: obs,
			PageID:      f.PageID,
		})
	}
	return out
}

// Snapshot returns a copy of all published findings for runID.
func (b *Blackboard) Snapshot(runID string) []PeerFinding {
	b.mu.Lock()
	defer b.mu.Unlock()
	src := b.findings[runID]
	out := make([]PeerFinding, len(src))
	copy(out, src)
	return out
}
