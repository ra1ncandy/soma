package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/store"
)

var (
	// ErrInvalidSkill indicates non-compliance with the AAIF Agent Skills specification.
	ErrInvalidSkill = errors.New("skills: invalid AAIF SKILL.md specification")
	// ErrSkillLintBlocked indicates pre-activation security lint found critical threats.
	ErrSkillLintBlocked = errors.New("skills: blocked by pre-activation security lint")
	aaifNameRe          = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// Frontmatter represents AAIF SKILL.md YAML frontmatter fields.
type Frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	AllowedTools  string            `yaml:"allowed-tools,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
}

// Skill represents a parsed AAIF skill package.
type Skill struct {
	Dir         string
	Frontmatter Frontmatter
	Body        string
}

// ParseSkillMD reads and strictly validates <dir>/SKILL.md against AAIF rules.
func ParseSkillMD(dir string) (Skill, error) {
	skillFile := filepath.Join(dir, "SKILL.md")
	rawBytes, err := os.ReadFile(skillFile)
	if err != nil {
		return Skill{}, fmt.Errorf("read SKILL.md: %w", err)
	}
	text := strings.ReplaceAll(string(rawBytes), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Skill{}, fmt.Errorf("%w: missing YAML frontmatter opener", ErrInvalidSkill)
	}
	rest := text[len("---\n"):]
	idx := strings.Index(rest, "\n---\n")
	sepLen := len("\n---\n")
	if idx == -1 {
		if strings.HasSuffix(rest, "\n---") {
			idx = len(rest) - len("\n---")
			sepLen = len("\n---")
		} else {
			return Skill{}, fmt.Errorf("%w: missing YAML frontmatter closer", ErrInvalidSkill)
		}
	}

	yamlPart := rest[:idx]
	bodyPart := strings.TrimSpace(rest[idx+sepLen:])

	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(yamlPart), &fm); err != nil {
		return Skill{}, fmt.Errorf("%w: parse YAML frontmatter: %v", ErrInvalidSkill, err)
	}
	if len(fm.Name) < 1 || len(fm.Name) > 64 || !aaifNameRe.MatchString(fm.Name) {
		return Skill{}, fmt.Errorf("%w: invalid name %q (must be 1-64 lowercase alphanumeric/single-hyphen chars)", ErrInvalidSkill, fm.Name)
	}
	folderName := filepath.Base(filepath.Clean(dir))
	if fm.Name != folderName {
		return Skill{}, fmt.Errorf("%w: frontmatter name %q must match folder name %q", ErrInvalidSkill, fm.Name, folderName)
	}
	if len(fm.Description) < 1 || len(fm.Description) > 1024 {
		return Skill{}, fmt.Errorf("%w: description must be 1-1024 chars", ErrInvalidSkill)
	}
	if len(fm.Compatibility) > 500 {
		return Skill{}, fmt.Errorf("%w: compatibility exceeds 500 chars", ErrInvalidSkill)
	}

	return Skill{
		Dir:         dir,
		Frontmatter: fm,
		Body:        bodyPart,
	}, nil
}

// LoadIndex performs Stage-1 progressive loading: validates and indexes name + description for Layer 0.
func LoadIndex(roots []string) ([]compiler.SkillMeta, error) {
	seen := make(map[string]compiler.SkillMeta)
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			sk, err := ParseSkillMD(filepath.Join(root, ent.Name()))
			if err != nil {
				continue
			}
			seen[sk.Frontmatter.Name] = compiler.SkillMeta{
				Name:        sk.Frontmatter.Name,
				Description: sk.Frontmatter.Description,
			}
		}
	}
	out := make([]compiler.SkillMeta, 0, len(seen))
	for _, meta := range seen {
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ActivateBody performs Stage-2 loading after running pre-activation security lint.
// Refuses activation if any critical security finding is detected.
func ActivateBody(skillDir string) (compiler.SkillBody, []LintFinding, error) {
	sk, err := ParseSkillMD(skillDir)
	if err != nil {
		return compiler.SkillBody{}, nil, err
	}
	findings, err := LintSkillDir(skillDir)
	if err != nil {
		return compiler.SkillBody{}, nil, err
	}
	for _, f := range findings {
		if f.Severity == "critical" {
			return compiler.SkillBody{}, findings, fmt.Errorf("%w: %s (%s)", ErrSkillLintBlocked, f.RuleID, f.File)
		}
	}
	return compiler.SkillBody{
		Name: sk.Frontmatter.Name,
		Body: sk.Body,
	}, findings, nil
}

// LoadAsset performs Stage-3 on-demand loading of scripts/, references/, or assets/ safely.
func LoadAsset(skillDir, relPath string) ([]byte, error) {
	cleanRel := filepath.Clean(relPath)
	if filepath.IsAbs(cleanRel) || strings.HasPrefix(cleanRel, "..") {
		return nil, fmt.Errorf("skills: invalid asset path %q", relPath)
	}
	top := strings.Split(filepath.ToSlash(cleanRel), "/")[0]
	if top != "scripts" && top != "references" && top != "assets" {
		return nil, fmt.Errorf("skills: asset %q must reside under scripts/, references/, or assets/", relPath)
	}
	absSkill, err := filepath.Abs(skillDir)
	if err != nil {
		return nil, err
	}
	target := filepath.Clean(filepath.Join(absSkill, cleanRel))
	if !strings.HasPrefix(target, absSkill+string(filepath.Separator)) {
		return nil, fmt.Errorf("skills: path escape %q", relPath)
	}
	return os.ReadFile(target)
}

// ProposeSkillPatch stages a reviewable skill modification in inbox_items instead of mutating SKILL.md on disk.
func ProposeSkillPatch(ctx context.Context, st *store.Store, skillName, unifiedDiff, reason string) error {
	return st.InsertInboxItem(ctx, store.InboxItem{
		ID:          fmt.Sprintf("inbox_skill_%d_%s", time.Now().UnixNano(), skillName),
		ItemKind:    "skill_patch",
		Title:       fmt.Sprintf("Review proposed patch for skill %s", skillName),
		Summary:     fmt.Sprintf("%s\n\n%s", reason, unifiedDiff),
		ArtifactRef: skillName,
		Urgency:     "normal",
	})
}
