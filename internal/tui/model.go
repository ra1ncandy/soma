package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/inbox"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/replay"
	"github.com/ra1ncandy/soma/internal/store"
)

// TabID identifies the active screen in the 4-tab operator interface.
type TabID int

const (
	TabDAG TabID = iota
	TabMemory
	TabXRay
	TabDaemon
)

const refreshInterval = 2 * time.Second

// TickMsg triggers periodic non-blocking WAL snapshot refreshes.
type TickMsg time.Time

// SnapshotMsg carries a newly loaded read-only workspace snapshot.
type SnapshotMsg struct {
	Snapshot Snapshot
	Err      error
}

// ActionResultMsg delivers the outcome of a background user action plus a fresh snapshot.
type ActionResultMsg struct {
	Status   string
	Err      error
	Snapshot Snapshot
}

// EditorDoneMsg delivers edits written in the operator's external $EDITOR.
type EditorDoneMsg struct {
	NodeID   string
	TempFile string
	Original string
	Err      error
}

// Model implements charm.land/bubbletea/v2 Model for soma's 4-tab kernel console.
type Model struct {
	store        *store.Store
	workspaceDir string
	styles       styleset
	width        int
	height       int

	activeTab TabID
	snap      Snapshot
	loaded    bool

	branchCursor int
	stepCursor   int
	memoryCursor int
	memoryPane   int // 0=Memory Nodes, 1=AAIF Skills
	showEdges    bool
	inboxCursor  int
	showDrawer   bool
	showHelp     bool

	statusMsg  string
	lastReplay string
	frameCache map[string]StepInspection
}

// New constructs the 4-tab Bubble Tea v2 Model for st and workspaceDir.
func New(st *store.Store, workspaceDir string) Model {
	m := Model{
		store:        st,
		workspaceDir: workspaceDir,
		styles:       defaultStyles(),
		width:        100,
		height:       32,
		frameCache:   make(map[string]StepInspection),
	}
	if st != nil {
		if snap, err := LoadSnapshot(context.Background(), st, workspaceDir); err == nil {
			m.snap = snap
			m.loaded = true
			m.clampCursors()
			m.ensureInspected()
		}
	}
	return m
}

// Init schedules initial snapshot loading and periodic refresh ticks.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadSnapshotCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

func (m Model) loadSnapshotCmd() tea.Cmd {
	st := m.store
	ws := m.workspaceDir
	return func() tea.Msg {
		snap, err := LoadSnapshot(context.Background(), st, ws)
		return SnapshotMsg{Snapshot: snap, Err: err}
	}
}

// Update handles terminal resize, keypresses, ticks, async snapshot refreshes, and editor exits.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case TickMsg:
		return m, tea.Batch(m.loadSnapshotCmd(), tickCmd())

	case SnapshotMsg:
		if msg.Err != nil {
			m.statusMsg = "snapshot error: " + msg.Err.Error()
			return m, nil
		}
		m.snap = msg.Snapshot
		m.loaded = true
		m.clampCursors()
		m.ensureInspected()
		return m, nil

	case ActionResultMsg:
		if len(msg.Snapshot.Runs) > 0 || msg.Snapshot.LoadedAt != (time.Time{}) {
			m.snap = msg.Snapshot
			m.loaded = true
			m.clampCursors()
		}
		if msg.Err != nil {
			m.statusMsg = "error: " + msg.Err.Error()
		} else if msg.Status != "" {
			m.statusMsg = msg.Status
		}
		m.ensureInspected()
		return m, nil

	case EditorDoneMsg:
		defer func() { _ = os.Remove(msg.TempFile) }()
		if msg.Err != nil {
			m.statusMsg = "editor error: " + msg.Err.Error()
			return m, nil
		}
		raw, err := os.ReadFile(msg.TempFile)
		if err != nil {
			m.statusMsg = "read edited memory failed: " + err.Error()
			return m, nil
		}
		updated := strings.TrimSpace(string(raw))
		if updated == "" || updated == strings.TrimSpace(msg.Original) {
			m.statusMsg = "memory unchanged"
			return m, nil
		}
		if err := m.store.UpdateMemoryContent(context.Background(), msg.NodeID, updated); err != nil {
			m.statusMsg = "save memory failed: " + err.Error()
			return m, nil
		}
		m.statusMsg = fmt.Sprintf("updated memory %s", msg.NodeID)
		return m, m.loadSnapshotCmd()

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "1":
		m.activeTab = TabDAG
		m.ensureInspected()
		return m, nil
	case "2":
		m.activeTab = TabMemory
		return m, nil
	case "3":
		m.activeTab = TabXRay
		m.ensureInspected()
		return m, nil
	case "4":
		m.activeTab = TabDaemon
		return m, nil
	case "tab":
		m.activeTab = (m.activeTab + 1) % 4
		m.ensureInspected()
		return m, nil
	case "shift+tab":
		m.activeTab = (m.activeTab + 3) % 4
		m.ensureInspected()
		return m, nil
	case "?":
		m.showHelp = !m.showHelp
		return m, nil
	case "R":
		m.statusMsg = "refreshing..."
		return m, m.loadSnapshotCmd()
	}

	if m.showHelp && (key == "esc" || key == "escape") {
		m.showHelp = false
		return m, nil
	}

	switch m.activeTab {
	case TabDAG:
		return m.handleTabDAGKey(key)
	case TabMemory:
		return m.handleTabMemoryKey(key)
	case TabXRay:
		return m.handleTabXRayKey(key)
	case TabDaemon:
		return m.handleTabDaemonKey(key)
	}
	return m, nil
}

func (m Model) handleTabDAGKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		if m.branchCursor+1 < len(m.snap.Branches) {
			m.branchCursor++
			m.stepCursor = maxInt(0, len(m.currentSteps())-1)
			m.ensureInspected()
		}
	case "k", "up":
		if m.branchCursor > 0 {
			m.branchCursor--
			m.stepCursor = maxInt(0, len(m.currentSteps())-1)
			m.ensureInspected()
		}
	case "x":
		if br, ok := m.selectedBranch(); ok {
			steps := m.snap.BranchSteps[br.ID]
			if len(steps) > 0 {
				m.stepCursor = len(steps) - 1
			}
			m.activeTab = TabXRay
			m.ensureInspected()
		}
	case "f":
		if br, ok := m.selectedBranch(); ok && br.HeadStepID != "" {
			return m, m.forkCmd(br.RunID, br.HeadStepID)
		}
		m.statusMsg = "selected branch has no head step to fork from"
	case "r":
		if br, ok := m.selectedBranch(); ok {
			return m, m.exactReplayCmd(br.ID)
		}
	}
	return m, nil
}

func (m Model) handleTabMemoryKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "h", "left", "l", "right":
		m.memoryPane = 1 - m.memoryPane
	case "j", "down":
		if m.memoryPane == 0 && m.memoryCursor+1 < len(m.snap.MemoryNodes) {
			m.memoryCursor++
		}
	case "k", "up":
		if m.memoryPane == 0 && m.memoryCursor > 0 {
			m.memoryCursor--
		}
	case "s":
		m.showEdges = !m.showEdges
	case " ", "space":
		if m.memoryPane == 0 {
			if node, ok := m.selectedMemory(); ok {
				return m, m.toggleMemoryCmd(node.ID, !node.Enabled)
			}
		}
	case "a":
		if m.memoryPane == 0 {
			if node, ok := m.selectedMemory(); ok && node.Status == "proposed" {
				return m, m.activateMemoryCmd(node.ID)
			}
		}
	case "e":
		if m.memoryPane == 0 {
			if node, ok := m.selectedMemory(); ok {
				return m, m.editMemoryCmd(node)
			}
		}
	}
	return m, nil
}

func (m Model) handleTabXRayKey(key string) (tea.Model, tea.Cmd) {
	steps := m.currentSteps()
	switch key {
	case "h", "left", "k", "up":
		if m.stepCursor > 0 {
			m.stepCursor--
			m.ensureInspected()
		}
	case "l", "right", "j", "down":
		if m.stepCursor+1 < len(steps) {
			m.stepCursor++
			m.ensureInspected()
		}
	case "r":
		if br, ok := m.selectedBranch(); ok {
			return m, m.exactReplayCmd(br.ID)
		}
	case "f":
		if br, ok := m.selectedBranch(); ok && len(steps) > 0 && m.stepCursor < len(steps) {
			return m, m.forkCmd(br.RunID, steps[m.stepCursor].ID)
		}
	}
	return m, nil
}

func (m Model) handleTabDaemonKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		if m.inboxCursor+1 < len(m.snap.InboxItems) {
			m.inboxCursor++
		}
	case "k", "up":
		if m.inboxCursor > 0 {
			m.inboxCursor--
		}
	case "v":
		m.showDrawer = !m.showDrawer
	case "a":
		if it, ok := m.selectedInboxItem(); ok {
			return m, m.inboxActionCmd(it.ID, true)
		}
	case "d":
		if it, ok := m.selectedInboxItem(); ok {
			return m, m.inboxActionCmd(it.ID, false)
		}
	}
	return m, nil
}

func (m Model) forkCmd(runID, stepID string) tea.Cmd {
	st := m.store
	ws := m.workspaceDir
	return func() tea.Msg {
		ctx := context.Background()
		exec := dag.NewExecutor(st, nil, nil, compiler.WorkerProfile())
		newID, err := exec.ForkBranch(ctx, runID, stepID, fmt.Sprintf("tui-%d", time.Now().Unix()%1000))
		snap, _ := LoadSnapshot(ctx, st, ws)
		if err != nil {
			return ActionResultMsg{Err: err, Snapshot: snap}
		}
		return ActionResultMsg{Status: fmt.Sprintf("forked branch %s from %s", newID, stepID), Snapshot: snap}
	}
}

func (m Model) exactReplayCmd(branchID string) tea.Cmd {
	st := m.store
	ws := m.workspaceDir
	return func() tea.Msg {
		ctx := context.Background()
		rep, err := replay.NewReplayer(st).Replay(ctx, branchID, replay.ReplayOptions{Mode: replay.ModeExact})
		snap, _ := LoadSnapshot(ctx, st, ws)
		if err != nil {
			return ActionResultMsg{Err: err, Snapshot: snap}
		}
		msg := fmt.Sprintf("exact replay %s: %d/%d SHA-256 verified (diverged=%v)",
			branchID, rep.HashVerifiedCount, rep.StepsChecked, rep.Diverged)
		return ActionResultMsg{Status: msg, Snapshot: snap}
	}
}

func (m Model) toggleMemoryCmd(nodeID string, enabled bool) tea.Cmd {
	st := m.store
	ws := m.workspaceDir
	return func() tea.Msg {
		ctx := context.Background()
		err := st.SetMemoryEnabled(ctx, nodeID, enabled)
		snap, _ := LoadSnapshot(ctx, st, ws)
		return ActionResultMsg{
			Status:   fmt.Sprintf("memory %s enabled=%v", nodeID, enabled),
			Err:      err,
			Snapshot: snap,
		}
	}
}

func (m Model) activateMemoryCmd(nodeID string) tea.Cmd {
	st := m.store
	ws := m.workspaceDir
	return func() tea.Msg {
		ctx := context.Background()
		err := memory.NewService(st).Activate(ctx, nodeID)
		snap, _ := LoadSnapshot(ctx, st, ws)
		return ActionResultMsg{
			Status:   fmt.Sprintf("activated memory %s", nodeID),
			Err:      err,
			Snapshot: snap,
		}
	}
}

func (m Model) editMemoryCmd(node store.MemoryNode) tea.Cmd {
	tmp, err := os.CreateTemp("", "soma-memory-*.md")
	if err != nil {
		return func() tea.Msg { return ActionResultMsg{Err: err} }
	}
	tmpPath := tmp.Name()
	_, _ = tmp.WriteString(node.Content + "\n")
	_ = tmp.Close()

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	return tea.ExecProcess(exec.Command(editor, tmpPath), func(err error) tea.Msg {
		return EditorDoneMsg{
			NodeID:   node.ID,
			TempFile: tmpPath,
			Original: node.Content,
			Err:      err,
		}
	})
}

func (m Model) inboxActionCmd(itemID string, approve bool) tea.Cmd {
	st := m.store
	ws := m.workspaceDir
	return func() tea.Msg {
		ctx := context.Background()
		svc := inbox.NewService(st)
		var out string
		var err error
		if approve {
			out, err = svc.Approve(ctx, itemID, "tui")
		} else {
			out, err = svc.Dismiss(ctx, itemID, "tui")
		}
		snap, _ := LoadSnapshot(ctx, st, ws)
		return ActionResultMsg{Status: out, Err: err, Snapshot: snap}
	}
}

// View renders the active tab, status bar, and key hints in a Bubble Tea v2 tea.View.
func (m Model) View() tea.View {
	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteByte('\n')

	if m.showHelp {
		b.WriteString(m.renderHelp())
	} else {
		switch m.activeTab {
		case TabDAG:
			b.WriteString(m.renderTabDAG())
		case TabMemory:
			b.WriteString(m.renderTabMemory())
		case TabXRay:
			b.WriteString(m.renderTabXRay())
		case TabDaemon:
			b.WriteString(m.renderTabDaemon())
		}
	}

	b.WriteByte('\n')
	b.WriteString(m.renderFooter())

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.WindowTitle = "soma — Agent Kernel"
	return v
}

func (m Model) renderHeader() string {
	tabs := []struct {
		id    TabID
		label string
	}{
		{TabDAG, "1:DAG & HUD"},
		{TabMemory, "2:Memory & Skills"},
		{TabXRay, "3:X-Ray & Replay"},
		{TabDaemon, fmt.Sprintf("4:Daemon/Inbox(%d)", m.snap.PendingInboxCount)},
	}
	parts := []string{m.styles.headerBar.Render("SOMA v0.3")}
	for _, t := range tabs {
		if m.activeTab == t.id {
			parts = append(parts, m.styles.tabActive.Render(t.label))
		} else {
			parts = append(parts, m.styles.tabIdle.Render(t.label))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

func (m Model) renderTabDAG() string {
	if len(m.snap.Branches) == 0 {
		return m.styles.panel.Render("No runs or branches recorded yet.\nRun `soma run --mock --goal \"...\"` to start a durable DAG run.")
	}

	var left strings.Builder
	left.WriteString(m.styles.title.Render("Branches") + "\n")
	for i, b := range m.snap.Branches {
		line := fmt.Sprintf("%s (%s) e%d steps=%d %s",
			b.ID, b.WorkspaceStrategy, b.Epoch, len(m.snap.BranchSteps[b.ID]), b.Status)
		if i == m.branchCursor {
			left.WriteString(m.styles.cursorRow.Render("> "+line) + "\n")
		} else {
			left.WriteString("  " + line + "\n")
		}
	}

	br, _ := m.selectedBranch()
	var right strings.Builder
	right.WriteString(m.styles.title.Render("HUD & Frame Breakdown") + "\n")
	fmt.Fprintf(&right, "Branch: %s | Run: %s %s\n", br.ID, br.RunID, m.styles.statusBadge(br.Status))
	fmt.Fprintf(&right, "Workspace: %s (%s)\n", br.WorkspaceDir, br.WorkspaceStrategy)
	fmt.Fprintf(&right, "Account: %s | Epoch: %d | Stalls: %d\n", emptyDash(br.ActiveAccountID), br.Epoch, br.StallCount)

	steps := m.snap.BranchSteps[br.ID]
	var inToks, cacheToks int
	for _, s := range steps {
		inToks += s.InputTokens + s.CacheReadTokens
		cacheToks += s.CacheReadTokens
	}
	hitPct := 0.0
	if inToks > 0 {
		hitPct = 100.0 * float64(cacheToks) / float64(inToks)
	}
	fmt.Fprintf(&right, "KV-Cache Hit: %.1f%% (%d/%d tokens)\n", hitPct, cacheToks, inToks)

	if br.HeadStepID != "" {
		if insp, ok := m.frameCache[br.HeadStepID]; ok {
			fmt.Fprintf(&right, "Frame: [L0:%d | L1:%d | L2:%d | L3:%d] = %d tok\n",
				insp.Frame.L0Tokens, insp.Frame.L1Tokens, insp.Frame.L2Tokens, insp.Frame.L3Tokens, insp.Frame.TotalTokens)
		}
	}

	ctrs := m.snap.Contracts[br.RunID]
	cruns := m.snap.ContractRuns[br.ID]
	passed := 0
	for _, cr := range cruns {
		if cr.Passed {
			passed++
		}
	}
	fmt.Fprintf(&right, "Oracles: %d contract(s), %d/%d runs passed\n", len(ctrs), passed, len(cruns))

	pages := m.snap.RunPages[br.RunID]
	if len(pages) > 0 {
		right.WriteString("\n" + m.styles.title.Render("@page Spills") + "\n")
		limit := len(pages)
		if limit > 3 {
			limit = 3
		}
		for i := 0; i < limit; i++ {
			pg := pages[i]
			fmt.Fprintf(&right, "  @%s (%s, %dB, %s): %s\n",
				pg.ID, pg.ContentType, pg.ByteSize, pg.TrustTier, oneLine(pg.PreviewText, 64))
		}
	}

	return lipgloss.JoinHorizontal(lipgloss.Top,
		m.styles.panel.Render(left.String()),
		m.styles.panel.Render(right.String()),
	)
}

func (m Model) renderTabMemory() string {
	var b strings.Builder
	paneName := "Memory Nodes"
	if m.memoryPane == 1 {
		paneName = "AAIF Skills"
	}
	fmt.Fprintf(&b, "%s  [h/l switch pane: %s]\n\n", m.styles.title.Render("Bi-Temporal Memory & Skills"), paneName)

	if m.memoryPane == 0 {
		if len(m.snap.MemoryNodes) == 0 {
			b.WriteString("No memory nodes stored.\n")
			return m.styles.panel.Render(b.String())
		}
		for i, n := range m.snap.MemoryNodes {
			en := "on"
			if !n.Enabled {
				en = "off"
			}
			row := fmt.Sprintf("[%s] %s (%s/%s, %s, enabled=%s) %s",
				n.ID, m.styles.statusBadge(n.Status), n.Scope, n.Kind, n.TrustTier, en, oneLine(n.Content, 60))
			if i == m.memoryCursor {
				b.WriteString(m.styles.cursorRow.Render("> "+row) + "\n")
			} else {
				b.WriteString("  " + row + "\n")
			}
		}
		if m.showEdges {
			if node, ok := m.selectedMemory(); ok {
				b.WriteString("\n" + m.styles.title.Render("Provenance Edges for "+node.ID) + "\n")
				edges := m.snap.MemoryEdges[node.ID]
				if len(edges) == 0 {
					b.WriteString("  (no SUPERSEDES/MERGES edges)\n")
				} else {
					for _, e := range edges {
						fmt.Fprintf(&b, "  %s --[%s]--> %s (step=%s)\n",
							e.FromNodeID, e.Relation, e.ToNodeID, emptyDash(e.ProvenanceStepID))
					}
				}
			}
		}
		return m.styles.panel.Render(b.String())
	}

	if len(m.snap.Skills) == 0 {
		b.WriteString("No AAIF skills found in .soma/skills or .skills.\n")
		return m.styles.panel.Render(b.String())
	}
	for _, sk := range m.snap.Skills {
		fmt.Fprintf(&b, "  - %s %s (findings=%d, critical=%d): %s\n",
			sk.Meta.Name, m.styles.statusBadge(sk.Badge), sk.LintCount, sk.CriticalCount, oneLine(sk.Meta.Description, 60))
	}
	return m.styles.panel.Render(b.String())
}

func (m Model) renderTabXRay() string {
	steps := m.currentSteps()
	if len(steps) == 0 {
		return m.styles.panel.Render("No Merkle steps recorded on selected branch.")
	}
	if m.stepCursor >= len(steps) {
		m.stepCursor = len(steps) - 1
	}
	step := steps[m.stepCursor]
	insp, ok := m.frameCache[step.ID]

	var b strings.Builder
	fmt.Fprintf(&b, "%s  Step %d/%d: %s (epoch=%d turn=%d model=%s)\n\n",
		m.styles.title.Render("3-Boundary X-Ray & Replay"), m.stepCursor+1, len(steps), step.ID, step.Epoch, step.TurnIndex, step.ModelID)

	b.WriteString(m.styles.title.Render("[1] RAW @PAGE OBSERVATION") + "\n")
	if step.ObservationPageID == "" {
		b.WriteString("  (inline observation / no @page spilled)\n")
	} else {
		fmt.Fprintf(&b, "  @%s: %s\n", step.ObservationPageID, oneLine(insp.PagePreview, 120))
	}

	b.WriteString("\n" + m.styles.title.Render("[2] RECONSTRUCTED WIRE FRAME") + "\n")
	if !ok {
		b.WriteString("  (reconstructing...)\n")
	} else {
		badge := "MISMATCH"
		if insp.Verified {
			badge = "VERIFIED"
		}
		fmt.Fprintf(&b, "  SHA-256: %s %s | PrefixL0L1: %s\n",
			insp.Frame.CompiledFrameHash[:16], m.styles.statusBadge(badge), insp.Frame.PrefixHashL0L1[:16])
		fmt.Fprintf(&b, "  Tokens: L0=%d | L1=%d | L2=%d | L3=%d | Total=%d\n",
			insp.Frame.L0Tokens, insp.Frame.L1Tokens, insp.Frame.L2Tokens, insp.Frame.L3Tokens, insp.Frame.TotalTokens)
	}

	b.WriteString("\n" + m.styles.title.Render("[3] EMITTED MODEL OUTPUT & PROVENANCE") + "\n")
	fmt.Fprintf(&b, "  Account: %s | InputPages: %s | Latency: %dms\n",
		step.AccountID, step.InputProvenanceJSON, step.LatencyMS)
	if ok {
		fmt.Fprintf(&b, "  Reasoning: %s\n", oneLine(insp.Emitted.Response.Reasoning, 140))
	}
	return m.styles.panel.Render(b.String())
}

func (m Model) renderTabDaemon() string {
	var b strings.Builder
	b.WriteString(m.styles.title.Render("Accounts & Quota Leases") + "\n")
	if len(m.snap.Accounts) == 0 {
		b.WriteString("  (no accounts configured)\n")
	} else {
		nowMs := time.Now().UnixMilli()
		for _, a := range m.snap.Accounts {
			state := "READY"
			if a.CooldownUntil > nowMs {
				state = fmt.Sprintf("COOL_%ds", (a.CooldownUntil-nowMs)/1000)
			}
			fmt.Fprintf(&b, "  - %s (%s/%s, %s) leases=%d/%d spend=$%.2f/$%.2f %s\n",
				a.ID, a.Provider, a.ModelID, a.BillingMode, a.ActiveLeases, a.MaxConcurrent, a.DailySpentUSD, a.DailyBudgetUSD, m.styles.statusBadge(state))
		}
	}

	var skipped, coalesced, done, failed int
	for _, tr := range m.snap.TriggerRuns {
		switch tr.Status {
		case "skipped":
			skipped++
		case "coalesced":
			coalesced++
		case "done":
			done++
		case "failed":
			failed++
		}
	}
	b.WriteString("\n" + m.styles.title.Render("Tier-0 Event Daemon Telemetry") + "\n")
	fmt.Fprintf(&b, "  Trigger runs: total=%d | zero_token_skipped=%d | coalesced=%d | done=%d | failed=%d\n",
		len(m.snap.TriggerRuns), skipped, coalesced, done, failed)

	b.WriteString("\n" + m.styles.title.Render(fmt.Sprintf("Operator Inbox (%d pending / %d total)", m.snap.PendingInboxCount, len(m.snap.InboxItems))) + "\n")
	if len(m.snap.InboxItems) == 0 {
		b.WriteString("  (inbox empty)\n")
	} else {
		for i, it := range m.snap.InboxItems {
			row := fmt.Sprintf("[%s] %s (%s/%s) %s",
				it.ID, m.styles.statusBadge(it.Status), it.ItemKind, it.Urgency, it.Title)
			if i == m.inboxCursor {
				b.WriteString(m.styles.cursorRow.Render("> "+row) + "\n")
			} else {
				b.WriteString("  " + row + "\n")
			}
		}
		if m.showDrawer {
			if it, ok := m.selectedInboxItem(); ok {
				fmt.Fprintf(&b, "\n%s\n  ArtifactRef: %s\n  Summary: %s\n",
					m.styles.title.Render("Inbox Detail Drawer"), emptyDash(it.ArtifactRef), it.Summary)
			}
		}
	}
	return m.styles.panel.Render(b.String())
}

func (m Model) renderHelp() string {
	return m.styles.panel.Render(strings.Join([]string{
		m.styles.title.Render("Keyboard Controls"),
		"  Global:     [1-4] Switch tab  [Tab/Shift+Tab] Cycle  [R] Refresh  [?] Help  [q] Quit",
		"  Tab 1 DAG:  [j/k] Select branch  [x] Inspect head in X-Ray  [f] Fork  [r] Exact replay",
		"  Tab 2 Mem:  [h/l] Memory/Skills pane  [Space] Toggle enabled  [a] Approve  [s] Edges  [e] $EDITOR",
		"  Tab 3 XRay: [h/l] Scrub steps  [r] Exact replay  [f] Fork isolated branch from step",
		"  Tab 4 Inbx: [j/k] Select item  [v] Toggle payload drawer  [a] Approve  [d] Dismiss",
	}, "\n"))
}

func (m Model) renderFooter() string {
	status := "ready"
	if m.statusMsg != "" {
		status = m.statusMsg
	}
	hints := "[1-4] Tabs  [?] Help  [R] Refresh  [q] Quit"
	return m.styles.statusBar.Render("Status: "+status) + "\n" + m.styles.footer.Render(hints)
}

func (m *Model) clampCursors() {
	if m.branchCursor >= len(m.snap.Branches) {
		m.branchCursor = maxInt(0, len(m.snap.Branches)-1)
	}
	steps := m.currentSteps()
	if m.stepCursor >= len(steps) {
		m.stepCursor = maxInt(0, len(steps)-1)
	}
	if m.memoryCursor >= len(m.snap.MemoryNodes) {
		m.memoryCursor = maxInt(0, len(m.snap.MemoryNodes)-1)
	}
	if m.inboxCursor >= len(m.snap.InboxItems) {
		m.inboxCursor = maxInt(0, len(m.snap.InboxItems)-1)
	}
}

func (m Model) selectedBranch() (store.Branch, bool) {
	if m.branchCursor < 0 || m.branchCursor >= len(m.snap.Branches) {
		return store.Branch{}, false
	}
	return m.snap.Branches[m.branchCursor], true
}

func (m Model) currentSteps() []store.Step {
	br, ok := m.selectedBranch()
	if !ok {
		return nil
	}
	return m.snap.BranchSteps[br.ID]
}

func (m Model) selectedMemory() (store.MemoryNode, bool) {
	if m.memoryCursor < 0 || m.memoryCursor >= len(m.snap.MemoryNodes) {
		return store.MemoryNode{}, false
	}
	return m.snap.MemoryNodes[m.memoryCursor], true
}

func (m Model) selectedInboxItem() (store.InboxItem, bool) {
	if m.inboxCursor < 0 || m.inboxCursor >= len(m.snap.InboxItems) {
		return store.InboxItem{}, false
	}
	return m.snap.InboxItems[m.inboxCursor], true
}

func (m *Model) ensureInspected() {
	if m.store == nil {
		return
	}
	if m.activeTab == TabDAG {
		if br, ok := m.selectedBranch(); ok && br.HeadStepID != "" {
			if _, cached := m.frameCache[br.HeadStepID]; !cached {
				steps := m.snap.BranchSteps[br.ID]
				for _, s := range steps {
					if s.ID == br.HeadStepID {
						if insp, err := inspectStep(context.Background(), m.store, s); err == nil {
							m.frameCache[s.ID] = insp
						}
						break
					}
				}
			}
		}
	}
	if m.activeTab == TabXRay {
		steps := m.currentSteps()
		if len(steps) > 0 && m.stepCursor >= 0 && m.stepCursor < len(steps) {
			s := steps[m.stepCursor]
			if _, cached := m.frameCache[s.ID]; !cached {
				if insp, err := inspectStep(context.Background(), m.store, s); err == nil {
					m.frameCache[s.ID] = insp
				}
			}
		}
	}
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "\r", ""), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
