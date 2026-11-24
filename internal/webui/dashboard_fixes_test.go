package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/store"
)

func newMockOpenAIServer(t *testing.T, onModel func(string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if onModel != nil {
			onModel(req.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":    "chatcmpl-test",
			"model": req.Model,
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "Completed via " + req.Model,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     12,
				"completion_tokens": 6,
				"total_tokens":      18,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestServer_ProviderRestartPersistenceAndRoleRouting(t *testing.T) {
	var mu sync.Mutex
	var calledModels []string
	mockLLM := newMockOpenAIServer(t, func(model string) {
		mu.Lock()
		calledModels = append(calledModels, model)
		mu.Unlock()
	})
	defer mockLLM.Close()

	ws := t.TempDir()
	st, err := store.Open(filepath.Join(ws, ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// Boot 1: No provider configured -> starts in read-only mode.
	srv1, err := NewServer(st, ServerConfig{
		WorkspaceDir: ws,
		Addr:         "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(srv1.Handler())

	post1 := func(path string, body any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts1.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Soma-CSRF", srv1.CSRFToken())
		req.Header.Set("Origin", ts1.URL)
		r, dErr := ts1.Client().Do(req)
		if dErr != nil {
			t.Fatal(dErr)
		}
		defer func() { _ = r.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// Add two role-tagged accounts via the dashboard UI API.
	code, _ := post1("/api/harness/accounts", map[string]any{
		"ID":            "acc-worker",
		"Provider":      "openai_compat",
		"ModelID":       "worker-fast-v1",
		"BillingMode":   "local",
		"MaxConcurrent": 2,
		"api_base":      mockLLM.URL + "/v1",
		"api_key":       "sk-worker",
		"role":          "worker",
		"set_active":    true,
	})
	if code != http.StatusOK {
		t.Fatalf("upsert worker account failed: %d", code)
	}
	code, _ = post1("/api/harness/accounts", map[string]any{
		"ID":            "acc-frontier",
		"Provider":      "openai_compat",
		"ModelID":       "frontier-deep-v2",
		"BillingMode":   "local",
		"MaxConcurrent": 2,
		"api_base":      mockLLM.URL + "/v1",
		"api_key":       "sk-frontier",
		"role":          "frontier",
		"set_active":    false,
	})
	if code != http.StatusOK {
		t.Fatalf("upsert frontier account failed: %d", code)
	}
	ts1.Close()

	// Boot 2: Fresh NewServer with Provider == nil & UseMock == false must auto-restore from .soma/harness.json!
	srv2, err := NewServer(st, ServerConfig{
		WorkspaceDir: ws,
		Addr:         "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	resp, err := ts2.Client().Get(ts2.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state StateDTO
	_ = json.NewDecoder(resp.Body).Decode(&state)
	_ = resp.Body.Close()
	if state.ProviderMode != "live" {
		t.Fatalf("expected provider_mode='live' after server restart, got %q", state.ProviderMode)
	}

	post2 := func(path string, body any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts2.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Soma-CSRF", srv2.CSRFToken())
		req.Header.Set("Origin", ts2.URL)
		r, dErr := ts2.Client().Do(req)
		if dErr != nil {
			t.Fatal(dErr)
		}
		defer func() { _ = r.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// Run with profile="frontier" -> must route to acc-frontier ("frontier-deep-v2"), not just active default!
	code, runOut := post2("/api/runs", map[string]any{
		"goal":    "Deep architectural review",
		"profile": "frontier",
	})
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/runs failed: %d %+v", code, runOut)
	}
	branchID := runOut["branch_id"].(string)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := ts2.Client().Get(ts2.URL + "/api/state")
		_ = json.NewDecoder(r.Body).Decode(&state)
		_ = r.Body.Close()
		settled := false
		for _, run := range state.Runs {
			for _, br := range run.Branches {
				if br.ID == branchID && br.Status == "completed" {
					settled = true
				}
			}
		}
		if settled {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	mu.Lock()
	modelsSnap := append([]string(nil), calledModels...)
	mu.Unlock()
	if len(modelsSnap) == 0 || modelsSnap[0] != "frontier-deep-v2" {
		t.Fatalf("expected role router to dispatch frontier-deep-v2, got %v", modelsSnap)
	}
}

func TestServer_WorkspaceDiffApplyDirectAndRunLifecycle(t *testing.T) {
	ctx := context.Background()
	srv, st, ws := newTestWebServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	postJSON := func(path string, body any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Soma-CSRF", srv.CSRFToken())
		req.Header.Set("Origin", ts.URL)
		r, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	waitBranch := func(branchID string) BranchDTO {
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			r, err := ts.Client().Get(ts.URL + "/api/state")
			if err != nil {
				t.Fatal(err)
			}
			var s StateDTO
			_ = json.NewDecoder(r.Body).Decode(&s)
			_ = r.Body.Close()
			for _, run := range s.Runs {
				for _, br := range run.Branches {
					if br.ID == branchID && br.Status == "completed" {
						return br
					}
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for branch %s", branchID)
		return BranchDTO{}
	}

	// 1. Isolated run writes artifacts/soma_mock_note.txt inside snapshot, NOT root ws.
	code, startOut := postJSON("/api/runs", map[string]any{"goal": "Write isolated file"})
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/runs: %d", code)
	}
	runID := startOut["run_id"].(string)
	branchID := startOut["branch_id"].(string)
	br := waitBranch(branchID)

	if br.ChangedFilesCount == 0 {
		t.Fatalf("expected ChangedFilesCount > 0 on isolated branch, got %d", br.ChangedFilesCount)
	}
	if _, err := os.Stat(filepath.Join(ws, "artifacts", "soma_mock_note.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected isolated edit not yet in root workspace")
	}

	// 2. GET /api/branches/{id}/diff returns unified diff for soma_mock_note.txt.
	diffResp, err := ts.Client().Get(ts.URL + "/api/branches/" + branchID + "/diff")
	if err != nil || diffResp.StatusCode != http.StatusOK {
		t.Fatalf("GET diff failed: resp=%v err=%v", diffResp, err)
	}
	var diffOut map[string]any
	_ = json.NewDecoder(diffResp.Body).Decode(&diffOut)
	_ = diffResp.Body.Close()
	files, _ := diffOut["files"].([]any)
	if len(files) == 0 {
		t.Fatalf("expected changed files in diff response: %+v", diffOut)
	}

	// 3. POST /api/branches/{id}/apply copies file into root ws and resets ChangedFilesCount to 0.
	code, appOut := postJSON("/api/branches/"+branchID+"/apply", map[string]any{})
	if code != http.StatusOK || appOut["ok"] != true {
		t.Fatalf("POST apply failed: code=%d out=%+v", code, appOut)
	}
	if _, err := os.Stat(filepath.Join(ws, "artifacts", "soma_mock_note.txt")); err != nil {
		t.Fatalf("expected soma_mock_note.txt applied into root ws: %v", err)
	}

	// 4. Rename run & Delete run (even with a memory_node referencing ProvenanceStepID on the branch).
	stepID := ""
	if len(br.Steps) > 0 {
		stepID = br.Steps[0].ID
	}
	if err := st.InsertMemoryNode(ctx, store.MemoryNode{
		ID:               "M-prov-ref",
		Scope:            "global",
		Kind:             "fact",
		Content:          "Derived from run",
		Status:           "active",
		TrustTier:        "user_confirmed",
		ProvenanceStepID: stepID,
		Enabled:          true,
	}); err != nil {
		t.Fatalf("InsertMemoryNode with provenance: %v", err)
	}

	code, _ = postJSON("/api/runs/"+runID+"/rename", map[string]any{"title": "Renamed Task Title"})
	if code != http.StatusOK {
		t.Fatalf("POST rename failed: %d", code)
	}
	renamedRun, err := st.GetRun(ctx, runID)
	if err != nil || renamedRun.Title != "Renamed Task Title" {
		t.Fatalf("expected renamed title, got %+v err=%v", renamedRun, err)
	}

	code, _ = postJSON("/api/runs/"+runID+"/delete", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("POST delete failed: %d", code)
	}
	if _, err := st.GetRun(ctx, runID); err == nil {
		t.Fatal("expected run deleted from store")
	}
	// Memory node survives with detached provenance!
	if memNode, mErr := st.GetMemoryNode(ctx, "M-prov-ref"); mErr != nil || memNode.ProvenanceStepID != "" {
		t.Fatalf("expected memory node preserved with detached ProvenanceStepID, got %+v err=%v", memNode, mErr)
	}

	// 5. Switch workspace_strategy to "direct" in Settings and verify task writes directly to ws.
	code, _ = postJSON("/api/harness/provider", map[string]any{
		"provider_type":      "local",
		"model_id":           "mock-worker-v1",
		"workspace_strategy": "direct",
		"sandbox_mode":       "auto",
	})
	if code != http.StatusOK {
		t.Fatalf("save direct strategy failed: %d", code)
	}
	_ = os.RemoveAll(filepath.Join(ws, "artifacts"))

	code, directStart := postJSON("/api/runs", map[string]any{"goal": "Direct workspace write"})
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/runs (direct): %d", code)
	}
	directBr := waitBranch(directStart["branch_id"].(string))
	if directBr.WorkspaceStrategy != "direct" || directBr.WorkspaceDir != ws {
		t.Fatalf("expected direct strategy at %s, got strat=%q dir=%q", ws, directBr.WorkspaceStrategy, directBr.WorkspaceDir)
	}
	if _, err := os.Stat(filepath.Join(ws, "artifacts", "soma_mock_note.txt")); err != nil {
		t.Fatalf("expected direct write into root workspace: %v", err)
	}
}

func TestServer_ApprovalsAutoResumeAndRichMetadata(t *testing.T) {
	ctx := context.Background()
	srv, st, ws := newTestWebServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	run := store.Run{
		ID:            "run_approval",
		Title:         "Deploy Config Task",
		WorkspacePath: ws,
		WorkspaceKind: "git",
		GoalSpec:      "Write approved_config.yaml",
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	br := store.Branch{
		ID:                "br_approval",
		RunID:             run.ID,
		Label:             "main",
		Status:            "queued",
		WorkspaceDir:      ws,
		WorkspaceStrategy: "direct",
	}
	if err := st.CreateBranch(ctx, br); err != nil {
		t.Fatal(err)
	}
	if err := st.PrepareAction(ctx, store.ActionJournalEntry{
		ActionID:       "act_write_1",
		RunID:          run.ID,
		BranchID:       br.ID,
		TurnIndex:      1,
		ToolName:       "write_file",
		ArgsJSON:       `{"path":"approved_config.yaml","content":"env: production\n"}`,
		ArgsHash:       "hash1",
		EffectClass:    "mutating_local",
		IdempotencyKey: "idem_write_1",
		Status:         "prepared",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertInboxItem(ctx, store.InboxItem{
		ID:          "inb_approve_1",
		RunID:       run.ID,
		BranchID:    br.ID,
		ItemKind:    "approval",
		Title:       "Approve write_file",
		Summary:     "Write production config",
		ArtifactRef: "act_write_1",
		Urgency:     "high",
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Verify GET /api/state enriches InboxDTO with RunTitle, ActionTool, and ProposedContent.
	resp, err := ts.Client().Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var s StateDTO
	_ = json.NewDecoder(resp.Body).Decode(&s)
	_ = resp.Body.Close()

	var found *InboxDTO
	for i := range s.Inbox {
		if s.Inbox[i].ID == "inb_approve_1" {
			found = &s.Inbox[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected inb_approve_1 in state.Inbox")
	}
	if found.RunTitle != "Deploy Config Task" || found.ActionTool != "write_file" || !strings.Contains(found.ActionArgs, "env: production") {
		t.Fatalf("expected enriched InboxDTO metadata, got %+v", *found)
	}

	// 2. POST /api/inbox/inb_approve_1/approve -> sets resumed=true and executes the paused branch!
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/inbox/inb_approve_1/approve", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Soma-CSRF", srv.CSRFToken())
	req.Header.Set("Origin", ts.URL)
	appResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var appOut map[string]any
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	_ = appResp.Body.Close()
	if appResp.StatusCode != http.StatusOK || appOut["resumed"] != true {
		t.Fatalf("expected 200 with resumed=true, got status=%d out=%+v", appResp.StatusCode, appOut)
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		bNow, bErr := st.GetBranch(ctx, br.ID)
		if bErr == nil && bNow.Status == "completed" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	bFinal, _ := st.GetBranch(ctx, br.ID)
	t.Fatalf("expected paused branch to auto-resume to completed, got status=%q", bFinal.Status)
}

func TestServer_TunnelSessionCookieNoSelfLockout(t *testing.T) {
	srv, _, _ := newTestWebServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/tunnel/config", strings.NewReader(`{"mode":"quick","generate_auth_token":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Soma-CSRF", srv.CSRFToken())
	req.Header.Set("Origin", ts.URL)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cookies := resp.Cookies()
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(cookies) == 0 {
		t.Fatalf("expected POST /api/tunnel/config to issue immediate soma_session cookie, status=%d cookies=%d", resp.StatusCode, len(cookies))
	}

	// Browser carrying the cookie from /api/tunnel/config stays authenticated immediately without 401 lockout!
	stateReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/state", nil)
	for _, c := range cookies {
		stateReq.AddCookie(c)
	}
	stateResp, err := ts.Client().Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK using session cookie issued during tunnel config save, got %d", stateResp.StatusCode)
	}
}

func TestServer_PartialMCPEnvMergeBridgeResetCronAndSkillFrontmatter(t *testing.T) {
	srv, _, ws := newTestWebServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	postJSON := func(path string, body any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Soma-CSRF", srv.CSRFToken())
		req.Header.Set("Origin", ts.URL)
		r, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// 1. Partial MCP Env merge: save two keys, update with one new key (existing preserved), then delete one key via "".
	code, _ := postJSON("/api/connectors/mcp", map[string]any{
		"name":      "custom-gh",
		"transport": "stdio",
		"command":   "npx",
		"args":      []string{"-y", "@modelcontextprotocol/server-github"},
		"env": map[string]string{
			"GITHUB_TOKEN": "ghp_keep_me",
			"TEMP_VAR":     "remove_me",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("create custom MCP failed: %d", code)
	}

	// Edit command/args and add EXTRA_VAR without resending GITHUB_TOKEN -> GITHUB_TOKEN must survive!
	code, _ = postJSON("/api/connectors/mcp", map[string]any{
		"name":      "custom-gh",
		"transport": "stdio",
		"command":   "npx",
		"args":      []string{"-y", "@modelcontextprotocol/server-github", "--verbose"},
		"env": map[string]string{
			"EXTRA_VAR": "added",
			"TEMP_VAR":  "",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("update custom MCP failed: %d", code)
	}
	cfg := LoadWorkspaceHarnessConfig(ws)
	var savedEnv map[string]string
	for _, s := range cfg.MCPServers {
		if s.Name == "custom-gh" {
			savedEnv = s.Env
		}
	}
	if savedEnv["GITHUB_TOKEN"] != "ghp_keep_me" || savedEnv["EXTRA_VAR"] != "added" {
		t.Fatalf("expected GITHUB_TOKEN preserved and EXTRA_VAR added, got %+v", savedEnv)
	}
	if _, exists := savedEnv["TEMP_VAR"]; exists {
		t.Fatalf("expected TEMP_VAR deleted via empty string, got %+v", savedEnv)
	}

	// 2. Bridge reset clears token and disables channel.
	code, _ = postJSON("/api/connectors/bridges", map[string]any{
		"discord": map[string]any{"enabled": true, "bot_token": "disc-tok-9999", "channel_id": "123"},
	})
	if code != http.StatusOK {
		t.Fatalf("save discord bridge: %d", code)
	}
	code, resetOut := postJSON("/api/connectors/bridges", map[string]any{
		"reset_channel": "discord",
	})
	if code != http.StatusOK {
		t.Fatalf("reset discord bridge: %d", code)
	}
	discAfter := resetOut["bridges"].(map[string]any)["discord"].(map[string]any)
	if discAfter["enabled"] != false || discAfter["has_bot_token"] != false {
		t.Fatalf("expected discord bridge cleared after reset_channel, got %+v", discAfter)
	}

	// 3. Cron schedule accepted ("0 9 * * 1-5") and invalid schedule rejected (400).
	code, _ = postJSON("/api/harness/triggers", map[string]any{
		"triggers": []map[string]any{
			{"ID": "weekday-standup", "Type": "cron", "Interval": "0 9 * * 1-5", "Prompt": "Summarize PRs"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("expected 5-field cron expression accepted, got %d", code)
	}
	badCode, _ := postJSON("/api/harness/triggers", map[string]any{
		"triggers": []map[string]any{
			{"ID": "broken-cron", "Type": "cron", "Interval": "not a valid schedule", "Prompt": "Fail"},
		},
	})
	if badCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid cron schedule, got %d", badCode)
	}

	// 4. Skill template frontmatter `name: my-skill` syncs cleanly to requested skill name.
	code, _ = postJSON("/api/skills", map[string]any{
		"name":    "release-audit",
		"content": "---\nname: my-skill\ndescription: Release audit checklist\n---\n\n# Steps\n1. Check tags\n",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/skills failed: %d", code)
	}
	skResp, err := ts.Client().Get(ts.URL + "/api/skills/release-audit")
	if err != nil {
		t.Fatal(err)
	}
	var skData map[string]any
	_ = json.NewDecoder(skResp.Body).Decode(&skData)
	_ = skResp.Body.Close()
	if !strings.Contains(skData["raw_content"].(string), "name: release-audit") {
		t.Fatalf("expected frontmatter name synced to release-audit, got %q", skData["raw_content"])
	}

	// 5. Workspace-scoped memory rule can be created and runs cleanly.
	code, _ = postJSON("/api/memory", map[string]any{
		"scope":   "workspace",
		"kind":    "rule",
		"content": "Prefer table-driven Go tests in this repo",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/memory(workspace) failed: %d", code)
	}

	// 6. Autonomy & sandbox internet access settings persist and wire into Executor/ToolRunner.
	code, provOut := postJSON("/api/harness/provider", map[string]any{
		"provider_type":         "local",
		"model_id":              "mock-worker-v1",
		"allow_sandbox_network": true,
		"autonomy_mode":         "autonomous",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/provider(autonomy) failed: %d", code)
	}
	provDTO := provOut["provider"].(map[string]any)
	if provDTO["allow_sandbox_network"] != true || provDTO["autonomy_mode"] != "autonomous" {
		t.Fatalf("expected allow_sandbox_network=true & autonomy_mode=autonomous, got %+v", provDTO)
	}
	// Saving unrelated provider fields without allow_sandbox_network must preserve it.
	code, provOut2 := postJSON("/api/harness/provider", map[string]any{
		"provider_type": "local",
		"model_id":      "mock-worker-v1",
		"max_turns":     25,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/provider(partial) failed: %d", code)
	}
	provDTO2 := provOut2["provider"].(map[string]any)
	if provDTO2["allow_sandbox_network"] != true || provDTO2["autonomy_mode"] != "autonomous" {
		t.Fatalf("expected partial save to preserve autonomy/network settings, got %+v", provDTO2)
	}
	exec, _, err := srv.executorForRequest(t.Context(), "worker", "")
	if err != nil {
		t.Fatalf("executorForRequest: %v", err)
	}
	if exec.AutonomyMode != "autonomous" {
		t.Fatalf("expected exec.AutonomyMode='autonomous', got %q", exec.AutonomyMode)
	}
	if tr, ok := exec.Tools.(*dag.LocalToolRunner); !ok || !tr.AllowSandboxNetwork {
		t.Fatalf("expected LocalToolRunner.AllowSandboxNetwork=true, got %+v", exec.Tools)
	}

	// 7. Desktop Take Over endpoint updates server takeover state cleanly.
	code, toOut := postJSON("/api/desktop/takeover", map[string]any{"active": true})
	if code != http.StatusOK || toOut["takeover"] != true {
		t.Fatalf("POST /api/desktop/takeover(true) failed: code=%d out=%+v", code, toOut)
	}
	code, toOut2 := postJSON("/api/desktop/takeover", map[string]any{"active": false})
	if code != http.StatusOK || toOut2["takeover"] != false {
		t.Fatalf("POST /api/desktop/takeover(false) failed: code=%d out=%+v", code, toOut2)
	}
}
