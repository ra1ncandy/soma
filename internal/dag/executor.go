package dag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/vmem"
	"github.com/ra1ncandy/soma/internal/workspace"
)

// BranchStatus values mirroring RFC-0001 Section 3.2.
type BranchStatus string

const (
	StatusQueued             BranchStatus = "queued"
	StatusRunning            BranchStatus = "running"
	StatusSuspendedQuota     BranchStatus = "suspended_quota"
	StatusSuspendedApproval  BranchStatus = "suspended_approval"
	StatusSuspendedReconcile BranchStatus = "suspended_reconcile"
	StatusSuspendedIO        BranchStatus = "suspended_io"
	StatusVerifying          BranchStatus = "verifying"
	StatusStalled            BranchStatus = "stalled"
	StatusCompleted          BranchStatus = "completed"
	StatusFailed             BranchStatus = "failed"
	StatusAbandoned          BranchStatus = "abandoned"
)

var (
	// ErrSimulatedCrash is returned by test AfterDispatch hooks to simulate an abrupt SIGKILL.
	ErrSimulatedCrash = errors.New("dag: simulated process crash after dispatch")
)

// SuspendedQuotaError indicates a branch yielded cleanly due to rate limits / quota cooldown.
type SuspendedQuotaError struct {
	BranchID string
	ResumeAt time.Time
}

func (e *SuspendedQuotaError) Error() string {
	return fmt.Sprintf("branch %s suspended for quota until %s", e.BranchID, e.ResumeAt.Format(time.RFC3339Nano))
}

// SuspendedReconcileError indicates a crash left an irreversible action in an ambiguous state.
type SuspendedReconcileError struct {
	ActionID string
	ToolName string
	Reason   string
}

func (e *SuspendedReconcileError) Error() string {
	return fmt.Sprintf("action %s (%s) requires reconciliation: %s", e.ActionID, e.ToolName, e.Reason)
}

// SuspendedApprovalError indicates an action requires explicit operator approval via policy/taint.
type SuspendedApprovalError struct {
	BranchID string
	ActionID string
	Reason   string
}

func (e *SuspendedApprovalError) Error() string {
	return fmt.Sprintf("branch %s awaiting approval for action %s: %s", e.BranchID, e.ActionID, e.Reason)
}

// ToolCall describes an action ready to execute inside a branch's isolated workspace.
type ToolCall struct {
	ActionID          string
	IdempotencyKey    string
	Name              string
	ArgsJSON          string
	EffectClass       taint.EffectClass
	CapabilityScope   string
	WorkspaceDir      string
	ReferencedPageIDs []string
}

// ToolOutput captures raw output and trust tier produced by a ToolRunner.
type ToolOutput struct {
	NameHint  string
	Content   []byte
	TrustTier string
	StateHash string
	Summary   string
}

// ToolRunner executes syscalls or mocked tools.
type ToolRunner interface {
	Execute(ctx context.Context, call ToolCall) (ToolOutput, error)
}

// ReconcileOutcome represents the external truth of a crashed in-flight action.
type ReconcileOutcome string

const (
	OutcomeUnknown   ReconcileOutcome = "unknown"
	OutcomeSucceeded ReconcileOutcome = "succeeded"
	OutcomeFailed    ReconcileOutcome = "failed"
)

// ActionReconciler optionally probes external systems to resolve ambiguous in-flight dispatches.
type ActionReconciler interface {
	Reconcile(ctx context.Context, entry store.ActionJournalEntry) (ReconcileOutcome, ToolOutput, error)
}

// VerifySummary aggregates external verification contract results for a branch.
type VerifySummary struct {
	AllPassed      bool
	Counterexample string
	OutputPageIDs  []string
}

// ContractVerifier checks attached external verification oracles before allowing StatusCompleted.
type ContractVerifier interface {
	VerifyBranch(ctx context.Context, run store.Run, branch store.Branch, stepID string) (VerifySummary, error)
}

// VerificationFailedError indicates a branch failed one or more required external verification contracts.
type VerificationFailedError struct {
	BranchID       string
	Counterexample string
}

func (e *VerificationFailedError) Error() string {
	return fmt.Sprintf("verification failed on branch %s: %s", e.BranchID, e.Counterexample)
}

// StallBreakerError indicates a branch made zero progress for MaxStalls consecutive turns.
type StallBreakerError struct {
	BranchID      string
	HeadStepID    string
	StallCount    int
	MaxStalls     int
	LastStateHash string
}

func (e *StallBreakerError) Error() string {
	return fmt.Sprintf("stall breaker tripped on branch %s (%d/%d consecutive unchanged state hashes: %s)",
		e.BranchID, e.StallCount, e.MaxStalls, e.LastStateHash)
}

// RunRequest configures a new Run and its root Branch.
type RunRequest struct {
	RunID         string
	Title         string
	WorkspacePath string
	WorkspaceKind string
	GoalSpec      string
	ContractSpec  string
	SystemPrompt  string
	Profile       compiler.ModelProfile
	AccountIDs    []string
}

// BranchResult summarizes the outcome of an ExecuteBranch call.
type BranchResult struct {
	RunID         string
	BranchID      string
	HeadStepID    string
	Status        BranchStatus
	TurnsExecuted int
	Rollovers     int
	TotalTokens   int
	CacheReadToks int
	FinalSummary  string
}

// EmittedPayload is serialized to EmittedBlobHash for deterministic X-Ray, crash recovery, and replay.
type EmittedPayload struct {
	Response             GenerateResponse    `json:"response"`
	ActionID             string              `json:"action_id,omitempty"`
	ResultingLogEntries  []compiler.LogEntry `json:"resulting_log_entries,omitempty"`
	ReconciledResolution string              `json:"reconciled_resolution,omitempty"`
}

// Executor drives the Merkle DAG execution loop with durable 2-phase journaling and @page context VM.
type Executor struct {
	Store             *store.Store
	VM                *vmem.Manager
	Workspace         *workspace.Manager
	Scheduler         *QuotaScheduler
	Provider          ModelProvider
	Tools             ToolRunner
	Reconciler        ActionReconciler
	Verifier          ContractVerifier
	Profile           compiler.ModelProfile
	SystemPrompt      string
	AccountIDs          []string
	AllowFailover       bool
	EnforceStallBreaker bool
	AfterDispatchHook   func(ctx context.Context, entry store.ActionJournalEntry) error
	seq               atomic.Uint64
}

// NewExecutor builds a configured DAG Executor.
func NewExecutor(
	st *store.Store,
	provider ModelProvider,
	tools ToolRunner,
	prof compiler.ModelProfile,
) *Executor {
	if tools == nil {
		tools = NewLocalToolRunner(vmem.NewManager(st))
	}
	if prof.Name == "" {
		prof = compiler.WorkerProfile()
	}
	return &Executor{
		Store:     st,
		VM:        vmem.NewManager(st),
		Workspace: workspace.NewManager(st.SomaDir()),
		Scheduler: NewQuotaScheduler(st),
		Provider:  provider,
		Tools:     tools,
		Profile:   prof,
	}
}

// ComputeStepID deterministically hashes a Merkle step's lineage and payload references.
func ComputeStepID(
	parentID, branchID, epochSnapshotID, modelID, frameDeltaHash, compiledFrameHash, emittedHash, obsPageID string,
) string {
	raw := strings.Join([]string{
		parentID, branchID, epochSnapshotID, modelID,
		frameDeltaHash, compiledFrameHash, emittedHash, obsPageID,
	}, "|")
	sum := sha256.Sum256([]byte(raw))
	return "step_" + hex.EncodeToString(sum[:12])
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// StartRun persists a new Run, creates its main Branch, and provisions an isolated workspace.
func (e *Executor) StartRun(ctx context.Context, req RunRequest) (string, string, error) {
	if req.Profile.Name != "" {
		e.Profile = req.Profile
	}
	if req.SystemPrompt != "" {
		e.SystemPrompt = req.SystemPrompt
	}
	if len(req.AccountIDs) > 0 {
		e.AccountIDs = req.AccountIDs
	}

	now := time.Now().UnixNano()
	runID := req.RunID
	if runID == "" {
		runID = fmt.Sprintf("run_%x_%04d", now, e.seq.Add(1))
	}
	title := req.Title
	if title == "" {
		title = req.GoalSpec
		if len(title) > 64 {
			title = title[:64]
		}
	}
	wsKind := req.WorkspaceKind
	if wsKind == "" {
		wsKind = detectWorkspaceKind(req.WorkspacePath)
	}

	run := store.Run{
		ID:            runID,
		Title:         title,
		WorkspacePath: req.WorkspacePath,
		WorkspaceKind: wsKind,
		GoalSpec:      req.GoalSpec,
		Status:        string(StatusQueued),
	}
	if err := e.Store.CreateRun(ctx, run); err != nil {
		return "", "", fmt.Errorf("create run: %w", err)
	}

	branchID := fmt.Sprintf("br_%x_%04d", time.Now().UnixNano(), e.seq.Add(1))
	branch := store.Branch{
		ID:     branchID,
		RunID:  runID,
		Label:  "main",
		Epoch:  1,
		Status: string(StatusQueued),
	}
	if err := e.Store.CreateBranch(ctx, branch); err != nil {
		return "", "", fmt.Errorf("create main branch: %w", err)
	}

	if _, _, err := e.Workspace.PrepareBranchWorkspace(ctx, e.Store, run, branch); err != nil {
		return "", "", fmt.Errorf("prepare branch workspace: %w", err)
	}
	return runID, branchID, nil
}

// ForkBranch creates a sibling branch at fromStepID with its own isolated workspace.
func (e *Executor) ForkBranch(ctx context.Context, runID, fromStepID, label string) (string, error) {
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	fromStep, err := e.Store.GetStep(ctx, fromStepID)
	if err != nil {
		return "", fmt.Errorf("lookup fork step %s: %w", fromStepID, err)
	}
	if fromStep.RunID != runID {
		return "", fmt.Errorf("step %s belongs to run %s, not %s", fromStepID, fromStep.RunID, runID)
	}

	branchID := fmt.Sprintf("br_%x_%04d", time.Now().UnixNano(), e.seq.Add(1))
	if label == "" {
		label = "fork-" + fromStepID[:10]
	}
	branch := store.Branch{
		ID:               branchID,
		RunID:            runID,
		ParentBranchID:   fromStep.BranchID,
		ForkedFromStepID: fromStepID,
		Label:            label,
		HeadStepID:       fromStepID,
		Epoch:            fromStep.Epoch,
		Status:           string(StatusQueued),
		ActiveAccountID:  fromStep.AccountID,
	}
	if err := e.Store.CreateBranch(ctx, branch); err != nil {
		return "", err
	}
	if _, _, err := e.Workspace.PrepareBranchWorkspace(ctx, e.Store, run, branch); err != nil {
		return "", fmt.Errorf("prepare forked workspace: %w", err)
	}
	return branchID, nil
}

// AbandonBranch transitions a branch to StatusAbandoned and writes an immutable Graveyard negative_path.
func (e *Executor) AbandonBranch(ctx context.Context, branchID, reason, evidencePageID string) error {
	branch, err := e.Store.GetBranch(ctx, branchID)
	if err != nil {
		return err
	}
	if err := e.Store.UpdateBranchStatus(ctx, branchID, string(StatusAbandoned), branch.ActiveAccountID, branch.StallCount); err != nil {
		return err
	}
	sig := sha256Hex([]byte(branch.Label + "|" + reason))
	node := store.MemoryNode{
		ID:               fmt.Sprintf("G-%x-%04d", time.Now().UnixNano(), e.seq.Add(1)),
		Scope:            "repo:" + branch.RunID,
		Kind:             "negative_path",
		Content:          fmt.Sprintf("approach=%q reason=%q", branch.Label, reason),
		SignatureHash:    sig,
		Status:           "active",
		TrustTier:        "trusted_repo",
		ProvenanceStepID: branch.HeadStepID,
		EvidencePageID:   evidencePageID,
		Enabled:          true,
	}
	return e.Store.InsertMemoryNode(ctx, node)
}

// ReconcileAction manually resolves an 'ambiguous' action_journal entry and unblocks the branch.
func (e *Executor) ReconcileAction(ctx context.Context, actionID string, outcome ReconcileOutcome, summary string) error {
	entry, err := e.Store.GetAction(ctx, actionID)
	if err != nil {
		return err
	}
	if entry.Status != "ambiguous" && entry.Status != "dispatched" {
		return fmt.Errorf("action %s is in status %q, cannot reconcile", actionID, entry.Status)
	}
	branch, err := e.Store.GetBranch(ctx, entry.BranchID)
	if err != nil {
		return err
	}
	run, err := e.Store.GetRun(ctx, entry.RunID)
	if err != nil {
		return err
	}

	snap, snapID, l2, _, nextTurn, err := e.loadOrInitEpochState(ctx, run, branch)
	if err != nil {
		return err
	}
	tail := compiler.LiveTailState{
		WorkspaceKind:       run.WorkspaceKind,
		StepBudgetRemaining: 100,
		AccountQuotaSummary: "reconciled",
		StallCount:          branch.StallCount,
		MaxStalls:           e.Profile.MaxStalls,
	}
	frame := compiler.Compile(snap, l2, tail, e.Profile)

	var pendingAppends []compiler.LogEntry
	if branch.HeadStepID != "" {
		headStep, err := e.Store.GetStep(ctx, branch.HeadStepID)
		if err == nil && headStep.EpochSnapshotID == snapID {
			if emBytes, err := e.Store.Blobs().Get(headStep.EmittedBlobHash); err == nil {
				var ep EmittedPayload
				if json.Unmarshal(emBytes, &ep) == nil {
					pendingAppends = ep.ResultingLogEntries
				}
			}
		}
	}

	deltaBytes, _ := json.Marshal(compiler.FrameDelta{
		TurnIndex:  nextTurn,
		AppendedL2: pendingAppends,
		LiveTail:   tail,
	})
	deltaHash, _, err := e.Store.Blobs().Put(deltaBytes)
	if err != nil {
		return err
	}

	msg := fmt.Sprintf("Reconciled ambiguous action %s (%s): outcome=%s summary=%s",
		entry.ActionID, entry.ToolName, outcome, summary)
	emPayload := EmittedPayload{
		Response: GenerateResponse{
			Reasoning: msg,
			ToolCall: &ToolCallSpec{
				Name:        entry.ToolName,
				ArgsJSON:    entry.ArgsJSON,
				EffectClass: taint.EffectClass(entry.EffectClass),
			},
			StateHash: "reconciled_" + entry.ActionID,
		},
		ActionID:             entry.ActionID,
		ReconciledResolution: string(outcome),
		ResultingLogEntries: []compiler.LogEntry{{
			Kind:        "turn",
			TurnIndex:   nextTurn,
			Reasoning:   msg,
			Action:      fmt.Sprintf("%s(%s)", entry.ToolName, entry.ArgsJSON),
			Observation: msg,
		}},
	}
	emBytes, _ := json.Marshal(emPayload)
	emHash, _, err := e.Store.Blobs().Put(emBytes)
	if err != nil {
		return err
	}

	modelID := "reconciler"
	accID := branch.ActiveAccountID
	if accID == "" {
		accID = "operator"
	}
	sid := ComputeStepID(branch.HeadStepID, branch.ID, snapID, modelID, deltaHash, frame.CompiledFrameHash, emHash, "")

	newStatus := string(StatusQueued)
	newJournalStatus := "reconciled"
	if outcome == OutcomeFailed {
		newJournalStatus = "aborted"
	}

	if err := e.Store.MarkActionStatus(ctx, entry.ActionID, newJournalStatus, "", msg, ""); err != nil {
		return err
	}
	if err := e.Store.AppendStep(ctx, store.Step{
		ID:                  sid,
		RunID:               branch.RunID,
		BranchID:            branch.ID,
		ParentStepID:        branch.HeadStepID,
		Epoch:               snap.Epoch,
		TurnIndex:           nextTurn,
		AccountID:           accID,
		ModelID:             modelID,
		StateHash:           "reconciled_" + entry.ActionID,
		EpochSnapshotID:     snapID,
		FrameDeltaBlobHash:  deltaHash,
		CompiledFrameHash:   frame.CompiledFrameHash,
		EmittedBlobHash:     emHash,
		InputProvenanceJSON: entry.InputProvenanceJSON,
	}, store.BranchUpdate{
		Epoch:           snap.Epoch,
		Status:          newStatus,
		ActiveAccountID: branch.ActiveAccountID,
		StallCount:      0,
	}); err != nil {
		return err
	}

	// Mark any pending reconcile inbox items resolved.
	if items, err := e.Store.ListInboxItems(ctx, "pending"); err == nil {
		for _, it := range items {
			if it.BranchID == branch.ID && it.ItemKind == "reconcile" {
				_ = e.Store.UpdateInboxStatus(ctx, it.ID, "approved")
			}
		}
	}
	return nil
}

// ExecuteBranch runs or resumes branchID up to maxTurns, handling 429s, crashes, rollovers, and provenance.
func (e *Executor) ExecuteBranch(ctx context.Context, branchID string, maxTurns int) (*BranchResult, error) {
	if maxTurns <= 0 {
		maxTurns = 50
	}
	branch, err := e.Store.GetBranch(ctx, branchID)
	if err != nil {
		return nil, err
	}
	run, err := e.Store.GetRun(ctx, branch.RunID)
	if err != nil {
		return nil, err
	}
	if branch.WorkspaceDir == "" {
		wsDir, strat, err := e.Workspace.PrepareBranchWorkspace(ctx, e.Store, run, branch)
		if err != nil {
			return nil, err
		}
		branch.WorkspaceDir = wsDir
		branch.WorkspaceStrategy = strat
	}
	_ = e.Store.UpdateRunStatus(ctx, run.ID, string(StatusRunning))

	snap, snapID, l2, pendingDeltaAppends, nextTurn, err := e.loadOrInitEpochState(ctx, run, branch)
	if err != nil {
		return nil, err
	}
	forkRolled := snap.Epoch > branch.Epoch
	branch.Epoch = snap.Epoch

	res := &BranchResult{
		RunID:      run.ID,
		BranchID:   branch.ID,
		HeadStepID: branch.HeadStepID,
		Status:     BranchStatus(branch.Status),
	}
	if forkRolled {
		res.Rollovers++
	}

	// Phase 0: Crash recovery of unfinished action_journal entries before taking any new LLM turns.
	recoveredAppends, err := e.recoverPendingActions(ctx, run, &branch, snap, snapID, &nextTurn, pendingDeltaAppends)
	if err != nil {
		res.Status = BranchStatus(branch.Status)
		return res, err
	}
	if len(recoveredAppends) > 0 {
		l2 = append(l2, recoveredAppends...)
		pendingDeltaAppends = recoveredAppends
	}

	lastStateHash := ""
	lastOracleError := ""
	if branch.HeadStepID != "" {
		if hs, err := e.Store.GetStep(ctx, branch.HeadStepID); err == nil {
			lastStateHash = hs.StateHash
		}
	}

	var preacquiredAcc *store.Account
	failoverAttempts := 0

	for turn := 0; turn < maxTurns; turn++ {
		if ctx.Err() != nil {
			if preacquiredAcc != nil {
				_ = e.Scheduler.Release(ctx, preacquiredAcc.ID, 0)
			}
			return res, ctx.Err()
		}

		// Acquire sticky-epoch account lease (returning to primary preference on resume if cooldown expired).
		var acc store.Account
		if preacquiredAcc != nil {
			acc = *preacquiredAcc
			preacquiredAcc = nil
		} else {
			preferred := branch.ActiveAccountID
			if turn == 0 && e.AllowFailover && len(e.AccountIDs) > 0 && preferred != "" && preferred != e.AccountIDs[0] {
				if primaryAcc, pErr := e.Store.GetAccount(ctx, e.AccountIDs[0]); pErr == nil && primaryAcc.CooldownUntil <= time.Now().UnixMilli() {
					preferred = primaryAcc.ID
				}
			}
			var acqErr error
			acc, acqErr = e.Scheduler.Acquire(ctx, preferred, e.AccountIDs)
			if acqErr != nil {
				var coolErr *ErrAllAccountsCoolingDown
				if errors.As(acqErr, &coolErr) {
					_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusSuspendedQuota), branch.ActiveAccountID, branch.StallCount)
					res.Status = StatusSuspendedQuota
					return res, &SuspendedQuotaError{BranchID: branch.ID, ResumeAt: coolErr.EarliestResumeAt}
				}
				return res, acqErr
			}
			if turn == 0 && e.AllowFailover && branch.ActiveAccountID != "" && acc.ID != branch.ActiveAccountID && len(l2) > 0 {
				if nextSnap, nextSnapID, handoffL2, rollErr := e.rolloverWithHandoff(ctx, run, branch, snap, l2, nextTurn, branch.ActiveAccountID, acc.ID); rollErr == nil {
					snap = nextSnap
					snapID = nextSnapID
					l2 = handoffL2
					pendingDeltaAppends = handoffL2
					branch.Epoch = nextSnap.Epoch
					branch.ActiveAccountID = acc.ID
					res.Rollovers++
				}
			}
		}

		tail := compiler.LiveTailState{
			WorkspaceKind:       run.WorkspaceKind,
			ResearchStatus:      fmt.Sprintf("workspace=%s (%s)", filepath.Base(branch.WorkspaceDir), branch.WorkspaceStrategy),
			LastOracleError:     lastOracleError,
			StepBudgetRemaining: maxTurns - turn,
			AccountQuotaSummary: fmt.Sprintf("%s:%s", acc.ID, acc.ModelID),
			StallCount:          branch.StallCount,
			MaxStalls:           e.Profile.MaxStalls,
		}

		// Compile current frame; check Hybrid Epoch Rollover if ceiling crossed.
		frame := compiler.Compile(snap, l2, tail, e.Profile)
		if compiler.NeedsRollover(frame, e.Profile) && len(l2) > 0 {
			activeMem, graveyard, memErr := e.loadMemoryAndGraveyard(ctx, run.ID)
			if memErr == nil {
				baseIn := e.buildEpochInput(run, activeMem, graveyard)
				nextSnap, skeleton := compiler.Rollover(snap, l2, activeMem, graveyard, baseIn)
				nextSnapID, saveErr := e.persistEpochSnapshot(ctx, run.ID, branch.ID, nextSnap)
				if saveErr == nil {
					snap = nextSnap
					snapID = nextSnapID
					l2 = skeleton
					pendingDeltaAppends = skeleton
					res.Rollovers++
					frame = compiler.Compile(snap, l2, tail, e.Profile)
				}
			}
		}

		// Persist FrameDelta BEFORE invoking provider.
		deltaBytes, _ := json.Marshal(compiler.FrameDelta{
			TurnIndex:  nextTurn,
			AppendedL2: pendingDeltaAppends,
			LiveTail:   tail,
		})
		deltaHash, _, err := e.Store.Blobs().Put(deltaBytes)
		if err != nil {
			_ = e.Scheduler.Release(ctx, acc.ID, 0)
			return res, err
		}

		genResp, genErr := e.Provider.Generate(ctx, GenerateRequest{
			RunID:     run.ID,
			BranchID:  branch.ID,
			AccountID: acc.ID,
			ModelID:   acc.ModelID,
			TurnIndex: nextTurn,
			Frame:     frame,
		})
		if genErr != nil {
			var rlErr *RateLimitError
			if errors.As(genErr, &rlErr) {
				_ = e.Scheduler.RecordRateLimit(ctx, acc.ID, rlErr.RetryAfter, rlErr.Error())
				_ = e.Scheduler.Release(ctx, acc.ID, 0)
				if e.AllowFailover && failoverAttempts < len(e.AccountIDs)+1 {
					fbAcc, fbErr := e.Scheduler.Acquire(ctx, "", e.AccountIDs)
					if fbErr == nil {
						failoverAttempts++
						nextSnap, nextSnapID, handoffL2, rollErr := e.rolloverWithHandoff(ctx, run, branch, snap, l2, nextTurn, acc.ID, fbAcc.ID)
						if rollErr == nil {
							snap = nextSnap
							snapID = nextSnapID
							l2 = handoffL2
							pendingDeltaAppends = handoffL2
							branch.Epoch = nextSnap.Epoch
							branch.ActiveAccountID = fbAcc.ID
							res.Rollovers++
							preacquiredAcc = &fbAcc
							turn--
							continue
						}
						_ = e.Scheduler.Release(ctx, fbAcc.ID, 0)
					}
				}
				_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusSuspendedQuota), acc.ID, branch.StallCount)
				res.Status = StatusSuspendedQuota
				return res, &SuspendedQuotaError{
					BranchID: branch.ID,
					ResumeAt: time.Now().Add(rlErr.RetryAfter),
				}
			}
			_ = e.Scheduler.Release(ctx, acc.ID, 0)
			return res, genErr
		}
		_ = e.Scheduler.Release(ctx, acc.ID, genResp.CostUSD)

		res.TotalTokens += genResp.InputTokens + genResp.CacheReadTokens + genResp.OutputTokens
		res.CacheReadToks += genResp.CacheReadTokens

		if genResp.StateHash != "" && genResp.StateHash == lastStateHash {
			branch.StallCount++
		} else {
			branch.StallCount = 0
		}
		lastStateHash = genResp.StateHash

		// Completion turn (no tool call).
		if genResp.Done || genResp.ToolCall == nil {
			doneEntry := compiler.LogEntry{
				Kind:        "turn",
				TurnIndex:   nextTurn,
				Reasoning:   genResp.Reasoning,
				Action:      "done",
				Observation: genResp.FinalSummary,
			}
			nextPending := appendDynamicEntries(nextTurn, genResp, doneEntry)
			emPayload := EmittedPayload{
				Response:            *genResp,
				ResultingLogEntries: nextPending,
			}
			emBytes, _ := json.Marshal(emPayload)
			emHash, _, err := e.Store.Blobs().Put(emBytes)
			if err != nil {
				return res, err
			}
			sid := ComputeStepID(branch.HeadStepID, branch.ID, snapID, acc.ModelID, deltaHash, frame.CompiledFrameHash, emHash, "")

			initialStatus := string(StatusCompleted)
			if e.Verifier != nil {
				initialStatus = string(StatusVerifying)
			}
			if err := e.Store.AppendStep(ctx, store.Step{
				ID:                 sid,
				RunID:              run.ID,
				BranchID:           branch.ID,
				ParentStepID:       branch.HeadStepID,
				Epoch:              snap.Epoch,
				TurnIndex:          nextTurn,
				AccountID:          acc.ID,
				ModelID:            acc.ModelID,
				StateHash:          genResp.StateHash,
				EpochSnapshotID:    snapID,
				FrameDeltaBlobHash: deltaHash,
				CompiledFrameHash:  frame.CompiledFrameHash,
				EmittedBlobHash:    emHash,
				InputTokens:        genResp.InputTokens,
				CacheReadTokens:    genResp.CacheReadTokens,
				CacheWriteTokens:   genResp.CacheWriteTokens,
				OutputTokens:       genResp.OutputTokens,
				CostUSD:            genResp.CostUSD,
				LatencyMS:          genResp.LatencyMS,
			}, store.BranchUpdate{
				Epoch:           snap.Epoch,
				Status:          initialStatus,
				ActiveAccountID: acc.ID,
				StallCount:      branch.StallCount,
			}); err != nil {
				return res, err
			}

			branch.HeadStepID = sid
			branch.ActiveAccountID = acc.ID
			res.HeadStepID = sid
			res.TurnsExecuted++

			if e.Verifier != nil {
				summary, vErr := e.Verifier.VerifyBranch(ctx, run, branch, sid)
				if vErr != nil {
					_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusFailed), acc.ID, branch.StallCount)
					res.Status = StatusFailed
					return res, vErr
				}
				if !summary.AllPassed {
					lastOracleError = summary.Counterexample
					oracleNotice := compiler.LogEntry{
						Kind:        "turn",
						TurnIndex:   nextTurn,
						Reasoning:   "External Verification Contract failed",
						Action:      "oracle_verify",
						Observation: "ORACLE FAILED:\n" + summary.Counterexample,
					}
					nextPending = append(nextPending, oracleNotice)
					l2 = append(l2, nextPending...)
					pendingDeltaAppends = nextPending
					nextTurn++

					if turn == maxTurns-1 {
						_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusFailed), acc.ID, branch.StallCount)
						_ = e.Store.UpdateRunStatus(ctx, run.ID, string(StatusFailed))
						res.Status = StatusFailed
						return res, &VerificationFailedError{BranchID: branch.ID, Counterexample: summary.Counterexample}
					}
					_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusRunning), acc.ID, branch.StallCount)
					res.Status = StatusRunning
					continue
				}
				_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusCompleted), acc.ID, branch.StallCount)
			}

			_ = e.Store.UpdateRunStatus(ctx, run.ID, string(StatusCompleted))
			res.Status = StatusCompleted
			res.FinalSummary = genResp.FinalSummary
			return res, nil
		}

		// Tool Call Turn: evaluate Data/Action-level taint policy BEFORE preparing dispatch.
		tc := genResp.ToolCall
		if tc.EffectClass == "" {
			tc.EffectClass = taint.EffectReadOnly
		}
		provJSONBytes, _ := json.Marshal(tc.ReferencedPageIDs)
		provJSON := string(provJSONBytes)

		decision, reason, err := taint.EvaluateAction(ctx, e.Store, taint.ActionPolicyInput{
			ToolName:          tc.Name,
			CapabilityScope:   tc.CapabilityScope,
			EffectClass:       tc.EffectClass,
			ReferencedPageIDs: tc.ReferencedPageIDs,
			ExplicitDeny:      tc.ExplicitDeny,
		})
		if err != nil {
			return res, err
		}

		actionID := fmt.Sprintf("act_%x_%04d", time.Now().UnixNano(), e.seq.Add(1))
		argsHash := sha256Hex([]byte(tc.ArgsJSON))
		idemKey := sha256Hex([]byte(fmt.Sprintf("%s|%d|%s|%s", branch.ID, nextTurn, tc.Name, argsHash)))

		if decision == taint.DecisionAsk {
			_ = e.Store.PrepareAction(ctx, store.ActionJournalEntry{
				ActionID:            actionID,
				RunID:               run.ID,
				BranchID:            branch.ID,
				ParentStepID:        branch.HeadStepID,
				Epoch:               snap.Epoch,
				TurnIndex:           nextTurn,
				ToolName:            tc.Name,
				ArgsJSON:            tc.ArgsJSON,
				ArgsHash:            argsHash,
				EffectClass:         string(tc.EffectClass),
				IdempotencyKey:      idemKey,
				InputProvenanceJSON: provJSON,
				Status:              "prepared",
			})
			_ = e.Store.InsertInboxItem(ctx, store.InboxItem{
				ID:          "inbox_" + actionID,
				RunID:       run.ID,
				BranchID:    branch.ID,
				ItemKind:    "approval",
				Title:       fmt.Sprintf("Approval required for %s (%s)", tc.Name, tc.EffectClass),
				Summary:     reason,
				ArtifactRef: actionID,
				Urgency:     "urgent",
			})
			_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusSuspendedApproval), acc.ID, branch.StallCount)
			res.Status = StatusSuspendedApproval
			return res, &SuspendedApprovalError{BranchID: branch.ID, ActionID: actionID, Reason: reason}
		}

		var obsText, obsPageID string
		if decision == taint.DecisionDeny {
			obsText = "POLICY DENY: " + reason
		} else {
			entry := store.ActionJournalEntry{
				ActionID:            actionID,
				RunID:               run.ID,
				BranchID:            branch.ID,
				ParentStepID:        branch.HeadStepID,
				Epoch:               snap.Epoch,
				TurnIndex:           nextTurn,
				ToolName:            tc.Name,
				ArgsJSON:            tc.ArgsJSON,
				ArgsHash:            argsHash,
				EffectClass:         string(tc.EffectClass),
				IdempotencyKey:      idemKey,
				InputProvenanceJSON: provJSON,
				Status:              "prepared",
			}
			if err := e.Store.PrepareAction(ctx, entry); err != nil {
				return res, err
			}
			if err := e.Store.MarkActionDispatched(ctx, actionID); err != nil {
				return res, err
			}

			out, execErr := e.Tools.Execute(ctx, ToolCall{
				ActionID:          actionID,
				IdempotencyKey:    idemKey,
				Name:              tc.Name,
				ArgsJSON:          tc.ArgsJSON,
				EffectClass:       tc.EffectClass,
				CapabilityScope:   tc.CapabilityScope,
				WorkspaceDir:      branch.WorkspaceDir,
				ReferencedPageIDs: tc.ReferencedPageIDs,
			})
			if e.AfterDispatchHook != nil {
				if hookErr := e.AfterDispatchHook(ctx, entry); hookErr != nil {
					return res, hookErr
				}
			}
			if execErr != nil {
				obsText = "TOOL ERROR: " + execErr.Error()
			} else {
				obsRes, obsErr := e.VM.Observe(ctx, vmem.ObserveRequest{
					RunID:                run.ID,
					NameHint:             out.NameHint,
					Content:              out.Content,
					TrustTier:            out.TrustTier,
					InlineThresholdBytes: e.Profile.InlineThresholdBytes,
				})
				if obsErr != nil {
					return res, obsErr
				}
				if obsRes.Paged {
					obsText = obsRes.Manifest
					obsPageID = obsRes.PageID
				} else {
					obsText = obsRes.InlineText
				}
			}
		}

		turnEntry := compiler.LogEntry{
			Kind:        "turn",
			TurnIndex:   nextTurn,
			Reasoning:   genResp.Reasoning,
			Action:      fmt.Sprintf("%s(%s)", tc.Name, tc.ArgsJSON),
			Observation: obsText,
			PageID:      obsPageID,
		}
		nextPending := appendDynamicEntries(nextTurn, genResp, turnEntry)
		emPayload := EmittedPayload{
			Response:            *genResp,
			ActionID:            actionID,
			ResultingLogEntries: nextPending,
		}
		emBytes, _ := json.Marshal(emPayload)
		emHash, _, err := e.Store.Blobs().Put(emBytes)
		if err != nil {
			return res, err
		}

		sid := ComputeStepID(branch.HeadStepID, branch.ID, snapID, acc.ModelID, deltaHash, frame.CompiledFrameHash, emHash, obsPageID)
		stepRow := store.Step{
			ID:                  sid,
			RunID:               run.ID,
			BranchID:            branch.ID,
			ParentStepID:        branch.HeadStepID,
			Epoch:               snap.Epoch,
			TurnIndex:           nextTurn,
			AccountID:           acc.ID,
			ModelID:             acc.ModelID,
			StateHash:           genResp.StateHash,
			EpochSnapshotID:     snapID,
			FrameDeltaBlobHash:  deltaHash,
			CompiledFrameHash:   frame.CompiledFrameHash,
			EmittedBlobHash:     emHash,
			ObservationPageID:   obsPageID,
			InputProvenanceJSON: provJSON,
			InputTokens:         genResp.InputTokens,
			CacheReadTokens:     genResp.CacheReadTokens,
			CacheWriteTokens:    genResp.CacheWriteTokens,
			OutputTokens:        genResp.OutputTokens,
			CostUSD:             genResp.CostUSD,
			LatencyMS:           genResp.LatencyMS,
		}

		if decision == taint.DecisionDeny {
			err = e.Store.AppendStep(ctx, stepRow, store.BranchUpdate{
				Epoch:           snap.Epoch,
				Status:          string(StatusRunning),
				ActiveAccountID: acc.ID,
				StallCount:      branch.StallCount,
			})
		} else {
			err = e.Store.CommitActionAndAppendStep(ctx, actionID, obsPageID, obsText, stepRow, store.BranchUpdate{
				Epoch:           snap.Epoch,
				Status:          string(StatusRunning),
				ActiveAccountID: acc.ID,
				StallCount:      branch.StallCount,
			})
		}
		if err != nil {
			return res, err
		}

		branch.HeadStepID = sid
		branch.ActiveAccountID = acc.ID
		l2 = append(l2, nextPending...)
		pendingDeltaAppends = nextPending
		nextTurn++
		res.HeadStepID = sid
		res.Status = StatusRunning
		res.TurnsExecuted++

		if e.EnforceStallBreaker && e.Profile.MaxStalls > 0 && branch.StallCount >= e.Profile.MaxStalls {
			_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusStalled), acc.ID, branch.StallCount)
			res.Status = StatusStalled
			return res, &StallBreakerError{
				BranchID:      branch.ID,
				HeadStepID:    sid,
				StallCount:    branch.StallCount,
				MaxStalls:     e.Profile.MaxStalls,
				LastStateHash: lastStateHash,
			}
		}
	}

	if lastOracleError != "" {
		_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusFailed), branch.ActiveAccountID, branch.StallCount)
		_ = e.Store.UpdateRunStatus(ctx, run.ID, string(StatusFailed))
		res.Status = StatusFailed
		return res, &VerificationFailedError{BranchID: branch.ID, Counterexample: lastOracleError}
	}
	return res, nil
}

func appendDynamicEntries(turn int, resp *GenerateResponse, turnEntry compiler.LogEntry) []compiler.LogEntry {
	var out []compiler.LogEntry
	if resp.DynamicSkill != nil {
		out = append(out, compiler.LogEntry{
			Kind:        "dynamic_skill",
			TurnIndex:   turn,
			Name:        resp.DynamicSkill.Name,
			Observation: resp.DynamicSkill.Body,
		})
	}
	if resp.DynamicMCP != nil {
		out = append(out, compiler.LogEntry{
			Kind:        "dynamic_mcp",
			TurnIndex:   turn,
			Name:        resp.DynamicMCP.Name,
			Observation: resp.DynamicMCP.JSONSchema,
		})
	}
	return append(out, turnEntry)
}

func (e *Executor) recoverPendingActions(
	ctx context.Context,
	run store.Run,
	branch *store.Branch,
	snap compiler.EpochSnapshot,
	snapID string,
	nextTurn *int,
	pendingAppends []compiler.LogEntry,
) ([]compiler.LogEntry, error) {
	pending, err := e.Store.ListPendingActions(ctx, branch.ID)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}

	var extraL2 []compiler.LogEntry
	for _, entry := range pending {
		switch entry.Status {
		case "ambiguous":
			return nil, &SuspendedReconcileError{
				ActionID: entry.ActionID,
				ToolName: entry.ToolName,
				Reason:   "action previously marked ambiguous and awaiting soma reconcile",
			}
		case "prepared":
			_ = e.Store.MarkActionStatus(ctx, entry.ActionID, "aborted", "", "recovered prepared action before dispatch", "")
		case "dispatched":
			switch taint.EffectClass(entry.EffectClass) {
			case taint.EffectReadOnly, taint.EffectWorkspaceWrite:
				_ = e.Store.MarkActionStatus(ctx, entry.ActionID, "aborted", "", "recovered safe local action after crash", "")
			case "idempotent_write":
				var out ToolOutput
				var recDone bool
				if e.Reconciler != nil {
					outcome, recOut, recErr := e.Reconciler.Reconcile(ctx, entry)
					if recErr == nil && outcome == OutcomeSucceeded {
						out = recOut
						recDone = true
					}
				}
				if !recDone {
					var execErr error
					out, execErr = e.Tools.Execute(ctx, ToolCall{
						ActionID:       entry.ActionID,
						IdempotencyKey: entry.IdempotencyKey,
						Name:           entry.ToolName,
						ArgsJSON:       entry.ArgsJSON,
						EffectClass:    taint.EffectClass(entry.EffectClass),
						WorkspaceDir:   branch.WorkspaceDir,
					})
					if execErr != nil {
						return nil, execErr
					}
				}
				entryL2, err := e.commitRecoveredAction(ctx, run, branch, snap, snapID, nextTurn, pendingAppends, entry, out)
				if err != nil {
					return nil, err
				}
				extraL2 = append(extraL2, entryL2...)
				pendingAppends = entryL2
			case taint.EffectIrreversible:
				if e.Reconciler != nil {
					outcome, recOut, recErr := e.Reconciler.Reconcile(ctx, entry)
					if recErr == nil && outcome == OutcomeSucceeded {
						entryL2, err := e.commitRecoveredAction(ctx, run, branch, snap, snapID, nextTurn, pendingAppends, entry, recOut)
						if err != nil {
							return nil, err
						}
						extraL2 = append(extraL2, entryL2...)
						pendingAppends = entryL2
						continue
					} else if recErr == nil && outcome == OutcomeFailed {
						_ = e.Store.MarkActionStatus(ctx, entry.ActionID, "aborted", "", "reconciler confirmed failed", "")
						continue
					}
				}
				// NEVER auto-retry uncertain irreversible writes!
				_ = e.Store.MarkActionStatus(ctx, entry.ActionID, "ambiguous", "", "", "process crashed while irreversible_write was dispatched")
				_ = e.Store.InsertInboxItem(ctx, store.InboxItem{
					ID:          "inbox_rec_" + entry.ActionID,
					RunID:       run.ID,
					BranchID:    branch.ID,
					ItemKind:    "reconcile",
					Title:       fmt.Sprintf("Reconcile ambiguous %s (%s)", entry.ToolName, entry.ActionID),
					Summary:     "Crash occurred while irreversible_write was in-flight. Confirm external state via soma reconcile.",
					ArtifactRef: entry.ActionID,
					Urgency:     "urgent",
				})
				_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusSuspendedReconcile), branch.ActiveAccountID, branch.StallCount)
				branch.Status = string(StatusSuspendedReconcile)
				return nil, &SuspendedReconcileError{
					ActionID: entry.ActionID,
					ToolName: entry.ToolName,
					Reason:   "uncertain irreversible_write suspended safely",
				}
			}
		}
	}
	return extraL2, nil
}

func (e *Executor) commitRecoveredAction(
	ctx context.Context,
	run store.Run,
	branch *store.Branch,
	snap compiler.EpochSnapshot,
	snapID string,
	nextTurn *int,
	pendingAppends []compiler.LogEntry,
	entry store.ActionJournalEntry,
	out ToolOutput,
) ([]compiler.LogEntry, error) {
	obsRes, err := e.VM.Observe(ctx, vmem.ObserveRequest{
		RunID:                run.ID,
		NameHint:             out.NameHint,
		Content:              out.Content,
		TrustTier:            out.TrustTier,
		InlineThresholdBytes: e.Profile.InlineThresholdBytes,
	})
	if err != nil {
		return nil, err
	}
	obsText := obsRes.InlineText
	if obsRes.Paged {
		obsText = obsRes.Manifest
	}

	tail := compiler.LiveTailState{
		WorkspaceKind:       run.WorkspaceKind,
		ResearchStatus:      "recovered_idempotent_action",
		StepBudgetRemaining: 100,
		AccountQuotaSummary: branch.ActiveAccountID,
		MaxStalls:           e.Profile.MaxStalls,
	}
	frame := compiler.Compile(snap, nil, tail, e.Profile)
	deltaBytes, _ := json.Marshal(compiler.FrameDelta{
		TurnIndex:  *nextTurn,
		AppendedL2: pendingAppends,
		LiveTail:   tail,
	})
	deltaHash, _, err := e.Store.Blobs().Put(deltaBytes)
	if err != nil {
		return nil, err
	}

	logEntry := compiler.LogEntry{
		Kind:        "turn",
		TurnIndex:   *nextTurn,
		Reasoning:   fmt.Sprintf("Recovered dispatched action %s via idempotency_key", entry.ActionID),
		Action:      fmt.Sprintf("%s(%s)", entry.ToolName, entry.ArgsJSON),
		Observation: obsText,
		PageID:      obsRes.PageID,
	}
	emPayload := EmittedPayload{
		Response: GenerateResponse{
			Reasoning: logEntry.Reasoning,
			ToolCall: &ToolCallSpec{
				Name:        entry.ToolName,
				ArgsJSON:    entry.ArgsJSON,
				EffectClass: taint.EffectClass(entry.EffectClass),
			},
			StateHash: "recovered_" + entry.ActionID,
		},
		ActionID:            entry.ActionID,
		ResultingLogEntries: []compiler.LogEntry{logEntry},
	}
	emBytes, _ := json.Marshal(emPayload)
	emHash, _, err := e.Store.Blobs().Put(emBytes)
	if err != nil {
		return nil, err
	}

	accID := branch.ActiveAccountID
	if accID == "" {
		accID = "recovery"
	}
	sid := ComputeStepID(branch.HeadStepID, branch.ID, snapID, "recovery", deltaHash, frame.CompiledFrameHash, emHash, obsRes.PageID)
	if err := e.Store.CommitActionAndAppendStep(ctx, entry.ActionID, obsRes.PageID, obsText, store.Step{
		ID:                  sid,
		RunID:               run.ID,
		BranchID:            branch.ID,
		ParentStepID:        branch.HeadStepID,
		Epoch:               snap.Epoch,
		TurnIndex:           *nextTurn,
		AccountID:           accID,
		ModelID:             "recovery",
		StateHash:           "recovered_" + entry.ActionID,
		EpochSnapshotID:     snapID,
		FrameDeltaBlobHash:  deltaHash,
		CompiledFrameHash:   frame.CompiledFrameHash,
		EmittedBlobHash:     emHash,
		ObservationPageID:   obsRes.PageID,
		InputProvenanceJSON: entry.InputProvenanceJSON,
	}, store.BranchUpdate{
		Epoch:           snap.Epoch,
		Status:          string(StatusRunning),
		ActiveAccountID: branch.ActiveAccountID,
		StallCount:      0,
	}); err != nil {
		return nil, err
	}

	branch.HeadStepID = sid
	*nextTurn++
	return []compiler.LogEntry{logEntry}, nil
}

func (e *Executor) loadOrInitEpochState(ctx context.Context, run store.Run, branch store.Branch) (compiler.EpochSnapshot, string, []compiler.LogEntry, []compiler.LogEntry, int, error) {
	if branch.HeadStepID == "" {
		activeMem, graveyard, err := e.loadMemoryAndGraveyard(ctx, run.ID)
		if err != nil {
			return compiler.EpochSnapshot{}, "", nil, nil, 1, err
		}
		snap := compiler.NewEpochSnapshot(branch.Epoch, e.buildEpochInput(run, activeMem, graveyard))
		snapID, err := e.persistEpochSnapshot(ctx, run.ID, branch.ID, snap)
		if err != nil {
			return compiler.EpochSnapshot{}, "", nil, nil, 1, err
		}
		return snap, snapID, nil, nil, 1, nil
	}

	headStep, err := e.Store.GetStep(ctx, branch.HeadStepID)
	if err != nil {
		return compiler.EpochSnapshot{}, "", nil, nil, 1, err
	}
	ancestry, err := e.Store.GetAncestry(ctx, branch.HeadStepID)
	if err != nil {
		return compiler.EpochSnapshot{}, "", nil, nil, 1, err
	}
	snapRow, err := e.Store.GetEpochSnapshot(ctx, headStep.EpochSnapshotID)
	if err != nil {
		return compiler.EpochSnapshot{}, "", nil, nil, 1, err
	}
	l0Bytes, err := e.Store.Blobs().Get(snapRow.Layer0BlobHash)
	if err != nil {
		return compiler.EpochSnapshot{}, "", nil, nil, 1, err
	}
	l1Bytes, err := e.Store.Blobs().Get(snapRow.Layer1BlobHash)
	if err != nil {
		return compiler.EpochSnapshot{}, "", nil, nil, 1, err
	}
	snap := compiler.EpochSnapshot{
		Epoch:          snapRow.Epoch,
		Layer0Text:     string(l0Bytes),
		Layer0Hash:     snapRow.Layer0BlobHash,
		Layer1Text:     string(l1Bytes),
		Layer1Hash:     snapRow.Layer1BlobHash,
		PrefixHashL0L1: snapRow.PrefixHashL0L1,
	}

	var l2 []compiler.LogEntry
	for _, s := range ancestry {
		if s.EpochSnapshotID != headStep.EpochSnapshotID {
			continue
		}
		dBytes, err := e.Store.Blobs().Get(s.FrameDeltaBlobHash)
		if err != nil {
			return compiler.EpochSnapshot{}, "", nil, nil, 1, err
		}
		var delta compiler.FrameDelta
		if err := json.Unmarshal(dBytes, &delta); err != nil {
			return compiler.EpochSnapshot{}, "", nil, nil, 1, err
		}
		l2 = append(l2, delta.AppendedL2...)
	}

	// Include the last step's emitted entries so the next turn sees the latest observation.
	var lastEmitted []compiler.LogEntry
	emBytes, err := e.Store.Blobs().Get(headStep.EmittedBlobHash)
	if err == nil {
		var ep EmittedPayload
		if json.Unmarshal(emBytes, &ep) == nil {
			lastEmitted = ep.ResultingLogEntries
			l2 = append(l2, lastEmitted...)
		}
	}

	// On a freshly forked branch, roll over cleanly into Epoch N+1 if active memory or Graveyard changed.
	if branch.ForkedFromStepID != "" && branch.HeadStepID == branch.ForkedFromStepID {
		activeMem, graveyard, memErr := e.loadMemoryAndGraveyard(ctx, run.ID)
		if memErr != nil {
			return compiler.EpochSnapshot{}, "", nil, nil, 1, memErr
		}
		baseIn := e.buildEpochInput(run, activeMem, graveyard)
		probe := compiler.NewEpochSnapshot(snap.Epoch, baseIn)
		if probe.PrefixHashL0L1 != snapRow.PrefixHashL0L1 {
			nextSnap, skeleton := compiler.Rollover(snap, l2, activeMem, graveyard, baseIn)
			nextSnapID, saveErr := e.persistEpochSnapshot(ctx, run.ID, branch.ID, nextSnap)
			if saveErr != nil {
				return compiler.EpochSnapshot{}, "", nil, nil, 1, saveErr
			}
			return nextSnap, nextSnapID, skeleton, skeleton, headStep.TurnIndex + 1, nil
		}
	}

	return snap, snapRow.ID, l2, lastEmitted, headStep.TurnIndex + 1, nil
}

func (e *Executor) persistEpochSnapshot(ctx context.Context, runID, branchID string, snap compiler.EpochSnapshot) (string, error) {
	l0Hash, _, err := e.Store.Blobs().Put([]byte(snap.Layer0Text))
	if err != nil {
		return "", err
	}
	l1Hash, _, err := e.Store.Blobs().Put([]byte(snap.Layer1Text))
	if err != nil {
		return "", err
	}
	metaBytes, _ := json.Marshal(compiler.SnapshotMetadata{ProviderFlavor: e.Profile.ProviderFlavor})
	snapID := fmt.Sprintf("snap_%s_e%d_%s", branchID, snap.Epoch, snap.PrefixHashL0L1[:10])
	if err := e.Store.PutEpochSnapshot(ctx, store.EpochSnapshot{
		ID:             snapID,
		RunID:          runID,
		BranchID:       branchID,
		Epoch:          snap.Epoch,
		Layer0BlobHash: l0Hash,
		Layer1BlobHash: l1Hash,
		PrefixHashL0L1: snap.PrefixHashL0L1,
		MetadataJSON:   string(metaBytes),
	}); err != nil {
		return "", err
	}
	return snapID, nil
}

func (e *Executor) rolloverWithHandoff(
	ctx context.Context,
	run store.Run,
	branch store.Branch,
	snap compiler.EpochSnapshot,
	l2 []compiler.LogEntry,
	turnIndex int,
	fromAccountID, toAccountID string,
) (compiler.EpochSnapshot, string, []compiler.LogEntry, error) {
	activeMem, graveyard, err := e.loadMemoryAndGraveyard(ctx, run.ID)
	if err != nil {
		return compiler.EpochSnapshot{}, "", nil, err
	}
	baseIn := e.buildEpochInput(run, activeMem, graveyard)
	nextSnap, skeleton := compiler.Rollover(snap, l2, activeMem, graveyard, baseIn)

	contracts, _ := e.Store.ListRunContracts(ctx, run.ID)
	pageSet := make(map[string]bool)
	var pages []string
	for _, entry := range l2 {
		if entry.PageID != "" && !pageSet[entry.PageID] {
			pageSet[entry.PageID] = true
			pages = append(pages, "@"+entry.PageID)
		}
	}
	pageSummary := "none"
	if len(pages) > 0 {
		pageSummary = strings.Join(pages, ",")
	}
	handoffSummary := fmt.Sprintf(
		"handoff from=%s to=%s | goal=%q | contracts=%d | graveyard=%d | pages=%s",
		fromAccountID, toAccountID, run.GoalSpec, len(contracts), len(graveyard), pageSummary,
	)
	skeleton = append(skeleton, compiler.LogEntry{
		Kind:        "handoff",
		TurnIndex:   turnIndex,
		Name:        toAccountID,
		Observation: handoffSummary,
	})

	nextSnapID, err := e.persistEpochSnapshot(ctx, run.ID, branch.ID, nextSnap)
	if err != nil {
		return compiler.EpochSnapshot{}, "", nil, err
	}
	return nextSnap, nextSnapID, skeleton, nil
}

func (e *Executor) loadMemoryAndGraveyard(ctx context.Context, runID string) ([]compiler.MemoryEntry, []compiler.GraveyardEntry, error) {
	nodes, err := e.Store.ListActiveMemoryByScopes(ctx, []string{"global", "repo:" + runID}, nil)
	if err != nil {
		return nil, nil, err
	}
	var mem []compiler.MemoryEntry
	var grave []compiler.GraveyardEntry
	for _, n := range nodes {
		if n.Kind == "negative_path" {
			grave = append(grave, compiler.GraveyardEntry{
				ID:                n.ID,
				SignatureHash:     n.SignatureHash,
				AttemptedApproach: n.Content,
				FailureReason:     n.Content,
				EvidencePageID:    n.EvidencePageID,
			})
		} else {
			mem = append(mem, compiler.MemoryEntry{
				ID:      n.ID,
				Scope:   n.Scope,
				Kind:    n.Kind,
				Content: n.Content,
			})
		}
	}
	return mem, grave, nil
}

func (e *Executor) buildEpochInput(run store.Run, activeMem []compiler.MemoryEntry, grave []compiler.GraveyardEntry) compiler.EpochInput {
	sys := e.SystemPrompt
	if sys == "" {
		sys = "You are soma, executing inside an isolated branch workspace. Verify facts directly before declaring completion."
	}
	return compiler.EpochInput{
		SystemPrompt:  sys,
		WorkspaceMode: run.WorkspaceKind,
		TaskGoal:      run.GoalSpec,
		Syscalls:      compiler.DefaultCoreSyscalls(),
		ActiveMemory:  activeMem,
		Graveyard:     grave,
	}
}

func detectWorkspaceKind(wsPath string) string {
	if wsPath == "" {
		return "generic"
	}
	if _, err := os.Stat(filepath.Join(wsPath, ".git")); err == nil {
		return "git"
	}
	return "generic"
}

// LocalToolRunner executes safe workspace-scoped operations for v0.1 CLI and tests.
type LocalToolRunner struct {
	vm *vmem.Manager
}

// NewLocalToolRunner constructs a workspace-scoped tool runner.
func NewLocalToolRunner(vm *vmem.Manager) *LocalToolRunner {
	return &LocalToolRunner{vm: vm}
}

func (r *LocalToolRunner) Execute(ctx context.Context, call ToolCall) (ToolOutput, error) {
	var args map[string]any
	if strings.TrimSpace(call.ArgsJSON) != "" {
		if err := json.Unmarshal([]byte(call.ArgsJSON), &args); err != nil {
			return ToolOutput{}, fmt.Errorf("parse args for %s: %w", call.Name, err)
		}
	}

	switch call.Name {
	case "fs_read":
		rel, _ := args["path"].(string)
		target, err := resolveInsideWorkspace(call.WorkspaceDir, rel)
		if err != nil {
			return ToolOutput{}, err
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{NameHint: rel, Content: data, TrustTier: "trusted_repo"}, nil

	case "fs_write":
		rel, _ := args["path"].(string)
		content, _ := args["content"].(string)
		target, err := resolveInsideWorkspace(call.WorkspaceDir, rel)
		if err != nil {
			return ToolOutput{}, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return ToolOutput{}, err
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{
			NameHint:  rel,
			Content:   []byte(fmt.Sprintf("wrote %d bytes to %s", len(content), rel)),
			TrustTier: "trusted_repo",
		}, nil

	case "page_slice":
		pid, _ := args["page_id"].(string)
		startF, _ := args["start_line"].(float64)
		endF, _ := args["end_line"].(float64)
		out, err := r.vm.Slice(ctx, pid, int(startF), int(endF))
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{NameHint: "slice.txt", Content: []byte(out), TrustTier: "trusted_repo"}, nil

	case "page_grep":
		pid, _ := args["page_id"].(string)
		pat, _ := args["pattern"].(string)
		out, err := r.vm.Grep(ctx, pid, pat, 40)
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{NameHint: "grep.txt", Content: []byte(out), TrustTier: "trusted_repo"}, nil

	default:
		return ToolOutput{
			NameHint:  call.Name + ".log",
			Content:   []byte(fmt.Sprintf("executed %s(%s)", call.Name, call.ArgsJSON)),
			TrustTier: "trusted_repo",
		}, nil
	}
}

func resolveInsideWorkspace(wsDir, rel string) (string, error) {
	if wsDir == "" {
		return "", errors.New("workspace dir is empty")
	}
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to workspace: %q", rel)
	}
	absRoot, err := filepath.Abs(wsDir)
	if err != nil {
		return "", err
	}
	target := filepath.Clean(filepath.Join(absRoot, rel))
	if target != absRoot && !strings.HasPrefix(target, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace boundary: %q", rel)
	}
	return target, nil
}
