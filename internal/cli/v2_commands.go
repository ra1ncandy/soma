package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/skills"
	"github.com/ra1ncandy/soma/internal/store"
)

func newMemoryCmd() *cobra.Command {
	memCmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect and manage the bi-temporal provenance memory graph",
	}

	var listDir, listScope string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List active bi-temporal memory nodes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(listDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			nodes, err := st.ListActiveMemory(cmd.Context(), listScope, nil)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Active memory nodes (%d):\n", len(nodes))
			for _, n := range nodes {
				fmt.Fprintf(out, "  [%s] scope=%s kind=%s tier=%s valid_from=%d: %s\n",
					n.ID, n.Scope, n.Kind, n.TrustTier, n.ValidFrom, n.Content)
			}
			return nil
		},
	}
	listCmd.Flags().StringVar(&listDir, "dir", ".", "Workspace directory")
	listCmd.Flags().StringVar(&listScope, "scope", "", "Filter by scope")

	var addDir, addScope, addKind, addContent string
	addCmd := &cobra.Command{
		Use:   "add",
		Short: "Add a trusted rule, fact, or preference",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(addContent) == "" {
				return errors.New("--content is required")
			}
			absDir, err := filepath.Abs(addDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			node, err := memory.NewService(st).Propose(cmd.Context(), memory.ProposeRequest{
				Scope:     addScope,
				Kind:      addKind,
				Content:   addContent,
				TrustTier: "trusted_user",
				Status:    "active",
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added memory node %s (scope=%s kind=%s)\n", node.ID, node.Scope, node.Kind)
			return nil
		},
	}
	addCmd.Flags().StringVar(&addDir, "dir", ".", "Workspace directory")
	addCmd.Flags().StringVar(&addScope, "scope", "global", "Scope (global | repo:<id>)")
	addCmd.Flags().StringVar(&addKind, "kind", "rule", "Kind (rule | fact | preference)")
	addCmd.Flags().StringVar(&addContent, "content", "", "Memory content")

	var supDir, supContent string
	supCmd := &cobra.Command{
		Use:   "supersede <old_id>",
		Short: "Supersede an existing memory node cleanly via SUPERSEDES edge",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(supContent) == "" {
				return errors.New("--content is required")
			}
			absDir, err := filepath.Abs(supDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			oldNode, err := st.GetMemoryNode(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			newNode, err := memory.NewService(st).Supersede(cmd.Context(), oldNode.ID, memory.ProposeRequest{
				Scope:     oldNode.Scope,
				Kind:      oldNode.Kind,
				Content:   supContent,
				TrustTier: "trusted_user",
			}, "")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Superseded %s -> %s\n", oldNode.ID, newNode.ID)
			return nil
		},
	}
	supCmd.Flags().StringVar(&supDir, "dir", ".", "Workspace directory")
	supCmd.Flags().StringVar(&supContent, "content", "", "Replacement content")

	var mergeDir, mergeContent string
	mergeCmd := &cobra.Command{
		Use:   "merge <id1,id2,...>",
		Short: "Merge multiple existing memory nodes into one via MERGES edges",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(mergeContent) == "" {
				return errors.New("--content is required")
			}
			oldIDs := strings.Split(args[0], ",")
			absDir, err := filepath.Abs(mergeDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			first, err := st.GetMemoryNode(cmd.Context(), oldIDs[0])
			if err != nil {
				return err
			}
			merged, err := memory.NewService(st).Merge(cmd.Context(), oldIDs, memory.ProposeRequest{
				Scope:     first.Scope,
				Kind:      first.Kind,
				Content:   mergeContent,
				TrustTier: "trusted_user",
			}, "")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Merged %d node(s) -> %s\n", len(oldIDs), merged.ID)
			return nil
		},
	}
	mergeCmd.Flags().StringVar(&mergeDir, "dir", ".", "Workspace directory")
	mergeCmd.Flags().StringVar(&mergeContent, "content", "", "Merged content")

	var auditDir, auditScope string
	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit active memory nodes against files present in workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(auditDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			findings, err := memory.NewService(st).AuditWorkspace(cmd.Context(), auditScope, absDir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Stale memory findings (%d):\n", len(findings))
			for _, f := range findings {
				fmt.Fprintf(out, "  [%s] missing %q (%s)\n", f.NodeID, f.MissingPath, f.Content)
			}
			return nil
		},
	}
	auditCmd.Flags().StringVar(&auditDir, "dir", ".", "Workspace directory")
	auditCmd.Flags().StringVar(&auditScope, "scope", "", "Filter by scope")

	var graphDir string
	graphCmd := &cobra.Command{
		Use:   "graph <node_id>",
		Short: "Display SUPERSEDES / MERGES / CONTRADICTS edges touching node_id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(graphDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			node, err := st.GetMemoryNode(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			edges, err := st.ListMemoryEdges(cmd.Context(), node.ID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Node %s [%s] (%s/%s): %s\n", node.ID, node.Status, node.Scope, node.Kind, node.Content)
			fmt.Fprintf(out, "Edges (%d):\n", len(edges))
			for _, e := range edges {
				fmt.Fprintf(out, "  %s --[%s]--> %s (step=%s)\n", e.FromNodeID, e.Relation, e.ToNodeID, e.ProvenanceStepID)
			}
			return nil
		},
	}
	graphCmd.Flags().StringVar(&graphDir, "dir", ".", "Workspace directory")

	var editDir, editContent string
	editCmd := &cobra.Command{
		Use:   "edit <node_id>",
		Short: "Edit a memory node directly via --content or $EDITOR",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(editDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			node, err := st.GetMemoryNode(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			newText := strings.TrimSpace(editContent)
			if newText == "" {
				tmp, err := os.CreateTemp("", "soma-mem-edit-*.md")
				if err != nil {
					return err
				}
				tmpPath := tmp.Name()
				defer func() { _ = os.Remove(tmpPath) }()
				_, _ = tmp.WriteString(node.Content + "\n")
				_ = tmp.Close()

				editor := os.Getenv("VISUAL")
				if editor == "" {
					editor = os.Getenv("EDITOR")
				}
				if editor == "" {
					editor = "vi"
				}
				edCmd := exec.CommandContext(cmd.Context(), editor, tmpPath)
				edCmd.Stdin = os.Stdin
				edCmd.Stdout = os.Stdout
				edCmd.Stderr = os.Stderr
				if err := edCmd.Run(); err != nil {
					return err
				}
				editedBytes, err := os.ReadFile(tmpPath)
				if err != nil {
					return err
				}
				newText = strings.TrimSpace(string(editedBytes))
				if newText == "" {
					return errors.New("aborted: edited memory content is empty")
				}
			}
			if err := st.UpdateMemoryContent(cmd.Context(), node.ID, newText); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated memory node %s\n", node.ID)
			return nil
		},
	}
	editCmd.Flags().StringVar(&editDir, "dir", ".", "Workspace directory")
	editCmd.Flags().StringVar(&editContent, "content", "", "New memory content (opens $EDITOR when omitted)")

	memCmd.AddCommand(listCmd, addCmd, supCmd, mergeCmd, auditCmd, graphCmd, editCmd)
	return memCmd
}

func newSkillCmd() *cobra.Command {
	skillCmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage and security-lint AAIF Agent Skills (SKILL.md)",
	}

	var listDir string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List validated AAIF skills in .soma/skills and .skills",
		RunE: func(cmd *cobra.Command, _ []string) error {
			absDir, err := filepath.Abs(listDir)
			if err != nil {
				return err
			}
			idx, err := skills.LoadIndex([]string{
				filepath.Join(absDir, ".soma", "skills"),
				filepath.Join(absDir, ".skills"),
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Skills (%d):\n", len(idx))
			for _, sk := range idx {
				fmt.Fprintf(out, "  - %s: %s\n", sk.Name, sk.Description)
			}
			return nil
		},
	}
	listCmd.Flags().StringVar(&listDir, "dir", ".", "Workspace directory")

	lintCmd := &cobra.Command{
		Use:   "lint <path>",
		Short: "Validate AAIF SKILL.md format and run pre-activation security lint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sk, err := skills.ParseSkillMD(args[0])
			if err != nil {
				return err
			}
			findings, err := skills.LintSkillDir(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(findings) == 0 {
				fmt.Fprintf(out, "PASS: skill %q is AAIF-valid with 0 security findings\n", sk.Frontmatter.Name)
				return nil
			}
			crit := 0
			for _, f := range findings {
				if f.Severity == "critical" {
					crit++
				}
				fmt.Fprintf(out, "[%s] %s (%s:%d): %s\n", strings.ToUpper(f.Severity), f.RuleID, f.File, f.Line, f.Detail)
			}
			if crit > 0 {
				return fmt.Errorf("skill %q blocked: %d critical security finding(s)", sk.Frontmatter.Name, crit)
			}
			return nil
		},
	}

	var instDir string
	installCmd := &cobra.Command{
		Use:   "install <path>",
		Short: "Lint and install a skill directory into .soma/skills/",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(instDir)
			if err != nil {
				return err
			}
			sk, err := skills.ParseSkillMD(args[0])
			if err != nil {
				return err
			}
			if _, _, err := skills.ActivateBody(args[0]); err != nil {
				return err
			}
			dst := filepath.Join(absDir, ".soma", "skills", sk.Frontmatter.Name)
			if err := copyDirTree(args[0], dst); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed AAIF skill %q -> %s\n", sk.Frontmatter.Name, dst)
			return nil
		},
	}
	installCmd.Flags().StringVar(&instDir, "dir", ".", "Workspace directory")

	skillCmd.AddCommand(listCmd, lintCmd, installCmd)
	return skillCmd
}

func newOracleCmd() *cobra.Command {
	oracleCmd := &cobra.Command{
		Use:   "oracle",
		Short: "Pin invariant files and verify Verification Contracts",
	}

	var lockDir, lockCmdBin string
	var lockArgs []string
	lockCmd := &cobra.Command{
		Use:   "lock <run_id> <file...>",
		Short: "Hash-pin files into a command_oracle contract on run_id",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(lockDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			runID := args[0]
			pinned := make(map[string]string)
			for _, rel := range args[1:] {
				full, err := oracle.ResolveSafePath(absDir, rel)
				if err != nil {
					return err
				}
				sha, err := oracle.HashFileSHA256(full)
				if err != nil {
					return err
				}
				pinned[filepath.ToSlash(filepath.Clean(rel))] = sha
			}
			specBytes, _ := json.Marshal(oracle.CommandOracleSpec{
				Command:     lockCmdBin,
				Args:        lockArgs,
				PinnedFiles: pinned,
			})
			cID := fmt.Sprintf("ctr_%x", time.Now().UnixNano())
			if err := st.CreateContract(cmd.Context(), store.Contract{
				ID:         cID,
				RunID:      runID,
				Kind:       "command_oracle",
				SpecJSON:   string(specBytes),
				PinnedHash: fmt.Sprintf("pin_%d", len(pinned)),
				Required:   true,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Locked %d invariant file(s) into contract %s on run %s\n", len(pinned), cID, runID)
			return nil
		},
	}
	lockCmd.Flags().StringVar(&lockDir, "dir", ".", "Workspace directory")
	lockCmd.Flags().StringVar(&lockCmdBin, "cmd", "/bin/true", "Verification command to execute inside sandbox")
	lockCmd.Flags().StringSliceVar(&lockArgs, "args", nil, "Command arguments")

	var verDir string
	verifyCmd := &cobra.Command{
		Use:   "verify <branch_id>",
		Short: "Run attached Verification Contracts for branch_id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(verDir)
			if err != nil {
				return err
			}
			st, err := openStore(absDir)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			br, err := st.GetBranch(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			run, err := st.GetRun(cmd.Context(), br.RunID)
			if err != nil {
				return err
			}
			ver := oracle.NewVerifier(st, sandbox.NewManager())
			sum, err := ver.VerifyBranch(cmd.Context(), run, br, br.HeadStepID)
			if err != nil {
				return err
			}
			if !sum.AllPassed {
				fmt.Fprintf(cmd.OutOrStdout(), "FAIL:\n%s\n", sum.Counterexample)
				return errors.New("verification failed")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "PASS: all verification contracts succeeded on %s\n", br.ID)
			return nil
		},
	}
	verifyCmd.Flags().StringVar(&verDir, "dir", ".", "Workspace directory")

	oracleCmd.AddCommand(lockCmd, verifyCmd)
	return oracleCmd
}

func newSandboxCmd() *cobra.Command {
	sbxCmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Run commands inside soma's fail-closed OS sandbox",
	}

	var dir string
	var allowNet bool
	execCmd := &cobra.Command{
		Use:   "exec -- <cmd> [args...]",
		Short: "Execute a command inside the workspace OS sandbox",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			mgr := sandbox.NewManager()
			res, err := mgr.Exec(cmd.Context(), sandbox.ExecSpec{
				WorkspaceDir: absDir,
				Command:      args[0],
				Args:         args[1:],
				AllowNetwork: allowNet,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "[sandbox:%s exit=%d]\n", res.Backend, res.ExitCode)
			if len(res.Stdout) > 0 {
				_, _ = out.Write(res.Stdout)
			}
			if len(res.Stderr) > 0 {
				_, _ = cmd.ErrOrStderr().Write(res.Stderr)
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("sandboxed command exited %d", res.ExitCode)
			}
			return nil
		},
	}
	execCmd.Flags().StringVar(&dir, "dir", ".", "Workspace directory")
	execCmd.Flags().BoolVar(&allowNet, "allow-net", false, "Allow network access inside sandbox")

	sbxCmd.AddCommand(execCmd)
	return sbxCmd
}

func copyDirTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer func() { _ = out.Close() }()
		_, err = io.Copy(out, in)
		return err
	})
}
