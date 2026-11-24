package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ra1ncandy/soma/internal/daemon"
	"github.com/ra1ncandy/soma/internal/mcp"
	"github.com/ra1ncandy/soma/internal/memory"
	"github.com/ra1ncandy/soma/internal/skills"
	"github.com/ra1ncandy/soma/internal/store"
)

// HermesConfig is a partial representation of ~/.hermes/config.yaml.
type HermesConfig struct {
	Model      string                        `yaml:"model,omitempty"`
	APIBase    string                        `yaml:"api_base,omitempty"`
	BaseURL    string                        `yaml:"base_url,omitempty"`
	MCPServers map[string]HermesMCPServerCfg `yaml:"mcp_servers,omitempty"`
}

// HermesMCPServerCfg mirrors an entry under mcp_servers in ~/.hermes/config.yaml.
type HermesMCPServerCfg struct {
	Enabled        *bool             `yaml:"enabled,omitempty"`
	Transport      string            `yaml:"transport,omitempty"`
	URL            string            `yaml:"url,omitempty"`
	AuthToken      string            `yaml:"auth_token,omitempty"`
	Headers        map[string]string `yaml:"headers,omitempty"`
	Command        string            `yaml:"command,omitempty"`
	Args           []string          `yaml:"args,omitempty"`
	Env            map[string]string `yaml:"env,omitempty"`
	IncludeTools   []string          `yaml:"include_tools,omitempty"`
	ExcludeTools   []string          `yaml:"exclude_tools,omitempty"`
	TimeoutSeconds int               `yaml:"timeout_seconds,omitempty"`
	OAuth          *mcp.OAuthConfig  `yaml:"oauth,omitempty"`
}

// HermesCronJob mirrors an entry in ~/.hermes/cron/jobs.json.
type HermesCronJob struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	Interval string `json:"interval,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// ImportOptions configures a migration from ~/.hermes/ into a soma workspace.
type ImportOptions struct {
	HermesDir    string
	WorkspaceDir string
	DryRun       bool
}

// ImportReport summarizes everything imported (or blocked by security lint) from ~/.hermes/.
type ImportReport struct {
	HermesDir        string               `json:"hermes_dir"`
	Model            string               `json:"model,omitempty"`
	APIBase          string               `json:"api_base,omitempty"`
	MCPServers       []mcp.ServerEndpoint `json:"mcp_servers,omitempty"`
	MemoriesImported int                  `json:"memories_imported"`
	SkillsImported   []string             `json:"skills_imported,omitempty"`
	SkillsBlocked    []string             `json:"skills_blocked,omitempty"`
	CronJobsImported int                  `json:"cron_jobs_imported"`
}

// DefaultHermesDir returns ~/.hermes on the current host.
func DefaultHermesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hermes"
	}
	return filepath.Join(home, ".hermes")
}

// Import migrates config.yaml, memories/{MEMORY.md,USER.md}, skills/*/SKILL.md (security-linted),
// and cron/jobs.json from opts.HermesDir into opts.WorkspaceDir and st.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (ImportReport, error) {
	hDir := strings.TrimSpace(opts.HermesDir)
	if hDir == "" {
		hDir = DefaultHermesDir()
	}
	rep := ImportReport{HermesDir: hDir}

	// 1. Parse ~/.hermes/config.yaml (models + MCP servers; never copies raw secrets).
	cfgPath := filepath.Join(hDir, "config.yaml")
	if raw, err := os.ReadFile(cfgPath); err == nil {
		var hc HermesConfig
		if yaml.Unmarshal(raw, &hc) == nil {
			rep.Model = strings.TrimSpace(hc.Model)
			rep.APIBase = strings.TrimSpace(hc.APIBase)
			if rep.APIBase == "" {
				rep.APIBase = strings.TrimSpace(hc.BaseURL)
			}
			for name, sc := range hc.MCPServers {
				rep.MCPServers = append(rep.MCPServers, mcp.ServerEndpoint{
					Name:           name,
					Transport:      sc.Transport,
					URL:            sc.URL,
					AuthToken:      sc.AuthToken,
					Headers:        sc.Headers,
					Command:        sc.Command,
					Args:           sc.Args,
					Env:            sc.Env,
					IncludeTools:   sc.IncludeTools,
					ExcludeTools:   sc.ExcludeTools,
					TimeoutSeconds: sc.TimeoutSeconds,
					OAuth:          sc.OAuth,
					Enabled:        sc.Enabled,
				})
			}
		}
	}

	// 2. Import MEMORY.md and USER.md into bi-temporal memory graph (global scope).
	memFiles := []struct {
		rel  string
		kind string
	}{
		{filepath.Join("memories", "MEMORY.md"), "fact"},
		{filepath.Join("memories", "USER.md"), "preference"},
		{"MEMORY.md", "fact"},
		{"USER.md", "preference"},
	}
	seenEntries := make(map[string]bool)
	var memSvc *memory.Service
	if st != nil {
		memSvc = memory.NewService(st)
	}
	for _, mf := range memFiles {
		raw, err := os.ReadFile(filepath.Join(hDir, mf.rel))
		if err != nil {
			continue
		}
		for _, entry := range parseMarkdownMemoryEntries(string(raw)) {
			if seenEntries[entry] {
				continue
			}
			seenEntries[entry] = true
			if !opts.DryRun && memSvc != nil {
				_, _ = memSvc.Propose(ctx, memory.ProposeRequest{
					Scope:     "global",
					Kind:      mf.kind,
					Content:   entry,
					TrustTier: "trusted_operator",
				})
			}
			rep.MemoriesImported++
		}
	}

	// 3. Import skills/*/SKILL.md through soma's pre-activation security linter.
	skillsSrc := filepath.Join(hDir, "skills")
	skillsDst := filepath.Join(opts.WorkspaceDir, ".soma", "skills")
	if entries, err := os.ReadDir(skillsSrc); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			srcDir := filepath.Join(skillsSrc, ent.Name())
			rawSkill, err := os.ReadFile(filepath.Join(srcDir, "SKILL.md"))
			if err != nil {
				continue
			}
			findings, _ := skills.LintSkillDir(srcDir)
			blocked := false
			for _, f := range findings {
				if f.Severity == "critical" {
					blocked = true
					break
				}
			}
			if blocked {
				rep.SkillsBlocked = append(rep.SkillsBlocked, ent.Name())
				continue
			}
			if !opts.DryRun && opts.WorkspaceDir != "" {
				if _, _, err := skills.CreateOrPatchSkill(skillsDst, ent.Name(), string(rawSkill)); err != nil {
					rep.SkillsBlocked = append(rep.SkillsBlocked, fmt.Sprintf("%s (%v)", ent.Name(), err))
					continue
				}
			}
			rep.SkillsImported = append(rep.SkillsImported, ent.Name())
		}
	}

	// 4. Import cron/jobs.json into .soma/triggers.yaml.
	cronPath := filepath.Join(hDir, "cron", "jobs.json")
	if raw, err := os.ReadFile(cronPath); err == nil {
		var jobs []HermesCronJob
		if json.Unmarshal(raw, &jobs) != nil {
			var wrapped struct {
				Jobs []HermesCronJob `json:"jobs"`
			}
			_ = json.Unmarshal(raw, &wrapped)
			jobs = wrapped.Jobs
		}
		if len(jobs) > 0 {
			trigPath := filepath.Join(opts.WorkspaceDir, ".soma", "triggers.yaml")
			dCfg := daemon.DefaultConfig()
			if existing, err := daemon.LoadConfig(trigPath); err == nil {
				dCfg = existing
			}
			existingIDs := make(map[string]bool)
			for _, t := range dCfg.Triggers {
				existingIDs[t.ID] = true
			}
			for i, j := range jobs {
				if j.Enabled != nil && !*j.Enabled {
					continue
				}
				id := strings.TrimSpace(j.ID)
				if id == "" {
					id = fmt.Sprintf("hermes-cron-%d", i+1)
				}
				sched := strings.TrimSpace(j.Interval)
				if sched == "" {
					sched = strings.TrimSpace(j.Schedule)
				}
				if sched == "" || strings.TrimSpace(j.Prompt) == "" {
					continue
				}
				if !existingIDs[id] {
					dCfg.Triggers = append(dCfg.Triggers, daemon.TriggerSpec{
						ID:       id,
						Type:     "cron",
						Interval: sched,
						Prompt:   strings.TrimSpace(j.Prompt),
					})
					existingIDs[id] = true
					rep.CronJobsImported++
				}
			}
			if !opts.DryRun && rep.CronJobsImported > 0 && opts.WorkspaceDir != "" {
				_ = os.MkdirAll(filepath.Dir(trigPath), 0o755)
				if outBytes, err := yaml.Marshal(&dCfg); err == nil {
					_ = os.WriteFile(trigPath, outBytes, 0o644)
				}
			}
		}
	}

	return rep, nil
}

// parseMarkdownMemoryEntries splits a Hermes MEMORY.md / USER.md into concise non-empty bullet/paragraph facts.
func parseMarkdownMemoryEntries(md string) []string {
	var out []string
	for _, rawLine := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || line == "---" {
			continue
		}
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		line = strings.TrimSpace(line)
		if len(line) >= 3 {
			out = append(out, line)
		}
	}
	return out
}
