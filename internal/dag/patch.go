package dag

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// HunkSpec represents a single search/replace block to apply to a file.
type HunkSpec struct {
	Search     string `json:"search"`
	Replace    string `json:"replace"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// ApplyFilePatch applies either SEARCH/REPLACE conflict-marker blocks, explicit HunkSpecs,
// or a unified diff to originalContent atomically. If any hunk fails to match, an error
// is returned and the caller never writes partial state.
func ApplyFilePatch(originalContent, patchText string, explicitHunks []HunkSpec) (string, int, error) {
	normOrig := strings.ReplaceAll(originalContent, "\r\n", "\n")
	useCRLF := strings.Contains(originalContent, "\r\n") && !strings.Contains(strings.ReplaceAll(originalContent, "\r\n", ""), "\n")

	hunks := append([]HunkSpec(nil), explicitHunks...)
	if strings.TrimSpace(patchText) != "" {
		parsed, err := parsePatchText(patchText)
		if err != nil {
			return "", 0, err
		}
		hunks = append(hunks, parsed...)
	}
	if len(hunks) == 0 {
		return "", 0, errors.New("fs_patch: no patch hunks provided (pass patch with SEARCH/REPLACE or unified diff, or search/replace fields)")
	}

	cur := normOrig
	for idx, h := range hunks {
		next, err := applySingleHunk(cur, h, idx+1)
		if err != nil {
			return "", idx, err
		}
		cur = next
	}

	if useCRLF {
		cur = strings.ReplaceAll(cur, "\n", "\r\n")
	}
	return cur, len(hunks), nil
}

func parsePatchText(raw string) ([]HunkSpec, error) {
	norm := strings.ReplaceAll(raw, "\r\n", "\n")

	// 1. Check for <<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE blocks.
	if strings.Contains(norm, "<<<<<<< SEARCH") && strings.Contains(norm, ">>>>>>> REPLACE") {
		var hunks []HunkSpec
		rem := norm
		for {
			sIdx := strings.Index(rem, "<<<<<<< SEARCH")
			if sIdx < 0 {
				break
			}
			afterStart := rem[sIdx+len("<<<<<<< SEARCH"):]
			if strings.HasPrefix(afterStart, "\n") {
				afterStart = afterStart[1:]
			}
			midIdx := strings.Index(afterStart, "\n=======\n")
			midLen := len("\n=======\n")
			if midIdx < 0 {
				if strings.HasPrefix(afterStart, "=======\n") {
					midIdx = 0
					midLen = len("=======\n")
				} else {
					return nil, errors.New("fs_patch: malformed SEARCH/REPLACE block (missing ======= separator)")
				}
			}
			searchPart := afterStart[:midIdx]
			afterMid := afterStart[midIdx+midLen:]
			endIdx := strings.Index(afterMid, "\n>>>>>>> REPLACE")
			if endIdx < 0 {
				if strings.HasPrefix(afterMid, ">>>>>>> REPLACE") {
					endIdx = 0
				} else {
					return nil, errors.New("fs_patch: malformed SEARCH/REPLACE block (missing >>>>>>> REPLACE footer)")
				}
			}
			replacePart := afterMid[:endIdx]
			hunks = append(hunks, HunkSpec{
				Search:  searchPart,
				Replace: replacePart,
			})
			rem = afterMid[endIdx+len(">>>>>>> REPLACE"):]
		}
		if len(hunks) == 0 {
			return nil, errors.New("fs_patch: no valid SEARCH/REPLACE blocks found")
		}
		return hunks, nil
	}

	// 2. Check for unified diff (@@ -start,count +start,count @@).
	if strings.Contains(norm, "@@") {
		return parseUnifiedDiffHunks(norm)
	}

	return nil, errors.New("fs_patch: unrecognized patch format (use <<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE or unified diff @@ hunks)")
}

func parseUnifiedDiffHunks(norm string) ([]HunkSpec, error) {
	lines := strings.Split(norm, "\n")
	var hunks []HunkSpec
	var inHunk bool
	var oldLines, newLines []string

	flush := func() {
		if inHunk && (len(oldLines) > 0 || len(newLines) > 0) {
			hunks = append(hunks, HunkSpec{
				Search:  strings.Join(oldLines, "\n"),
				Replace: strings.Join(newLines, "\n"),
			})
		}
		oldLines = nil
		newLines = nil
		inHunk = false
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index ") {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			flush()
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		if strings.HasPrefix(line, `\ No newline at end of file`) {
			continue
		}
		if line == "" {
			// Empty context line in unified diff.
			oldLines = append(oldLines, "")
			newLines = append(newLines, "")
			continue
		}
		switch line[0] {
		case ' ':
			oldLines = append(oldLines, line[1:])
			newLines = append(newLines, line[1:])
		case '-':
			oldLines = append(oldLines, line[1:])
		case '+':
			newLines = append(newLines, line[1:])
		default:
			oldLines = append(oldLines, line)
			newLines = append(newLines, line)
		}
	}
	flush()
	// Trim trailing empty context noise that split("\n") adds at end of diff text.
	for i := range hunks {
		for strings.HasSuffix(hunks[i].Search, "\n") && strings.HasSuffix(hunks[i].Replace, "\n") {
			hunks[i].Search = strings.TrimSuffix(hunks[i].Search, "\n")
			hunks[i].Replace = strings.TrimSuffix(hunks[i].Replace, "\n")
		}
	}
	if len(hunks) == 0 {
		return nil, errors.New("fs_patch: unified diff contained 0 applicable @@ hunks")
	}
	return hunks, nil
}

func applySingleHunk(content string, h HunkSpec, hunkNum int) (string, error) {
	search := strings.ReplaceAll(h.Search, "\r\n", "\n")
	replace := strings.ReplaceAll(h.Replace, "\r\n", "\n")
	if search == "" {
		return "", fmt.Errorf("fs_patch hunk #%d: empty search block", hunkNum)
	}

	// Tier 1: Exact substring match.
	count := strings.Count(content, search)
	if count == 1 || (count > 1 && h.ReplaceAll) {
		if h.ReplaceAll {
			return strings.ReplaceAll(content, search, replace), nil
		}
		return strings.Replace(content, search, replace, 1), nil
	}
	if count > 1 && !h.ReplaceAll {
		return "", fmt.Errorf("fs_patch hunk #%d: search block matched %d locations (include more surrounding context lines or set replace_all=true)", hunkNum, count)
	}

	// Tier 2: Line-by-line right-trimmed whitespace match.
	if patched, ok, multi := matchByLineTransform(content, search, replace, func(s string) string {
		return strings.TrimRight(s, " \t")
	}); ok {
		if multi && !h.ReplaceAll {
			return "", fmt.Errorf("fs_patch hunk #%d: trimmed-whitespace search matched multiple locations", hunkNum)
		}
		return patched, nil
	}

	// Tier 3: Indentation-insensitive fuzzy line match (strips both leading & trailing horizontal whitespace).
	if patched, ok, multi := matchByLineTransform(content, search, replace, func(s string) string {
		return strings.TrimSpace(s)
	}); ok {
		if multi && !h.ReplaceAll {
			return "", fmt.Errorf("fs_patch hunk #%d: fuzzy-indentation search matched multiple locations", hunkNum)
		}
		return patched, nil
	}

	preview := search
	if len(preview) > 120 {
		preview = preview[:117] + "..."
	}
	return "", fmt.Errorf("fs_patch hunk #%d failed to match target file (search=%s)", hunkNum, strconv.Quote(preview))
}

func matchByLineTransform(content, search, replace string, normFn func(string) string) (string, bool, bool) {
	cLines := strings.Split(content, "\n")
	sLines := strings.Split(search, "\n")
	if len(sLines) == 0 || len(sLines) > len(cLines) {
		return "", false, false
	}

	var matchIndices []int
	for i := 0; i+len(sLines) <= len(cLines); i++ {
		matched := true
		for j := 0; j < len(sLines); j++ {
			if normFn(cLines[i+j]) != normFn(sLines[j]) {
				matched = false
				break
			}
		}
		if matched {
			matchIndices = append(matchIndices, i)
		}
	}
	if len(matchIndices) == 0 {
		return "", false, false
	}

	firstIdx := matchIndices[0]
	rLines := strings.Split(replace, "\n")

	// Preserve leading indentation offset if Tier-3 indentation shift occurred on the first non-empty line.
	indentPrefix := ""
	for j := 0; j < len(sLines); j++ {
		if strings.TrimSpace(sLines[j]) != "" && strings.TrimSpace(cLines[firstIdx+j]) != "" {
			cIndent := leadingWhitespace(cLines[firstIdx+j])
			sIndent := leadingWhitespace(sLines[j])
			if len(cIndent) > len(sIndent) && strings.HasPrefix(cIndent, sIndent) {
				indentPrefix = cIndent[len(sIndent):]
			}
			break
		}
	}
	if indentPrefix != "" {
		for j := range rLines {
			if strings.TrimSpace(rLines[j]) != "" {
				rLines[j] = indentPrefix + rLines[j]
			}
		}
	}

	var outLines []string
	outLines = append(outLines, cLines[:firstIdx]...)
	outLines = append(outLines, rLines...)
	outLines = append(outLines, cLines[firstIdx+len(sLines):]...)
	return strings.Join(outLines, "\n"), true, len(matchIndices) > 1
}

func leadingWhitespace(s string) string {
	for i, r := range s {
		if r != ' ' && r != '\t' {
			return s[:i]
		}
	}
	return s
}
