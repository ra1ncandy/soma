package dag

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ra1ncandy/soma/internal/skills"
	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/triggers"
)

// cronjobManage lists, adds or removes cron triggers in the PROJECT's .soma/triggers.yaml
// (never a branch workspace copy, which the daemon does not read). It round-trips the full
// triggers.Config, so brakes, webhook secrets and other triggers are preserved, and it can only
// add or remove agent-created cron triggers, never operator-defined ones.
func (r *LocalToolRunner) cronjobManage(call ToolCall, args map[string]any) (ToolOutput, error) {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	somaDir := r.ProjectSomaDir
	if somaDir == "" {
		if call.WorkspaceDir == "" {
			return ToolOutput{}, errors.New("cronjob_manage: no project .soma directory configured")
		}
		somaDir = filepath.Join(call.WorkspaceDir, ".soma")
	}
	trigPath := filepath.Join(somaDir, "triggers.yaml")
	cfg, err := triggers.LoadOrDefault(trigPath)
	if err != nil {
		return ToolOutput{}, fmt.Errorf("cronjob_manage: %w", err)
	}
	out := func(msg string) (ToolOutput, error) {
		return ToolOutput{NameHint: "cron.txt", Content: []byte(msg), TrustTier: "trusted_repo"}, nil
	}

	switch action {
	case "list":
		var sb strings.Builder
		n := 0
		for _, tr := range cfg.Triggers {
			if tr.Type != "cron" {
				continue
			}
			n++
			fmt.Fprintf(&sb, "- [%s] schedule=%q enabled=%v created_by=%s prompt=%q\n", tr.ID, tr.Interval, tr.IsEnabled(), orDefault(tr.CreatedBy, "operator"), tr.Prompt)
		}
		if n == 0 {
			return out("cronjob_manage: 0 scheduled cron triggers")
		}
		return out(strings.TrimRight(sb.String(), "\n"))

	case "add":
		id, _ := args["id"].(string)
		schedule, _ := args["schedule"].(string)
		prompt, _ := args["prompt"].(string)
		id, schedule, prompt = strings.TrimSpace(id), strings.TrimSpace(schedule), strings.TrimSpace(prompt)
		if id == "" || schedule == "" || prompt == "" {
			return ToolOutput{}, errors.New("cronjob_manage add requires id, schedule, and prompt")
		}
		if _, err := triggers.ParseSchedule(schedule); err != nil {
			return ToolOutput{}, fmt.Errorf("cronjob_manage: invalid schedule %q: %w", schedule, err)
		}
		creator := "agent:" + call.RunID
		replaced := false
		for i := range cfg.Triggers {
			if cfg.Triggers[i].ID != id {
				continue
			}
			if !strings.HasPrefix(cfg.Triggers[i].CreatedBy, "agent:") || cfg.Triggers[i].Type != "cron" {
				return ToolOutput{}, fmt.Errorf("cronjob_manage: trigger %q is operator-defined and cannot be changed by the agent", id)
			}
			cfg.Triggers[i].Interval = schedule
			cfg.Triggers[i].Prompt = prompt
			cfg.Triggers[i].CreatedBy = creator
			replaced = true
			break
		}
		if !replaced {
			cfg.Triggers = append(cfg.Triggers, triggers.TriggerSpec{
				ID: id, Type: "cron", Interval: schedule, Prompt: prompt, CreatedBy: creator,
				// Agent-scheduled runs are unattended: keep them notify-only by default.
				Capability: "notify_only",
			})
		}
		if err := triggers.SaveConfig(trigPath, cfg); err != nil {
			return ToolOutput{}, err
		}
		if r.OnTriggersChanged != nil {
			r.OnTriggersChanged()
		}
		return out(fmt.Sprintf("cronjob_manage: saved cron trigger %q (%s) in the project's .soma/triggers.yaml", id, schedule))

	case "remove":
		id, _ := args["id"].(string)
		id = strings.TrimSpace(id)
		idx := -1
		for i, tr := range cfg.Triggers {
			if tr.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return ToolOutput{}, fmt.Errorf("cronjob_manage: trigger %q not found", id)
		}
		if !strings.HasPrefix(cfg.Triggers[idx].CreatedBy, "agent:") {
			return ToolOutput{}, fmt.Errorf("cronjob_manage: trigger %q is operator-defined and cannot be removed by the agent", id)
		}
		cfg.Triggers = append(cfg.Triggers[:idx], cfg.Triggers[idx+1:]...)
		if err := triggers.SaveConfig(trigPath, cfg); err != nil {
			return ToolOutput{}, err
		}
		if r.OnTriggersChanged != nil {
			r.OnTriggersChanged()
		}
		return out(fmt.Sprintf("cronjob_manage: removed trigger %q", id))

	default:
		return ToolOutput{}, fmt.Errorf("cronjob_manage: unsupported action %q (use list|add|remove)", action)
	}
}

// cronjobEffectClass: listing is local; scheduling future autonomous runs persists beyond this
// run, so it is gated like any external write once the run has seen untrusted content.
func cronjobEffectClass(argsJSON string) taint.EffectClass {
	if strings.Contains(strings.ReplaceAll(strings.ToLower(argsJSON), " ", ""), `"action":"list"`) {
		return taint.EffectReadOnly
	}
	return taint.EffectExternalWrite
}

// skillRoots returns the skill search path: explicit SkillRoots, else the shared default roots of
// the project (never the throwaway branch workspace copy).
func (r *LocalToolRunner) skillRoots(call ToolCall) []string {
	if len(r.SkillRoots) > 0 {
		return r.SkillRoots
	}
	if r.ProjectSomaDir != "" {
		return skills.DefaultRoots(filepath.Dir(r.ProjectSomaDir), r.ProjectSomaDir)
	}
	if call.WorkspaceDir != "" {
		return skills.DefaultRoots(call.WorkspaceDir, filepath.Join(call.WorkspaceDir, ".soma"))
	}
	return nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
