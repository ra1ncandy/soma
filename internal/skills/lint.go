package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LintFinding represents a single security issue flagged inside a skill package.
type LintFinding struct {
	RuleID   string
	Severity string // critical | warning
	File     string
	Line     int
	Detail   string
}

type lintRule struct {
	id       string
	severity string
	re       *regexp.Regexp
	detail   string
}

var securityLintRules = []lintRule{
	{
		id:       "prompt_injection_override",
		severity: "critical",
		re:       regexp.MustCompile(`(?i)(ignore\s+(all\s+)?(previous|prior)\s+instructions|disregard\s+system\s+prompt|you\s+are\s+now\s+in\s+developer\s+mode|<\s*/?\s*system\s*>)`),
		detail:   "Prompt-injection instruction override phrase detected",
	},
	{
		id:       "credential_harvesting",
		severity: "critical",
		re:       regexp.MustCompile(`(~\/\.ssh|\/\.ssh\/id_rsa|\/etc\/shadow|AWS_SECRET_ACCESS_KEY|OPENAI_API_KEY|ANTHROPIC_API_KEY|os\.environ|printenv)`),
		detail:   "Potential credential or secret harvesting reference detected",
	},
	{
		id:       "obfuscated_pipe_exec",
		severity: "critical",
		re:       regexp.MustCompile(`(?i)(curl\s+[^\n|]*\|\s*(sh|bash)|wget\s+[^\n|]*\|\s*(sh|bash)|base64\s+-d\s*\|\s*(sh|bash)|eval\s+\$\()`),
		detail:   "Obfuscated remote shell execution pattern detected",
	},
	{
		id:       "undeclared_exfiltration",
		severity: "critical",
		re:       regexp.MustCompile(`(?i)\bexfiltrate\b`),
		detail:   "Explicit exfiltration instruction detected",
	},
}

// LintSkillDir scans SKILL.md and all files under scripts/ inside skillDir.
func LintSkillDir(skillDir string) ([]LintFinding, error) {
	var findings []LintFinding

	skillMD := filepath.Join(skillDir, "SKILL.md")
	fMD, err := lintFile(skillMD, "SKILL.md")
	if err != nil {
		return nil, err
	}
	findings = append(findings, fMD...)

	scriptsDir := filepath.Join(skillDir, "scripts")
	if info, err := os.Stat(scriptsDir); err == nil && info.IsDir() {
		err = filepath.WalkDir(scriptsDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(skillDir, path)
			fsFound, err := lintFile(path, filepath.ToSlash(rel))
			if err != nil {
				return err
			}
			findings = append(findings, fsFound...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return findings, nil
}

func lintFile(absPath, relLabel string) ([]LintFinding, error) {
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	return LintText(relLabel, string(raw)), nil
}

// LintText applies the security lint rules to arbitrary instruction text (skill bodies,
// context files such as AGENTS.md, persona files).
func LintText(relLabel, text string) []LintFinding {
	var findings []LintFinding

	// Check hidden Unicode zero-width and BiDi directional override characters.
	for i, r := range text {
		if isHiddenControlRune(r) {
			findings = append(findings, LintFinding{
				RuleID:   "hidden_unicode_control",
				Severity: "critical",
				File:     relLabel,
				Line:     strings.Count(text[:i], "\n") + 1,
				Detail:   fmt.Sprintf("Hidden zero-width/directional Unicode character U+%04X detected", r),
			})
			break
		}
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for idx, line := range lines {
		for _, rule := range securityLintRules {
			if rule.re.MatchString(line) {
				findings = append(findings, LintFinding{
					RuleID:   rule.id,
					Severity: rule.severity,
					File:     relLabel,
					Line:     idx + 1,
					Detail:   rule.detail,
				})
			}
		}
	}
	return findings
}

func isHiddenControlRune(r rune) bool {
	return (r >= 0x200B && r <= 0x200F) ||
		(r >= 0x202A && r <= 0x202E) ||
		(r >= 0x2066 && r <= 0x2069) ||
		r == 0xFEFF
}
