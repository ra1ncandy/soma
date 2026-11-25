package dag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/taint"
	"github.com/ra1ncandy/soma/internal/triggers"
)

// cronjob_manage must write the project's triggers.yaml (not the branch copy), keep every
// other field and trigger intact, and never touch operator-defined triggers.
func TestCronjobManagePreservesConfigAndOperatorTriggers(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	somaDir := filepath.Join(project, ".soma")
	branchWS := t.TempDir()
	trigPath := filepath.Join(somaDir, "triggers.yaml")
	if err := os.MkdirAll(somaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `version: 2
quiet_hours: "22:00-07:00"
max_pending_inbox: 5
daily_metered_budget_usd: 3.5
webhook_secret: s3cret
triggers:
  - id: ci
    type: webhook
    filter: event == "push"
    notify_channel: telegram
    notify_target: "42"
    urgency: urgent
`
	if err := os.WriteFile(trigPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := 0
	r := &LocalToolRunner{ProjectSomaDir: somaDir, OnTriggersChanged: func() { reloaded++ }}
	call := func(args string) (string, error) {
		out, err := r.Execute(ctx, ToolCall{RunID: "run_x", Name: "cronjob_manage", ArgsJSON: args, WorkspaceDir: branchWS})
		return string(out.Content), err
	}

	if _, err := call(`{"action":"add","id":"nightly","schedule":"0 3 * * *","prompt":"check"}`); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := call(`{"action":"add","id":"bad","schedule":"every full moon","prompt":"x"}`); err == nil {
		t.Fatal("invalid schedule accepted")
	}
	if _, err := call(`{"action":"remove","id":"ci"}`); err == nil {
		t.Fatal("agent removed an operator-defined trigger")
	}
	if _, err := call(`{"action":"add","id":"ci","schedule":"1h","prompt":"x"}`); err == nil {
		t.Fatal("agent overwrote an operator-defined trigger")
	}
	if _, err := os.Stat(filepath.Join(branchWS, ".soma", "triggers.yaml")); !os.IsNotExist(err) {
		t.Fatal("cronjob_manage wrote into the branch workspace copy")
	}

	cfg, err := triggers.LoadConfig(trigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuietHours != "22:00-07:00" || cfg.MaxPendingInbox != 5 || cfg.DailyMeteredBudgetUSD != 3.5 || cfg.WebhookSecret != "s3cret" {
		t.Fatalf("global brakes/secrets lost: %+v", cfg)
	}
	if len(cfg.Triggers) != 2 || cfg.Triggers[0].Filter == "" || cfg.Triggers[0].NotifyChannel != "telegram" || cfg.Triggers[0].Urgency != "urgent" {
		t.Fatalf("operator trigger fields lost: %+v", cfg.Triggers)
	}
	if cfg.Triggers[1].CreatedBy != "agent:run_x" || cfg.Triggers[1].Capability != "notify_only" {
		t.Fatalf("agent trigger metadata wrong: %+v", cfg.Triggers[1])
	}
	if st, _ := os.Stat(trigPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("triggers.yaml mode = %v, want 0600", st.Mode().Perm())
	}
	if list, _ := call(`{"action":"list"}`); !strings.Contains(list, "nightly") {
		t.Fatalf("list missing nightly: %s", list)
	}
	if _, err := call(`{"action":"remove","id":"nightly"}`); err != nil {
		t.Fatalf("remove own trigger: %v", err)
	}
	if reloaded != 2 {
		t.Fatalf("OnTriggersChanged called %d times, want 2", reloaded)
	}
	if cronjobEffectClass(`{"action": "list"}`) != taint.EffectReadOnly || cronjobEffectClass(`{"action":"add"}`) != taint.EffectExternalWrite {
		t.Fatal("cronjob effect classes wrong")
	}
}
