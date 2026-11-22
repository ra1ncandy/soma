package vmem

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/embed"
	"github.com/ra1ncandy/soma/internal/store"
)

const (
	// MaxSliceLines caps a single page_slice syscall to avoid blowing context back up.
	MaxSliceLines = 200
	// DefaultPreviewLines bounds structural previews in Layer 2 manifests.
	DefaultPreviewLines = 15
)

// ObserveRequest describes an observation produced by a tool call or external fetch.
type ObserveRequest struct {
	RunID                string
	NameHint             string
	Content              []byte
	TrustTier            string
	InlineThresholdBytes int
}

// ObserveResult is either an inline string (when <= threshold) or a spilled @page manifest.
type ObserveResult struct {
	Paged      bool
	InlineText string
	PageID     string
	Manifest   string
	Page       store.Page
}

// Manager manages @page spilling, previews, slicing, regex grep, and hybrid BM25 + int8 MRL search against Store.
type Manager struct {
	store    *store.Store
	embedder embed.Embedder
	seq      atomic.Uint64
}

// NewManager constructs a virtual memory manager backed by st with a default 256-dim int8 Matryoshka embedder.
func NewManager(st *store.Store) *Manager {
	return &Manager{
		store:    st,
		embedder: embed.NewLocalSimHashEmbedder(embed.DefaultMatryoshkaDims),
	}
}

// SetEmbedder configures a custom local or remote Embedder for @page chunk indexing and search.
func (m *Manager) SetEmbedder(emb embed.Embedder) {
	if emb != nil {
		m.embedder = emb
	}
}

// Store returns the underlying SQLite store backing this virtual memory manager.
func (m *Manager) Store() *store.Store { return m.store }

// Observe implements the Anti-Syscall-Tax rule: small outputs return inline; large outputs spill to zstd blobs.
func (m *Manager) Observe(ctx context.Context, req ObserveRequest) (ObserveResult, error) {
	threshold := req.InlineThresholdBytes
	if threshold <= 0 {
		threshold = 3 * 1024
	}
	if req.TrustTier == "" {
		req.TrustTier = "trusted_repo"
	}

	if len(req.Content) <= threshold {
		return ObserveResult{
			Paged:      false,
			InlineText: string(req.Content),
		}, nil
	}

	page, err := m.spill(ctx, req)
	if err != nil {
		return ObserveResult{}, err
	}
	pageID, hash, ct, preview := page.ID, page.ContentHash, ContentType(page.ContentType), page.PreviewText

	manifest := fmt.Sprintf(
		"@%s [%s | %s | %d bytes | %d lines | sha256:%s]\n%s\n(Use page_grep(%q, pattern) or page_slice(%q, start, end) to inspect raw lines.)",
		pageID, ct, req.TrustTier, page.ByteSize, page.LineCount, hash[:12], preview, pageID, pageID,
	)

	return ObserveResult{
		Paged:    true,
		PageID:   pageID,
		Manifest: manifest,
		Page:     page,
	}, nil
}

// RecordProvenance stores content as an @page regardless of size and returns its page ID, without
// producing a manifest. The executor uses it to give small inline untrusted observations a
// provenance record, so taint tracking cannot be dodged by keeping payloads under the inline threshold.
func (m *Manager) RecordProvenance(ctx context.Context, req ObserveRequest) (string, error) {
	if req.TrustTier == "" {
		req.TrustTier = "trusted_repo"
	}
	page, err := m.spill(ctx, req)
	if err != nil {
		return "", err
	}
	return page.ID, nil
}

func (m *Manager) spill(ctx context.Context, req ObserveRequest) (store.Page, error) {
	hash, relPath, err := m.store.Blobs().Put(req.Content)
	if err != nil {
		return store.Page{}, fmt.Errorf("vmem spill blob: %w", err)
	}

	ct := DetectContentType(req.NameHint, req.Content)
	preview := GeneratePreview(ct, req.Content, DefaultPreviewLines)
	lineCount := countLines(req.Content)
	n := m.seq.Add(1)
	pageID := fmt.Sprintf("page_%d_%04d_%s", time.Now().UnixNano(), n, hash[:6])

	page := store.Page{
		ID:          pageID,
		RunID:       req.RunID,
		ContentHash: hash,
		BlobPath:    relPath,
		ContentType: string(ct),
		TrustTier:   req.TrustTier,
		ByteSize:    int64(len(req.Content)),
		LineCount:   lineCount,
		PreviewText: preview,
	}
	if err := m.store.PutPage(ctx, page); err != nil {
		return store.Page{}, fmt.Errorf("vmem index page: %w", err)
	}
	return page, nil
}

// Slice returns 1-indexed inclusive lines [startLine, endLine] capped at MaxSliceLines.
func (m *Manager) Slice(ctx context.Context, pageID string, startLine, endLine int) (string, error) {
	page, raw, err := m.loadPage(ctx, pageID)
	if err != nil {
		return "", err
	}
	lines := splitLines(raw)
	if len(lines) == 0 {
		return fmt.Sprintf("@%s: empty (0 lines)", page.ID), nil
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine + 49
	}
	if endLine-startLine+1 > MaxSliceLines {
		endLine = startLine + MaxSliceLines - 1
	}
	if startLine > len(lines) {
		return fmt.Sprintf("@%s: startLine %d exceeds line_count %d", page.ID, startLine, len(lines)), nil
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "@%s lines %d..%d of %d:\n", page.ID, startLine, endLine, len(lines))
	for i := startLine; i <= endLine; i++ {
		fmt.Fprintf(&b, "L%d: %s\n", i, lines[i-1])
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// Grep searches a spilled @page with a Go regex and returns up to maxMatches numbered lines.
func (m *Manager) Grep(ctx context.Context, pageID, pattern string, maxMatches int) (string, error) {
	if maxMatches <= 0 {
		maxMatches = 40
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid grep pattern: %w", err)
	}
	page, raw, err := m.loadPage(ctx, pageID)
	if err != nil {
		return "", err
	}
	lines := splitLines(raw)

	var matches []string
	total := 0
	for i, line := range lines {
		if re.MatchString(line) {
			total++
			if len(matches) < maxMatches {
				matches = append(matches, fmt.Sprintf("L%d: %s", i+1, truncateLine(line, 200)))
			}
		}
	}
	if total == 0 {
		return fmt.Sprintf("@%s: 0 matches for %q (%d lines)", page.ID, pattern, len(lines)), nil
	}
	header := fmt.Sprintf("@%s: %d match(es) for %q (showing %d):", page.ID, total, pattern, len(matches))
	return header + "\n" + strings.Join(matches, "\n"), nil
}

// Search performs Hybrid BM25 + Matryoshka int8 Vector RRF search across 40-line chunks of spilled @page blobs
// in runID (or a specific pageIDFilter if non-empty), returning top-K matching line ranges.
func (m *Manager) Search(ctx context.Context, runID, query, pageIDFilter string, topK int) (string, error) {
	if topK <= 0 {
		topK = 5
	}
	if topK > 10 {
		topK = 10
	}
	pageIDFilter = strings.TrimPrefix(strings.TrimSpace(pageIDFilter), "@")

	var pages []store.Page
	if pageIDFilter != "" {
		p, err := m.store.GetPage(ctx, pageIDFilter)
		if err != nil {
			return "", err
		}
		pages = []store.Page{p}
	} else {
		var err error
		pages, err = m.store.ListRunPages(ctx, runID, 50)
		if err != nil {
			return "", err
		}
		if len(pages) == 0 && runID != "" {
			pages, err = m.store.ListRunPages(ctx, "", 50)
			if err != nil {
				return "", err
			}
		}
	}
	if len(pages) == 0 {
		return "page_search: no spilled @page handles available", nil
	}

	const chunkLines = 40
	type chunkMeta struct {
		pageID    string
		chunkIdx  int
		startLine int
		endLine   int
		text      string
	}

	var docs []embed.Document
	metaByDocID := make(map[string]chunkMeta)

	for _, pg := range pages {
		raw, err := m.store.Blobs().Get(pg.ContentHash)
		if err != nil {
			continue
		}
		_ = m.store.TouchPage(ctx, pg.ID)
		lines := splitLines(raw)
		if len(lines) == 0 {
			continue
		}

		cachedVecs := make(map[int][]byte)
		if recs, err := m.store.GetEmbeddingsByTargets(ctx, "page_chunk", []string{pg.ID}); err == nil {
			for _, r := range recs {
				if len(r.VecInt8) > 0 {
					cachedVecs[r.ChunkIndex] = r.VecInt8
				}
			}
		}

		var uncachedIdx []int
		var uncachedMeta []chunkMeta
		var uncachedTexts []string

		numChunks := (len(lines) + chunkLines - 1) / chunkLines
		pageChunks := make([]chunkMeta, 0, numChunks)
		for cIdx := 0; cIdx < numChunks; cIdx++ {
			start := cIdx*chunkLines + 1
			end := start + chunkLines - 1
			if end > len(lines) {
				end = len(lines)
			}
			var sb strings.Builder
			for l := start; l <= end; l++ {
				fmt.Fprintf(&sb, "L%d: %s\n", l, truncateLine(lines[l-1], 180))
			}
			txt := strings.TrimRight(sb.String(), "\n")
			cm := chunkMeta{
				pageID:    pg.ID,
				chunkIdx:  cIdx,
				startLine: start,
				endLine:   end,
				text:      txt,
			}
			pageChunks = append(pageChunks, cm)
			if _, ok := cachedVecs[cIdx]; !ok && m.embedder != nil {
				uncachedIdx = append(uncachedIdx, cIdx)
				uncachedMeta = append(uncachedMeta, cm)
				uncachedTexts = append(uncachedTexts, txt)
			}
		}

		if len(uncachedTexts) > 0 && m.embedder != nil {
			if vecs, err := m.embedder.Embed(ctx, uncachedTexts, false); err == nil && len(vecs) == len(uncachedIdx) {
				for j, cIdx := range uncachedIdx {
					cachedVecs[cIdx] = vecs[j]
					cm := uncachedMeta[j]
					_ = m.store.UpsertEmbedding(ctx, store.EmbeddingRecord{
						Kind:       "page_chunk",
						TargetID:   pg.ID,
						ChunkIndex: cIdx,
						StartLine:  cm.startLine,
						EndLine:    cm.endLine,
						ModelID:    m.embedder.ModelID(),
						Dims:       len(vecs[j]),
						VecInt8:    vecs[j],
					})
				}
			}
		}

		for _, cm := range pageChunks {
			docID := fmt.Sprintf("%s#%d", cm.pageID, cm.chunkIdx)
			metaByDocID[docID] = cm
			docs = append(docs, embed.Document{
				ID:      docID,
				Text:    cm.text,
				VecInt8: cachedVecs[cm.chunkIdx],
				Boost:   1.0,
			})
		}
	}

	ranked, err := embed.HybridSearch(ctx, m.embedder, query, docs, topK)
	if err != nil {
		return "", err
	}
	if len(ranked) == 0 {
		return fmt.Sprintf("page_search: 0 matches for %q across %d page(s)", query, len(pages)), nil
	}

	var out strings.Builder
	fmt.Fprintf(&out, "page_search: top %d chunk(s) for %q (Hybrid BM25 + int8 MRL RRF):\n", len(ranked), query)
	for rankIdx, r := range ranked {
		cm := metaByDocID[r.ID]
		fmt.Fprintf(&out, "--- [%d] @%s lines %d..%d (rrf=%.4f bm25=%.2f cos=%.2f) ---\n",
			rankIdx+1, cm.pageID, cm.startLine, cm.endLine, r.RRFScore, r.BM25Score, r.CosineScore)
		// Show up to 10 best matching lines from the chunk so observation stays compact.
		out.WriteString(compactChunkSnippet(cm.text, query, 10))
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func compactChunkSnippet(chunkText, query string, maxLines int) string {
	lines := strings.Split(chunkText, "\n")
	if len(lines) <= maxLines {
		return chunkText
	}
	qToks := embed.Tokenize(query)
	type scoredLine struct {
		idx  int
		hits int
		line string
	}
	var matched []int
	for i, l := range lines {
		low := strings.ToLower(l)
		hits := 0
		for _, qt := range qToks {
			if strings.Contains(low, qt) {
				hits++
			}
		}
		if hits > 0 {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		return strings.Join(lines[:maxLines], "\n")
	}
	keep := make(map[int]bool)
	for _, idx := range matched {
		for d := -1; d <= 1; d++ {
			if idx+d >= 0 && idx+d < len(lines) && len(keep) < maxLines {
				keep[idx+d] = true
			}
		}
	}
	var out []string
	for i, l := range lines {
		if keep[i] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func (m *Manager) loadPage(ctx context.Context, pageID string) (store.Page, []byte, error) {
	page, err := m.store.GetPage(ctx, pageID)
	if err != nil {
		return store.Page{}, nil, err
	}
	raw, err := m.store.Blobs().Get(page.ContentHash)
	if err != nil {
		return store.Page{}, nil, err
	}
	_ = m.store.TouchPage(ctx, pageID)
	return page, raw, nil
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func splitLines(raw []byte) []string {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
