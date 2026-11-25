package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// DefaultRoots is the single skill search path shared by the agent, the prompt compiler, the
// CLI, the TUI, the dashboard and the MCP server. Earlier roots take precedence.
func DefaultRoots(projectDir, somaDir string) []string {
	var roots []string
	if somaDir != "" {
		roots = append(roots, filepath.Join(somaDir, "skills"))
	}
	if projectDir != "" {
		roots = append(roots, filepath.Join(projectDir, ".agents", "skills"), filepath.Join(projectDir, ".skills"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".soma", "skills"))
	}
	return roots
}

// ValidateSkillContent normalises frontmatter and runs AAIF validation plus the security lint on
// content without touching any skill directory. It returns the content that would be written.
func ValidateSkillContent(name, content string) (Skill, []LintFinding, string, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		content = SyncTemplateFrontmatterName(content, name)
	}
	if !aaifNameRe.MatchString(name) || len(name) > 64 {
		return Skill{}, nil, "", fmt.Errorf("%w: invalid skill name %q (must match ^[a-z0-9]+(-[a-z0-9]+)*$)", ErrInvalidSkill, name)
	}
	stageParent, err := os.MkdirTemp("", "soma-skill-stage-*")
	if err != nil {
		return Skill{}, nil, "", err
	}
	defer func() { _ = os.RemoveAll(stageParent) }()
	stageDir := filepath.Join(stageParent, name)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return Skill{}, nil, "", err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		return Skill{}, nil, "", err
	}
	sk, err := ParseSkillMD(stageDir)
	if err != nil {
		return Skill{}, nil, "", err
	}
	findings, err := LintSkillDir(stageDir)
	if err != nil {
		return Skill{}, nil, "", err
	}
	for _, f := range findings {
		if f.Severity == "critical" {
			return Skill{}, findings, "", fmt.Errorf("%w: %s (%s:%d)", ErrSkillLintBlocked, f.Detail, f.File, f.Line)
		}
	}
	return sk, findings, content, nil
}

// skillProposal is the staged change behind a skill_patch inbox item.
type skillProposal struct {
	Name     string `json:"name"`
	Root     string `json:"root"`
	RunID    string `json:"run_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Proposed int64  `json:"proposed_unix"`
}

func proposalDir(st *store.Store, id string) string {
	return filepath.Join(st.SomaDir(), "skill_proposals", id)
}

// ProposeSkillChange validates a new or updated SKILL.md and stages it for operator review. The
// active skill on disk is never modified; ApplySkillProposal does that once the inbox item is
// approved. It returns the inbox item ID.
func ProposeSkillChange(ctx context.Context, st *store.Store, root, name, content, reason, runID string) (string, error) {
	if st == nil {
		return "", errors.New("skills: no store to stage the proposal in")
	}
	sk, _, normalized, err := ValidateSkillContent(name, content)
	if err != nil {
		return "", err
	}
	name = sk.Frontmatter.Name
	id := fmt.Sprintf("skp_%x_%s", time.Now().UnixNano(), name)
	dir := proposalDir(st, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(normalized), 0o600); err != nil {
		return "", err
	}
	meta, _ := json.Marshal(skillProposal{Name: name, Root: root, RunID: runID, Reason: reason, Proposed: time.Now().Unix()})
	if err := os.WriteFile(filepath.Join(dir, "proposal.json"), meta, 0o600); err != nil {
		return "", err
	}
	verb := "Create"
	if prev, err := os.ReadFile(filepath.Join(root, name, "SKILL.md")); err == nil {
		verb = "Update"
		if string(prev) == normalized {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("skills: proposed content for %q is identical to the active skill", name)
		}
	}
	itemID := "inbox_" + id
	err = st.InsertInboxItem(ctx, store.InboxItem{
		ID:          itemID,
		RunID:       runID,
		ItemKind:    "skill_patch",
		Title:       fmt.Sprintf("%s skill %s", verb, name),
		Summary:     fmt.Sprintf("%s\n\n%s", strings.TrimSpace(reason), previewText(normalized, 40)),
		ArtifactRef: id,
		Urgency:     "normal",
	})
	if err != nil {
		return "", err
	}
	return itemID, nil
}

// ApplySkillProposal writes an approved staged proposal to its skill root, keeping the previous
// SKILL.md under <skill>/.history/ so every agent-proposed change is reversible.
func ApplySkillProposal(st *store.Store, proposalID string) (Skill, error) {
	dir := proposalDir(st, proposalID)
	raw, err := os.ReadFile(filepath.Join(dir, "proposal.json"))
	if err != nil {
		return Skill{}, fmt.Errorf("skills: proposal %s has no staged content: %w", proposalID, err)
	}
	var p skillProposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return Skill{}, fmt.Errorf("skills: corrupt proposal %s: %w", proposalID, err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, err
	}
	// Re-validate: lint rules may have changed since the proposal was staged.
	sk, _, normalized, err := ValidateSkillContent(p.Name, string(content))
	if err != nil {
		return Skill{}, err
	}
	target := filepath.Join(p.Root, p.Name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return Skill{}, err
	}
	activePath := filepath.Join(target, "SKILL.md")
	if prev, err := os.ReadFile(activePath); err == nil {
		hist := filepath.Join(target, ".history")
		if err := os.MkdirAll(hist, 0o755); err != nil {
			return Skill{}, err
		}
		if err := os.WriteFile(filepath.Join(hist, fmt.Sprintf("SKILL.%d.md", time.Now().UnixNano())), prev, 0o644); err != nil {
			return Skill{}, err
		}
	}
	tmp := activePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(normalized), 0o644); err != nil {
		return Skill{}, err
	}
	if err := os.Rename(tmp, activePath); err != nil {
		return Skill{}, err
	}
	_ = os.RemoveAll(dir)
	sk.Dir = target
	return sk, nil
}

// DiscardSkillProposal removes the staged content of a dismissed proposal.
func DiscardSkillProposal(st *store.Store, proposalID string) {
	if st == nil || proposalID == "" || strings.ContainsAny(proposalID, `/\`) {
		return
	}
	_ = os.RemoveAll(proposalDir(st, proposalID))
}

func previewText(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n… (%d more lines)", len(lines)-maxLines)
}
