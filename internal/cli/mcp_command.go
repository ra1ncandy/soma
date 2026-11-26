package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/store"
)

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP servers, connector presets, agent importers, OAuth, and reverse MCP serve mode",
	}

	cmd.AddCommand(
		newMCPListCmd(),
		newMCPAddCmd(),
		newMCPRemoveCmd(),
		newMCPEnableCmd(true),
		newMCPEnableCmd(false),
		newMCPPresetsCmd(),
		newMCPPresetCmd(),
		newMCPImportCmd(),
		newMCPTestCmd(),
		newMCPAuthCmd(),
		newMCPServeCmd(),
		newMCPTrustCmd(),
	)
	return cmd
}

func newMCPTrustCmd() *cobra.Command {
	var dir string
	var all bool
	cmd := &cobra.Command{
		Use:   "trust [name...]",
		Short: "Allow stdio MCP servers from this project's .soma/mcp.yaml to run (lists untrusted ones without arguments)",
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			pending := mcp.UntrustedWorkspaceServers(abs)
			if len(args) == 0 && !all {
				if len(pending) == 0 {
					fmt.Fprintln(out, "All stdio MCP servers in this project are trusted.")
					return nil
				}
				fmt.Fprintln(out, "Not launched until trusted (review each command first):")
				for _, ep := range pending {
					fmt.Fprintf(out, "  %s: %s %s\n", ep.Name, ep.Command, strings.Join(ep.Args, " "))
				}
				fmt.Fprintln(out, "Run: soma mcp trust <name> (or --all)")
				return nil
			}
			want := map[string]bool{}
			for _, a := range args {
				want[a] = true
			}
			var picked []mcp.ServerEndpoint
			for _, ep := range pending {
				if all || want[ep.Name] {
					picked = append(picked, ep)
					delete(want, ep.Name)
				}
			}
			for name := range want {
				return fmt.Errorf("no untrusted stdio server named %q in %s", name, mcp.WorkspaceConfigPath(abs))
			}
			if err := mcp.TrustWorkspaceServers(abs, picked...); err != nil {
				return err
			}
			for _, ep := range picked {
				fmt.Fprintf(out, "trusted %s (%s %s)\n", ep.Name, ep.Command, strings.Join(ep.Args, " "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Project directory")
	cmd.Flags().BoolVar(&all, "all", false, "Trust every untrusted stdio server in this project")
	return cmd
}

func newMCPListCmd() *cobra.Command {
	var wsDir string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured MCP servers (global + workspace)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			eps, err := mcp.LoadServerEndpoints(absWS)
			if err != nil {
				return err
			}
			if len(eps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No MCP servers configured. Run `soma mcp presets` or `soma mcp add` to configure one.")
				return nil
			}
			for _, ep := range eps {
				state := "enabled"
				if !ep.IsEnabled() {
					state = "disabled"
				}
				target := ep.URL
				if ep.EffectiveTransport() == "stdio" {
					target = strings.TrimSpace(ep.Command + " " + strings.Join(ep.Args, " "))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-18s %-15s %-8s %s\n", ep.Name, ep.EffectiveTransport(), state, target)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	return cmd
}

func newMCPAddCmd() *cobra.Command {
	var wsDir, transport, urlStr, command, authToken string
	var args, envs, headers, includeTools, excludeTools, scopes []string
	var oauthEnabled, global bool
	var clientID, authServerURL string
	var timeoutSec int

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update an MCP server endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			ep := mcp.ServerEndpoint{
				Name:           pos[0],
				Transport:      transport,
				URL:            urlStr,
				AuthToken:      authToken,
				Command:        command,
				Args:           args,
				Env:            parseKVList(envs),
				Headers:        parseKVList(headers),
				IncludeTools:   includeTools,
				ExcludeTools:   excludeTools,
				TimeoutSeconds: timeoutSec,
			}
			if oauthEnabled || clientID != "" || authServerURL != "" || len(scopes) > 0 {
				ep.OAuth = &mcp.OAuthConfig{
					Enabled:       true,
					ClientID:      clientID,
					AuthServerURL: authServerURL,
					Scopes:        scopes,
				}
			}
			cfgPath := mcp.DefaultConfigPath(absWS, global)
			if err := mcp.SaveServerEndpoint(cfgPath, ep); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved MCP server %q to %s\n", ep.Name, cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	cmd.Flags().BoolVar(&global, "global", false, "Save to ~/.soma/mcp.yaml instead of workspace .soma/mcp.yaml")
	cmd.Flags().StringVar(&transport, "transport", "", "Transport: stdio | streamable_http | sse | http")
	cmd.Flags().StringVar(&urlStr, "url", "", "Remote MCP server URL")
	cmd.Flags().StringVar(&command, "command", "", "Local stdio command")
	cmd.Flags().StringArrayVar(&args, "arg", nil, "Command argument (repeatable)")
	cmd.Flags().StringArrayVar(&envs, "env", nil, "Environment variable KEY=VAL (repeatable)")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "HTTP header KEY=VAL (repeatable)")
	cmd.Flags().StringVar(&authToken, "auth-token", "", "Bearer token or ${ENV_VAR}")
	cmd.Flags().StringArrayVar(&includeTools, "include", nil, "Tool allowlist pattern (repeatable)")
	cmd.Flags().StringArrayVar(&excludeTools, "exclude", nil, "Tool denylist pattern (repeatable)")
	cmd.Flags().IntVar(&timeoutSec, "timeout", 0, "Request timeout in seconds")
	cmd.Flags().BoolVar(&oauthEnabled, "oauth", false, "Enable OAuth 2.1 PKCE")
	cmd.Flags().StringVar(&clientID, "client-id", "", "OAuth client ID")
	cmd.Flags().StringVar(&authServerURL, "auth-server", "", "OAuth authorization server URL")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "OAuth scope (repeatable)")
	return cmd
}

func newMCPRemoveCmd() *cobra.Command {
	var wsDir string
	var global bool
	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a configured MCP server",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			cfgPath := mcp.DefaultConfigPath(absWS, global)
			removed, err := mcp.RemoveServerEndpoint(cfgPath, pos[0])
			if err != nil {
				return err
			}
			if !removed {
				return fmt.Errorf("mcp server %q not found in %s", pos[0], cfgPath)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed MCP server %q from %s\n", pos[0], cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	cmd.Flags().BoolVar(&global, "global", false, "Modify ~/.soma/mcp.yaml")
	return cmd
}

func newMCPEnableCmd(enable bool) *cobra.Command {
	use := "enable <name>"
	short := "Enable a configured MCP server"
	if !enable {
		use = "disable <name>"
		short = "Disable a configured MCP server"
	}
	var wsDir string
	var global bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			cfgPath := mcp.DefaultConfigPath(absWS, global)
			if err := mcp.SetServerEnabled(cfgPath, pos[0], enable); err != nil {
				return err
			}
			state := "enabled"
			if !enable {
				state = "disabled"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "MCP server %q %s in %s\n", pos[0], state, cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	cmd.Flags().BoolVar(&global, "global", false, "Modify ~/.soma/mcp.yaml")
	return cmd
}

func newMCPPresetsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "presets",
		Short: "List curated MCP connector presets",
		Run: func(cmd *cobra.Command, _ []string) {
			for _, p := range mcp.ListPresets() {
				envHint := ""
				if len(p.RequiredEnv) > 0 {
					envHint = fmt.Sprintf(" [env: %s]", strings.Join(p.RequiredEnv, ", "))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %s%s\n", p.Name, p.Description, envHint)
			}
		},
	}
}

func newMCPPresetCmd() *cobra.Command {
	var wsDir string
	var global bool
	cmd := &cobra.Command{
		Use:   "preset <name> [args...]",
		Short: "Add a curated MCP connector preset to soma config",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			ep, info, err := mcp.GetPreset(pos[0], pos[1:]...)
			if err != nil {
				return err
			}
			cfgPath := mcp.DefaultConfigPath(absWS, global)
			if err := mcp.SaveServerEndpoint(cfgPath, ep); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added preset %q to %s\n", ep.Name, cfgPath)
			if len(info.RequiredEnv) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Required env vars: %s\n", strings.Join(info.RequiredEnv, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	cmd.Flags().BoolVar(&global, "global", false, "Save to ~/.soma/mcp.yaml")
	return cmd
}

func newMCPImportCmd() *cobra.Command {
	var wsDir, filePath string
	var global, dryRun bool
	cmd := &cobra.Command{
		Use:   "import <claude-code|cursor|codex|hermes>",
		Short: "Import MCP servers from Claude Code, Cursor, Codex, or Hermes config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			srcPath := strings.TrimSpace(filePath)
			if srcPath == "" {
				srcPath, err = mcp.DefaultAgentConfigPath(pos[0], absWS)
				if err != nil {
					return err
				}
			}
			eps, err := mcp.ImportMCPFromFile(pos[0], srcPath)
			if err != nil {
				return err
			}
			if dryRun {
				b, _ := json.MarshalIndent(eps, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			cfgPath := mcp.DefaultConfigPath(absWS, global)
			for _, ep := range eps {
				if err := mcp.SaveServerEndpoint(cfgPath, ep); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Imported %d MCP server(s) from %s into %s\n", len(eps), srcPath, cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&filePath, "file", "", "Override source config file path")
	cmd.Flags().BoolVar(&global, "global", false, "Save to ~/.soma/mcp.yaml")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview imported servers without writing")
	return cmd
}

func newMCPTestCmd() *cobra.Command {
	var wsDir string
	cmd := &cobra.Command{
		Use:   "test <name>",
		Short: "Connect to an MCP server, ping it, and list discovered tools/resources/prompts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			eps, err := mcp.LoadServerEndpoints(absWS)
			if err != nil {
				return err
			}
			var target *mcp.ServerEndpoint
			for i := range eps {
				if eps[i].Name == pos[0] {
					target = &eps[i]
					break
				}
			}
			if target == nil {
				return fmt.Errorf("mcp server %q not found", pos[0])
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()

			mgr := mcp.NewManager()
			defer func() { _ = mgr.Close() }()
			if err := mgr.Connect(ctx, *target); err != nil {
				return err
			}
			if err := mgr.Ping(ctx, target.Name); err != nil {
				return err
			}
			tools := mgr.ListTools(target.Name)[target.Name]
			resMap, _ := mgr.ListResources(ctx, target.Name)
			promptMap, _ := mgr.ListPrompts(ctx, target.Name)
			fmt.Fprintf(cmd.OutOrStdout(), "OK %s (%s): %d tool(s), %d resource(s), %d prompt(s)\n",
				target.Name, target.EffectiveTransport(), len(tools), len(resMap[target.Name]), len(promptMap[target.Name]))
			for _, t := range tools {
				fmt.Fprintf(cmd.OutOrStdout(), "  - %s (%s)\n", mcp.HermesToolName(target.Name, t.Name), t.Description)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	return cmd
}

func newMCPAuthCmd() *cobra.Command {
	var wsDir string
	cmd := &cobra.Command{
		Use:   "auth <name>",
		Short: "Authenticate with an OAuth 2.1 PKCE MCP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			eps, err := mcp.LoadServerEndpoints(absWS)
			if err != nil {
				return err
			}
			var target *mcp.ServerEndpoint
			for i := range eps {
				if eps[i].Name == pos[0] {
					target = &eps[i]
					break
				}
			}
			if target == nil {
				return fmt.Errorf("mcp server %q not found", pos[0])
			}
			if target.OAuth == nil {
				return fmt.Errorf("mcp server %q does not have oauth configured", target.Name)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()

			tok, err := mcp.StartPKCEFlow(ctx, *target, func(authURL string) error {
				fmt.Fprintf(cmd.OutOrStdout(), "Open this URL to authorize %s:\n%s\n", target.Name, authURL)
				_ = openBrowserURL(authURL)
				return nil
			}, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Authenticated %q (token_type=%s, expires=%s)\n", target.Name, tok.TokenType, tok.ExpiresAt.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	return cmd
}

func newMCPServeCmd() *cobra.Command {
	var wsDir, transport, addr string
	var tools []string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run soma itself as an MCP server (stdio or Streamable HTTP)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			var st *store.Store
			somaDir := filepath.Join(absWS, ".soma")
			if opened, oErr := store.Open(somaDir); oErr == nil {
				st = opened
				defer func() { _ = st.Close() }()
			}
			return mcp.RunServer(cmd.Context(), st, mcp.ServeOptions{
				WorkspaceDir: absWS,
				Transport:    transport,
				Addr:         addr,
				ToolGroups:   tools,
			})
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&transport, "transport", "stdio", "Serve transport: stdio | http")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8765", "Listen address when --transport=http")
	cmd.Flags().StringArrayVar(&tools, "tools", nil, "Tool groups to expose (status, memory, skills)")
	return cmd
}

func parseKVList(items []string) map[string]string {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func openBrowserURL(u string) error {
	if strings.TrimSpace(u) == "" {
		return errors.New("empty url")
	}
	switch runtime.GOOS {
	case "linux":
		return exec.Command("xdg-open", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return nil
	}
}
