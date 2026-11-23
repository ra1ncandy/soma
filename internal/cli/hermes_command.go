package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/hermes"
	"github.com/ra1ncandy/soma/internal/store"
)

func newImportHermesCmd() *cobra.Command {
	var fromDir, wsDir string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "import-hermes",
		Short: "Import ~/.hermes/ config, MEMORY.md/USER.md, security-linted SKILL.md packages, and cron jobs into soma",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absWS, err := filepath.Abs(wsDir)
			if err != nil {
				return err
			}
			var st *store.Store
			if !dryRun {
				st, err = store.Open(filepath.Join(absWS, ".soma"))
				if err != nil {
					return err
				}
				defer func() { _ = st.Close() }()
			}
			rep, err := hermes.Import(cmd.Context(), st, hermes.ImportOptions{
				HermesDir:    fromDir,
				WorkspaceDir: absWS,
				DryRun:       dryRun,
			})
			if err != nil {
				return err
			}
			b, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
	cmd.Flags().StringVar(&fromDir, "from", hermes.DefaultHermesDir(), "Path to Hermes home directory")
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Target soma workspace directory")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview what would be imported without writing to disk")
	return cmd
}
