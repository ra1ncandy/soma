package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

// DelegateTask is one subtask handed to a child agent.
type DelegateTask struct {
	Goal    string `json:"goal"`
	Context string `json:"context,omitempty"`
}

const (
	defaultDelegateTurns = 30
	maxDelegateTurns     = 80
	maxDelegateTasks     = 8
	maxDelegateParallel  = 4
)

func init() {
	registerTool(ToolDef{
		Name:      "delegate_task",
		Signature: "delegate_task(goal? | tasks?, context?, max_turns?)",
		Description: "Hand self-contained subtasks to child agents that run in parallel, each in its own copy of your workspace with a fresh context. " +
			"Each child sees only its goal and the context you give it, so include file paths, constraints and what to return. " +
			"When the children finish, their file changes are 3-way merged into your workspace (conflicts are reported, not overwritten) and you receive each child's final report. " +
			"Use for independent work (separate modules, parallel research); do small steps yourself.",
		Properties: map[string]any{
			"goal":      strProp("A single subtask (use tasks for several)"),
			"context":   strProp("Background every child needs: relevant paths, constraints, expected output"),
			"tasks":     map[string]any{"type": "array", "description": "Up to 8 independent subtasks, run in parallel", "items": map[string]any{"type": "object", "properties": map[string]any{"goal": map[string]any{"type": "string"}, "context": map[string]any{"type": "string"}}, "required": []string{"goal"}}},
			"max_turns": map[string]any{"type": "integer", "description": "Turn budget per child (default 30, max 80)"},
		},
		// Children's own actions pass through policy, taint and the journal individually.
		Effect:          taint.EffectWorkspaceWrite,
		Handler:         toolDelegateTask,
		SubagentBlocked: true,
	})
}

func toolDelegateTask(ctx context.Context, r *LocalToolRunner, call ToolCall, args map[string]any) (ToolOutput, error) {
	if r.DelegateFunc == nil {
		return ToolOutput{}, errors.New("delegate_task: delegation is not available in this runtime")
	}
	shared, _ := args["context"].(string)
	var tasks []DelegateTask
	if raw, ok := args["tasks"]; ok && raw != nil {
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &tasks); err != nil {
			return ToolOutput{}, fmt.Errorf("delegate_task: tasks must be [{goal, context?}]: %w", err)
		}
	}
	if g, _ := args["goal"].(string); strings.TrimSpace(g) != "" {
		tasks = append(tasks, DelegateTask{Goal: g})
	}
	var clean []DelegateTask
	for _, t := range tasks {
		t.Goal = strings.TrimSpace(t.Goal)
		if t.Goal == "" {
			continue
		}
		if shared != "" {
			t.Context = strings.TrimSpace(shared + "\n" + t.Context)
		}
		clean = append(clean, t)
	}
	if len(clean) == 0 {
		return ToolOutput{}, errors.New("delegate_task requires goal or tasks")
	}
	if len(clean) > maxDelegateTasks {
		return ToolOutput{}, fmt.Errorf("delegate_task: at most %d tasks per call", maxDelegateTasks)
	}
	turns := defaultDelegateTurns
	if f, ok := args["max_turns"].(float64); ok && f > 0 {
		turns = int(f)
	}
	if turns > maxDelegateTurns {
		turns = maxDelegateTurns
	}
	return r.DelegateFunc(ctx, call, clean, turns)
}

type delegateResult struct {
	Task          string   `json:"task"`
	BranchID      string   `json:"branch_id"`
	Status        string   `json:"status"`
	Turns         int      `json:"turns"`
	Report        string   `json:"report"`
	Error         string   `json:"error,omitempty"`
	MergedFiles   []string `json:"merged_files,omitempty"`
	Conflicts     []string `json:"conflicts,omitempty"`
	MergeError    string   `json:"merge_error,omitempty"`
	ElapsedSecond int      `json:"elapsed_seconds"`
}

// RunDelegatedTasks runs each task on a durable child branch of the caller's branch, in
// parallel, each in an isolated copy of the parent workspace. Children cannot delegate further
// or ask the user. Completed children's changes are merged back with the 3-way workspace merge.
// Child branches are ordinary DAG branches: they are journaled, replayable and inspectable.
func (e *Executor) RunDelegatedTasks(ctx context.Context, call ToolCall, tasks []DelegateTask, maxTurns int) (ToolOutput, error) {
	parent, err := e.Store.GetBranch(ctx, call.BranchID)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("delegate_task: load parent branch: %w", err)
	}
	run, err := e.Store.GetRun(ctx, call.RunID)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("delegate_task: load run: %w", err)
	}
	baseLT, ok := e.Tools.(*LocalToolRunner)
	if !ok {
		return ToolOutput{}, errors.New("delegate_task requires a LocalToolRunner")
	}
	parentWS := call.WorkspaceDir
	if parentWS == "" {
		parentWS = parent.WorkspaceDir
	}

	results := make([]delegateResult, len(tasks))
	childDirs := make([]string, len(tasks))
	sem := make(chan struct{}, maxDelegateParallel)
	var wg sync.WaitGroup
	for i, t := range tasks {
		childID := fmt.Sprintf("br_dlg_%x_%04d", time.Now().UnixNano(), e.seq.Add(1))
		results[i] = delegateResult{Task: t.Goal, BranchID: childID, Status: string(StatusQueued)}
		child := store.Branch{
			ID:              childID,
			RunID:           run.ID,
			ParentBranchID:  parent.ID,
			Label:           fmt.Sprintf("delegate-%d", i+1),
			Epoch:           1,
			Status:          string(StatusQueued),
			ActiveAccountID: parent.ActiveAccountID,
		}
		if err := e.Store.CreateBranch(ctx, child); err != nil {
			results[i].Status = string(StatusFailed)
			results[i].Error = err.Error()
			continue
		}
		dir, _, err := e.Workspace.PrepareBranchWorkspace(ctx, e.Store, run, child)
		if err != nil {
			results[i].Status = string(StatusFailed)
			results[i].Error = "prepare workspace: " + err.Error()
			continue
		}
		childDirs[i] = dir

		wg.Add(1)
		go func(i int, t DelegateTask, childID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			childLT := *baseLT
			childLT.subagentDepth = baseLT.subagentDepth + 1
			childLT.DelegateFunc = nil
			sub := &Executor{
				Store:               e.Store,
				VM:                  e.VM,
				Memory:              e.Memory,
				Blackboard:          e.Blackboard,
				Workspace:           e.Workspace,
				Scheduler:           e.Scheduler,
				Provider:            e.Provider,
				Tools:               &childLT,
				Reconciler:          e.Reconciler,
				Profile:             e.Profile,
				SystemPrompt:        delegateSystemPrompt(t),
				MaxLayer1Memories:   e.MaxLayer1Memories,
				AccountIDs:          append([]string(nil), e.AccountIDs...),
				AllowFailover:       e.AllowFailover,
				EnforceStallBreaker: true,
				AutonomyMode:        e.AutonomyMode,
				OnStep:              e.OnStep,
			}
			res, execErr := sub.ExecuteBranch(ctx, childID, maxTurns)
			r := &results[i]
			r.ElapsedSecond = int(time.Since(start).Seconds())
			if res != nil {
				r.Status = string(res.Status)
				r.Turns = res.TurnsExecuted
				r.Report = res.FinalSummary
			}
			if execErr != nil {
				r.Error = execErr.Error()
				if res == nil || res.Status == "" || res.Status == StatusRunning {
					r.Status = string(StatusFailed)
				}
			}
		}(i, t, childID)
	}
	wg.Wait()

	// Merge completed children one by one into the parent's workspace.
	for i := range results {
		if results[i].Status != string(StatusCompleted) || childDirs[i] == "" {
			continue
		}
		rep, mErr := e.Workspace.ApplyBranchWorkspace(ctx, parentWS, childDirs[i])
		results[i].MergedFiles = append(append(append(results[i].MergedFiles, rep.AddedFiles...), rep.ModifiedFiles...), rep.DeletedFiles...)
		results[i].Conflicts = rep.Conflicts
		if mErr != nil {
			results[i].MergeError = mErr.Error()
		}
	}

	tier := "trusted_repo"
	if untrusted, uErr := e.Store.ListUntrustedPageIDs(ctx, run.ID); uErr == nil && len(untrusted) > 0 {
		tier = "untrusted_external"
	}
	raw, _ := json.MarshalIndent(map[string]any{"subtasks": results}, "", "  ")
	return ToolOutput{NameHint: "delegate_task_result.json", Content: raw, TrustTier: tier}, nil
}

func delegateSystemPrompt(t DelegateTask) string {
	var sb strings.Builder
	sb.WriteString(defaultKernelSystemPrompt())
	sb.WriteString("\n\nYou are a delegated subagent working for a parent agent, in your own copy of its workspace. ")
	sb.WriteString("You cannot ask the user questions or delegate further. Do the subtask below completely, verify it, then finish with a concise report of what you did, which files you changed, and anything the parent must know.\n")
	sb.WriteString("SUBTASK: ")
	sb.WriteString(t.Goal)
	if strings.TrimSpace(t.Context) != "" {
		sb.WriteString("\nCONTEXT FROM PARENT:\n")
		sb.WriteString(t.Context)
	}
	return sb.String()
}
