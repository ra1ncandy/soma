package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/ra1ncandy/soma/internal/store"
)

// checkpointFile is one file in a content-addressed workspace checkpoint.
type checkpointFile struct {
	Blob  string `json:"b"` // blob-store hash of the file content
	Mode  uint32 `json:"m"`
	Size  int64  `json:"s"`
	MTime int64  `json:"t"`
}

// Checkpoint limits: very large files are recorded without content, and a workspace with more
// files than MaxCheckpointFiles is not checkpointed at all (the run continues without one).
const (
	MaxCheckpointFileBytes = 25 << 20
	MaxCheckpointFiles     = 50_000
)

// ErrCheckpointTooLarge reports a workspace too big to checkpoint.
var ErrCheckpointTooLarge = errors.New("workspace: too many files to checkpoint")

// CheckpointManifest maps workspace-relative slash paths to file content.
type CheckpointManifest map[string]checkpointFile

// LoadCheckpoint reads a checkpoint manifest from the blob store.
func LoadCheckpoint(blobs *store.BlobStore, hash string) (CheckpointManifest, error) {
	raw, err := blobs.Get(hash)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint %s: %w", hash, err)
	}
	var m CheckpointManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode checkpoint %s: %w", hash, err)
	}
	return m, nil
}

// SnapshotWorkspace stores every workspace file (same set as diff/merge: no .git, .soma or
// dependency caches) in the blob store and returns the manifest hash. Files whose size and mtime
// match prev are not re-read, so a checkpoint after a small edit costs one directory walk plus
// the changed files. Identical content is stored once.
func SnapshotWorkspace(blobs *store.BlobStore, wsDir string, prev CheckpointManifest) (string, CheckpointManifest, error) {
	if blobs == nil {
		return "", nil, errors.New("workspace: no blob store for checkpoint")
	}
	m := CheckpointManifest{}
	err := walkWorkspaceFiles(wsDir, func(abs, rel string, d fs.DirEntry) error {
		if len(m) >= MaxCheckpointFiles {
			return ErrCheckpointTooLarge
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxCheckpointFileBytes {
			// Recorded without content: restore leaves such files as they are.
			m[rel] = checkpointFile{Mode: uint32(info.Mode().Perm()), Size: info.Size(), MTime: info.ModTime().UnixNano()}
			return nil
		}
		if old, ok := prev[rel]; ok && old.Size == info.Size() && old.MTime == info.ModTime().UnixNano() && old.Mode == uint32(info.Mode().Perm()) {
			m[rel] = old
			return nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		h, _, err := blobs.Put(data)
		if err != nil {
			return err
		}
		m[rel] = checkpointFile{Blob: h, Mode: uint32(info.Mode().Perm()), Size: info.Size(), MTime: info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("checkpoint workspace: %w", err)
	}
	raw, err := json.Marshal(m) // map keys marshal sorted: identical trees give identical hashes
	if err != nil {
		return "", nil, err
	}
	h, _, err := blobs.Put(raw)
	if err != nil {
		return "", nil, err
	}
	return h, m, nil
}

// RestoreReport lists what RestoreWorkspace changed.
type RestoreReport struct {
	Written []string `json:"written,omitempty"`
	Deleted []string `json:"deleted,omitempty"`
}

// RestoreWorkspace makes wsDir match a checkpoint: files that differ are rewritten, files the
// checkpoint does not contain are deleted. .git, .soma and dependency caches are never touched,
// and writes never pass through symlinks.
func RestoreWorkspace(blobs *store.BlobStore, wsDir, manifestHash string) (RestoreReport, error) {
	var rep RestoreReport
	m, err := LoadCheckpoint(blobs, manifestHash)
	if err != nil {
		return rep, err
	}
	current := map[string]bool{}
	err = walkWorkspaceFiles(wsDir, func(abs, rel string, d fs.DirEntry) error {
		current[rel] = true
		if _, keep := m[rel]; !keep {
			if err := os.Remove(abs); err != nil {
				return err
			}
			rep.Deleted = append(rep.Deleted, rel)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	paths := make([]string, 0, len(m))
	for rel := range m {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		f := m[rel]
		if f.Blob == "" {
			continue // oversized file recorded without content
		}
		target := filepath.Join(wsDir, filepath.FromSlash(rel))
		if current[rel] {
			// Blob hashes are the SHA-256 of the content, so an unchanged file is skipped cheaply.
			if h, err := hashFile(target); err == nil && h == f.Blob {
				continue
			}
		}
		if err := refuseSymlinkTraversal(wsDir, rel); err != nil {
			return rep, err
		}
		data, err := blobs.Get(f.Blob)
		if err != nil {
			return rep, fmt.Errorf("restore %s: %w", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return rep, err
		}
		mode := fs.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			return rep, err
		}
		_ = os.Chmod(target, mode)
		rep.Written = append(rep.Written, rel)
	}
	return rep, nil
}
