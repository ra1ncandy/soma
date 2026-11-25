package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// manifestEntry records one file of a branch workspace as it was when the workspace was created.
type manifestEntry struct {
	Hash  string `json:"h"`
	Mode  uint32 `json:"m"`
	Size  int64  `json:"s"`
	MTime int64  `json:"t"`
}

// baseManifestPath is the record of what the branch started from relative to the project. Diff and
// Apply compare the branch against it, so files that merely never existed in the workspace
// (gitignored build output, files the operator created meanwhile) are not mistaken for deletions.
func baseManifestPath(wsDir string) string { return filepath.Clean(wsDir) + ".base.json" }

// forkManifestPath records a forked/swarm workspace as it was at the fork point, which is the
// common ancestor when merging the fork back into its parent workspace.
func forkManifestPath(wsDir string) string { return filepath.Clean(wsDir) + ".fork.json" }

// skipWorkspaceDir reports directories that are never part of a diff, merge, or manifest.
func skipWorkspaceDir(base string) bool {
	switch base {
	case "__pycache__", ".pytest_cache", "node_modules", ".venv":
		return true
	}
	return false
}

// walkWorkspaceFiles visits every regular file under root that participates in diff/merge.
func walkWorkspaceFiles(root string, fn func(abs, relSlash string, d fs.DirEntry) error) error {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
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
			if skipWorkspaceDir(base) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, sockets, devices never cross the workspace boundary
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return fn(p, filepath.ToSlash(rel), d)
	})
}

func buildManifest(wsDir string) (map[string]manifestEntry, error) {
	out := make(map[string]manifestEntry)
	err := walkWorkspaceFiles(wsDir, func(abs, rel string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			return err
		}
		hash, err := hashFile(abs)
		if err != nil {
			return err
		}
		out[rel] = manifestEntry{Hash: hash, Mode: uint32(info.Mode().Perm()), Size: info.Size(), MTime: info.ModTime().UnixNano()}
		return nil
	})
	return out, err
}

func writeManifest(path string, m map[string]manifestEntry) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readManifest(path string) (map[string]manifestEntry, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var m map[string]manifestEntry
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// recordBaseManifest snapshots wsDir as the branch's starting state.
func recordBaseManifest(wsDir string) error {
	m, err := buildManifest(wsDir)
	if err != nil {
		return fmt.Errorf("build workspace base manifest: %w", err)
	}
	return writeManifest(baseManifestPath(wsDir), m)
}

func manifestAsStates(m map[string]manifestEntry) map[string]fileState {
	out := make(map[string]fileState, len(m))
	for rel, e := range m {
		out[rel] = fileState{exists: true, hash: e.Hash, mode: fs.FileMode(e.Mode)}
	}
	return out
}

// refuseSymlinkTraversal fails when writing targetDir/rel would pass through (or replace) a
// symlink that lives in the target tree, which could redirect the write outside it.
func refuseSymlinkTraversal(targetDir, rel string) error {
	cur := filepath.Clean(targetDir)
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace: refusing to write through symlink %s", cur)
		}
	}
	return nil
}
