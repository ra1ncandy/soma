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

	"github.com/ra1ncandy/soma/internal/embed"
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

// SearchResult pairs a matched MemoryNode with its fused RRF, BM25, and int8 cosine scores.
type SearchResult struct {
	Node        store.MemoryNode
	RRFScore    float64
	BM25Score   float64
	CosineScore float64
}

// Service manages bi-temporal memory graphs, anti-poisoning guards, and hybrid BM25 + int8 MRL retrieval.
type Service struct {
	store    *store.Store
	embedder embed.Embedder
	seq      atomic.Uint64
}

// NewService constructs a Memory Service backed by st with a default 256-dim int8 Matryoshka embedder.
func NewService(st *store.Store) *Service {
	return &Service{
		store:    st,
		embedder: embed.NewLocalSimHashEmbedder(embed.DefaultMatryoshkaDims),
	}
}

// SetEmbedder configures a custom local or remote Embedder (e.g. Qwen3-Embedding, Codestral Embed, Voyage Code 3).
func (s *Service) SetEmbedder(emb embed.Embedder) {
	if emb != nil {
		s.embedder = emb
	}
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
	s.indexNodeEmbedding(ctx, node)

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
	s.indexNodeEmbedding(ctx, node)
	return node, nil
}

func (s *Service) indexNodeEmbedding(ctx context.Context, node store.MemoryNode) {
	if s.embedder == nil || strings.TrimSpace(node.Content) == "" {
		return
	}
	vecs, err := s.embedder.Embed(ctx, []string{node.Content}, false)
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		return
	}
	_ = s.store.UpsertEmbedding(ctx, store.EmbeddingRecord{
		Kind:       "memory",
		TargetID:   node.ID,
		ChunkIndex: 0,
		ModelID:    s.embedder.ModelID(),
		Dims:       len(vecs[0]),
		VecInt8:    vecs[0],
		Snippet:    node.Content,
		CreatedAt:  node.IngestedAt,
	})
}

// Search ranks active memory nodes across scopes and kinds using Hybrid BM25 + Matryoshka int8 Vector RRF.
func (s *Service) Search(ctx context.Context, scopes, kinds []string, query string, topK int) ([]SearchResult, error) {
	nodes, err := s.store.ListActiveMemoryByScopes(ctx, scopes, kinds)
	if err != nil || len(nodes) == 0 {
		return nil, err
	}
	if topK <= 0 || topK > len(nodes) {
		topK = len(nodes)
	}
	if strings.TrimSpace(query) == "" {
		out := make([]SearchResult, 0, topK)
		for i := 0; i < topK; i++ {
			out = append(out, SearchResult{Node: nodes[i], RRFScore: 1.0})
		}
		return out, nil
	}

	ids := make([]string, len(nodes))
	nodeByID := make(map[string]store.MemoryNode, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
		nodeByID[n.ID] = n
	}

	storedVecs := make(map[string][]byte, len(nodes))
	if recs, err := s.store.GetEmbeddingsByTargets(ctx, "memory", ids); err == nil {
		for _, r := range recs {
			if r.ChunkIndex == 0 && len(r.VecInt8) > 0 {
				storedVecs[r.TargetID] = r.VecInt8
			}
		}
	}

	docs := make([]embed.Document, len(nodes))
	for i, n := range nodes {
		vec := storedVecs[n.ID]
		if len(vec) == 0 && s.embedder != nil {
			if gen, gErr := s.embedder.Embed(ctx, []string{n.Content}, false); gErr == nil && len(gen) == 1 && len(gen[0]) > 0 {
				vec = gen[0]
				_ = s.store.UpsertEmbedding(ctx, store.EmbeddingRecord{
					Kind:       "memory",
					TargetID:   n.ID,
					ChunkIndex: 0,
					ModelID:    s.embedder.ModelID(),
					Dims:       len(vec),
					VecInt8:    vec,
					Snippet:    n.Content,
				})
			}
		}
		docs[i] = embed.Document{
			ID:      n.ID,
			Text:    n.Content,
			VecInt8: vec,
			Boost:   1.0,
		}
	}

	ranked, err := embed.HybridSearch(ctx, s.embedder, query, docs, topK)
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(ranked))
	for _, r := range ranked {
		if n, ok := nodeByID[r.ID]; ok {
			out = append(out, SearchResult{
				Node:        n,
				RRFScore:    r.RRFScore,
				BM25Score:   r.BM25Score,
				CosineScore: r.CosineScore,
			})
		}
	}
	return out, nil
}

// FindSemanticConflicts returns active memory nodes in (scope, kind) whose int8 Matryoshka cosine similarity
// with candidateContent is >= minCosine (default 0.80), catching differently worded duplicate or conflicting rules.
func (s *Service) FindSemanticConflicts(ctx context.Context, scope, kind, candidateContent string, minCosine float64) ([]store.MemoryNode, error) {
	if s.embedder == nil || strings.TrimSpace(candidateContent) == "" {
		return nil, nil
	}
	if minCosine <= 0 {
		minCosine = 0.80
	}
	var kinds []string
	if kind != "" {
		kinds = []string{kind}
	}
	nodes, err := s.store.ListActiveMemory(ctx, scope, kinds)
	if err != nil || len(nodes) == 0 {
		return nil, err
	}
	qVecs, err := s.embedder.Embed(ctx, []string{candidateContent}, false)
	if err != nil || len(qVecs) != 1 || len(qVecs[0]) == 0 {
		return nil, err
	}
	qVec := qVecs[0]

	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	storedVecs := make(map[string][]byte, len(nodes))
	if recs, err := s.store.GetEmbeddingsByTargets(ctx, "memory", ids); err == nil {
		for _, r := range recs {
			if r.ChunkIndex == 0 && len(r.VecInt8) > 0 {
				storedVecs[r.TargetID] = r.VecInt8
			}
		}
	}

	var conflicts []store.MemoryNode
	for _, n := range nodes {
		vec := storedVecs[n.ID]
		if len(vec) == 0 {
			if gen, gErr := s.embedder.Embed(ctx, []string{n.Content}, false); gErr == nil && len(gen) == 1 {
				vec = gen[0]
			}
		}
		if len(vec) > 0 && embed.CosineInt8(qVec, vec) >= minCosine {
			conflicts = append(conflicts, n)
		}
	}
	return conflicts, nil
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
