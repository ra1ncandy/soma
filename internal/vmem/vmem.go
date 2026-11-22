package vmem

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

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

// Manager manages @page spilling, previews, slicing, and regex grep against Store.
type Manager struct {
	store *store.Store
	seq   atomic.Uint64
}

// NewManager constructs a virtual memory manager backed by st.
func NewManager(st *store.Store) *Manager {
	return &Manager{store: st}
}

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

	hash, relPath, err := m.store.Blobs().Put(req.Content)
	if err != nil {
		return ObserveResult{}, fmt.Errorf("vmem spill blob: %w", err)
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
		return ObserveResult{}, fmt.Errorf("vmem index page: %w", err)
	}

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
