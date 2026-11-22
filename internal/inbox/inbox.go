package inbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

// Service executes shared operator approval and dismissal workflows across CLI, TUI, and Bridge.
type Service struct {
	store *store.Store
}

// NewService constructs a shared Inbox Service backed by st.
func NewService(st *store.Store) *Service {
	return &Service{store: st}
}

// Approve validates and applies an InboxItem according to its item_kind, marking it 'approved'.
func (s *Service) Approve(ctx context.Context, itemID, approver string) (string, error) {
	if approver == "" {
		approver = "operator"
	}
	item, err := s.store.GetInboxItem(ctx, itemID)
	if err != nil {
		return "", err
	}
	if item.Status != "pending" {
		return "", fmt.Errorf("inbox item %s is already %s", itemID, item.Status)
	}

	switch item.ItemKind {
	case "memory_proposal":
		if err := memory.NewService(s.store).Activate(ctx, item.ArtifactRef); err != nil {
			return "", fmt.Errorf("activate memory proposal %s: %w", item.ArtifactRef, err)
		}
		if err := s.store.UpdateInboxStatus(ctx, item.ID, "approved"); err != nil {
			return "", err
		}
		return fmt.Sprintf("approved memory proposal %s", item.ArtifactRef), nil

	case "approval":
		entry, err := s.store.GetAction(ctx, item.ArtifactRef)
		if err != nil {
			return "", fmt.Errorf("lookup action %s: %w", item.ArtifactRef, err)
		}
		var pageIDs []string
		if entry.InputProvenanceJSON != "" {
			_ = json.Unmarshal([]byte(entry.InputProvenanceJSON), &pageIDs)
		}
		declassified := 0
		for _, pid := range pageIDs {
			if pid == "" {
				continue
			}
			pg, pErr := s.store.GetPage(ctx, pid)
			if pErr == nil && pg.TrustTier == "untrusted_external" {
				if err := taint.ApproveDeclassification(ctx, s.store, pid, "*", approver); err != nil {
					return "", fmt.Errorf("declassify @%s: %w", pid, err)
				}
				declassified++
			}
		}
		if entry.Status == "prepared" || entry.Status == "dispatched" {
			_ = s.store.MarkActionStatus(ctx, entry.ActionID, "aborted", "", "approved by "+approver+"; ready to resume", "")
		}
		branchID := entry.BranchID
		if branchID == "" {
			branchID = item.BranchID
		}
		if branchID != "" {
			if br, bErr := s.store.GetBranch(ctx, branchID); bErr == nil {
				_ = s.store.UpdateBranchStatus(ctx, br.ID, string(dag.StatusQueued), br.ActiveAccountID, br.StallCount)
			}
		}
		if err := s.store.UpdateInboxStatus(ctx, item.ID, "approved"); err != nil {
			return "", err
		}
		return fmt.Sprintf("approved action %s (declassified %d page(s)); branch %s queued", entry.ActionID, declassified, branchID), nil

	case "reconcile":
		exec := dag.NewExecutor(s.store, nil, nil, compiler.WorkerProfile())
		if err := exec.ReconcileAction(ctx, item.ArtifactRef, dag.OutcomeSucceeded, "approved by "+approver); err != nil {
			return "", fmt.Errorf("reconcile action %s: %w", item.ArtifactRef, err)
		}
		// Ensure the targeted item itself is marked approved even if BranchID was unset.
		_ = s.store.UpdateInboxStatus(ctx, item.ID, "approved")
		return fmt.Sprintf("reconciled action %s as succeeded", item.ArtifactRef), nil

	default:
		if err := s.store.UpdateInboxStatus(ctx, item.ID, "approved"); err != nil {
			return "", err
		}
		return fmt.Sprintf("approved %s %s", item.ItemKind, item.ID), nil
	}
}

// Dismiss rejects an InboxItem safely and updates any linked pending record.
func (s *Service) Dismiss(ctx context.Context, itemID, operator string) (string, error) {
	if operator == "" {
		operator = "operator"
	}
	item, err := s.store.GetInboxItem(ctx, itemID)
	if err != nil {
		return "", err
	}
	if item.Status != "pending" {
		return "", fmt.Errorf("inbox item %s is already %s", itemID, item.Status)
	}

	switch item.ItemKind {
	case "memory_proposal":
		if item.ArtifactRef != "" {
			if err := s.store.ArchiveMemoryNode(ctx, item.ArtifactRef, 0); err != nil {
				return "", fmt.Errorf("archive memory proposal %s: %w", item.ArtifactRef, err)
			}
		}
	case "reconcile":
		if item.ArtifactRef != "" {
			exec := dag.NewExecutor(s.store, nil, nil, compiler.WorkerProfile())
			if err := exec.ReconcileAction(ctx, item.ArtifactRef, dag.OutcomeFailed, "dismissed by "+operator); err != nil {
				return "", fmt.Errorf("reconcile failed %s: %w", item.ArtifactRef, err)
			}
		}
	case "approval":
		if item.ArtifactRef != "" {
			if entry, err := s.store.GetAction(ctx, item.ArtifactRef); err == nil {
				if entry.Status == "prepared" || entry.Status == "dispatched" {
					_ = s.store.MarkActionStatus(ctx, entry.ActionID, "aborted", "", "dismissed by "+operator, "")
				}
				if br, bErr := s.store.GetBranch(ctx, entry.BranchID); bErr == nil && br.Status == string(dag.StatusSuspendedApproval) {
					_ = s.store.UpdateBranchStatus(ctx, br.ID, string(dag.StatusAbandoned), br.ActiveAccountID, br.StallCount)
				}
			}
		}
	}

	if err := s.store.UpdateInboxStatus(ctx, item.ID, "dismissed"); err != nil {
		return "", err
	}
	return fmt.Sprintf("dismissed %s %s", item.ItemKind, item.ID), nil
}
