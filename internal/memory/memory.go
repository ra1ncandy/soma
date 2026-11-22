package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// ErrUntrustedMutation indicates untrusted external data attempted to auto-activate or overwrite trusted memory.
var ErrUntrustedMutation = errors.New("memory: untrusted_external observations cannot directly activate or supersede rules")

// ProposeRequest specifies a candidate or trusted rule, fact, or preference.
type ProposeRequest struct {
	ID               string
	Scope            string
	Kind             string // rule | fact | preference | negative_path
	Content          string
	TrustTier        string // trusted_user | trusted_repo | untrusted_external
	Status           string // active | proposed (overridden to proposed if untrusted_external)
	ProvenanceStepID string
	EvidencePageID   string
	ValidFrom        int64
}

// StaleFinding describes an active factual memory entry referencing a workspace path that no longer exists.
type StaleFinding struct {
	NodeID      string
	Scope       string
	MissingPath string
	Content     string
}

// Service manages bi-temporal memory graphs and anti-poisoning guards.
type Service struct {
	store *store.Store
	seq   atomic.Uint64
}

// NewService constructs a Memory Service backed by st.
func NewService(st *store.Store) *Service {
	return &Service{store: st}
}

// Propose inserts a MemoryNode. Any untrusted_external input is strictly forced to 'proposed'
// and queued in inbox_items (MINJA poisoning defense).
func (s *Service) Propose(ctx context.Context, req ProposeRequest) (store.MemoryNode, error) {
	now := time.Now().Unix()
	id := req.ID
	if id == "" {
		id = fmt.Sprintf("M-%x-%04d", time.Now().UnixNano(), s.seq.Add(1))
	}
	scope := req.Scope
	if scope == "" {
		scope = "global"
	}
	kind := req.Kind
	if kind == "" {
		kind = "rule"
	}
	tier := req.TrustTier
	if tier == "" {
		tier = "trusted_user"
	}
	status := req.Status
	if status == "" {
		status = "active"
	}
	if tier == "untrusted_external" {
		status = "proposed"
	}
	vf := req.ValidFrom
	if vf == 0 {
		vf = now
	}

	node := store.MemoryNode{
		ID:               id,
		Scope:            scope,
		Kind:             kind,
		Content:          req.Content,
		Status:           status,
		TrustTier:        tier,
		ProvenanceStepID: req.ProvenanceStepID,
		EvidencePageID:   req.EvidencePageID,
		ValidFrom:        vf,
		IngestedAt:       now,
		Enabled:          true,
	}
	if err := s.store.InsertMemoryNode(ctx, node); err != nil {
		return store.MemoryNode{}, err
	}

	if status == "proposed" {
		_ = s.store.InsertInboxItem(ctx, store.InboxItem{
			ID:          "inbox_" + node.ID,
			ItemKind:    "memory_proposal",
			Title:       fmt.Sprintf("Review proposed %s (%s)", node.Kind, node.TrustTier),
			Summary:     node.Content,
			ArtifactRef: node.ID,
			Urgency:     "normal",
		})
	}
	return node, nil
}

// Activate promotes a proposed MemoryNode to active.
func (s *Service) Activate(ctx context.Context, nodeID string) error {
	if err := s.store.ActivateMemoryNode(ctx, nodeID); err != nil {
		return err
	}
	_ = s.store.UpdateInboxStatus(ctx, "inbox_"+nodeID, "approved")
	return nil
}

// Supersede atomically invalidates oldID and activates newReq, blocking untrusted direct overwrites.
func (s *Service) Supersede(ctx context.Context, oldID string, newReq ProposeRequest, provenanceStepID string) (store.MemoryNode, error) {
	return s.Merge(ctx, []string{oldID}, newReq, provenanceStepID)
}

// Merge atomically invalidates oldIDs into a consolidated MemoryNode, blocking untrusted direct overwrites.
func (s *Service) Merge(ctx context.Context, oldIDs []string, mergedReq ProposeRequest, provenanceStepID string) (store.MemoryNode, error) {
	if mergedReq.TrustTier == "untrusted_external" {
		return store.MemoryNode{}, ErrUntrustedMutation
	}
	now := time.Now().Unix()
	id := mergedReq.ID
	if id == "" {
		id = fmt.Sprintf("M-%x-%04d", time.Now().UnixNano(), s.seq.Add(1))
	}
	scope := mergedReq.Scope
	if scope == "" {
		scope = "global"
	}
	kind := mergedReq.Kind
	if kind == "" {
		kind = "rule"
	}
	tier := mergedReq.TrustTier
	if tier == "" {
		tier = "trusted_user"
	}
	vf := mergedReq.ValidFrom
	if vf == 0 {
		vf = now
	}
	node := store.MemoryNode{
		ID:               id,
		Scope:            scope,
		Kind:             kind,
		Content:          mergedReq.Content,
		Status:           "active",
		TrustTier:        tier,
		ProvenanceStepID: provenanceStepID,
		EvidencePageID:   mergedReq.EvidencePageID,
		ValidFrom:        vf,
		IngestedAt:       now,
		Enabled:          true,
	}
	if err := s.store.MergeMemoryNodes(ctx, oldIDs, node, provenanceStepID); err != nil {
		return store.MemoryNode{}, err
	}
	return node, nil
}

// QueryAsOf retrieves enabled memory nodes valid at point-in-time asOfUnix.
func (s *Service) QueryAsOf(ctx context.Context, scopes, kinds []string, asOfUnix int64) ([]store.MemoryNode, error) {
	return s.store.QueryMemoryAsOf(ctx, scopes, kinds, asOfUnix)
}

var pathRefRe = regexp.MustCompile(`(?:file:|` + "`" + `)([A-Za-z0-9_./-]+\.[A-Za-z0-9]{1,8})(?:` + "`" + `|\b)`)

// AuditWorkspace checks active facts/rules for references to relative file paths missing under workspaceDir.
func (s *Service) AuditWorkspace(ctx context.Context, scope, workspaceDir string) ([]StaleFinding, error) {
	var scopes []string
	if scope != "" {
		scopes = []string{scope}
	}
	nodes, err := s.store.ListActiveMemoryByScopes(ctx, scopes, []string{"fact", "rule"})
	if err != nil {
		return nil, err
	}

	var findings []StaleFinding
	for _, n := range nodes {
		matches := pathRefRe.FindAllStringSubmatch(n.Content, -1)
		for _, m := range matches {
			rel := filepath.Clean(m[1])
			if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
				continue
			}
			// Only check explicit file: hints or slash-containing project paths to reduce false positives on extensions like v1.0.
			if !strings.Contains(m[0], "file:") && !strings.Contains(rel, "/") {
				continue
			}
			full := filepath.Join(workspaceDir, rel)
			if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
				findings = append(findings, StaleFinding{
					NodeID:      n.ID,
					Scope:       n.Scope,
					MissingPath: rel,
					Content:     n.Content,
				})
			}
		}
	}
	return findings, nil
}
