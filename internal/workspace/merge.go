package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

func sameState(a, b fileState) bool { return a.exists == b.exists && a.hash == b.hash }

type mergeOpts struct {
	// preferForkBase uses the fork-point manifest (merge back into the parent workspace) rather
	// than the project-base manifest (apply to the project).
	preferForkBase bool
	// inferDeletes lets a workspace with no recorded base treat "present in base, absent in
	// worker" as a deletion. Never set when the target is the operator's project directory.
	inferDeletes bool
}

// MergeBranchWorkspaces merges changes from concurrent workerDirs into targetDir. Each worker's
// changes are computed against the manifest recorded when its workspace was created (falling back
// to baseDir for workspaces without one). Disjoint file edits apply directly; concurrent edits to
// the same file use 3-way line merging (`git merge-file`), failing closed with ErrMergeConflict on
// overlapping line edits or when the target changed underneath a worker with no common base.
func (m *Manager) MergeBranchWorkspaces(
	ctx context.Context,
	baseDir string,
	workerDirs []string,
	targetDir string,
) (MergeReport, error) {
	return m.mergeWorkspaces(ctx, baseDir, workerDirs, targetDir, mergeOpts{preferForkBase: true, inferDeletes: true})
}

func (m *Manager) mergeWorkspaces(
	ctx context.Context,
	baseDir string,
	workerDirs []string,
	targetDir string,
	opts mergeOpts,
) (MergeReport, error) {
	var rep MergeReport
	if len(workerDirs) == 0 {
		return rep, nil
	}

	baseFiles, err := snapshotDirFiles(baseDir)
	if err != nil {
		return rep, fmt.Errorf("snapshot base workspace: %w", err)
	}

	// merged tracks the accumulated target state for every path a worker touched.
	merged := make(map[string]fileState)
	current := func(rel string) fileState {
		if st, ok := merged[rel]; ok {
			return st
		}
		return baseFiles[rel]
	}

	for wIdx, wDir := range workerDirs {
		if strings.TrimSpace(wDir) == "" {
			continue
		}
		var (
			wBase    map[string]fileState
			wChanged map[string]fileState
			wDeleted []string
			legacy   bool
		)
		manifest, ok := map[string]manifestEntry(nil), false
		if opts.preferForkBase {
			manifest, ok = readManifest(forkManifestPath(wDir))
		}
		if !ok {
			manifest, ok = readManifest(baseManifestPath(wDir))
		}
		if ok {
			wBase = manifestAsStates(manifest)
			wChanged, wDeleted, err = branchChanges(wDir, manifest, true)
			if err != nil {
				return rep, fmt.Errorf("scan worker %d workspace: %w", wIdx, err)
			}
		} else {
			// No recorded starting state: fall back to comparing against the live base.
			legacy = true
			wBase = baseFiles
			wFiles, snapErr := snapshotDirFiles(wDir)
			if snapErr != nil {
				return rep, fmt.Errorf("snapshot worker %d workspace: %w", wIdx, snapErr)
			}
			wChanged = make(map[string]fileState)
			for rel, st := range wFiles {
				if !sameState(baseFiles[rel], st) {
					wChanged[rel] = st
				}
			}
			if opts.inferDeletes {
				for rel := range baseFiles {
					if _, present := wFiles[rel]; !present {
						wDeleted = append(wDeleted, rel)
					}
				}
			}
		}

		paths := make([]string, 0, len(wChanged)+len(wDeleted))
		for rel := range wChanged {
			paths = append(paths, rel)
		}
		paths = append(paths, wDeleted...)
		sort.Strings(paths)

		for _, rel := range paths {
			workSt := wChanged[rel] // zero value (exists=false) for deletions
			startSt := wBase[rel]
			curSt := current(rel)

			// Target already equals the worker's result (same change made elsewhere).
			if sameState(curSt, workSt) {
				continue
			}
			// Target is still what the worker started from: take the worker's version.
			if sameState(curSt, startSt) {
				merged[rel] = workSt
				continue
			}
			// Both sides diverged from the worker's starting point.
			if !curSt.exists || !workSt.exists {
				rep.Conflicts = append(rep.Conflicts, rel)
				continue
			}
			// A 3-way merge needs the common ancestor's bytes, which we only have while the
			// original base file is still unchanged on disk.
			ancestor := baseFiles[rel]
			if !legacy && !sameState(ancestor, startSt) {
				rep.Conflicts = append(rep.Conflicts, rel)
				continue
			}
			mergedBytes, ok := threeWayMergeBytes(ctx, ancestor.content, curSt.content, workSt.content)
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
		if sameState(baseSt, finalSt) {
			continue
		}
		if err := refuseSymlinkTraversal(targetDir, rel); err != nil {
			return rep, err
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

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func snapshotDirFiles(root string) (map[string]fileState, error) {
	out := make(map[string]fileState)
	err := walkWorkspaceFiles(root, func(abs, rel string, d fs.DirEntry) error {
		raw, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if fInfo, err := d.Info(); err == nil {
			mode = fInfo.Mode().Perm()
		}
		out[rel] = fileState{exists: true, hash: sha256HexBytes(raw), content: raw, mode: mode}
		return nil
	})
	return out, err
}

// branchChanges returns the files in branchDir that differ from manifest and the manifest paths
// no longer present. Files whose size and mtime still match the manifest are not re-read, so an
// untouched workspace costs one stat per file.
func branchChanges(branchDir string, manifest map[string]manifestEntry, withContent bool) (map[string]fileState, []string, error) {
	changed := make(map[string]fileState)
	seen := make(map[string]bool, len(manifest))
	err := walkWorkspaceFiles(branchDir, func(abs, rel string, d fs.DirEntry) error {
		seen[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry, known := manifest[rel]
		if known && info.Size() == entry.Size && info.ModTime().UnixNano() == entry.MTime {
			return nil
		}
		st := fileState{exists: true, mode: info.Mode().Perm()}
		if withContent {
			raw, err := os.ReadFile(abs)
			if err != nil {
				return err
			}
			st.content = raw
			st.hash = sha256HexBytes(raw)
		} else {
			if st.hash, err = hashFile(abs); err != nil {
				return err
			}
		}
		if known && st.hash == entry.Hash {
			return nil
		}
		changed[rel] = st
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	var deleted []string
	for rel := range manifest {
		if !seen[rel] {
			deleted = append(deleted, rel)
		}
	}
	sort.Strings(deleted)
	return changed, deleted, nil
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

// CountWorkspaceChanges returns the number of files the branch changed since it was created.
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

// DiffBranchWorkspace computes the files the branch added, modified, or deleted since it was
// created, with unified diffs rendered against the project's current content.
func (m *Manager) DiffBranchWorkspace(ctx context.Context, baseDir, branchDir string) (WorkspaceDiffReport, error) {
	if sameCleanedPath(baseDir, branchDir) || strings.TrimSpace(branchDir) == "" {
		return WorkspaceDiffReport{Files: []FileDiff{}}, nil
	}
	return m.diffWorkspaces(ctx, baseDir, branchDir, true)
}

// ApplyBranchWorkspace applies the branch's own changes into baseDir. Files the branch never
// touched are left alone, and a file changed on both sides fails with ErrMergeConflict rather
// than overwriting the operator's edit.
func (m *Manager) ApplyBranchWorkspace(ctx context.Context, baseDir, branchDir string) (MergeReport, error) {
	if sameCleanedPath(baseDir, branchDir) {
		return MergeReport{}, nil
	}
	rep, err := m.mergeWorkspaces(ctx, baseDir, []string{branchDir}, baseDir, mergeOpts{})
	if err != nil {
		return rep, err
	}
	// The project now contains the branch's changes; the branch's new starting point is its current state.
	if _, ok := readManifest(baseManifestPath(branchDir)); ok {
		_ = recordBaseManifest(branchDir)
	}
	return rep, nil
}

func readProjectFile(baseDir, rel string) fileState {
	p := filepath.Join(baseDir, filepath.FromSlash(rel))
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return fileState{}
	}
	const maxPreview = 512 * 1024
	if info.Size() > maxPreview {
		return fileState{exists: true, content: make([]byte, maxPreview+1)}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return fileState{}
	}
	return fileState{exists: true, content: raw, hash: sha256HexBytes(raw)}
}

func (m *Manager) diffWorkspaces(ctx context.Context, baseDir, branchDir string, includeDiff bool) (WorkspaceDiffReport, error) {
	out := WorkspaceDiffReport{Files: []FileDiff{}}

	manifest, ok := readManifest(baseManifestPath(branchDir))
	if !ok {
		return m.legacyDiffWorkspaces(ctx, baseDir, branchDir, includeDiff)
	}
	changed, deleted, err := branchChanges(branchDir, manifest, includeDiff)
	if err != nil {
		return out, err
	}
	paths := make([]string, 0, len(changed)+len(deleted))
	for rel := range changed {
		paths = append(paths, rel)
	}
	paths = append(paths, deleted...)
	sort.Strings(paths)

	for _, rel := range paths {
		wSt, present := changed[rel]
		_, wasInBase := manifest[rel]
		status := "modified"
		switch {
		case !present:
			status = "deleted"
		case !wasInBase:
			status = "added"
		}
		fd := FileDiff{Path: rel, Status: status}
		if includeDiff {
			fd.UnifiedDiff = buildUnifiedFileDiff(ctx, rel, readProjectFile(baseDir, rel), wSt)
		}
		out.Files = append(out.Files, fd)
	}
	out.TotalChanges = len(out.Files)
	return out, nil
}

// legacyDiffWorkspaces handles workspaces created before base manifests existed. Without a
// recorded starting state a file missing from the branch cannot be told apart from one that was
// never copied, so deletions are not reported (and never applied).
func (m *Manager) legacyDiffWorkspaces(ctx context.Context, baseDir, branchDir string, includeDiff bool) (WorkspaceDiffReport, error) {
	out := WorkspaceDiffReport{Files: []FileDiff{}}
	baseFiles, err := snapshotDirFiles(baseDir)
	if err != nil {
		return out, err
	}
	branchFiles, err := snapshotDirFiles(branchDir)
	if err != nil {
		return out, err
	}
	paths := make([]string, 0, len(branchFiles))
	for rel := range branchFiles {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		bSt := baseFiles[rel]
		wSt := branchFiles[rel]
		if sameState(bSt, wSt) {
			continue
		}
		status := "modified"
		if !bSt.exists {
			status = "added"
		}
		fd := FileDiff{Path: rel, Status: status}
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
