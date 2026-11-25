package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/desktop"
	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

func attachSessionMCP(ctx context.Context, wsDir string, enableBrowser, enableDesktop bool, tools *dag.LocalToolRunner) (*mcp.Manager, error) {
	var endpoints []mcp.ServerEndpoint
	if configured, err := mcp.LoadServerEndpoints(wsDir); err == nil {
		for _, ep := range configured {
			if ep.IsEnabled() {
				endpoints = append(endpoints, ep)
			}
		}
	}
	if enableDesktop {
		dm := desktop.NewManager()
		st, running, err := dm.Status(ctx, wsDir)
		if err != nil {
			return nil, err
		}
		if !running {
			st, err = dm.Start(ctx, desktop.Config{WorkspaceDir: wsDir})
			if err != nil {
				return nil, fmt.Errorf("start isolated desktop: %w", err)
			}
			fmt.Printf("[desktop] started %s — watch live at %s\n", st.ContainerName, st.NoVNCURL())
		}
		endpoints = append(endpoints, dm.MCPEndpoints(st, enableBrowser)...)
	} else if enableBrowser {
		endpoints = append(endpoints, mcp.PlaywrightPreset("", true))
	}

	if len(endpoints) == 0 {
		return nil, nil
	}
	mgr := mcp.NewManager()
	if err := mgr.ConnectAll(ctx, endpoints); err != nil {
		_ = mgr.Close()
		return nil, err
	}
	cat := mcp.NewEpochCatalog()
	mgr.PopulateCatalog(cat)
	tools.MCPManager = mgr
	tools.MCPCatalog = cat
	return mgr, nil
}

func newAskCmd() *cobra.Command {
	var wsDir, profileName, guiModel string
	var useMock, enableBrowser, enableDesktop bool
	var maxTurns int

	cmd := &cobra.Command{
		Use:   "ask <prompt>",
		Short: "Zero-ceremony one-shot task execution with auto-init and optional --browser / --desktop",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			goal := strings.Join(args, " ")
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			st, err := store.Open(filepath.Join(absWS, ".soma"))
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			prov, modelID, err := selectProvider(absWS, useMock)
			if err != nil {
				return err
			}
			accID, err := ensureModelAccount(cmd.Context(), st, modelID, !useMock)
			if err != nil {
				return err
			}
			tools := dag.NewSandboxedToolRunner(vmem.NewManager(st), sandbox.NewManager())
			mcpMgr, err := attachSessionMCP(cmd.Context(), absWS, enableBrowser, enableDesktop, tools)
			if err != nil {
				return err
			}
			if mcpMgr != nil {
				defer func() { _ = mcpMgr.Close() }()
			}

			execDag := dag.NewExecutor(st, prov, tools, selectProfile(profileName))
			execDag.AccountIDs = []string{accID}
			if gm := resolveGUIModel(guiModel); gm != "" {
				execDag.GUIConfig = &dag.GUISubagentConfig{Provider: prov, ModelID: gm, MCPManager: mcpMgr}
			}

			_, brID, err := execDag.StartRun(cmd.Context(), dag.RunRequest{
				Title:         "ask",
				WorkspacePath: absWS,
				GoalSpec:      goal,
				AccountIDs:    []string{accID},
			})
			if err != nil {
				return err
			}
			res, err := execDag.ExecuteBranch(cmd.Context(), brID, maxTurns)
			if err != nil {
				return err
			}
			if res.Status == dag.StatusCompleted {
				if br, bErr := st.GetBranch(cmd.Context(), brID); bErr == nil && br.WorkspaceDir != "" && br.WorkspaceDir != absWS {
					if _, applyErr := execDag.Workspace.ApplyBranchWorkspace(cmd.Context(), absWS, br.WorkspaceDir); applyErr != nil {
						fmt.Fprintf(os.Stderr, "warning: changes were NOT applied to %s: %v\n  branch workspace kept at %s\n", absWS, applyErr, br.WorkspaceDir)
					}
				}
			}
			if res.FinalSummary != "" {
				fmt.Println(res.FinalSummary)
			} else {
				fmt.Printf("status=%s turns=%d\n", res.Status, res.TurnsExecuted)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory (auto-initializes .soma/)")
	cmd.Flags().StringVar(&profileName, "profile", "worker", "Model profile (worker | frontier | research)")
	cmd.Flags().StringVar(&guiModel, "gui-model", "", "Optional fast vision model for browser_task / desktop_task subagent (or SOMA_GUI_MODEL)")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Use offline MockProvider")
	cmd.Flags().BoolVar(&enableBrowser, "browser", false, "Attach @playwright/mcp browser automation")
	cmd.Flags().BoolVar(&enableDesktop, "desktop", false, "Attach isolated Docker desktop (Xvfb + XFCE + noVNC + AT-SPI2)")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 40, "Maximum turns")
	return cmd
}

func newChatCmd() *cobra.Command {
	var wsDir, profileName, guiModel string
	var useMock, enableBrowser, enableDesktop bool
	var maxTurns int

	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Interactive REPL for everyday tasks with auto-init, memory, and optional --browser / --desktop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			st, err := store.Open(filepath.Join(absWS, ".soma"))
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			prov, modelID, err := selectProvider(absWS, useMock)
			if err != nil {
				return err
			}
			accID, err := ensureModelAccount(cmd.Context(), st, modelID, !useMock)
			if err != nil {
				return err
			}
			tools := dag.NewSandboxedToolRunner(vmem.NewManager(st), sandbox.NewManager())
			mcpMgr, err := attachSessionMCP(cmd.Context(), absWS, enableBrowser, enableDesktop, tools)
			if err != nil {
				return err
			}
			if mcpMgr != nil {
				defer func() { _ = mcpMgr.Close() }()
			}

			execDag := dag.NewExecutor(st, prov, tools, selectProfile(profileName))
			execDag.AccountIDs = []string{accID}
			if gm := resolveGUIModel(guiModel); gm != "" {
				execDag.GUIConfig = &dag.GUISubagentConfig{Provider: prov, ModelID: gm, MCPManager: mcpMgr}
			}

			fmt.Printf("soma chat (model=%s workspace=%s browser=%v desktop=%v)\n", modelID, absWS, enableBrowser, enableDesktop)
			fmt.Println("Commands: /status, /exit")

			scanner := bufio.NewScanner(os.Stdin)
			for {
				fmt.Print("\nsoma> ")
				if !scanner.Scan() {
					break
				}
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				switch line {
				case "/exit", "/quit":
					return nil
				case "/status":
					runs, _ := st.ListRuns(cmd.Context())
					fmt.Printf("runs=%d model=%s\n", len(runs), modelID)
					continue
				}
				_, brID, err := execDag.StartRun(cmd.Context(), dag.RunRequest{
					Title:         "chat",
					WorkspacePath: absWS,
					GoalSpec:      line,
					AccountIDs:    []string{accID},
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					continue
				}
				res, err := execDag.ExecuteBranch(cmd.Context(), brID, maxTurns)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					continue
				}
				if res.FinalSummary != "" {
					fmt.Println(res.FinalSummary)
				} else {
					fmt.Printf("[completed turns=%d tokens=%d cached=%d]\n", res.TurnsExecuted, res.TotalTokens, res.CacheReadToks)
				}
			}
			return scanner.Err()
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory (auto-initializes .soma/)")
	cmd.Flags().StringVar(&profileName, "profile", "worker", "Model profile (worker | frontier | research)")
	cmd.Flags().StringVar(&guiModel, "gui-model", "", "Optional fast vision model for GUI subagent (or SOMA_GUI_MODEL)")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Use offline MockProvider")
	cmd.Flags().BoolVar(&enableBrowser, "browser", false, "Attach @playwright/mcp browser automation")
	cmd.Flags().BoolVar(&enableDesktop, "desktop", false, "Attach isolated Docker desktop")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 60, "Maximum turns per message")
	return cmd
}

func resolveGUIModel(flagVal string) string {
	if s := strings.TrimSpace(flagVal); s != "" {
		return s
	}
	return strings.TrimSpace(os.Getenv("SOMA_GUI_MODEL"))
}
