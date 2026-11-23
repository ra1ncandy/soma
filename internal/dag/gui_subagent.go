package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/taint"
)

// GUISubagentConfig configures the fast, isolated vision subagent used by browser_task and desktop_task.
// Keeping GUI loops inside a dedicated child branch prevents screenshots and accessibility trees from
// polluting the main reasoning agent's context, allows using high-throughput/low-cost vision models,
// and enforces a strict privilege boundary (no host shell, no secret brokers, no trusted memory writes).
type GUISubagentConfig struct {
	Provider   ModelProvider
	ModelID    string
	AccountID  string
	MCPManager *mcp.Manager
	MCPCatalog *mcp.EpochCatalog
	MaxSteps   int
}

// restrictedGUIToolRunner wraps a LocalToolRunner and permits ONLY safe GUI/inspection tools.
type restrictedGUIToolRunner struct {
	base *LocalToolRunner
}

var allowedGUISubagentTools = map[string]bool{
	"mcp_call":    true,
	"tool_search": true,
	"page_grep":   true,
	"page_slice":  true,
	"page_search": true,
	"fs_read":     true,
	"fs_list":     true,
}

func (r *restrictedGUIToolRunner) Execute(ctx context.Context, call ToolCall) (ToolOutput, error) {
	if !allowedGUISubagentTools[call.Name] {
		return ToolOutput{}, fmt.Errorf("gui subagent: tool %q is not permitted inside restricted GUI subagent (allowed: mcp_call, tool_search, page_grep, page_slice, page_search, fs_read, fs_list)", call.Name)
	}
	return r.base.Execute(ctx, call)
}

// RunGUISubagent executes a browser or desktop sub-task on a dedicated child branch under runID,
// returning a concise JSON summary tagged untrusted_external back to the parent branch.
func (e *Executor) RunGUISubagent(ctx context.Context, runID, parentBranchID, mode, goal, startURL string, maxSteps int) (ToolOutput, error) {
	if strings.TrimSpace(goal) == "" {
		return ToolOutput{}, errors.New("gui subagent requires non-empty goal")
	}
	if maxSteps <= 0 {
		if mode == "browser" {
			maxSteps = 30
		} else {
			maxSteps = 40
		}
	}
	maxCap := 100
	if mode == "browser" {
		maxCap = 80
	}
	if maxSteps > maxCap {
		maxSteps = maxCap
	}

	baseLT, ok := e.Tools.(*LocalToolRunner)
	if !ok || baseLT == nil || baseLT.MCPManager == nil {
		return ToolOutput{}, fmt.Errorf("%s_task requires an active MCP manager (start with --browser or --desktop)", mode)
	}

	servers := baseLT.MCPManager.ListServers()
	if len(servers) == 0 {
		return ToolOutput{}, fmt.Errorf("%s_task: no MCP servers connected (start with --browser or --desktop)", mode)
	}

	parentBr, err := e.Store.GetBranch(ctx, parentBranchID)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("load parent branch: %w", err)
	}

	childID := fmt.Sprintf("br_gui_%x_%04d", time.Now().UnixNano(), e.seq.Add(1))
	childLabel := fmt.Sprintf("%s-subagent", mode)
	childBr := store.Branch{
		ID:                childID,
		RunID:             runID,
		ParentBranchID:    parentBranchID,
		Label:             childLabel,
		Epoch:             1,
		Status:            string(StatusQueued),
		WorkspaceDir:      parentBr.WorkspaceDir,
		WorkspaceStrategy: parentBr.WorkspaceStrategy,
	}

	prov := e.Provider
	accs := append([]string(nil), e.AccountIDs...)
	if e.GUIConfig != nil {
		if e.GUIConfig.Provider != nil {
			prov = e.GUIConfig.Provider
		}
		if e.GUIConfig.AccountID != "" {
			accs = []string{e.GUIConfig.AccountID}
			childBr.ActiveAccountID = e.GUIConfig.AccountID
		} else if e.GUIConfig.ModelID != "" {
			guiAccID := "gui-" + e.GUIConfig.ModelID
			_ = e.Store.UpsertAccount(ctx, store.Account{
				ID: guiAccID, Provider: "gui-subagent", ModelID: e.GUIConfig.ModelID, BillingMode: "subscription", MaxConcurrent: 4,
			})
			accs = []string{guiAccID}
			childBr.ActiveAccountID = guiAccID
		}
	}

	if err := e.Store.CreateBranch(ctx, childBr); err != nil {
		return ToolOutput{}, fmt.Errorf("create gui subagent branch: %w", err)
	}

	// Build a focused system prompt for the fast GUI subagent.
	var toolList strings.Builder
	for _, srv := range servers {
		tools := baseLT.MCPManager.ListTools(srv)[srv]
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		fmt.Fprintf(&toolList, "  - server=%q tools=[%s]\n", srv, strings.Join(names, ", "))
	}

	subGoal := goal
	if strings.TrimSpace(startURL) != "" {
		subGoal = fmt.Sprintf("Start at URL %s. %s", strings.TrimSpace(startURL), goal)
	}
	subSys := fmt.Sprintf(
		"You are soma's fast %s GUI subagent operating inside an isolated sandbox. "+
			"Connected MCP servers:\n%s"+
			"Use mcp_call(server, tool, arguments_json) to inspect and interact with the UI. "+
			"Prefer compact accessibility-tree / ref-based snapshots first; use screenshots (displayed in Layer 3 viewport) when visual grounding is needed. "+
			"You have NO host shell or secret access. Complete the sub-goal concisely and finish with a structured summary of what you observed/accomplished and any files saved.\n"+
			"Sub-goal: %s",
		mode, toolList.String(), subGoal,
	)

	// Clone LocalToolRunner with restricted tool surface.
	clonedLT := *baseLT
	restrictedTools := &restrictedGUIToolRunner{base: &clonedLT}

	subProf := compiler.WorkerProfile()
	subProf.MaxStalls = 3
	subExec := &Executor{
		Store:               e.Store,
		VM:                  e.VM,
		Memory:              e.Memory,
		Blackboard:          e.Blackboard,
		Workspace:           e.Workspace,
		Scheduler:           e.Scheduler,
		Provider:            prov,
		Tools:               restrictedTools,
		Profile:             subProf,
		SystemPrompt:        subSys,
		AccountIDs:          accs,
		AllowFailover:       true,
		EnforceStallBreaker: true,
	}

	res, execErr := subExec.ExecuteBranch(ctx, childID, maxSteps)
	summary := map[string]any{
		"subagent_branch_id": childID,
		"mode":               mode,
		"status":             "completed",
		"turns":              0,
		"summary":            "",
	}
	if res != nil {
		summary["status"] = string(res.Status)
		summary["turns"] = res.TurnsExecuted
		summary["summary"] = res.FinalSummary
	}
	if execErr != nil {
		summary["error"] = execErr.Error()
		if res == nil || res.Status == "" {
			summary["status"] = "failed"
		}
	}
	raw, _ := json.MarshalIndent(summary, "", "  ")
	return ToolOutput{
		NameHint:  fmt.Sprintf("%s_task_result.json", mode),
		Content:   raw,
		TrustTier: "untrusted_external",
	}, nil
}

// resolveMCPCallEffectClass classifies an mcp_call action so isolated Docker desktop/browser
// interactions are EffectWorkspaceWrite (avoiding taint deadlock after observing screen data),
// read-only snapshots/screenshots are EffectReadOnly, and host/remote MCP writes stay EffectExternalWrite.
func resolveMCPCallEffectClass(mgr *mcp.Manager, argsJSON string) taint.EffectClass {
	var args map[string]any
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return taint.EffectExternalWrite
	}
	srv, _ := args["server"].(string)
	tool, _ := args["tool"].(string)
	tLower := strings.ToLower(strings.TrimSpace(tool))
	if strings.Contains(tLower, "snapshot") || strings.Contains(tLower, "screenshot") ||
		strings.Contains(tLower, "a11y") || strings.Contains(tLower, "list_") ||
		strings.Contains(tLower, "get_") || strings.Contains(tLower, "console") ||
		strings.Contains(tLower, "network") || strings.Contains(tLower, "crop") {
		return taint.EffectReadOnly
	}
	if mgr != nil && mgr.IsIsolatedDesktop(srv) {
		return taint.EffectWorkspaceWrite
	}
	return taint.EffectExternalWrite
}
