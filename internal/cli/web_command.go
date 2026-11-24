package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/webui"
)

func newWebCmd() *cobra.Command {
	var dir, addr string
	var useMock, allowRemote bool

	cmd := &cobra.Command{
		Use:   "web",
		Short: "Launch the simple localhost web UI",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			prov, modelID, pErr := selectOptionalProvider(absDir, useMock)
			if pErr == nil && modelID != "" {
				_, _ = ensureModelAccount(cmd.Context(), st, modelID, !useMock)
			}

			srv, err := webui.NewServer(st, webui.ServerConfig{
				WorkspaceDir: absDir,
				Addr:         addr,
				Version:      Version,
				Provider:     prov,
				ModelID:      modelID,
				UseMock:      useMock,
				AllowRemote:  allowRemote,
				AuthToken:    strings.TrimSpace(os.Getenv("SOMA_WEB_TOKEN")),
			})
			if err != nil {
				return err
			}

			ln, err := srv.Listen()
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			fmt.Fprintf(cmd.OutOrStdout(), "soma web listening at http://%s\n", ln.Addr().String())
			return srv.ServeListener(ctx, ln)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7420", "Listen address (loopback only unless --allow-remote + SOMA_WEB_TOKEN)")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Use deterministic local simulation provider for starting new tasks")
	cmd.Flags().BoolVar(&allowRemote, "allow-remote", false, "Allow binding non-loopback address when SOMA_WEB_TOKEN is set")
	cmd.AddCommand(newWebInstallServiceCmd(), newWebUninstallServiceCmd())
	return cmd
}

func newWebInstallServiceCmd() *cobra.Command {
	var dir, addr string
	var useMock bool
	cmd := &cobra.Command{
		Use:   "install-service",
		Short: "Install and enable a systemd user service so soma web (and auto-start tunnel) survives reboots",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			binPath, err := os.Executable()
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			unitDir := filepath.Join(home, ".config", "systemd", "user")
			if err := os.MkdirAll(unitDir, 0o755); err != nil {
				return err
			}
			execLine := fmt.Sprintf("%s web --dir %s --addr %s", binPath, absDir, addr)
			if useMock {
				execLine += " --mock"
			}
			unit := fmt.Sprintf(`[Unit]
Description=soma Dashboard & Cloudflared Tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, absDir, execLine)
			unitPath := filepath.Join(unitDir, "soma-web.service")
			if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
				return err
			}
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			if out, err := exec.Command("systemctl", "--user", "enable", "--now", "soma-web.service").CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl --user enable --now soma-web.service: %w (%s)", err, strings.TrimSpace(string(out)))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed & started %s\nTip: run `loginctl enable-linger $USER` so it starts at boot before login.\n", unitPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7420", "Listen address")
	cmd.Flags().BoolVar(&useMock, "mock", false, "Run in mock mode")
	return cmd
}

func newWebUninstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-service",
		Short: "Stop, disable, and remove the soma-web systemd user service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			_ = exec.Command("systemctl", "--user", "disable", "--now", "soma-web.service").Run()
			unitPath := filepath.Join(home, ".config", "systemd", "user", "soma-web.service")
			if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", unitPath)
			return nil
		},
	}
}
