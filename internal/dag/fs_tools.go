package dag

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// heavyDir reports dependency/cache directories that listings summarise and searches skip.
func heavyDir(name string) bool {
	switch name {
	case "node_modules", "__pycache__", ".venv", ".pytest_cache":
		return true
	}
	return false
}

// rootStart validates rel and returns the slash path to start a walk from inside the workspace root.
func rootStart(wsDir, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" || rel == "." {
		return ".", nil
	}
	clean, err := wsRelPath(wsDir, rel, false)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(clean), nil
}

func (r *LocalToolRunner) toolFSList(call ToolCall, args map[string]any) (ToolOutput, error) {
	rel, _ := args["path"].(string)
	start, err := rootStart(call.WorkspaceDir, rel)
	if err != nil {
		return ToolOutput{}, err
	}
	root, err := openWorkspaceRoot(call.WorkspaceDir)
	if err != nil {
		return ToolOutput{}, err
	}
	defer func() { _ = root.Close() }()

	const maxEntries = 2000
	var paths []string
	truncated := false
	err = fs.WalkDir(root.FS(), start, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == start {
				return walkErr
			}
			return nil // unreadable subtree: keep listing the rest
		}
		if p == start {
			return nil
		}
		name := d.Name()
		if name == ".git" || name == ".soma" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if heavyDir(name) {
				paths = append(paths, p+"/ (contents not listed)")
				return fs.SkipDir
			}
			return nil
		}
		if len(paths) >= maxEntries {
			truncated = true
			return fs.SkipAll
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return ToolOutput{}, err
	}
	out := strings.Join(paths, "\n")
	if truncated {
		out += fmt.Sprintf("\n... (listing truncated at %d entries; pass a subdirectory path to see more)", maxEntries)
	}
	return ToolOutput{NameHint: "fs_list.txt", Content: []byte(out), TrustTier: "trusted_repo"}, nil
}

func (r *LocalToolRunner) toolFSGrep(call ToolCall, args map[string]any) (ToolOutput, error) {
	pattern, _ := args["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return ToolOutput{}, errors.New("fs_grep requires non-empty pattern")
	}
	ignoreCase, _ := args["ignore_case"].(bool)
	ctxF, _ := args["context_lines"].(float64)
	ctxLines := int(ctxF)
	if ctxLines < 0 {
		ctxLines = 0
	}
	if ctxLines > 5 {
		ctxLines = 5
	}
	expr := pattern
	if ignoreCase && !strings.HasPrefix(expr, "(?i)") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("fs_grep invalid pattern: %w", err)
	}
	rel, _ := args["path"].(string)
	start, err := rootStart(call.WorkspaceDir, rel)
	if err != nil {
		return ToolOutput{}, err
	}
	globPat, _ := args["glob"].(string)
	globPat = strings.TrimSpace(globPat)

	root, err := openWorkspaceRoot(call.WorkspaceDir)
	if err != nil {
		return ToolOutput{}, err
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	const (
		maxMatches      = 80
		maxFileBytes    = 8 << 20 // 8 MiB per file cap
		maxLineChars    = 400
		maxScannedFiles = 5000
	)
	var outLines []string
	totalMatches := 0
	matchedFiles := 0
	scannedFiles := 0
	capped := false

	clip := func(line []byte) string {
		if len(line) > maxLineChars {
			return string(line[:maxLineChars-3]) + "..."
		}
		return string(line)
	}

	searchFile := func(p string, d fs.DirEntry) error {
		if !d.Type().IsRegular() {
			return nil // never follow symlinks or read special files
		}
		if globPat != "" {
			mBase, _ := path.Match(globPat, path.Base(p))
			mFull, _ := path.Match(globPat, p)
			if !mBase && !mFull {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileBytes {
			return nil
		}
		if scannedFiles >= maxScannedFiles {
			capped = true
			return fs.SkipAll
		}
		scannedFiles++
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		// Skip binary files (NUL byte in first 4 KB).
		sample := raw
		if len(sample) > 4096 {
			sample = sample[:4096]
		}
		if bytes.IndexByte(sample, 0) >= 0 {
			return nil
		}
		lines := bytes.Split(raw, []byte("\n"))
		var fileMatchIdxs []int
		for i, line := range lines {
			line = bytes.TrimSuffix(line, []byte("\r"))
			lines[i] = line
			if !re.Match(line) {
				continue
			}
			if totalMatches >= maxMatches {
				capped = true
				break
			}
			totalMatches++
			fileMatchIdxs = append(fileMatchIdxs, i)
		}
		if len(fileMatchIdxs) > 0 {
			matchedFiles++
			if ctxLines == 0 {
				for _, idx := range fileMatchIdxs {
					outLines = append(outLines, fmt.Sprintf("%s:L%d: %s", p, idx+1, clip(lines[idx])))
				}
			} else {
				mSet := make(map[int]bool, len(fileMatchIdxs))
				for _, idx := range fileMatchIdxs {
					mSet[idx] = true
				}
				lastEmitted := -2
				for _, idx := range fileMatchIdxs {
					lo := max(idx-ctxLines, 0)
					hi := min(idx+ctxLines, len(lines)-1)
					if lastEmitted >= 0 && lo > lastEmitted+1 {
						outLines = append(outLines, "--")
					}
					for j := lo; j <= hi; j++ {
						if j <= lastEmitted {
							continue
						}
						sep := "-"
						if mSet[j] {
							sep = ":"
						}
						outLines = append(outLines, fmt.Sprintf("%s:L%d%s %s", p, j+1, sep, clip(lines[j])))
						lastEmitted = j
					}
				}
			}
		}
		if capped {
			return fs.SkipAll // enough matches: stop reading the rest of the tree
		}
		return nil
	}

	err = fs.WalkDir(fsys, start, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == start {
				return walkErr
			}
			return nil
		}
		name := d.Name()
		if p != start && (name == ".git" || name == ".soma" || (d.IsDir() && heavyDir(name))) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		return searchFile(p, d)
	})
	if err != nil {
		return ToolOutput{}, err
	}
	if totalMatches == 0 {
		return ToolOutput{
			NameHint:  "fs_grep.txt",
			Content:   []byte(fmt.Sprintf("fs_grep: 0 matches for %q across %d file(s)", pattern, scannedFiles)),
			TrustTier: "trusted_repo",
		}, nil
	}
	header := fmt.Sprintf("fs_grep: %d match(es) in %d file(s) for %q:", totalMatches, matchedFiles, pattern)
	if capped {
		header = fmt.Sprintf("fs_grep: showing the first %d match(es) in %d file(s) for %q (search stopped early; narrow the pattern, path, or glob for the rest):", totalMatches, matchedFiles, pattern)
	}
	return ToolOutput{
		NameHint:  "fs_grep.txt",
		Content:   []byte(header + "\n" + strings.Join(outLines, "\n")),
		TrustTier: "trusted_repo",
	}, nil
}

func (r *LocalToolRunner) toolFSRead(call ToolCall, args map[string]any) (ToolOutput, error) {
	rel, _ := args["path"].(string)
	data, err := wsReadFile(call.WorkspaceDir, rel)
	if err != nil {
		return ToolOutput{}, err
	}
	startF, hasStart := args["start_line"].(float64)
	endF, hasEnd := args["end_line"].(float64)
	if !hasStart && !hasEnd {
		return ToolOutput{NameHint: rel, Content: data, TrustTier: "trusted_repo"}, nil
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	startLine := max(int(startF), 1)
	endLine := int(endF)
	if endLine < startLine {
		endLine = startLine + 199
	}
	const maxReadRange = 400
	if endLine-startLine+1 > maxReadRange {
		endLine = startLine + maxReadRange - 1
	}
	if startLine > len(lines) {
		return ToolOutput{
			NameHint:  rel,
			Content:   []byte(fmt.Sprintf("%s: start_line %d exceeds line_count %d", rel, startLine, len(lines))),
			TrustTier: "trusted_repo",
		}, nil
	}
	endLine = min(endLine, len(lines))
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s lines %d..%d of %d:\n", rel, startLine, endLine, len(lines))
	for i := startLine; i <= endLine; i++ {
		fmt.Fprintf(&sb, "L%d: %s\n", i, lines[i-1])
	}
	return ToolOutput{NameHint: rel, Content: []byte(strings.TrimRight(sb.String(), "\n")), TrustTier: "trusted_repo"}, nil
}

func (r *LocalToolRunner) toolFSWrite(call ToolCall, args map[string]any) (ToolOutput, error) {
	rel, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if err := wsWriteFile(call.WorkspaceDir, rel, []byte(content)); err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{
		NameHint:  rel,
		Content:   []byte(fmt.Sprintf("wrote %d bytes to %s", len(content), rel)),
		TrustTier: "trusted_repo",
	}, nil
}

func (r *LocalToolRunner) toolFSPatch(call ToolCall, args map[string]any) (ToolOutput, error) {
	rel, _ := args["path"].(string)
	if _, err := wsRelPath(call.WorkspaceDir, rel, true); err != nil {
		return ToolOutput{}, err
	}
	origBytes, err := wsReadFile(call.WorkspaceDir, rel)
	if err != nil {
		return ToolOutput{}, err
	}
	patchText, _ := args["patch"].(string)
	if patchText == "" {
		patchText, _ = args["unified_diff"].(string)
	}
	var explicit []HunkSpec
	if sVal, ok := args["search"].(string); ok && sVal != "" {
		rVal, _ := args["replace"].(string)
		repAll, _ := args["replace_all"].(bool)
		explicit = append(explicit, HunkSpec{Search: sVal, Replace: rVal, ReplaceAll: repAll})
	}
	patched, appliedCount, err := ApplyFilePatch(string(origBytes), patchText, explicit)
	if err != nil {
		return ToolOutput{}, err
	}
	if err := wsWriteFile(call.WorkspaceDir, rel, []byte(patched)); err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{
		NameHint:  rel,
		Content:   []byte(fmt.Sprintf("patched %s (%d hunk(s) applied, %d bytes)", rel, appliedCount, len(patched))),
		TrustTier: "trusted_repo",
	}, nil
}
