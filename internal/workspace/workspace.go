package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

const (
	StrategyGitWorktree  = "git_worktree"
	StrategyCopySnapshot = "copy_snapshot"
	StrategyArtifactDir  = "artifact_dir"
	StrategyDirect       = "direct"
)

// NormalizeStrategy maps UI/CLI aliases ("worktree", "copy", "direct") to canonical strategy constants.
// An empty or unrecognized value returns "" so PrepareBranchWorkspace can auto-select.
func NormalizeStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "worktree", StrategyGitWorktree:
		return StrategyGitWorktree
	case "copy", StrategyCopySnapshot:
		return StrategyCopySnapshot
	case StrategyDirect, "in_place", "inplace":
		return StrategyDirect
	case StrategyArtifactDir:
		return StrategyArtifactDir
	default:
		return ""
	}
}

// ErrRemoteResourceBusy indicates another active branch holds a lease on the remote resource.
var ErrRemoteResourceBusy = errors.New("workspace: remote resource held by another active branch")

// Manager provisions and cleans up per-branch isolated filesystems and remote leases.
type Manager struct {
	somaDir string
}

// NewManager creates a workspace Manager rooted under somaDir (e.g., <repo>/.soma).
func NewManager(somaDir string) *Manager {
	return &Manager{somaDir: somaDir}
}

// PrepareBranchWorkspace isolates a working directory for branch using git worktree when clean,
// an isolated copy-snapshot (with an artifacts/ subdir) for dirty git or non-git workspaces,
// or run.WorkspacePath directly when StrategyDirect is requested on a root (non-fork) branch.
func (m *Manager) PrepareBranchWorkspace(ctx context.Context, st *store.Store, run store.Run, branch store.Branch) (string, string, error) {
	reqStrat := NormalizeStrategy(branch.WorkspaceStrategy)
	if reqStrat == StrategyDirect && branch.ParentBranchID == "" && strings.TrimSpace(run.WorkspacePath) != "" {
		absWS, err := filepath.Abs(run.WorkspacePath)
		if err != nil {
			absWS = run.WorkspacePath
		}
		if err := os.MkdirAll(absWS, 0o755); err != nil {
			return "", "", fmt.Errorf("mkdir direct workspace: %w", err)
		}
		if st != nil {
			_ = st.UpdateBranchWorkspace(ctx, branch.ID, absWS, StrategyDirect)
		}
		return absWS, StrategyDirect, nil
	}

	wsDir := filepath.Join(m.somaDir, "workspaces", branch.ID)
	if info, err := os.Stat(wsDir); err == nil && info.IsDir() {
		strategy := reqStrat
		if strategy == "" || strategy == StrategyDirect {
			strategy = StrategyCopySnapshot
		}
		return wsDir, strategy, nil
	}

	if err := os.MkdirAll(filepath.Dir(wsDir), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir workspaces root: %w", err)
	}

	// A fork or swarm worker continues from its parent branch's files, not from the pristine project:
	// the model's history already contains the parent's edits.
	parentDir := ""
	if branch.ParentBranchID != "" && st != nil {
		if parent, pErr := st.GetBranch(ctx, branch.ParentBranchID); pErr == nil && parent.WorkspaceDir != "" &&
			!sameCleanedPath(parent.WorkspaceDir, run.WorkspacePath) {
			if info, sErr := os.Stat(parent.WorkspaceDir); sErr == nil && info.IsDir() {
				parentDir = parent.WorkspaceDir
			}
		}
	}

	var strategy string
	switch {
	case parentDir != "":
		if err := copyWorkspaceSnapshot(parentDir, wsDir); err != nil {
			return "", "", fmt.Errorf("copy parent branch workspace: %w", err)
		}
		strategy = StrategyCopySnapshot
		// Relative to the project the fork starts from the same base as its parent.
		if parentBase, ok := readManifest(baseManifestPath(parentDir)); ok {
			if err := writeManifest(baseManifestPath(wsDir), parentBase); err != nil {
				return "", "", fmt.Errorf("write fork base manifest: %w", err)
			}
		}
		if forkState, err := buildManifest(wsDir); err == nil {
			_ = writeManifest(forkManifestPath(wsDir), forkState)
		}
	case reqStrat != StrategyCopySnapshot && isCleanGitRepoWithHead(ctx, run.WorkspacePath):
		cmd := exec.CommandContext(ctx, "git", "-C", run.WorkspacePath, "worktree", "add", "--detach", wsDir, "HEAD")
		if out, err := cmd.CombinedOutput(); err == nil {
			strategy = StrategyGitWorktree
		} else {
			// Fallback to copy snapshot if worktree add fails (e.g., shallow/bare edge case).
			_ = os.RemoveAll(wsDir)
			if copyErr := copyWorkspaceSnapshot(run.WorkspacePath, wsDir); copyErr != nil {
				return "", "", fmt.Errorf("git worktree failed (%v: %s) and snapshot failed: %w", err, string(out), copyErr)
			}
			strategy = StrategyCopySnapshot
		}
	default:
		if err := copyWorkspaceSnapshot(run.WorkspacePath, wsDir); err != nil {
			return "", "", fmt.Errorf("create copy snapshot: %w", err)
		}
		strategy = StrategyCopySnapshot
	}
	if _, ok := readManifest(baseManifestPath(wsDir)); !ok {
		if err := recordBaseManifest(wsDir); err != nil {
			return "", "", err
		}
	}

	if st != nil {
		_ = st.UpdateBranchWorkspace(ctx, branch.ID, wsDir, strategy)
	}
	return wsDir, strategy, nil
}

// CleanupBranchWorkspace removes an isolated branch worktree or snapshot directory.
// Direct workspaces pointing at run.WorkspacePath are never removed.
func (m *Manager) CleanupBranchWorkspace(ctx context.Context, run store.Run, branch store.Branch) error {
	if branch.WorkspaceDir == "" || branch.WorkspaceStrategy == StrategyDirect {
		return nil
	}
	if run.WorkspacePath != "" {
		absRun, err1 := filepath.Abs(run.WorkspacePath)
		absBr, err2 := filepath.Abs(branch.WorkspaceDir)
		if err1 == nil && err2 == nil && filepath.Clean(absRun) == filepath.Clean(absBr) {
			return nil
		}
	}
	if branch.WorkspaceStrategy == StrategyGitWorktree && run.WorkspacePath != "" {
		cmd := exec.CommandContext(ctx, "git", "-C", run.WorkspacePath, "worktree", "remove", "--force", branch.WorkspaceDir)
		_ = cmd.Run()
	}
	_ = os.Remove(baseManifestPath(branch.WorkspaceDir))
	_ = os.Remove(forkManifestPath(branch.WorkspaceDir))
	return os.RemoveAll(branch.WorkspaceDir)
}

// AcquireRemoteNamespace acquires an exclusive lease on resourceKey for branchID and returns
// its isolated remote path namespace (runs/<run_id>/<branch_id>).
func (m *Manager) AcquireRemoteNamespace(ctx context.Context, st *store.Store, resourceKey, branchID string, ttl time.Duration) (string, error) {
	branch, err := st.GetBranch(ctx, branchID)
	if err != nil {
		return "", fmt.Errorf("lookup branch for remote lease: %w", err)
	}
	ns := fmt.Sprintf("runs/%s/%s", branch.RunID, branch.ID)
	lease, err := st.AcquireRemoteLease(ctx, resourceKey, branchID, ns, ttl)
	if err != nil {
		if errors.Is(err, store.ErrLeaseBusy) {
			return "", fmt.Errorf("%w: %v", ErrRemoteResourceBusy, err)
		}
		return "", err
	}
	return lease.RemoteNamespace, nil
}

func isCleanGitRepoWithHead(ctx context.Context, dir string) bool {
	if dir == "" {
		return false
	}
	inside := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := inside.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return false
	}
	// `git worktree add` checks out the whole repository. When the workspace is a subdirectory of
	// a larger repo that layout would not line up with the project directory, so use a copy instead.
	top, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	realDir, err1 := filepath.EvalSymlinks(absDir)
	realTop, err2 := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err1 != nil || err2 != nil || filepath.Clean(realDir) != filepath.Clean(realTop) {
		return false
	}
	head := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "HEAD")
	if err := head.Run(); err != nil {
		return false
	}
	status := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--", ".", ":(exclude).soma")
	stOut, err := status.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(stOut)) == ""
}

func copyWorkspaceSnapshot(srcRoot, dstRoot string) error {
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dstRoot, "artifacts"), 0o755); err != nil {
		return err
	}
	if srcRoot == "" {
		return nil
	}
	info, err := os.Stat(srcRoot)
	if err != nil || !info.IsDir() {
		return nil
	}

	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		base := filepath.Base(rel)
		if base == ".soma" || base == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Never copy nested workspaces inside dstRoot.
		absPath, _ := filepath.Abs(path)
		absDst, _ := filepath.Abs(dstRoot)
		if absPath == absDst {
			return filepath.SkipDir
		}

		target := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Skip symlinks during snapshot to avoid escaping isolated directory.
			return nil
		}
		return copyFile(path, target, d)
	})
}

func copyFile(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}
