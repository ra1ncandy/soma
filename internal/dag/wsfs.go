package dag

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxToolReadBytes caps how much of one file a host-side fs tool will load into memory.
const maxToolReadBytes = 32 << 20

// The fs_* tools run in the soma process on the host, not inside the OS sandbox, while the model
// can create arbitrary symlinks in the workspace through exec. Every host-side file access
// therefore goes through os.Root, which refuses any path that leaves the workspace directory
// (via "..", an absolute target, or a symlink), with no window between check and use.

// wsRelPath validates a model-supplied path and returns it cleaned and slash-free relative to the
// workspace. write selects the stricter rules for mutating tools.
func wsRelPath(wsDir, rel string, write bool) (string, error) {
	if wsDir == "" {
		return "", errors.New("workspace dir is empty")
	}
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to workspace: %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace boundary: %q", rel)
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		// .soma holds kernel state and credentials when the workspace is the project directory.
		if part == ".soma" {
			return "", fmt.Errorf("path %q is inside .soma, which is reserved for the kernel", rel)
		}
		// Writing under .git (hooks, config) would run code on the operator's next git command.
		if write && part == ".git" {
			return "", fmt.Errorf("path %q is inside .git, which workspace tools may not modify", rel)
		}
	}
	return clean, nil
}

func openWorkspaceRoot(wsDir string) (*os.Root, error) {
	if wsDir == "" {
		return nil, errors.New("workspace dir is empty")
	}
	return os.OpenRoot(wsDir)
}

// wsReadFile reads a workspace file without following links out of the workspace.
func wsReadFile(wsDir, rel string) ([]byte, error) {
	clean, err := wsRelPath(wsDir, rel, false)
	if err != nil {
		return nil, err
	}
	root, err := openWorkspaceRoot(wsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return readRootFile(root, clean, rel)
}

func readRootFile(root *os.Root, clean, display string) ([]byte, error) {
	f, err := root.Open(clean)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory (use fs_list)", display)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", display)
	}
	if info.Size() > maxToolReadBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d MiB read limit (use fs_grep, or exec with head/tail/sed)", display, info.Size(), maxToolReadBytes>>20)
	}
	return io.ReadAll(io.LimitReader(f, maxToolReadBytes+1))
}

// wsWriteFile writes a workspace file, creating parent directories, without following links out
// of the workspace.
func wsWriteFile(wsDir, rel string, data []byte) error {
	clean, err := wsRelPath(wsDir, rel, true)
	if err != nil {
		return err
	}
	root, err := openWorkspaceRoot(wsDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if dir := filepath.Dir(clean); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	perm := os.FileMode(0o644)
	if info, sErr := root.Stat(clean); sErr == nil && info.Mode().IsRegular() {
		perm = info.Mode().Perm() // keep an existing file's mode (e.g. executable scripts)
	}
	return root.WriteFile(clean, data, perm)
}

// resolveInsideWorkspace returns the absolute path of rel inside wsDir for callers that only need
// a location (directory walks). It rejects lexical escapes and paths whose real location, after
// resolving symlinks, lies outside the workspace. File contents must be read through wsReadFile.
func resolveInsideWorkspace(wsDir, rel string) (string, error) {
	clean, err := wsRelPath(wsDir, rel, false)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(wsDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absRoot, clean)
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", err
	}
	if realTarget, err := filepath.EvalSymlinks(target); err == nil {
		if realTarget != realRoot && !strings.HasPrefix(realTarget, realRoot+string(filepath.Separator)) {
			return "", fmt.Errorf("path escapes workspace boundary: %q", rel)
		}
	}
	return target, nil
}
