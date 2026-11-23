package cli

import (
	"fmt"
	"os"
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
	return cmd
}
