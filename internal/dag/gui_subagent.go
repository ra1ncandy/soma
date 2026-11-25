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
	Provider    ModelProvider
	ModelID     string
	AccountID   string
	MCPManager  *mcp.Manager
	MCPCatalog  *mcp.EpochCatalog
	MaxSteps    int
	EnsureReady func(ctx context.Context, mode string) (*mcp.Manager, error)
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
			maxSteps = 20
		} else {
			maxSteps = 15
		}
	}
	maxCap := 25
	if mode == "browser" {
		maxCap = 30
	}
	if maxSteps > maxCap {
		maxSteps = maxCap
	}

	baseLT, ok := e.Tools.(*LocalToolRunner)
	if !ok || baseLT == nil {
		return ToolOutput{}, fmt.Errorf("%s_task requires a LocalToolRunner", mode)
	}
	if e.GUIConfig != nil && e.GUIConfig.EnsureReady != nil && (baseLT.MCPManager == nil || len(baseLT.MCPManager.ListServers()) == 0 || mode == "desktop") {
		mgr, ensureErr := e.GUIConfig.EnsureReady(ctx, mode)
		if ensureErr != nil {
			return ToolOutput{}, fmt.Errorf("%s_task auto-launch failed: %w", mode, ensureErr)
		}
		if mgr != nil {
			baseLT.MCPManager = mgr
			e.GUIConfig.MCPManager = mgr
		}
	}
	if baseLT.MCPManager == nil {
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

	// Build a focused system prompt for the fast GUI subagent, including parameter signatures
	// so smaller vision/flash models never guess wrong argument names or try unavailable shell tools.
	var toolList strings.Builder
	hasLaunchApp := false
	launchSrv := ""
	for _, srv := range servers {
		tools := baseLT.MCPManager.ListTools(srv)[srv]
		fmt.Fprintf(&toolList, "  Server %q:\n", srv)
		for _, t := range tools {
			if t.Name == "desktop_launch_app" {
				hasLaunchApp = true
				launchSrv = srv
			}
			var propKeys []string
			if t.InputSchema != nil {
				if rawSchema, err := json.Marshal(t.InputSchema); err == nil {
					var schema struct {
						Properties map[string]any `json:"properties"`
						Required   []string       `json:"required"`
					}
					if json.Unmarshal(rawSchema, &schema) == nil && len(schema.Properties) > 0 {
						reqSet := make(map[string]bool, len(schema.Required))
						for _, r := range schema.Required {
							reqSet[r] = true
						}
						for k := range schema.Properties {
							if reqSet[k] {
								propKeys = append(propKeys, k)
							} else {
								propKeys = append(propKeys, k+"?")
							}
						}
					}
				}
			}
			fmt.Fprintf(&toolList, "    - %s(%s): %s\n", t.Name, strings.Join(propKeys, ", "), t.Description)
		}
	}

	// If a start_url was supplied for desktop mode and desktop_launch_app is available, launch Chromium immediately
	// so the browser is already opening on turn 1 instead of wasting turns on an empty desktop.
	if mode == "desktop" && strings.TrimSpace(startURL) != "" && hasLaunchApp {
		cleanURL := strings.TrimSpace(startURL)
		if !strings.HasPrefix(cleanURL, "http://") && !strings.HasPrefix(cleanURL, "https://") {
			cleanURL = "https://" + cleanURL
		}
		_, _ = baseLT.MCPManager.CallTool(ctx, launchSrv, "desktop_launch_app", map[string]any{
			"command": "chromium --no-sandbox --start-maximized " + cleanURL,
		})
		select {
		case <-ctx.Done():
			return ToolOutput{}, ctx.Err()
		case <-time.After(2500 * time.Millisecond):
		}
	}

	subGoal := goal
	if strings.TrimSpace(startURL) != "" {
		subGoal = fmt.Sprintf("URL %s was just launched in Chromium. %s", strings.TrimSpace(startURL), goal)
	}
	subSys := fmt.Sprintf(
		"You are soma's fast %s GUI subagent operating inside an isolated virtual desktop.\n"+
			"Available MCP tools (invoke ONLY via mcp_call(server, tool, arguments_json)):\n%s\n"+
			"Rules:\n"+
			"1. Do NOT call host `exec` — it is disabled here. To open a GUI app/URL inside the desktop, use desktop_launch_app(command=\"chromium --no-sandbox --start-maximized https://...\"). If desktop_exec is listed above, you may use it to run commands or install packages (sudo apt-get, pip, npm) inside the isolated desktop container.\n"+
			"2. Start by calling desktop_screenshot() or desktop_list_windows() to see the current screen.\n"+
			"3. Use desktop_click(x, y or ref), desktop_type(text, enter?), desktop_key(keys), and desktop_scroll(clicks, x?, y?) to interact. Never repeat desktop_scroll more than twice without taking desktop_screenshot().\n"+
			"4. Once the requested page/app is open and visible, stop immediately and return a concise 1-3 sentence summary.\n"+
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
		AutonomyMode:        e.AutonomyMode,
		AfterDispatchHook:   e.AfterDispatchHook,
		OnStep:              e.OnStep,
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

// mcpToolLooksReadOnly recognises observation-only MCP tools by name. GUI servers (browser_*,
// desktop_*) use loose keywords for their screenshot/snapshot/inspection tools; for any other
// server only a leading read verb counts, so names that merely contain "get_" or "network"
// (forget_user, delete_network, create_snapshot) stay gated as external writes.
func mcpToolLooksReadOnly(tLower string) bool {
	if strings.HasPrefix(tLower, "browser_") || strings.HasPrefix(tLower, "desktop_") {
		for _, kw := range []string{"snapshot", "screenshot", "a11y", "list_", "get_", "console", "network", "crop"} {
			if strings.Contains(tLower, kw) {
				return true
			}
		}
		return false
	}
	for _, verb := range []string{"get_", "list_", "read_", "search_", "find_", "describe_", "query_", "fetch_"} {
		if strings.HasPrefix(tLower, verb) {
			return true
		}
	}
	return false
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
	if mcpToolLooksReadOnly(tLower) {
		return taint.EffectReadOnly
	}
	if mgr != nil && mgr.IsIsolatedDesktop(srv) {
		return taint.EffectWorkspaceWrite
	}
	return taint.EffectExternalWrite
}
