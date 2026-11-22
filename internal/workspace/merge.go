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
		if d.IsDir() {
			if base == ".soma" || base == ".git" {
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
