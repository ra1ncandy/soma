package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/skills"
	"github.com/ra1ncandy/soma/internal/store"
)

// SkillStatus summarizes an AAIF SKILL.md package and its security lint outcome for Tab 2.
type SkillStatus struct {
	Meta          compiler.SkillMeta
	Dir           string
	LintCount     int
	CriticalCount int
	Badge         string // CLEAN | WARN | BLOCKED
}

// Snapshot holds a consistent point-in-time read-only view across all soma SQLite tables.
type Snapshot struct {
	LoadedAt          time.Time
	Runs              []store.Run
	Branches          []store.Branch
	BranchSteps       map[string][]store.Step
	RunPages          map[string][]store.Page
	Contracts         map[string][]store.Contract
	ContractRuns      map[string][]store.ContractRun
	MemoryNodes       []store.MemoryNode
	MemoryEdges       map[string][]store.MemoryEdge
	Skills            []SkillStatus
	Accounts          []store.Account
	TriggerRuns       []store.TriggerRun
	InboxItems        []store.InboxItem
	PendingInboxCount int
	LoadErr           string
}

// StepInspection bundles reconstructed X-Ray frame and raw/emitted payloads for Tab 3.
type StepInspection struct {
	Step        store.Step
	Frame       compiler.CompiledFrame
	Verified    bool
	PagePreview string
	Emitted     dag.EmittedPayload
}

// LoadSnapshot queries WAL reader views for all 4 TUI tabs cleanly without blocking active writers.
func LoadSnapshot(ctx context.Context, st *store.Store, workspaceDir string) (Snapshot, error) {
	snap := Snapshot{
		LoadedAt:     time.Now(),
		BranchSteps:  make(map[string][]store.Step),
		RunPages:     make(map[string][]store.Page),
		Contracts:    make(map[string][]store.Contract),
		ContractRuns: make(map[string][]store.ContractRun),
		MemoryEdges:  make(map[string][]store.MemoryEdge),
	}

	runs, err := st.ListRuns(ctx)
	if err != nil {
		return snap, err
	}
	snap.Runs = runs
	for _, r := range runs {
		if pages, pErr := st.ListRunPages(ctx, r.ID, 25); pErr == nil {
			snap.RunPages[r.ID] = pages
		}
		if ctrs, cErr := st.ListRunContracts(ctx, r.ID); cErr == nil {
			snap.Contracts[r.ID] = ctrs
		}
	}

	branches, err := st.ListBranches(ctx, "")
	if err != nil {
		return snap, err
	}
	snap.Branches = branches
	for _, b := range branches {
		if steps, sErr := st.ListBranchSteps(ctx, b.ID); sErr == nil {
			snap.BranchSteps[b.ID] = steps
		}
		if cruns, crErr := st.ListBranchContractRuns(ctx, b.ID); crErr == nil {
			snap.ContractRuns[b.ID] = cruns
		}
	}

	memNodes, err := st.ListAllMemoryNodes(ctx, "")
	if err != nil {
		return snap, err
	}
	snap.MemoryNodes = memNodes
	for _, m := range memNodes {
		if edges, eErr := st.ListMemoryEdges(ctx, m.ID); eErr == nil && len(edges) > 0 {
			snap.MemoryEdges[m.ID] = edges
		}
	}

	if workspaceDir != "" {
		snap.Skills = loadSkillStatuses([]string{
			filepath.Join(workspaceDir, ".soma", "skills"),
			filepath.Join(workspaceDir, ".skills"),
		})
	}

	if accs, aErr := st.ListAccounts(ctx); aErr == nil {
		snap.Accounts = accs
	}
	if trigs, tErr := st.ListTriggerRuns(ctx, "", 50); tErr == nil {
		snap.TriggerRuns = trigs
	}
	if inb, iErr := st.ListInboxItems(ctx, ""); iErr == nil {
		snap.InboxItems = inb
		for _, it := range inb {
			if it.Status == "pending" {
				snap.PendingInboxCount++
			}
		}
	}

	return snap, nil
}

func loadSkillStatuses(roots []string) []SkillStatus {
	seen := make(map[string]SkillStatus)
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			continue
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			dir := filepath.Join(root, ent.Name())
			sk, pErr := skills.ParseSkillMD(dir)
			if pErr != nil {
				continue
			}
			findings, _ := skills.LintSkillDir(dir)
			crit := 0
			for _, f := range findings {
				if f.Severity == "critical" {
					crit++
				}
			}
			badge := "CLEAN"
			if crit > 0 {
				badge = "BLOCKED"
			} else if len(findings) > 0 {
				badge = "WARN"
			}
			seen[sk.Frontmatter.Name] = SkillStatus{
				Meta: compiler.SkillMeta{
					Name:        sk.Frontmatter.Name,
					Description: sk.Frontmatter.Description,
				},
				Dir:           dir,
				LintCount:     len(findings),
				CriticalCount: crit,
				Badge:         badge,
			}
		}
	}
	out := make([]SkillStatus, 0, len(seen))
	for _, st := range seen {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out
}

func inspectStep(ctx context.Context, st *store.Store, step store.Step) (StepInspection, error) {
	frame, err := compiler.ReconstructFrame(ctx, st, step.ID, compiler.WorkerProfile())
	if err != nil {
		return StepInspection{}, err
	}
	var em dag.EmittedPayload
	if raw, bErr := st.Blobs().Get(step.EmittedBlobHash); bErr == nil {
		_ = json.Unmarshal(raw, &em)
	}
	var pagePreview string
	if step.ObservationPageID != "" {
		if pg, pErr := st.GetPage(ctx, step.ObservationPageID); pErr == nil {
			pagePreview = pg.PreviewText
		}
	}
	return StepInspection{
		Step:        step,
		Frame:       frame,
		Verified:    frame.CompiledFrameHash == step.CompiledFrameHash,
		PagePreview: pagePreview,
		Emitted:     em,
	}, nil
}
