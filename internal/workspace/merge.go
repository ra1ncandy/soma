package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ErrMergeConflict indicates two concurrent worker workspaces modified overlapping lines in the same file.
var ErrMergeConflict = errors.New("workspace: concurrent branch merge conflict")

// MergeReport summarizes files merged across concurrent cooperative worker workspaces.
type MergeReport struct {
	AddedFiles     []string `json:"added_files,omitempty"`
	ModifiedFiles  []string `json:"modified_files,omitempty"`
	DeletedFiles   []string `json:"deleted_files,omitempty"`
	ThreeWayMerged []string `json:"three_way_merged,omitempty"`
	Conflicts      []string `json:"conflicts,omitempty"`
}

type fileState struct {
	exists  bool
	hash    string
	content []byte
	mode    fs.FileMode
}

// MergeBranchWorkspaces merges changes from concurrent workerDirs (computed relative to baseDir)
// into targetDir. Disjoint file edits apply directly; concurrent edits to the same file use 3-way
// line merging (`git merge-file`), failing closed with ErrMergeConflict on overlapping line edits.
func (m *Manager) MergeBranchWorkspaces(
	ctx context.Context,
	baseDir string,
	workerDirs []string,
	targetDir string,
) (MergeReport, error) {
	var rep MergeReport
	if len(workerDirs) == 0 {
		return rep, nil
	}

	baseFiles, err := snapshotDirFiles(baseDir)
	if err != nil {
		return rep, fmt.Errorf("snapshot base workspace: %w", err)
	}

	// Track merged state starting from baseFiles.
	merged := make(map[string]fileState, len(baseFiles))
	ownerByPath := make(map[string]int) // relPath -> first worker index that modified it
	for rel, st := range baseFiles {
		merged[rel] = st
	}

	for wIdx, wDir := range workerDirs {
		if strings.TrimSpace(wDir) == "" {
			continue
		}
		wFiles, err := snapshotDirFiles(wDir)
		if err != nil {
			return rep, fmt.Errorf("snapshot worker %d workspace: %w", wIdx, err)
		}

		// Collect union of paths in baseFiles and wFiles in deterministic sorted order.
		pathSet := make(map[string]bool, len(baseFiles)+len(wFiles))
		for rel := range baseFiles {
			pathSet[rel] = true
		}
		for rel := range wFiles {
			pathSet[rel] = true
		}
		var paths []string
		for rel := range pathSet {
			paths = append(paths, rel)
		}
		sort.Strings(paths)

		for _, rel := range paths {
			baseSt := baseFiles[rel]
			workSt := wFiles[rel]
			// Skip if worker did not change file relative to base.
			if baseSt.exists == workSt.exists && baseSt.hash == workSt.hash {
				continue
			}

			curSt := merged[rel]
			prevOwner, alreadyTouched := ownerByPath[rel]
			if !alreadyTouched {
				merged[rel] = workSt
				ownerByPath[rel] = wIdx
				continue
			}

			// If a previous worker made the exact same change, no conflict.
			if curSt.exists == workSt.exists && curSt.hash == workSt.hash {
				continue
			}
			_ = prevOwner

			// Conflict if one deleted and another modified, or both added incompatible files without common base.
			if !curSt.exists || !workSt.exists {
				rep.Conflicts = append(rep.Conflicts, rel)
				continue
			}

			// Both workers modified or added the same file differently: attempt 3-way merge via git merge-file.
			mergedBytes, ok := threeWayMergeBytes(ctx, baseSt.content, curSt.content, workSt.content)
			if !ok {
				rep.Conflicts = append(rep.Conflicts, rel)
				continue
			}
			rep.ThreeWayMerged = append(rep.ThreeWayMerged, rel)
			merged[rel] = fileState{
				exists:  true,
				hash:    sha256HexBytes(mergedBytes),
				content: mergedBytes,
				mode:    workSt.mode,
			}
		}
	}

	if len(rep.Conflicts) > 0 {
		sort.Strings(rep.Conflicts)
		return rep, fmt.Errorf("%w on file(s): %s", ErrMergeConflict, strings.Join(rep.Conflicts, ", "))
	}

	// Materialize merged state into targetDir.
	allPaths := make([]string, 0, len(merged))
	for rel := range merged {
		allPaths = append(allPaths, rel)
	}
	sort.Strings(allPaths)

	for _, rel := range allPaths {
		baseSt := baseFiles[rel]
		finalSt := merged[rel]
		if baseSt.exists == finalSt.exists && baseSt.hash == finalSt.hash {
			continue
		}
		dstPath := filepath.Join(targetDir, filepath.FromSlash(rel))
		if !finalSt.exists {
			_ = os.Remove(dstPath)
			rep.DeletedFiles = append(rep.DeletedFiles, rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return rep, err
		}
		mode := finalSt.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(dstPath, finalSt.content, mode); err != nil {
			return rep, err
		}
		if !baseSt.exists {
			rep.AddedFiles = append(rep.AddedFiles, rel)
		} else {
			rep.ModifiedFiles = append(rep.ModifiedFiles, rel)
		}
	}
	return rep, nil
}

func snapshotDirFiles(root string) (map[string]fileState, error) {
	out := make(map[string]fileState)
	if strings.TrimSpace(root) == "" {
		return out, nil
	}
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return out, nil
	}

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		base := d.Name()
		if base == ".soma" || base == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if base == "__pycache__" || base == ".pytest_cache" || base == "node_modules" || base == ".venv" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fInfo, err := d.Info()
		mode := fs.FileMode(0o644)
		if err == nil {
			mode = fInfo.Mode().Perm()
		}
		out[relSlash] = fileState{
			exists:  true,
			hash:    sha256HexBytes(raw),
			content: raw,
			mode:    mode,
		}
		return nil
	})
	return out, err
}

func threeWayMergeBytes(ctx context.Context, base, ours, theirs []byte) ([]byte, bool) {
	if bytes.Equal(ours, theirs) {
		return ours, true
	}
	tmpDir, err := os.MkdirTemp("", "soma-merge3-*")
	if err != nil {
		return nil, false
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	baseFile := filepath.Join(tmpDir, "base")
	oursFile := filepath.Join(tmpDir, "ours")
	theirsFile := filepath.Join(tmpDir, "theirs")

	if os.WriteFile(baseFile, base, 0o600) != nil ||
		os.WriteFile(oursFile, ours, 0o600) != nil ||
		os.WriteFile(theirsFile, theirs, 0o600) != nil {
		return nil, false
	}

	cmd := exec.CommandContext(ctx, "git", "merge-file", "-p", "--quiet", oursFile, baseFile, theirsFile)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FileDiff represents a single file change between a branch workspace and the project directory.
type FileDiff struct {
	Path        string `json:"path"`
	Status      string `json:"status"` // "added" | "modified" | "deleted"
	UnifiedDiff string `json:"unified_diff,omitempty"`
}

// WorkspaceDiffReport summarizes unapplied changes in an isolated branch workspace relative to baseDir.
type WorkspaceDiffReport struct {
	Files        []FileDiff `json:"files"`
	TotalChanges int        `json:"total_changes"`
}

func sameCleanedPath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

// CountWorkspaceChanges returns the number of files in branchDir that differ from baseDir.
func (m *Manager) CountWorkspaceChanges(ctx context.Context, baseDir, branchDir string) int {
	if sameCleanedPath(baseDir, branchDir) || strings.TrimSpace(branchDir) == "" {
		return 0
	}
	if _, err := os.Stat(branchDir); err != nil {
		return 0
	}
	rep, err := m.diffWorkspaces(ctx, baseDir, branchDir, false)
	if err != nil {
		return 0
	}
	return rep.TotalChanges
}

// DiffBranchWorkspace computes added, modified, and deleted files (with unified diffs) in branchDir relative to baseDir.
func (m *Manager) DiffBranchWorkspace(ctx context.Context, baseDir, branchDir string) (WorkspaceDiffReport, error) {
	if sameCleanedPath(baseDir, branchDir) || strings.TrimSpace(branchDir) == "" {
		return WorkspaceDiffReport{Files: []FileDiff{}}, nil
	}
	return m.diffWorkspaces(ctx, baseDir, branchDir, true)
}

// ApplyBranchWorkspace applies all added, modified, and deleted files from branchDir into baseDir.
func (m *Manager) ApplyBranchWorkspace(ctx context.Context, baseDir, branchDir string) (MergeReport, error) {
	if sameCleanedPath(baseDir, branchDir) {
		return MergeReport{}, nil
	}
	return m.MergeBranchWorkspaces(ctx, baseDir, []string{branchDir}, baseDir)
}

func (m *Manager) diffWorkspaces(ctx context.Context, baseDir, branchDir string, includeDiff bool) (WorkspaceDiffReport, error) {
	out := WorkspaceDiffReport{Files: []FileDiff{}}
	baseFiles, err := snapshotDirFiles(baseDir)
	if err != nil {
		return out, err
	}
	branchFiles, err := snapshotDirFiles(branchDir)
	if err != nil {
		return out, err
	}

	pathSet := make(map[string]bool, len(baseFiles)+len(branchFiles))
	for rel := range baseFiles {
		pathSet[rel] = true
	}
	for rel := range branchFiles {
		pathSet[rel] = true
	}
	paths := make([]string, 0, len(pathSet))
	for rel := range pathSet {
		paths = append(paths, rel)
	}
	sort.Strings(paths)

	for _, rel := range paths {
		bSt := baseFiles[rel]
		wSt := branchFiles[rel]
		if bSt.exists == wSt.exists && bSt.hash == wSt.hash {
			continue
		}
		var status string
		switch {
		case !bSt.exists && wSt.exists:
			status = "added"
		case bSt.exists && !wSt.exists:
			status = "deleted"
		default:
			status = "modified"
		}
		fd := FileDiff{
			Path:   rel,
			Status: status,
		}
		if includeDiff {
			fd.UnifiedDiff = buildUnifiedFileDiff(ctx, rel, bSt, wSt)
		}
		out.Files = append(out.Files, fd)
	}
	out.TotalChanges = len(out.Files)
	return out, nil
}

func buildUnifiedFileDiff(ctx context.Context, rel string, baseSt, branchSt fileState) string {
	const maxDiffBytes = 256 * 1024
	if len(baseSt.content) > maxDiffBytes || len(branchSt.content) > maxDiffBytes {
		return fmt.Sprintf("(file exceeds %d KB diff preview limit)", maxDiffBytes/1024)
	}
	if bytes.IndexByte(baseSt.content, 0) >= 0 || bytes.IndexByte(branchSt.content, 0) >= 0 {
		return "(binary file changed)"
	}

	tmpDir, err := os.MkdirTemp("", "soma-diff-*")
	if err == nil {
		defer func() { _ = os.RemoveAll(tmpDir) }()
		aFile := filepath.Join(tmpDir, "a")
		bFile := filepath.Join(tmpDir, "b")
		_ = os.WriteFile(aFile, baseSt.content, 0o600)
		_ = os.WriteFile(bFile, branchSt.content, 0o600)
		cmd := exec.CommandContext(ctx, "git", "diff", "--no-index", "--no-color", "--unified=3", aFile, bFile)
		raw, _ := cmd.Output()
		if len(raw) > 0 {
			s := string(raw)
			s = strings.ReplaceAll(s, aFile, "a/"+rel)
			s = strings.ReplaceAll(s, bFile, "b/"+rel)
			lines := strings.Split(s, "\n")
			// Trim leading `diff --git` and `index ...` metadata lines for cleaner UI display.
			start := 0
			for start < len(lines) && (strings.HasPrefix(lines[start], "diff --git") || strings.HasPrefix(lines[start], "index ") || strings.HasPrefix(lines[start], "new file") || strings.HasPrefix(lines[start], "deleted file")) {
				start++
			}
			return strings.TrimSpace(strings.Join(lines[start:], "\n"))
		}
	}

	// Fallback line diff if git is unavailable.
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", rel, rel)
	if baseSt.exists {
		for _, ln := range strings.Split(strings.TrimSuffix(string(baseSt.content), "\n"), "\n") {
			fmt.Fprintf(&sb, "-%s\n", ln)
		}
	}
	if branchSt.exists {
		for _, ln := range strings.Split(strings.TrimSuffix(string(branchSt.content), "\n"), "\n") {
			fmt.Fprintf(&sb, "+%s\n", ln)
		}
	}
	return strings.TrimSpace(sb.String())
}
