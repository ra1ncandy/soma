package dag

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/skills"
)

// Project instruction files loaded into Layer 0, in order. All present files are loaded (unlike
// Hermes, which takes only the first match); identical content (e.g. CLAUDE.md symlinked to
// AGENTS.md) is included once. A persona file sets the agent's voice for this project.
var contextFileNames = []string{"AGENTS.md", "CLAUDE.md", ".cursorrules", filepath.Join(".soma", "persona.md")}

const (
	maxContextFileBytes  = 20_000
	maxContextTotalBytes = 48_000
)

// LoadProjectInstructions reads the project's instruction files for Layer 0. Each file is
// security-linted with the skill rules; a file with a critical finding is replaced by a BLOCKED
// marker so the operator sees why it was ignored and injected text never reaches the model.
// Files are frozen per epoch, so edits take effect at the next epoch (L0/L1 immutability).
func LoadProjectInstructions(projectDir string) []compiler.ContextFile {
	if projectDir == "" {
		return nil
	}
	var out []compiler.ContextFile
	seen := map[[32]byte]bool{}
	total := 0
	for _, name := range contextFileNames {
		raw, err := os.ReadFile(filepath.Join(projectDir, name))
		if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		sum := sha256.Sum256(raw)
		if seen[sum] {
			continue
		}
		seen[sum] = true
		text := string(raw)
		if !utf8.ValidString(text) {
			continue
		}
		for _, f := range skills.LintText(name, text) {
			if f.Severity == "critical" {
				text = fmt.Sprintf("[BLOCKED: %s ignored by soma: %s at line %d]", name, f.Detail, f.Line)
				break
			}
		}
		text = truncateHeadTail(text, maxContextFileBytes)
		if total+len(text) > maxContextTotalBytes {
			text = truncateHeadTail(text, maxContextTotalBytes-total)
		}
		if len(text) == 0 {
			break
		}
		total += len(text)
		out = append(out, compiler.ContextFile{Path: filepath.ToSlash(name), Content: text})
	}
	return out
}

// truncateHeadTail keeps the first 70% and last 20% of s when it exceeds limit bytes.
func truncateHeadTail(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	head := limit * 7 / 10
	tail := limit * 2 / 10
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n… [%d bytes truncated] …\n", start-head) + s[start:]
}

// skillIndexFor returns the Layer 0 skill index for a project.
func skillIndexFor(projectDir, somaDir string) []compiler.SkillMeta {
	idx, err := skills.LoadIndex(skills.DefaultRoots(projectDir, somaDir))
	if err != nil {
		return nil
	}
	return idx
}
