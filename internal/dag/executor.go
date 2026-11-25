package dag

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/broker"
	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/embed"
	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/skills"
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
	// StatusAwaitingInput: the model called clarify; a follow-up message resumes the branch.
	StatusAwaitingInput BranchStatus = "awaiting_input"
	// StatusBudgetExhausted: the turn budget ran out before completion; resumable.
	StatusBudgetExhausted BranchStatus = "budget_exhausted"
	StatusFailed          BranchStatus = "failed"
	StatusAbandoned       BranchStatus = "abandoned"
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
	RunID             string
	BranchID          string
	BranchLabel       string
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
	Viewport  *compiler.ViewportFrame
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
	RunID             string
	Title             string
	WorkspacePath     string
	WorkspaceKind     string
	WorkspaceStrategy string
	GoalSpec          string
	ContractSpec      string
	SystemPrompt      string
	AutonomyMode      string // "standard" | "autonomous"
	Profile           compiler.ModelProfile
	AccountIDs        []string
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
	Store               *store.Store
	VM                  *vmem.Manager
	Memory              *memory.Service
	Blackboard          *Blackboard
	Workspace           *workspace.Manager
	Scheduler           *QuotaScheduler
	Provider            ModelProvider
	Tools               ToolRunner
	Reconciler          ActionReconciler
	Verifier            ContractVerifier
	Profile             compiler.ModelProfile
	SystemPrompt        string
	MaxLayer1Memories   int
	AccountIDs          []string
	AllowFailover       bool
	EnforceStallBreaker bool
	AutonomyMode        string // "standard" | "autonomous"
	GUIConfig           *GUISubagentConfig
	AfterDispatchHook   func(ctx context.Context, entry store.ActionJournalEntry) error
	OnStep              func(step store.Step)
	// OnDelta receives streamed model text for a branch as it is generated.
	OnDelta func(branchID, text string)
	seq     atomic.Uint64
}

// NewExecutor builds a configured DAG Executor.
func NewExecutor(
	st *store.Store,
	provider ModelProvider,
	tools ToolRunner,
	prof compiler.ModelProfile,
) *Executor {
	vm := vmem.NewManager(st)
	bb := NewBlackboard()
	if tools == nil {
		lt := NewSandboxedToolRunner(vm, sandbox.NewManager())
		lt.Blackboard = bb
		tools = lt
	} else if lt, ok := tools.(*LocalToolRunner); ok && lt.Blackboard == nil {
		lt.Blackboard = bb
	}
	if prof.Name == "" {
		prof = compiler.WorkerProfile()
	}
	ex := &Executor{
		Store:             st,
		VM:                vm,
		Memory:            memory.NewService(st),
		Blackboard:        bb,
		Workspace:         workspace.NewManager(st.SomaDir()),
		Scheduler:         NewQuotaScheduler(st),
		Provider:          provider,
		Tools:             tools,
		Profile:           prof,
		MaxLayer1Memories: 25,
		// A model repeating the same tool calls burns its whole budget; stop it early everywhere,
		// not only in search and swarm runs.
		EnforceStallBreaker: true,
	}
	if lt, ok := ex.Tools.(*LocalToolRunner); ok {
		if lt.ForkBranchFunc == nil {
			lt.ForkBranchFunc = ex.forkBranchFromCurrentHead
		}
		if lt.GUITaskFunc == nil {
			lt.GUITaskFunc = ex.RunGUISubagent
		}
		if lt.Memory == nil {
			lt.Memory = ex.Memory
		}
		if lt.ProjectSomaDir == "" && st != nil {
			lt.ProjectSomaDir = st.SomaDir()
		}
		if lt.DelegateFunc == nil {
			lt.DelegateFunc = ex.RunDelegatedTasks
		}
	}
	return ex
}

func (e *Executor) forkBranchFromCurrentHead(ctx context.Context, runID, fromBranchID, label, reason string) (string, error) {
	if e.Store == nil {
		return "", errors.New("branch_fork: store not initialized")
	}
	br, err := e.Store.GetBranch(ctx, fromBranchID)
	if err != nil {
		return "", err
	}
	if br.HeadStepID != "" {
		return e.ForkBranch(ctx, runID, br.HeadStepID, label)
	}
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	branchID := fmt.Sprintf("br_%x_%04d", time.Now().UnixNano(), e.seq.Add(1))
	if label == "" {
		label = "fork-" + fromBranchID
	}
	newBr := store.Branch{
		ID:              branchID,
		RunID:           runID,
		ParentBranchID:  fromBranchID,
		Label:           label,
		Epoch:           br.Epoch,
		Status:          string(StatusQueued),
		ActiveAccountID: br.ActiveAccountID,
	}
	if err := e.Store.CreateBranch(ctx, newBr); err != nil {
		return "", err
	}
	if _, _, err := e.Workspace.PrepareBranchWorkspace(ctx, e.Store, run, newBr); err != nil {
		return "", fmt.Errorf("prepare forked workspace: %w", err)
	}
	return branchID, nil
}

// SetEmbedder configures a custom local or remote Matryoshka int8 Embedder for both memory and @page retrieval.
func (e *Executor) SetEmbedder(emb embed.Embedder) {
	if emb == nil {
		return
	}
	if e.VM != nil {
		e.VM.SetEmbedder(emb)
	}
	if e.Memory != nil {
		e.Memory.SetEmbedder(emb)
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
	if req.AutonomyMode != "" {
		e.AutonomyMode = req.AutonomyMode
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
		ID:                branchID,
		RunID:             runID,
		Label:             "main",
		Epoch:             1,
		Status:            string(StatusQueued),
		WorkspaceStrategy: workspace.NormalizeStrategy(req.WorkspaceStrategy),
	}
	if err := e.Store.CreateBranch(ctx, branch); err != nil {
		return "", "", fmt.Errorf("create main branch: %w", err)
	}

	if _, _, err := e.Workspace.PrepareBranchWorkspace(ctx, e.Store, run, branch); err != nil {
		return "", "", fmt.Errorf("prepare branch workspace: %w", err)
	}
	_ = e.Store.IndexTranscript(ctx, []store.TranscriptEntry{{RunID: runID, BranchID: branchID, Kind: "goal", Text: title + "\n" + req.GoalSpec}})
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
	// The fork's history ends at fromStepID, so its files must too (not the parent's current
	// files). Without a checkpoint (older runs) the fork keeps the parent's current files.
	if _, err := e.RestoreStepFiles(ctx, branchID, fromStepID); err != nil && !errors.Is(err, store.ErrNotFound) {
		if _, cpErr := e.checkpointAtStep(ctx, fromStepID); cpErr == nil {
			return "", fmt.Errorf("restore forked workspace to step %s: %w", fromStepID, err)
		}
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
	if err := e.Store.InsertMemoryNode(ctx, node); err != nil {
		return err
	}
	if e.Blackboard != nil {
		_, _ = e.Blackboard.Publish(ctx, e.Store, PeerFinding{
			ID:              node.ID,
			RunID:           branch.RunID,
			FromBranch:      branch.ID,
			FromLabel:       branch.Label,
			Kind:            "graveyard_notice",
			Summary:         node.Content,
			PageID:          evidencePageID,
			PersistToMemory: false,
		})
	}
	return nil
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
			ToolCallID:  "call_rec_" + entry.ActionID,
			ToolName:    entry.ToolName,
			ArgsJSON:    entry.ArgsJSON,
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

// AppendFollowUpUserTurn records a user follow-up message on branchID, preserving L0/L1 KV prefix and history.
func (e *Executor) AppendFollowUpUserTurn(ctx context.Context, branchID, message string) error {
	msg := strings.TrimSpace(message)
	if msg == "" {
		return errors.New("follow-up message is required")
	}
	branch, err := e.Store.GetBranch(ctx, branchID)
	if err != nil {
		return err
	}
	run, err := e.Store.GetRun(ctx, branch.RunID)
	if err != nil {
		return err
	}
	snap, snapID, _, pendingDeltaAppends, nextTurn, err := e.loadOrInitEpochState(ctx, run, branch)
	if err != nil {
		return err
	}
	deltaBytes, _ := json.Marshal(compiler.FrameDelta{AppendedL2: pendingDeltaAppends})
	deltaHash, _, err := e.Store.Blobs().Put(deltaBytes)
	if err != nil {
		return err
	}
	emPayload := EmittedPayload{
		Response: GenerateResponse{
			Reasoning: msg,
		},
		ResultingLogEntries: []compiler.LogEntry{
			{
				Kind:        "user",
				TurnIndex:   nextTurn,
				Action:      "user_message",
				Observation: msg,
			},
		},
	}
	emBytes, _ := json.Marshal(emPayload)
	emHash, _, err := e.Store.Blobs().Put(emBytes)
	if err != nil {
		return err
	}
	sid := ComputeStepID(branch.HeadStepID, branch.ID, snapID, branch.ActiveAccountID, deltaHash, "user_turn", emHash, msg)
	stepRow := store.Step{
		ID:                 sid,
		RunID:              run.ID,
		BranchID:           branch.ID,
		ParentStepID:       branch.HeadStepID,
		Epoch:              snap.Epoch,
		TurnIndex:          nextTurn,
		AccountID:          branch.ActiveAccountID,
		ModelID:            "user",
		StateHash:          "user_turn",
		EpochSnapshotID:    snapID,
		FrameDeltaBlobHash: deltaHash,
		CompiledFrameHash:  "user_turn",
		EmittedBlobHash:    emHash,
	}
	if err := e.Store.AppendStep(ctx, stepRow, store.BranchUpdate{
		Epoch:           snap.Epoch,
		Status:          string(StatusQueued),
		ActiveAccountID: branch.ActiveAccountID,
		StallCount:      0,
	}); err != nil {
		return err
	}
	e.indexTranscript(ctx, branch.RunID, branch.ID, sid, []compiler.LogEntry{{Kind: "user", Observation: msg}})
	if e.OnStep != nil {
		e.OnStep(stepRow)
	}
	return nil
}

// releaseLease returns an account lease even when ctx is already cancelled (operator Stop, run
// timeout). Releasing on the cancelled context would fail and leak the lease permanently.
func (e *Executor) releaseLease(ctx context.Context, accountID string, spentUSD float64) error {
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return e.Scheduler.Release(relCtx, accountID, spentUSD)
}

// ExecuteBranch runs or resumes branchID up to maxTurns, handling 429s, crashes, rollovers, and provenance.
func (e *Executor) ExecuteBranch(ctx context.Context, branchID string, maxTurns int) (*BranchResult, error) {
	if maxTurns <= 0 {
		maxTurns = 100
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

	e.checkpointStart(ctx, run, branch)

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
	prevStateHash := ""
	lastOracleError := ""
	var currentViewport *compiler.ViewportFrame
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
				_ = e.releaseLease(ctx, preacquiredAcc.ID, 0)
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
			// Every eligible account is at its concurrency cap (parallel subagents, swarm workers):
			// wait for a lease to free up instead of failing the branch.
			for wait := 0; errors.Is(acqErr, store.ErrNoCapacity) && wait < 600; wait++ {
				select {
				case <-ctx.Done():
					return res, ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
				acc, acqErr = e.Scheduler.Acquire(ctx, preferred, e.AccountIDs)
			}
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
			Viewport:            currentViewport,
		}

		// Drain any pending cross-agent Blackboard findings/graveyard notices into Layer 2 (keeping Layer 0+1 frozen).
		if e.Blackboard != nil {
			if lt, ok := e.Tools.(*LocalToolRunner); ok && lt.Blackboard == nil {
				lt.Blackboard = e.Blackboard
			}
			if notices := e.Blackboard.DrainForBranch(run.ID, branch.ID, nextTurn); len(notices) > 0 {
				l2 = append(l2, notices...)
				pendingDeltaAppends = append(pendingDeltaAppends, notices...)
			}
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
			_ = e.releaseLease(ctx, acc.ID, 0)
			return res, err
		}

		genCtx := ctx
		if e.OnDelta != nil {
			bid := branch.ID
			genCtx = WithDeltaSink(ctx, func(t string) { e.OnDelta(bid, t) })
		}
		genResp, genErr := e.Provider.Generate(genCtx, GenerateRequest{
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
				_ = e.releaseLease(ctx, acc.ID, 0)
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
						_ = e.releaseLease(ctx, fbAcc.ID, 0)
					}
				}
				_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusSuspendedQuota), acc.ID, branch.StallCount)
				res.Status = StatusSuspendedQuota
				return res, &SuspendedQuotaError{
					BranchID: branch.ID,
					ResumeAt: time.Now().Add(rlErr.RetryAfter),
				}
			}
			_ = e.releaseLease(ctx, acc.ID, 0)
			// Leave a truthful, resumable status instead of "running": a transient upstream
			// failure is suspended_io (retry later), anything else failed.
			status := StatusFailed
			var trErr *TransientError
			if errors.As(genErr, &trErr) || errors.Is(genErr, context.Canceled) || errors.Is(genErr, context.DeadlineExceeded) {
				status = StatusSuspendedIO
			}
			_ = e.Store.UpdateBranchStatus(context.WithoutCancel(ctx), branch.ID, string(status), acc.ID, branch.StallCount)
			res.Status = status
			return res, genErr
		}
		_ = e.releaseLease(ctx, acc.ID, genResp.CostUSD)

		res.TotalTokens += genResp.InputTokens + genResp.CacheReadTokens + genResp.OutputTokens
		res.CacheReadToks += genResp.CacheReadTokens

		if genResp.StateHash != "" && !isPollOnlyTurn(genResp) && (genResp.StateHash == lastStateHash || (prevStateHash != "" && genResp.StateHash == prevStateHash)) {
			branch.StallCount++
		} else {
			branch.StallCount = 0
		}
		prevStateHash = lastStateHash
		lastStateHash = genResp.StateHash

		calls := genResp.ToolCalls
		if len(calls) == 0 && genResp.ToolCall != nil {
			calls = []ToolCallSpec{*genResp.ToolCall}
		}

		// Completion turn (no tool call).
		if genResp.Done || len(calls) == 0 {
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

		// Tool Call Turn (single or batched): pre-evaluate Data/Action-level taint policy across all calls BEFORE dispatching.
		type plannedCall struct {
			spec     ToolCallSpec
			decision taint.PolicyDecision
			reason   string
			actionID string
			argsHash string
			idemKey  string
			provJSON string
		}
		planned := make([]plannedCall, len(calls))
		var allRefs []string
		for idx, tc := range calls {
			if tc.Name == "mcp_call" {
				var mcpMgr *mcp.Manager
				switch runner := e.Tools.(type) {
				case *LocalToolRunner:
					mcpMgr = runner.MCPManager
				case *restrictedGUIToolRunner:
					if runner.base != nil {
						mcpMgr = runner.base.MCPManager
					}
				}
				tc.EffectClass = resolveMCPCallEffectClass(mcpMgr, tc.ArgsJSON)
			} else if tc.Name == "cronjob_manage" {
				tc.EffectClass = cronjobEffectClass(tc.ArgsJSON)
			} else if tc.EffectClass == "" {
				tc.EffectClass = inferToolEffectClass(tc.Name)
			}
			// With sandbox networking enabled a shell command can send data anywhere, so it is an
			// external write and subject to the untrusted-provenance check, not a local edit.
			if (tc.Name == "exec" || tc.Name == "bg_exec" || tc.Name == "execute_code") && tc.EffectClass == taint.EffectWorkspaceWrite {
				if lt, ok := e.Tools.(*LocalToolRunner); ok && lt.AllowSandboxNetwork {
					tc.EffectClass = taint.EffectExternalWrite
				}
			}
			// The model only declares the pages it chooses to cite. For anything leaving the
			// sandbox, every untrusted page this run has observed is implied provenance: the
			// model may have retyped that content into the arguments.
			if tc.EffectClass != taint.EffectReadOnly && tc.EffectClass != taint.EffectWorkspaceWrite {
				implied, err := e.Store.ListUntrustedPageIDs(ctx, run.ID)
				if err != nil {
					return res, fmt.Errorf("load run taint provenance: %w", err)
				}
				tc.ReferencedPageIDs = mergePageRefs(tc.ReferencedPageIDs, implied)
			}
			provJSONBytes, _ := json.Marshal(tc.ReferencedPageIDs)
			provJSON := string(provJSONBytes)
			allRefs = append(allRefs, tc.ReferencedPageIDs...)

			decision, reason, err := taint.EvaluateAction(ctx, e.Store, taint.ActionPolicyInput{
				ToolName:          tc.Name,
				CapabilityScope:   tc.CapabilityScope,
				EffectClass:       tc.EffectClass,
				ReferencedPageIDs: tc.ReferencedPageIDs,
				ExplicitDeny:      tc.ExplicitDeny,
				ArgsJSON:          tc.ArgsJSON,
			})
			if err != nil {
				return res, err
			}

			actionID := fmt.Sprintf("act_%x_%04d", time.Now().UnixNano(), e.seq.Add(1))
			argsHash := sha256Hex([]byte(tc.ArgsJSON))
			idemRaw := fmt.Sprintf("%s|%d|%s|%s", branch.ID, nextTurn, tc.Name, argsHash)
			if idx > 0 {
				idemRaw = fmt.Sprintf("%s|%d", idemRaw, idx)
			}
			idemKey := sha256Hex([]byte(idemRaw))

			// An operator approval authorises one dispatch of exactly this tool call. Without this an
			// approved irreversible action would be re-evaluated to "ask" forever on resume.
			if decision == taint.DecisionAsk {
				approved, apErr := e.Store.ConsumeApprovedAction(ctx, branch.ID, tc.Name, argsHash)
				if apErr != nil {
					return res, fmt.Errorf("check operator approval: %w", apErr)
				}
				if approved {
					decision = taint.DecisionAllow
					reason = "approved by operator"
				}
			}
			if decision == taint.DecisionAsk && strings.EqualFold(e.AutonomyMode, "autonomous") && tc.EffectClass != taint.EffectIrreversible {
				decision = taint.DecisionAllow
				reason = "auto-approved (autonomous mode)"
			}
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
			planned[idx] = plannedCall{
				spec:     tc,
				decision: decision,
				reason:   reason,
				actionID: actionID,
				argsHash: argsHash,
				idemKey:  idemKey,
				provJSON: provJSON,
			}
		}

		var turnEntries []compiler.LogEntry
		var lastActionID, lastObsPageID, lastObsText string
		var hasCommittedAction bool

		// Execute batched read-only tool calls concurrently while preserving deterministic
		// journal/observation commit order (0..N-1).
		canParallelReadOnly := len(planned) > 1 && e.AfterDispatchHook == nil
		if canParallelReadOnly {
			for _, pc := range planned {
				if pc.decision == taint.DecisionDeny || (pc.spec.EffectClass != taint.EffectReadOnly && pc.spec.EffectClass != taint.EffectExternalRead) {
					canParallelReadOnly = false
					break
				}
			}
		}

		type batchExecOutcome struct {
			out ToolOutput
			err error
		}
		var parallelOutcomes []batchExecOutcome
		if canParallelReadOnly {
			for _, pc := range planned {
				tc := pc.spec
				entry := store.ActionJournalEntry{
					ActionID:            pc.actionID,
					RunID:               run.ID,
					BranchID:            branch.ID,
					ParentStepID:        branch.HeadStepID,
					Epoch:               snap.Epoch,
					TurnIndex:           nextTurn,
					ToolName:            tc.Name,
					ArgsJSON:            tc.ArgsJSON,
					ArgsHash:            pc.argsHash,
					EffectClass:         string(tc.EffectClass),
					IdempotencyKey:      pc.idemKey,
					InputProvenanceJSON: pc.provJSON,
					Status:              "prepared",
				}
				if err := e.Store.PrepareAction(ctx, entry); err != nil {
					return res, err
				}
				if err := e.Store.MarkActionDispatched(ctx, pc.actionID); err != nil {
					return res, err
				}
			}
			parallelOutcomes = make([]batchExecOutcome, len(planned))
			sem := make(chan struct{}, 8)
			var wg sync.WaitGroup
			for idx, pc := range planned {
				wg.Add(1)
				go func(i int, p plannedCall) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					out, execErr := e.Tools.Execute(ctx, ToolCall{
						ActionID:          p.actionID,
						IdempotencyKey:    p.idemKey,
						RunID:             run.ID,
						BranchID:          branch.ID,
						BranchLabel:       branch.Label,
						Name:              p.spec.Name,
						ArgsJSON:          p.spec.ArgsJSON,
						EffectClass:       p.spec.EffectClass,
						CapabilityScope:   p.spec.CapabilityScope,
						WorkspaceDir:      branch.WorkspaceDir,
						ReferencedPageIDs: p.spec.ReferencedPageIDs,
					})
					parallelOutcomes[i] = batchExecOutcome{out: out, err: execErr}
				}(idx, pc)
			}
			wg.Wait()
		}

		for idx, pc := range planned {
			tc := pc.spec
			var obsText, obsPageID string
			if pc.decision == taint.DecisionDeny {
				obsText = "POLICY DENY: " + pc.reason
			} else {
				if hasCommittedAction {
					if err := e.Store.MarkActionStatus(ctx, lastActionID, "committed", lastObsPageID, lastObsText, ""); err != nil {
						return res, err
					}
				}
				var out ToolOutput
				var execErr error
				if canParallelReadOnly {
					out = parallelOutcomes[idx].out
					execErr = parallelOutcomes[idx].err
				} else {
					entry := store.ActionJournalEntry{
						ActionID:            pc.actionID,
						RunID:               run.ID,
						BranchID:            branch.ID,
						ParentStepID:        branch.HeadStepID,
						Epoch:               snap.Epoch,
						TurnIndex:           nextTurn,
						ToolName:            tc.Name,
						ArgsJSON:            tc.ArgsJSON,
						ArgsHash:            pc.argsHash,
						EffectClass:         string(tc.EffectClass),
						IdempotencyKey:      pc.idemKey,
						InputProvenanceJSON: pc.provJSON,
						Status:              "prepared",
					}
					if err := e.Store.PrepareAction(ctx, entry); err != nil {
						return res, err
					}
					if err := e.Store.MarkActionDispatched(ctx, pc.actionID); err != nil {
						return res, err
					}

					out, execErr = e.Tools.Execute(ctx, ToolCall{
						ActionID:          pc.actionID,
						IdempotencyKey:    pc.idemKey,
						RunID:             run.ID,
						BranchID:          branch.ID,
						BranchLabel:       branch.Label,
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
					if err := e.recordUntrustedInline(ctx, run.ID, out, obsRes); err != nil {
						return res, err
					}
					if obsRes.Paged {
						obsText = obsRes.Manifest
						obsPageID = obsRes.PageID
					} else {
						obsText = obsRes.InlineText
					}
					if out.Viewport != nil {
						currentViewport = out.Viewport
					}
				}
				lastActionID = pc.actionID
				lastObsPageID = obsPageID
				lastObsText = obsText
				hasCommittedAction = true
			}

			entryReasoning := genResp.Reasoning
			asstContent := ""
			asstReasoningContent := ""
			if idx == 0 {
				asstContent = genResp.AssistantContent
				asstReasoningContent = genResp.ReasoningContent
			} else {
				entryReasoning = fmt.Sprintf("batched tool call (%d/%d)", idx+1, len(planned))
			}
			callID := strings.TrimSpace(tc.CallID)
			if callID == "" {
				callID = fmt.Sprintf("call_e%d_t%d_%d", snap.Epoch, nextTurn, idx)
			}
			turnEntries = append(turnEntries, compiler.LogEntry{
				Kind:             "turn",
				TurnIndex:        nextTurn,
				ToolCallID:       callID,
				ToolName:         tc.Name,
				ArgsJSON:         tc.ArgsJSON,
				AssistantContent: asstContent,
				ReasoningContent: asstReasoningContent,
				Reasoning:        entryReasoning,
				Action:           fmt.Sprintf("%s(%s)", tc.Name, tc.ArgsJSON),
				Observation:      obsText,
				PageID:           obsPageID,
			})
		}

		nextPending := appendDynamicEntries(nextTurn, genResp, turnEntries...)
		emPayload := EmittedPayload{
			Response:            *genResp,
			ActionID:            lastActionID,
			ResultingLogEntries: nextPending,
		}
		emBytes, _ := json.Marshal(emPayload)
		emHash, _, err := e.Store.Blobs().Put(emBytes)
		if err != nil {
			return res, err
		}

		allProvBytes, _ := json.Marshal(allRefs)
		allProvJSON := string(allProvBytes)
		if len(planned) == 1 {
			allProvJSON = planned[0].provJSON
		}

		sid := ComputeStepID(branch.HeadStepID, branch.ID, snapID, acc.ModelID, deltaHash, frame.CompiledFrameHash, emHash, lastObsPageID)
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
			ObservationPageID:   lastObsPageID,
			InputProvenanceJSON: allProvJSON,
			InputTokens:         genResp.InputTokens,
			CacheReadTokens:     genResp.CacheReadTokens,
			CacheWriteTokens:    genResp.CacheWriteTokens,
			OutputTokens:        genResp.OutputTokens,
			CostUSD:             genResp.CostUSD,
			LatencyMS:           genResp.LatencyMS,
		}

		if !hasCommittedAction {
			err = e.Store.AppendStep(ctx, stepRow, store.BranchUpdate{
				Epoch:           snap.Epoch,
				Status:          string(StatusRunning),
				ActiveAccountID: acc.ID,
				StallCount:      branch.StallCount,
			})
		} else {
			err = e.Store.CommitActionAndAppendStep(ctx, lastActionID, lastObsPageID, lastObsText, stepRow, store.BranchUpdate{
				Epoch:           snap.Epoch,
				Status:          string(StatusRunning),
				ActiveAccountID: acc.ID,
				StallCount:      branch.StallCount,
			})
		}
		if err != nil {
			return res, err
		}
		e.indexTranscript(ctx, run.ID, branch.ID, sid, nextPending)
		e.checkpointAfterTurn(ctx, run.ID, branch, sid, calls)
		if e.OnStep != nil {
			e.OnStep(stepRow)
		}

		branch.HeadStepID = sid
		branch.ActiveAccountID = acc.ID
		l2 = append(l2, nextPending...)
		pendingDeltaAppends = nextPending
		nextTurn++
		res.HeadStepID = sid
		res.Status = StatusRunning
		res.TurnsExecuted++

		// clarify: the model asked the user a question. Stop here, durably, until a follow-up
		// message answers it (any surface: web, CLI chat, bridges, API).
		if q, asked := clarifyQuestion(calls); asked {
			_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusAwaitingInput), acc.ID, branch.StallCount)
			_ = e.Store.UpdateRunStatus(ctx, run.ID, string(StatusAwaitingInput))
			_ = e.Store.InsertInboxItem(ctx, store.InboxItem{
				ID:          fmt.Sprintf("inbox_q_%s_%d", branch.ID, nextTurn),
				RunID:       run.ID,
				BranchID:    branch.ID,
				ItemKind:    "question",
				Title:       "The agent is asking a question",
				Summary:     q,
				ArtifactRef: sid,
				Urgency:     "normal",
			})
			res.Status = StatusAwaitingInput
			res.FinalSummary = q
			return res, nil
		}

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
	if res.Status != StatusCompleted {
		// The turn budget ran out before the model finished. Record it instead of leaving the
		// branch "running"; ExecuteBranch can resume it with a fresh budget.
		_ = e.Store.UpdateBranchStatus(context.WithoutCancel(ctx), branch.ID, string(StatusBudgetExhausted), branch.ActiveAccountID, branch.StallCount)
		_ = e.Store.UpdateRunStatus(context.WithoutCancel(ctx), run.ID, string(StatusBudgetExhausted))
		res.Status = StatusBudgetExhausted
	}
	return res, nil
}

// isPollOnlyTurn reports turns that only wait on background jobs. Repeating an identical
// bg_poll while a build runs is progress, not a loop, so it never counts toward the stall breaker.
func isPollOnlyTurn(resp *GenerateResponse) bool {
	calls := resp.ToolCalls
	if len(calls) == 0 && resp.ToolCall != nil {
		calls = []ToolCallSpec{*resp.ToolCall}
	}
	if len(calls) == 0 {
		return false
	}
	for _, c := range calls {
		if c.Name != "bg_poll" {
			return false
		}
	}
	return true
}

func appendDynamicEntries(turn int, resp *GenerateResponse, turnEntries ...compiler.LogEntry) []compiler.LogEntry {
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
	return append(out, turnEntries...)
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
			case taint.EffectReadOnly, taint.EffectWorkspaceWrite, taint.EffectExternalRead:
				_ = e.Store.MarkActionStatus(ctx, entry.ActionID, "aborted", "", "recovered side-effect-free or local action after crash", "")
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
			default:
				// external_write, irreversible_write and any unknown class: the side effect may
				// already have happened outside the workspace, so never auto-retry.
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
				// NEVER auto-retry uncertain external or irreversible writes!
				_ = e.Store.MarkActionStatus(ctx, entry.ActionID, "ambiguous", "", "", "process crashed while "+entry.EffectClass+" was dispatched")
				_ = e.Store.InsertInboxItem(ctx, store.InboxItem{
					ID:          "inbox_rec_" + entry.ActionID,
					RunID:       run.ID,
					BranchID:    branch.ID,
					ItemKind:    "reconcile",
					Title:       fmt.Sprintf("Reconcile ambiguous %s (%s)", entry.ToolName, entry.ActionID),
					Summary:     "Crash occurred while " + entry.EffectClass + " was in-flight. Confirm external state via soma reconcile.",
					ArtifactRef: entry.ActionID,
					Urgency:     "urgent",
				})
				_ = e.Store.UpdateBranchStatus(ctx, branch.ID, string(StatusSuspendedReconcile), branch.ActiveAccountID, branch.StallCount)
				branch.Status = string(StatusSuspendedReconcile)
				return nil, &SuspendedReconcileError{
					ActionID: entry.ActionID,
					ToolName: entry.ToolName,
					Reason:   "uncertain " + entry.EffectClass + " suspended safely",
				}
			}
		}
	}
	return extraL2, nil
}

// recordUntrustedInline gives an untrusted observation that was small enough to stay inline its own
// @page record. Without it, only spilled (large) untrusted outputs would ever taint the run.
func (e *Executor) recordUntrustedInline(ctx context.Context, runID string, out ToolOutput, obs vmem.ObserveResult) error {
	if out.TrustTier != "untrusted_external" || obs.Paged || len(out.Content) == 0 {
		return nil
	}
	if _, err := e.VM.RecordProvenance(ctx, vmem.ObserveRequest{
		RunID:     runID,
		NameHint:  out.NameHint,
		Content:   out.Content,
		TrustTier: out.TrustTier,
	}); err != nil {
		return fmt.Errorf("record untrusted provenance: %w", err)
	}
	return nil
}

func mergePageRefs(declared, implied []string) []string {
	if len(implied) == 0 {
		return declared
	}
	seen := make(map[string]bool, len(declared)+len(implied))
	out := make([]string, 0, len(declared)+len(implied))
	for _, id := range append(append([]string(nil), declared...), implied...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
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
	if err := e.recordUntrustedInline(ctx, run.ID, out, obsRes); err != nil {
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
		ToolCallID:  "call_rec_" + entry.ActionID,
		ToolName:    entry.ToolName,
		ArgsJSON:    entry.ArgsJSON,
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

// WorkspaceScopeKey derives a deterministic workspace memory scope identifier for workspacePath.
func WorkspaceScopeKey(workspacePath string) string {
	trimmed := strings.TrimSpace(workspacePath)
	if trimmed == "" {
		return "workspace"
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		abs = filepath.Clean(trimmed)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return "workspace:" + hex.EncodeToString(sum[:6])
}

func (e *Executor) loadMemoryAndGraveyard(ctx context.Context, runID string) ([]compiler.MemoryEntry, []compiler.GraveyardEntry, error) {
	scopes := []string{"global", "workspace", "repo:" + runID}
	if run, rErr := e.Store.GetRun(ctx, runID); rErr == nil && strings.TrimSpace(run.WorkspacePath) != "" {
		scopes = append(scopes, WorkspaceScopeKey(run.WorkspacePath))
	}
	nodes, err := e.Store.ListActiveMemoryByScopes(ctx, scopes, nil)
	if err != nil {
		return nil, nil, err
	}
	var mem []compiler.MemoryEntry
	var grave []compiler.GraveyardEntry
	for _, n := range nodes {
		if n.Kind == "negative_path" {
			approach, reason := parseGraveyardContent(n.Content)
			grave = append(grave, compiler.GraveyardEntry{
				ID:                n.ID,
				SignatureHash:     n.SignatureHash,
				AttemptedApproach: approach,
				FailureReason:     reason,
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

	// Epoch-Pinned Hybrid Retrieval: when active memory exceeds MaxLayer1Memories, rank against the Run's
	// GoalSpec using Hybrid BM25 + Matryoshka int8 Vector RRF and freeze top-K for the entire epoch.
	maxMem := e.MaxLayer1Memories
	if maxMem > 0 && len(mem) > maxMem && e.Memory != nil {
		var goal string
		if run, rErr := e.Store.GetRun(ctx, runID); rErr == nil {
			goal = run.GoalSpec
		}
		if strings.TrimSpace(goal) != "" {
			hits, sErr := e.Memory.Search(ctx, scopes, []string{"rule", "fact", "preference"}, goal, maxMem)
			if sErr == nil && len(hits) > 0 {
				selected := make([]compiler.MemoryEntry, 0, len(hits))
				for _, h := range hits {
					selected = append(selected, compiler.MemoryEntry{
						ID:      h.Node.ID,
						Scope:   h.Node.Scope,
						Kind:    h.Node.Kind,
						Content: h.Node.Content,
					})
				}
				mem = selected
			}
		}
	}
	return mem, grave, nil
}

func parseGraveyardContent(content string) (string, string) {
	// Parse structured format written by AbandonBranch: approach="<label>" reason="<reason>"
	if strings.HasPrefix(content, "approach=") {
		if idx := strings.Index(content, " reason="); idx > len("approach=") {
			rawApp := strings.TrimSpace(content[len("approach="):idx])
			rawRea := strings.TrimSpace(content[idx+len(" reason="):])
			if unq, err := strconvUnquote(rawApp); err == nil {
				rawApp = unq
			}
			if unq, err := strconvUnquote(rawRea); err == nil {
				rawRea = unq
			}
			return rawApp, rawRea
		}
	}
	return "abandoned_path", content
}

func strconvUnquote(s string) (string, error) {
	var out string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return s, err
	}
	return out, nil
}

func defaultKernelSystemPrompt() string {
	return "You are soma, executing inside an isolated branch workspace. Available workspace tools: fs_list, fs_grep, fs_read(path, start_line?, end_line?), fs_write, fs_patch, exec(command, timeout_sec?), bg_exec, bg_poll(job_id, wait_seconds?), bg_kill, page_grep(page_id, pattern, context_lines?, ignore_case?), page_slice, page_search, memory_search, publish_finding, web_extract, desktop_task(goal, start_url?). Efficiency & rigor rules: (0) Match effort and response length to the request: answer simple questions, greetings, or quick queries directly and concisely (1–3 sentences) without running unnecessary workspace tools or writing long reports; only use tools when the user asks you to inspect, build, edit, run, or verify something in the workspace. (1) Batch independent read-only calls (fs_list, fs_grep, fs_read, page_grep, page_slice) in a single turn — they execute in parallel. (2) Use relative paths inside `/workspace` (e.g. `artifacts/...`) — `/workspace` is shared with the Virtual Desktop (`desktop_task`), whereas `/tmp` is separate per container. (3) Use fs_grep to locate symbols or configs instead of reading whole directories; prefer fs_patch for targeted edits to existing files. (4) For heavy compute or servers, pass timeout_sec to exec (up to 300s) or use bg_exec + bg_poll. (5) For websites, UIs, or desktop apps, call desktop_task(goal, start_url?) directly — the host auto-starts the Virtual Desktop (server name is `docker-desktop` if using mcp_call; never run `docker` inside exec). (6) For complex engineering tasks, verify edge cases with tests before finishing, then summarize concisely."
}

func (e *Executor) buildEpochInput(run store.Run, activeMem []compiler.MemoryEntry, grave []compiler.GraveyardEntry) compiler.EpochInput {
	sys := e.SystemPrompt
	if sys == "" {
		sys = defaultKernelSystemPrompt()
	}
	somaDir := ""
	if e.Store != nil {
		somaDir = e.Store.SomaDir()
	}
	return compiler.EpochInput{
		SystemPrompt:        sys,
		ProjectInstructions: LoadProjectInstructions(run.WorkspacePath),
		WorkspaceMode:       run.WorkspaceKind,
		TaskGoal:            run.GoalSpec,
		Syscalls:            CoreSyscalls(),
		SkillIndex:          skillIndexFor(run.WorkspacePath, somaDir),
		ActiveMemory:        activeMem,
		Graveyard:           grave,
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

// pageReadTier labels content read back out of an @page: untrusted pages stay untrusted, so
// re-reading hostile web text through page_slice/page_grep/page_search cannot launder it.
func pageReadTier(pageTier string) string {
	if pageTier == "untrusted_external" {
		return "untrusted_external"
	}
	return "trusted_repo"
}

// ErrUnknownTool is returned by LocalToolRunner when a tool name does not match any registered syscall.
var ErrUnknownTool = errors.New("dag: unknown tool")

// LocalToolRunner executes safe workspace-scoped operations (fs_list, fs_read, fs_write, fs_patch, exec, bg_exec, bg_poll, bg_kill, page_slice, page_grep, page_search, memory_search, publish_finding, web_search, web_extract, vision_analyze, broker_call, tool_search, branch_fork).
type LocalToolRunner struct {
	vm      *vmem.Manager
	Sandbox *sandbox.Manager
	Broker  *broker.Router
	// Memory is the memory service (with its configured embedder) used by memory_search/memory_write.
	Memory         *memory.Service
	MCPCatalog     *mcp.EpochCatalog
	MCPManager     *mcp.Manager
	ForkBranchFunc func(ctx context.Context, runID, fromBranchID, label, reason string) (string, error)
	GUITaskFunc    func(ctx context.Context, runID, parentBranchID, mode, goal, startURL string, maxSteps int) (ToolOutput, error)
	SkillRoots     []string
	Blackboard     *Blackboard
	BgProcs        *BgProcessManager
	HTTPClient     *http.Client
	SearchEndpoint string
	VisionBaseURL  string
	// OnClose releases resources created for this runner (e.g. MCP server processes it spawned).
	OnClose func()
	// AllowUnsandboxedExec lets exec/bg_exec run a bare host shell when Sandbox is nil.
	// It is off by default (fail-closed) and must never be enabled for model-driven runs.
	AllowUnsandboxedExec bool
	// AllowSandboxNetwork permits outbound network access inside the OS sandbox (disables --unshare-net / deny network*).
	AllowSandboxNetwork bool
	// ProjectSomaDir is the project's .soma directory (not the branch workspace copy). Tools that
	// change project-level configuration, such as cronjob_manage, write here so the daemon sees it.
	ProjectSomaDir string
	// OnTriggersChanged is called after cronjob_manage changes triggers.yaml (e.g. to hot-reload
	// the embedded daemon).
	OnTriggersChanged func()
	// DelegateFunc runs delegate_task subtasks as child branches (set by NewExecutor).
	DelegateFunc func(ctx context.Context, call ToolCall, tasks []DelegateTask, maxTurns int) (ToolOutput, error)
	// subagentDepth > 0 marks a runner used by a delegated child: delegation and other
	// SubagentBlocked tools are refused.
	subagentDepth int
}

// Close stops this runner's background jobs and releases anything registered through OnClose.
// A runner is created per run, so its jobs are unreachable (and would be orphaned) afterwards.
func (r *LocalToolRunner) Close() error {
	if r.BgProcs != nil {
		r.BgProcs.KillAll()
	}
	if r.OnClose != nil {
		r.OnClose()
	}
	return nil
}

// NewLocalToolRunner constructs a workspace-scoped tool runner with no OS sandbox. Its exec and
// bg_exec tools fail closed with ErrUnsandboxedExec; filesystem, @page and memory tools work.
func NewLocalToolRunner(vm *vmem.Manager) *LocalToolRunner {
	return &LocalToolRunner{
		vm:      vm,
		BgProcs: NewBgProcessManager(),
	}
}

// NewUnsandboxedToolRunner constructs a tool runner whose exec/bg_exec run directly on the host
// with only a scrubbed environment. For tests and fully trusted input only.
func NewUnsandboxedToolRunner(vm *vmem.Manager) *LocalToolRunner {
	r := NewLocalToolRunner(vm)
	r.AllowUnsandboxedExec = true
	return r
}

// NewSandboxedToolRunner constructs a workspace-scoped tool runner executing shell commands inside sbx.
// A nil sbx selects the default fail-closed sandbox.Manager rather than disabling isolation.
func NewSandboxedToolRunner(vm *vmem.Manager, sbx *sandbox.Manager) *LocalToolRunner {
	if sbx == nil {
		sbx = sandbox.NewManager()
	}
	return &LocalToolRunner{
		vm:      vm,
		Sandbox: sbx,
		BgProcs: NewBgProcessManager(),
	}
}

func (r *LocalToolRunner) Execute(ctx context.Context, call ToolCall) (ToolOutput, error) {
	var args map[string]any
	if strings.TrimSpace(call.ArgsJSON) != "" {
		if err := json.Unmarshal([]byte(call.ArgsJSON), &args); err != nil {
			return ToolOutput{}, fmt.Errorf("parse args for %s: %w", call.Name, err)
		}
	}

	switch call.Name {
	case "fs_list":
		return r.toolFSList(call, args)

	case "fs_grep":
		return r.toolFSGrep(call, args)

	case "fs_read":
		return r.toolFSRead(call, args)

	case "fs_write":
		return r.toolFSWrite(call, args)

	case "fs_patch":
		return r.toolFSPatch(call, args)

	case "bg_exec":
		command, _ := args["command"].(string)
		label, _ := args["label"].(string)
		if r.BgProcs == nil {
			r.BgProcs = NewBgProcessManager()
		}
		jobID, err := r.BgProcs.Start(BgStartSpec{
			RunID:            call.RunID,
			WorkspaceDir:     call.WorkspaceDir,
			Command:          command,
			Label:            label,
			Sandbox:          r.Sandbox,
			AllowUnsandboxed: r.AllowUnsandboxedExec,
			AllowNetwork:     r.AllowSandboxNetwork,
		})
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{
			NameHint:  "bg_exec.txt",
			Content:   []byte(fmt.Sprintf("started background job %s (use bg_poll(%q) to inspect output/@page or bg_kill(%q) to stop)", jobID, jobID, jobID)),
			TrustTier: "trusted_repo",
		}, nil

	case "bg_poll":
		jobID, _ := args["job_id"].(string)
		waitSecs, _ := args["wait_seconds"].(float64)
		if r.BgProcs == nil {
			return ToolOutput{}, errors.New("bg_poll: no background jobs started")
		}
		out, err := r.BgProcs.PollWait(ctx, r.vm, jobID, 3*1024, waitSecs)
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{
			NameHint:  "bg_poll.txt",
			Content:   []byte(out),
			TrustTier: shellOutputTier(r.AllowSandboxNetwork),
		}, nil

	case "bg_kill":
		jobID, _ := args["job_id"].(string)
		if r.BgProcs == nil {
			return ToolOutput{}, errors.New("bg_kill: no background jobs started")
		}
		out, err := r.BgProcs.Kill(ctx, r.vm, jobID, 3*1024)
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{
			NameHint:  "bg_kill.txt",
			Content:   []byte(out),
			TrustTier: shellOutputTier(r.AllowSandboxNetwork),
		}, nil

	case "web_search":
		query, _ := args["query"].(string)
		maxF, _ := args["max_results"].(float64)
		return ExecuteWebSearch(ctx, r.HTTPClient, r.SearchEndpoint, query, int(maxF))

	case "web_extract":
		targetURL, _ := args["url"].(string)
		return ExecuteWebExtract(ctx, r.HTTPClient, targetURL)

	case "vision_analyze":
		imgPath, _ := args["image_path"].(string)
		if imgPath == "" {
			imgPath, _ = args["path"].(string)
		}
		prompt, _ := args["prompt"].(string)
		return ExecuteVisionAnalyze(ctx, r.HTTPClient, r.VisionBaseURL, "", "", call.WorkspaceDir, imgPath, prompt)

	case "exec":
		command, _ := args["command"].(string)
		if strings.TrimSpace(command) == "" {
			return ToolOutput{}, errors.New("exec requires non-empty command")
		}
		timeout := 20 * time.Second
		if tsF, ok := args["timeout_sec"].(float64); ok && tsF > 0 {
			sec := int(tsF)
			if sec > 300 {
				sec = 300
			}
			timeout = time.Duration(sec) * time.Second
		}
		if r.Sandbox != nil {
			res, err := r.Sandbox.Exec(ctx, sandbox.ExecSpec{
				WorkspaceDir: call.WorkspaceDir,
				Command:      "/bin/sh",
				Args:         []string{"-c", command},
				AllowNetwork: r.AllowSandboxNetwork,
				Timeout:      timeout,
			})
			if err != nil {
				if errors.Is(err, sandbox.ErrCommandTimeout) {
					body := fmt.Sprintf("exit_code=-1 (TIMEOUT after %s; use timeout_sec or bg_exec for longer commands)\n%s", timeout, string(res.Combined))
					return ToolOutput{NameHint: "exec.log", Content: []byte(body), TrustTier: shellOutputTier(r.AllowSandboxNetwork)}, nil
				}
				return ToolOutput{}, err
			}
			body := fmt.Sprintf("exit_code=%d\n%s", res.ExitCode, string(res.Combined))
			return ToolOutput{NameHint: "exec.log", Content: []byte(body), TrustTier: shellOutputTier(r.AllowSandboxNetwork)}, nil
		}
		if !r.AllowUnsandboxedExec {
			return ToolOutput{}, ErrUnsandboxedExec
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", command)
		cmd.WaitDelay = 500 * time.Millisecond
		cmd.Dir = call.WorkspaceDir
		cmd.Env = sandbox.ScrubEnv(nil)
		out, err := cmd.CombinedOutput()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			body := fmt.Sprintf("exit_code=-1 (TIMEOUT after %s; use timeout_sec or bg_exec for longer commands)\n%s", timeout, string(out))
			return ToolOutput{NameHint: "exec.log", Content: []byte(body), TrustTier: "trusted_repo"}, nil
		}
		exitCode := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exitCode = ee.ExitCode()
			} else {
				return ToolOutput{}, err
			}
		}
		body := fmt.Sprintf("exit_code=%d\n%s", exitCode, string(out))
		return ToolOutput{NameHint: "exec.log", Content: []byte(body), TrustTier: "trusted_repo"}, nil

	case "page_slice":
		pid, _ := args["page_id"].(string)
		pid = strings.TrimPrefix(strings.TrimSpace(pid), "@")
		startF, _ := args["start_line"].(float64)
		endF, _ := args["end_line"].(float64)
		out, err := r.vm.Slice(ctx, pid, int(startF), int(endF))
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{NameHint: "slice.txt", Content: []byte(out), TrustTier: pageReadTier(r.vm.PageTrustTier(ctx, pid))}, nil

	case "page_grep":
		pid, _ := args["page_id"].(string)
		pid = strings.TrimPrefix(strings.TrimSpace(pid), "@")
		pat, _ := args["pattern"].(string)
		ctxF, _ := args["context_lines"].(float64)
		ignoreCase, _ := args["ignore_case"].(bool)
		out, err := r.vm.GrepWithOptions(ctx, pid, pat, 40, int(ctxF), ignoreCase)
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{NameHint: "grep.txt", Content: []byte(out), TrustTier: pageReadTier(r.vm.PageTrustTier(ctx, pid))}, nil

	case "page_search":
		if r.vm == nil {
			return ToolOutput{}, errors.New("page_search: virtual memory manager not initialized")
		}
		query, _ := args["query"].(string)
		pid, _ := args["page_id"].(string)
		out, tier, err := r.vm.SearchWithTier(ctx, call.RunID, query, pid, 5)
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{NameHint: "page_search.txt", Content: []byte(out), TrustTier: pageReadTier(tier)}, nil

	case "publish_finding":
		summary, _ := args["summary"].(string)
		if strings.TrimSpace(summary) == "" {
			return ToolOutput{}, errors.New("publish_finding requires non-empty summary")
		}
		pid, _ := args["page_id"].(string)
		bb := r.Blackboard
		if bb == nil {
			bb = NewBlackboard()
			r.Blackboard = bb
		}
		var st *store.Store
		if r.vm != nil {
			st = r.vm.Store()
		}
		// A finding from a run that has seen untrusted content may carry injected instructions:
		// share it with siblings for this run only, and quarantine the durable copy for review.
		tainted := false
		if st != nil && call.RunID != "" {
			if untrusted, uErr := st.ListUntrustedPageIDs(ctx, call.RunID); uErr == nil && len(untrusted) > 0 {
				tainted = true
			}
		}
		f, err := bb.Publish(ctx, st, PeerFinding{
			RunID:           call.RunID,
			FromBranch:      call.BranchID,
			FromLabel:       call.BranchLabel,
			Kind:            "rule_notice",
			Summary:         summary,
			PageID:          pid,
			PersistToMemory: !tainted,
		})
		if err != nil {
			return ToolOutput{}, err
		}
		if tainted && r.Memory != nil {
			if _, err := r.Memory.Propose(ctx, memory.ProposeRequest{
				Scope: "repo:" + call.RunID, Kind: "fact", Content: summary,
				TrustTier: "untrusted_external", EvidencePageID: pid,
			}); err != nil {
				return ToolOutput{}, fmt.Errorf("publish_finding: quarantine durable copy: %w", err)
			}
		}
		return ToolOutput{
			NameHint:  "publish_finding.txt",
			Content:   []byte(fmt.Sprintf("published finding %s to blackboard for sibling agents", f.ID)),
			TrustTier: "trusted_repo",
		}, nil

	case "memory_search":
		if r.vm == nil || r.vm.Store() == nil {
			return ToolOutput{}, errors.New("memory_search: store not initialized")
		}
		query, _ := args["query"].(string)
		scope, _ := args["scope"].(string)
		var scopes []string
		if strings.TrimSpace(scope) != "" {
			scopes = []string{strings.TrimSpace(scope)}
		}
		// Use the executor's memory service so queries are embedded with the same model as the
		// stored vectors; a fresh service would fall back to the default embedder.
		memSvc := r.Memory
		if memSvc == nil {
			memSvc = memory.NewService(r.vm.Store())
		}
		hits, err := memSvc.Search(ctx, scopes, nil, query, 5)
		if err != nil {
			return ToolOutput{}, err
		}
		if len(hits) == 0 {
			return ToolOutput{
				NameHint:  "memory_search.txt",
				Content:   []byte(fmt.Sprintf("memory_search: 0 matches for %q", query)),
				TrustTier: "trusted_repo",
			}, nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "memory_search: top %d match(es) for %q (Hybrid BM25 + int8 MRL RRF):\n", len(hits), query)
		for i, h := range hits {
			fmt.Fprintf(&sb, "%d. [%s] (%s/%s rrf=%.4f bm25=%.2f cos=%.2f): %s\n",
				i+1, h.Node.ID, h.Node.Scope, h.Node.Kind, h.RRFScore, h.BM25Score, h.CosineScore, h.Node.Content)
		}
		return ToolOutput{
			NameHint:  "memory_search.txt",
			Content:   []byte(strings.TrimRight(sb.String(), "\n")),
			TrustTier: "trusted_repo",
		}, nil

	case "broker_call":
		if r.Broker == nil {
			return ToolOutput{}, errors.New("broker_call: no capability broker router configured")
		}
		capName, _ := args["capability"].(string)
		if capName == "" {
			capName, _ = args["broker"].(string)
		}
		op, _ := args["op"].(string)
		target, _ := args["target"].(string)
		paramsJSON, _ := args["params_json"].(string)
		if paramsJSON == "" {
			if rawParams, ok := args["params"]; ok && rawParams != nil {
				if b, err := json.Marshal(rawParams); err == nil {
					paramsJSON = string(b)
				}
			}
		}
		eff := call.EffectClass
		// The model may declare a call riskier than inferred, never safer: a self-declared
		// "read_only" would skip the taint check on what is really an external write.
		if effStr, ok := args["effect_class"].(string); ok {
			switch declared := taint.EffectClass(strings.TrimSpace(effStr)); declared {
			case taint.EffectExternalWrite, taint.EffectIrreversible:
				eff = declared
			}
		}
		if eff == "" {
			if strings.EqualFold(op, "GET") || strings.EqualFold(op, "read") {
				eff = taint.EffectReadOnly
			} else {
				eff = taint.EffectExternalWrite
			}
		}
		resp, err := r.Broker.Call(ctx, broker.BrokerRequest{
			RunID:             call.RunID,
			BranchID:          call.BranchID,
			ActionID:          call.ActionID,
			IdempotencyKey:    call.IdempotencyKey,
			Broker:            strings.TrimSpace(capName),
			Op:                strings.TrimSpace(op),
			Target:            strings.TrimSpace(target),
			ParamsJSON:        paramsJSON,
			EffectClass:       eff,
			CapabilityScope:   call.CapabilityScope,
			ReferencedPageIDs: call.ReferencedPageIDs,
		})
		if err != nil {
			return ToolOutput{}, err
		}
		tier := resp.TrustTier
		if tier == "" {
			tier = "untrusted_external"
		}
		return ToolOutput{
			NameHint:  fmt.Sprintf("broker_%s_%s.txt", capName, op),
			Content:   resp.Body,
			TrustTier: tier,
		}, nil

	case "tool_search":
		query, _ := args["query"].(string)
		q := strings.ToLower(strings.TrimSpace(query))
		var sb strings.Builder
		var matches int
		for _, sc := range CoreSyscalls() {
			if q == "" || strings.Contains(strings.ToLower(sc.Name), q) || strings.Contains(strings.ToLower(sc.Signature), q) {
				fmt.Fprintf(&sb, "- [core] %s: %s\n", sc.Name, sc.Signature)
				matches++
			}
		}
		if r.MCPCatalog != nil {
			for _, ts := range r.MCPCatalog.Search(query) {
				fmt.Fprintf(&sb, "- [mcp] %s: %s\n", ts.Name, ts.JSONSchema)
				matches++
			}
		}
		if matches == 0 {
			return ToolOutput{
				NameHint:  "tool_search.txt",
				Content:   []byte(fmt.Sprintf("tool_search: 0 matches for %q", query)),
				TrustTier: "trusted_repo",
			}, nil
		}
		header := fmt.Sprintf("tool_search: %d match(es) for %q:\n", matches, query)
		return ToolOutput{
			NameHint:  "tool_search.txt",
			Content:   []byte(header + strings.TrimRight(sb.String(), "\n")),
			TrustTier: "trusted_repo",
		}, nil

	case "mcp_call":
		if r.MCPManager == nil {
			return ToolOutput{}, errors.New("mcp_call: no MCP manager attached (configure mcp_servers or pass --browser / --desktop)")
		}
		serverName, _ := args["server"].(string)
		toolName, _ := args["tool"].(string)
		if strings.TrimSpace(serverName) == "" {
			if parsedSrv, parsedTool, ok := mcp.ParseToolName(toolName); ok {
				serverName = parsedSrv
				toolName = parsedTool
			}
		}
		// Auto-resolve common server aliases ("desktop" or empty) so smaller models don't waste turns guessing.
		connected := r.MCPManager.ListServers()
		if strings.EqualFold(strings.TrimSpace(serverName), "desktop") {
			for _, srv := range connected {
				if srv == "docker-desktop" {
					serverName = srv
					break
				}
			}
		} else if strings.TrimSpace(serverName) == "" && len(connected) == 1 {
			serverName = connected[0]
		}
		var toolArgs map[string]any
		if rawArgs, ok := args["arguments_json"].(string); ok && strings.TrimSpace(rawArgs) != "" {
			if err := json.Unmarshal([]byte(rawArgs), &toolArgs); err != nil {
				return ToolOutput{}, fmt.Errorf("mcp_call parse arguments_json: %w", err)
			}
		} else if rawMap, ok := args["arguments"].(map[string]any); ok {
			toolArgs = rawMap
		}
		mcpOut, err := r.MCPManager.CallTool(ctx, serverName, toolName, toolArgs)
		if err != nil {
			return ToolOutput{}, err
		}
		var vp *compiler.ViewportFrame
		text := mcpOut.Content
		if len(mcpOut.Images) > 0 && r.vm != nil && r.vm.Store() != nil {
			img := mcpOut.Images[len(mcpOut.Images)-1]
			blobHash, _, bErr := r.vm.Store().Blobs().Put(img.Data)
			if bErr == nil {
				var w, h int
				if cfg, _, dErr := image.DecodeConfig(bytes.NewReader(img.Data)); dErr == nil {
					w, h = cfg.Width, cfg.Height
				}
				mime := img.MIMEType
				if mime == "" {
					mime = "image/png"
				}
				vp = &compiler.ViewportFrame{
					BlobHash: blobHash,
					MimeType: mime,
					Width:    w,
					Height:   h,
					PHash:    blobHash[:16],
					Summary:  fmt.Sprintf("%s.%s", serverName, toolName),
					DataURI:  "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
				}
				note := fmt.Sprintf("[viewport updated: %s %dx%d sha256:%s]", mime, w, h, blobHash[:12])
				if text == "" {
					text = note
				} else {
					text = text + "\n" + note
				}
			}
		}
		return ToolOutput{
			NameHint:  fmt.Sprintf("mcp_%s_%s.txt", serverName, toolName),
			Content:   []byte(text),
			TrustTier: "untrusted_external",
			Viewport:  vp,
		}, nil

	case "browser_task":
		if r.GUITaskFunc == nil {
			return ToolOutput{}, errors.New("browser_task: GUI subagent runner not attached")
		}
		goal, _ := args["goal"].(string)
		startURL, _ := args["start_url"].(string)
		maxF, _ := args["max_steps"].(float64)
		return r.GUITaskFunc(ctx, call.RunID, call.BranchID, "browser", goal, startURL, int(maxF))

	case "desktop_task":
		if r.GUITaskFunc == nil {
			return ToolOutput{}, errors.New("desktop_task: GUI subagent runner not attached")
		}
		goal, _ := args["goal"].(string)
		maxF, _ := args["max_steps"].(float64)
		return r.GUITaskFunc(ctx, call.RunID, call.BranchID, "desktop", goal, "", int(maxF))

	case "memory_write":
		if r.vm == nil || r.vm.Store() == nil {
			return ToolOutput{}, errors.New("memory_write: store not initialized")
		}
		content, _ := args["content"].(string)
		content = strings.TrimSpace(content)
		if content == "" {
			return ToolOutput{}, errors.New("memory_write requires non-empty content")
		}
		kind, _ := args["kind"].(string)
		kind = strings.TrimSpace(kind)
		if kind == "" {
			kind = "fact"
		}
		scope, _ := args["scope"].(string)
		scope = strings.TrimSpace(scope)
		if scope == "" {
			scope = "global"
		}
		// Anti-poisoning (*MINJA*): if this run has observed untrusted external data, quarantine the
		// written memory until reviewed instead of activating it silently.
		tier := "trusted_operator"
		if call.RunID != "" {
			if untrusted, uErr := r.vm.Store().ListUntrustedPageIDs(ctx, call.RunID); uErr == nil && len(untrusted) > 0 {
				tier = "untrusted_external"
			}
		}
		memSvc := r.Memory
		if memSvc == nil {
			memSvc = memory.NewService(r.vm.Store())
		}
		node, err := memSvc.Propose(ctx, memory.ProposeRequest{
			Scope:     scope,
			Kind:      kind,
			Content:   content,
			TrustTier: tier,
		})
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{
			NameHint:  "memory_write.txt",
			Content:   []byte(fmt.Sprintf("saved memory %s (scope=%s kind=%s status=%s trust=%s)", node.ID, node.Scope, node.Kind, node.Status, node.TrustTier)),
			TrustTier: "trusted_repo",
		}, nil

	case "session_search":
		if r.vm == nil || r.vm.Store() == nil {
			return ToolOutput{}, errors.New("session_search: store not initialized")
		}
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return ToolOutput{}, errors.New("session_search requires non-empty query")
		}
		limitF, _ := args["limit"].(float64)
		limit := int(limitF)
		if limit <= 0 {
			limit = 5
		}
		if limit > 20 {
			limit = 20
		}
		text, tier, err := searchSessions(ctx, r.vm.Store(), query, limit)
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{NameHint: "session_search.txt", Content: []byte(text), TrustTier: tier}, nil

	case "skill_manage":
		action, _ := args["action"].(string)
		action = strings.ToLower(strings.TrimSpace(action))
		name, _ := args["name"].(string)
		content, _ := args["content"].(string)
		roots := r.skillRoots(call)
		if len(roots) == 0 {
			return ToolOutput{}, errors.New("skill_manage: no skill root configured")
		}
		switch action {
		case "list":
			idx, err := skills.LoadIndex(roots)
			if err != nil {
				return ToolOutput{}, err
			}
			if len(idx) == 0 {
				return ToolOutput{NameHint: "skills.txt", Content: []byte("skill_manage: 0 skills installed"), TrustTier: "trusted_repo"}, nil
			}
			var sb strings.Builder
			for _, sm := range idx {
				fmt.Fprintf(&sb, "- %s: %s\n", sm.Name, sm.Description)
			}
			return ToolOutput{NameHint: "skills.txt", Content: []byte(strings.TrimRight(sb.String(), "\n")), TrustTier: "trusted_repo"}, nil
		case "read":
			for _, rt := range roots {
				dir := filepath.Join(rt, strings.TrimSpace(name))
				if _, sErr := os.Stat(filepath.Join(dir, "SKILL.md")); sErr == nil {
					body, _, err := skills.ActivateBody(dir)
					if err != nil {
						return ToolOutput{}, err
					}
					return ToolOutput{NameHint: name + "_SKILL.md", Content: []byte(body.Body), TrustTier: "trusted_repo"}, nil
				}
			}
			return ToolOutput{}, fmt.Errorf("skill_manage: skill %q not found", name)
		case "create", "patch":
			// Agent runs never write SKILL.md directly: the validated, linted content is staged
			// and applied only when an operator approves the inbox item (with history kept).
			var st *store.Store
			if r.vm != nil {
				st = r.vm.Store()
			}
			reason := "Proposed by agent run " + call.RunID
			if st != nil && call.RunID != "" {
				if untrusted, uErr := st.ListUntrustedPageIDs(ctx, call.RunID); uErr == nil && len(untrusted) > 0 {
					reason += " (WARNING: the run had observed untrusted external content)"
				}
			}
			itemID, err := skills.ProposeSkillChange(ctx, st, roots[0], name, content, reason, call.RunID)
			if err != nil {
				return ToolOutput{}, err
			}
			return ToolOutput{
				NameHint:  "skill_manage.txt",
				Content:   []byte(fmt.Sprintf("skill_manage: validated and security-linted skill %q; staged for operator approval as inbox item %s (it becomes active once approved)", strings.TrimSpace(name), itemID)),
				TrustTier: "trusted_repo",
			}, nil
		default:
			return ToolOutput{}, fmt.Errorf("skill_manage: unsupported action %q (use list|read|create|patch)", action)
		}

	case "cronjob_manage":
		return r.cronjobManage(call, args)

	case "branch_fork":
		label, _ := args["label"].(string)
		reason, _ := args["reason"].(string)
		if strings.TrimSpace(label) == "" && strings.TrimSpace(reason) == "" {
			return ToolOutput{}, errors.New("branch_fork requires label or reason")
		}
		if r.ForkBranchFunc == nil {
			return ToolOutput{}, errors.New("branch_fork: no branch forker attached to tool runner")
		}
		forkID, err := r.ForkBranchFunc(ctx, call.RunID, call.BranchID, strings.TrimSpace(label), strings.TrimSpace(reason))
		if err != nil {
			return ToolOutput{}, err
		}
		return ToolOutput{
			NameHint:  "branch_fork.txt",
			Content:   []byte(fmt.Sprintf("forked sibling branch %s (label=%q reason=%q)", forkID, label, reason)),
			TrustTier: "trusted_repo",
		}, nil

	default:
		if def, ok := lookupTool(call.Name); ok && def.Handler != nil {
			if def.SubagentBlocked && r.subagentDepth > 0 {
				return ToolOutput{}, fmt.Errorf("%s is not available to delegated subagents; report back to the parent instead", call.Name)
			}
			return def.Handler(ctx, r, call, args)
		}
		return ToolOutput{}, fmt.Errorf("%w: %q (use tool_search to inspect available syscalls)", ErrUnknownTool, call.Name)
	}
}
