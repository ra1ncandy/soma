package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/bridge"
)

func TestServer_HarnessAndConnectorsCRUD(t *testing.T) {
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
			t.Fatalf("POST %s: %v", path, err)
		}
		defer func() { _ = r.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	// 1. GET /api/harness returns presets & desktop status.
	resp, err := ts.Client().Get(ts.URL + "/api/harness")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/harness failed: resp=%v err=%v", resp, err)
	}
	var har HarnessDTO
	_ = json.NewDecoder(resp.Body).Decode(&har)
	_ = resp.Body.Close()
	if len(har.Presets) < 4 {
		t.Fatalf("expected built-in MCP presets, got %d", len(har.Presets))
	}

	// 2. Install MCP preset + toggle + delete.
	code, _ := postJSON("/api/connectors/mcp", map[string]any{"preset_id": "playwright"})
	if code != http.StatusOK {
		t.Fatalf("install playwright preset failed: %d", code)
	}
	code, _ = postJSON("/api/connectors/mcp/playwright/toggle", map[string]any{"enabled": false})
	if code != http.StatusOK {
		t.Fatalf("toggle playwright failed: %d", code)
	}
	code, _ = postJSON("/api/connectors/mcp/playwright/delete", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("delete playwright failed: %d", code)
	}

	// 3. Save Bridges with secret token and verify masking + preservation.
	code, brOut := postJSON("/api/connectors/bridges", map[string]any{
		"discord": map[string]any{
			"enabled":    true,
			"bot_token":  "discord-secret-1234",
			"channel_id": "chan-1",
		},
		"telegram": map[string]any{
			"enabled":       true,
			"bot_token":     "tg-secret-9876",
			"alert_chat_id": 42,
		},
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/connectors/bridges failed: %d", code)
	}
	bridges := brOut["bridges"].(map[string]any)
	disc := bridges["discord"].(map[string]any)
	if disc["has_bot_token"] != true || disc["bot_token_masked"] != "••••1234" {
		t.Fatalf("unexpected discord masking: %+v", disc)
	}

	// Re-save with empty token; existing token must be preserved.
	code, brOut2 := postJSON("/api/connectors/bridges", map[string]any{
		"discord": map[string]any{"enabled": false},
	})
	if code != http.StatusOK {
		t.Fatalf("re-save bridges failed: %d", code)
	}
	disc2 := brOut2["bridges"].(map[string]any)["discord"].(map[string]any)
	if disc2["has_bot_token"] != true {
		t.Fatalf("expected existing discord bot token to be preserved")
	}

	// 4. Save Provider config.
	code, provOut := postJSON("/api/harness/provider", map[string]any{
		"provider_type":      "openai_compat",
		"api_base":           "http://127.0.0.1:11434/v1",
		"api_key":            "sk-test-5555",
		"model_id":           "qwen3:32b",
		"workspace_strategy": "worktree",
		"sandbox_mode":       "bwrap",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/provider failed: %d", code)
	}
	prov := provOut["provider"].(map[string]any)
	if prov["model_id"] != "qwen3:32b" || prov["api_key_masked"] != "••••5555" {
		t.Fatalf("unexpected provider DTO: %+v", prov)
	}

	// 5. Save Triggers & verify .soma/triggers.yaml written.
	code, _ = postJSON("/api/harness/triggers", map[string]any{
		"quiet_hours":              "23:00-07:00",
		"max_pending_inbox":        5,
		"daily_metered_budget_usd": 12.5,
		"triggers": []map[string]any{
			{"ID": "nightly", "Type": "cron", "Interval": "24h", "Prompt": "Run health check"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/triggers failed: %d", code)
	}
	if raw, err := os.ReadFile(filepath.Join(ws, ".soma", "triggers.yaml")); err != nil || !strings.Contains(string(raw), "nightly") {
		t.Fatalf("expected .soma/triggers.yaml to contain nightly trigger, err=%v", err)
	}

	// 6. Upsert Account.
	code, accOut := postJSON("/api/harness/accounts", map[string]any{
		"ID":             "local-ollama",
		"Provider":       "openai_compat",
		"ModelID":        "qwen3:32b",
		"BillingMode":    "local",
		"MaxConcurrent":  4,
		"DailyBudgetUSD": 0,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/accounts failed: %d", code)
	}
	if len(accOut["accounts"].([]any)) < 2 {
		t.Fatalf("expected new account in accounts list")
	}

	// 7. Virtual desktop proxy returns 503 when not running, and CSP includes frame-src 'self'.
	vncResp, err := ts.Client().Get(ts.URL + "/desktop-vnc/vnc.html")
	if err != nil {
		t.Fatalf("GET /desktop-vnc/vnc.html: %v", err)
	}
	_ = vncResp.Body.Close()
	if vncResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when desktop stopped, got %d", vncResp.StatusCode)
	}
	if !strings.Contains(vncResp.Header.Get("Content-Security-Policy"), "frame-src 'self'") {
		t.Fatalf("expected frame-src 'self' in CSP, got %q", vncResp.Header.Get("Content-Security-Policy"))
	}

	// 8. Import MCP servers from raw Claude Code JSON and verify .soma/mcp.yaml sync.
	code, impOut := postJSON("/api/connectors/mcp/import", map[string]any{
		"source":  "claude-code",
		"content": `{"mcpServers":{"linear":{"type":"streamable_http","url":"https://mcp.linear.app/mcp","include_tools":["list_*"]}}}`,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/connectors/mcp/import failed: %d out=%+v", code, impOut)
	}
	if mcpRaw, mErr := os.ReadFile(filepath.Join(ws, ".soma", "mcp.yaml")); mErr != nil || !strings.Contains(string(mcpRaw), "linear") {
		t.Fatalf("expected .soma/mcp.yaml to contain imported linear server, err=%v", mErr)
	}

	// 9. Save expanded Bridges (Slack + Email) and verify secret masking.
	code, brOut = postJSON("/api/connectors/bridges", map[string]any{
		"slack": map[string]any{
			"enabled":          true,
			"bot_token":        "xoxb-1234567890",
			"allowed_user_ids": []string{"U1"},
		},
		"email": map[string]any{
			"enabled":   true,
			"smtp_host": "smtp.example.com",
			"smtp_port": 587,
			"from":      "soma@example.com",
			"password":  "secret-pass",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/connectors/bridges failed: %d", code)
	}
	bridgesDTO := brOut["bridges"].(map[string]any)
	slackDTO := bridgesDTO["slack"].(map[string]any)
	if slackDTO["has_bot_token"] != true || slackDTO["bot_token_masked"] != "••••7890" {
		t.Fatalf("unexpected slack DTO: %+v", slackDTO)
	}

	// 10. DM Pairing approve & revoke via dashboard API.
	ps := bridge.NewPairingStore(filepath.Join(ws, ".soma"))
	req, err := ps.IssueCode("slack", "U777", "alice")
	if err != nil {
		t.Fatal(err)
	}
	code, pairOut := postJSON("/api/connectors/bridges/pair", map[string]any{"code": req.Code})
	if code != http.StatusOK {
		t.Fatalf("POST /api/connectors/bridges/pair failed: %d", code)
	}
	pairing := pairOut["pairing"].(map[string]any)
	if len(pairing["approved"].([]any)) != 1 || len(pairing["pending"].([]any)) != 0 {
		t.Fatalf("unexpected pairing DTO after approve: %+v", pairing)
	}
	code, unpairOut := postJSON("/api/connectors/bridges/unpair", map[string]any{"channel": "slack", "sender_id": "U777"})
	if code != http.StatusOK {
		t.Fatalf("POST /api/connectors/bridges/unpair failed: %d", code)
	}
	pairing2 := unpairOut["pairing"].(map[string]any)
	if len(pairing2["approved"].([]any)) != 0 {
		t.Fatalf("expected 0 approved after unpair: %+v", pairing2)
	}

	// 11. Selecting a Hermes AI provider preset auto-resolves BaseURL and DefaultModel.
	code, orOut := postJSON("/api/harness/provider", map[string]any{
		"provider_type": "openrouter",
		"api_key":       "sk-or-test-1234",
		"model_id":      "",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/provider(openrouter) failed: %d", code)
	}
	orProv := orOut["provider"].(map[string]any)
	if orProv["api_base"] != "https://openrouter.ai/api/v1" || orProv["model_id"] == "" {
		t.Fatalf("expected openrouter base URL and default model, got %+v", orProv)
	}

	// 12. Start Provider OAuth (openrouter) returns auth_url.
	code, poOut := postJSON("/api/harness/provider/oauth/start", map[string]any{"provider": "openrouter"})
	if code != http.StatusOK || !strings.Contains(poOut["auth_url"].(string), "openrouter.ai/auth") {
		t.Fatalf("POST /api/harness/provider/oauth/start failed: code=%d out=%+v", code, poOut)
	}

	// 13. MCP OAuth logout endpoint succeeds cleanly.
	code, _ = postJSON("/api/connectors/mcp/linear/oauth/logout", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("POST /api/connectors/mcp/linear/oauth/logout failed: %d", code)
	}

	// 14. Create Memory node and Skill via dashboard APIs, then inspect skill.
	code, memOut := postJSON("/api/memory", map[string]any{
		"scope":   "global",
		"kind":    "rule",
		"content": "Always run race tests",
	})
	if code != http.StatusOK || memOut["ok"] != true {
		t.Fatalf("POST /api/memory failed: code=%d out=%+v", code, memOut)
	}
	code, skOut := postJSON("/api/skills", map[string]any{
		"name":    "race-check",
		"content": "---\nname: race-check\ndescription: Run go test -race\n---\n\n# Steps\n\n1. Run `go test -race ./...`\n",
	})
	if code != http.StatusOK || skOut["ok"] != true {
		t.Fatalf("POST /api/skills failed: code=%d out=%+v", code, skOut)
	}
	skResp, err := ts.Client().Get(ts.URL + "/api/skills/race-check")
	if err != nil {
		t.Fatalf("GET /api/skills/race-check: %v", err)
	}
	_ = skResp.Body.Close()
	if skResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/skills/race-check status=%d", skResp.StatusCode)
	}

	// 15. TunnelManager refuses starting without AuthToken.
	tm := NewTunnelManager()
	if err := tm.Start(t.Context(), "http://127.0.0.1:7420", TunnelConfig{Mode: "quick"}); err == nil {
		t.Fatal("expected TunnelManager.Start to refuse empty AuthToken")
	}

	// 16. Configure tunnel with generated auth_token -> enforces auth, login cookie unlocks /api/state, logout revokes.
	code, tunOut := postJSON("/api/tunnel/config", map[string]any{
		"mode":                "quick",
		"generate_auth_token": true,
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/tunnel/config failed: %d", code)
	}
	genTok, _ := tunOut["generated_auth_token"].(string)
	if genTok == "" {
		t.Fatalf("expected generated_auth_token in response: %+v", tunOut)
	}

	// Unauthenticated GET /api/state -> 401
	unauthResp, err := ts.Client().Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after setting tunnel auth_token, got %d", unauthResp.StatusCode)
	}

	// Wrong token login -> 401
	code, _ = postJSON("/api/auth/login", map[string]any{"token": "wrong-token"})
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong login token, got %d", code)
	}

	// Valid token login -> sets soma_session cookie
	loginReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/login", strings.NewReader(`{"token":"`+genTok+`"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("X-Soma-CSRF", srv.CSRFToken())
	loginResp, err := ts.Client().Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginResp.Cookies()
	_ = loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK || len(cookies) == 0 {
		t.Fatalf("expected 200 + soma_session cookie, got %d cookies=%d", loginResp.StatusCode, len(cookies))
	}

	authedReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/state", nil)
	for _, c := range cookies {
		authedReq.AddCookie(c)
	}
	authedResp, err := ts.Client().Do(authedReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = authedResp.Body.Close()
	if authedResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with soma_session cookie, got %d", authedResp.StatusCode)
	}

	// 17. Fetch models, upsert local model (budget forced to 0), activate, and delete via authenticated cookie.
	doAuthedPost := func(path string, body string) (int, map[string]any) {
		r, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Soma-CSRF", srv.CSRFToken())
		for _, c := range cookies {
			r.AddCookie(c)
		}
		resp, dErr := ts.Client().Do(r)
		if dErr != nil {
			t.Fatal(dErr)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	code, modOut := doAuthedPost("/api/harness/models", `{"provider":"mock"}`)
	if code != http.StatusOK || len(modOut["models"].([]any)) < 2 {
		t.Fatalf("POST /api/harness/models failed: code=%d out=%+v", code, modOut)
	}
	code, accUpd := doAuthedPost("/api/harness/accounts", `{"ID":"ollama-work","Provider":"ollama","ModelID":"hermes3:8b","BillingMode":"local","MaxConcurrent":4,"DailyBudgetUSD":99,"role":"worker"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/accounts failed: %d", code)
	}
	for _, rawAcc := range accUpd["accounts"].([]any) {
		acc := rawAcc.(map[string]any)
		if acc["id"] == "ollama-work" && acc["daily_budget_usd"].(float64) != 0 {
			t.Fatalf("expected local billing to zero daily_budget_usd, got %+v", acc)
		}
	}
	code, _ = doAuthedPost("/api/harness/accounts/ollama-work/activate", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/accounts/ollama-work/activate failed: %d", code)
	}
	code, delOut := doAuthedPost("/api/harness/accounts/ollama-work/delete", `{}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/harness/accounts/ollama-work/delete failed: %d", code)
	}
	for _, rawAcc := range delOut["accounts"].([]any) {
		if rawAcc.(map[string]any)["id"] == "ollama-work" {
			t.Fatal("expected ollama-work to be deleted")
		}
	}

	// 18. Trigger Run Now endpoint starts an async run for configured trigger.
	code, runTrigOut := doAuthedPost("/api/harness/triggers/nightly/run", `{}`)
	if code != http.StatusAccepted || runTrigOut["run_id"] == "" {
		t.Fatalf("POST /api/harness/triggers/nightly/run failed: code=%d out=%+v", code, runTrigOut)
	}
}

func TestServer_ChatFollowUpStopAndMemorySkillCRUD(t *testing.T) {
	srv, _, _ := newTestWebServer(t, nil)
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
			t.Fatalf("POST %s: %v", path, err)
		}
		defer func() { _ = r.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}
	getState := func() StateDTO {
		resp, err := ts.Client().Get(ts.URL + "/api/state")
		if err != nil {
			t.Fatalf("GET /api/state: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var st StateDTO
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		return st
	}
	waitBranchSettled := func(branchID string) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			st := getState()
			for _, run := range st.Runs {
				for _, br := range run.Branches {
					if br.ID == branchID && br.Status != "running" {
						return
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// 1. Start initial run, wait for completion, continue conversation, and verify user_message step + stop endpoint.
	code, runOut := postJSON("/api/runs", map[string]any{"goal": "Initial UI question", "profile": "worker"})
	if code != http.StatusAccepted || runOut["branch_id"] == "" {
		t.Fatalf("POST /api/runs failed: code=%d out=%+v", code, runOut)
	}
	branchID := runOut["branch_id"].(string)
	waitBranchSettled(branchID)

	code, contOut := postJSON("/api/branches/"+branchID+"/continue", map[string]any{
		"goal":    "Follow up question",
		"profile": "frontier",
	})
	if code != http.StatusAccepted || contOut["branch_id"] != branchID {
		t.Fatalf("POST /continue failed: code=%d out=%+v", code, contOut)
	}
	waitBranchSettled(branchID)

	st := getState()
	foundUserTurn := false
	for _, run := range st.Runs {
		for _, br := range run.Branches {
			if br.ID == branchID {
				for _, step := range br.Steps {
					if step.StepType == "user_message" && strings.Contains(step.Reasoning, "Follow up question") {
						foundUserTurn = true
					}
				}
			}
		}
	}
	if !foundUserTurn {
		t.Fatalf("expected user_message step for Follow up question on branch %s", branchID)
	}

	code, stopOut := postJSON("/api/branches/"+branchID+"/stop", map[string]any{})
	if code != http.StatusOK || stopOut["status"] != "stopped" {
		t.Fatalf("POST /stop failed: code=%d out=%+v", code, stopOut)
	}

	// 2. Memory update & delete.
	code, _ = postJSON("/api/memory", map[string]any{"scope": "global", "kind": "fact", "content": "initial fact"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /api/memory failed: code=%d", code)
	}
	st = getState()
	var memID string
	for _, m := range st.Memory {
		if m.Content == "initial fact" {
			memID = m.ID
			break
		}
	}
	if memID == "" {
		t.Fatal("expected created memory node in state")
	}
	code, _ = postJSON("/api/memory/"+memID+"/update", map[string]any{"content": "updated fact"})
	if code != http.StatusOK {
		t.Fatalf("POST /api/memory/%s/update failed: code=%d", memID, code)
	}
	st = getState()
	foundUpdated := false
	for _, m := range st.Memory {
		if m.ID == memID && m.Content == "updated fact" {
			foundUpdated = true
		}
	}
	if !foundUpdated {
		t.Fatalf("expected memory node %s content to be updated", memID)
	}
	code, _ = postJSON("/api/memory/"+memID+"/delete", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("POST /api/memory/%s/delete failed: code=%d", memID, code)
	}
	st = getState()
	for _, m := range st.Memory {
		if m.ID == memID {
			t.Fatalf("expected memory node %s to be deleted", memID)
		}
	}

	// 3. Skill raw_content & delete.
	code, _ = postJSON("/api/skills", map[string]any{
		"name":    "temp-skill",
		"content": "---\nname: temp-skill\ndescription: temporary\n---\n\nbody\n",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/skills failed: code=%d", code)
	}
	skResp, err := ts.Client().Get(ts.URL + "/api/skills/temp-skill")
	if err != nil {
		t.Fatal(err)
	}
	var skOut map[string]any
	_ = json.NewDecoder(skResp.Body).Decode(&skOut)
	_ = skResp.Body.Close()
	if skResp.StatusCode != http.StatusOK || !strings.Contains(skOut["raw_content"].(string), "name: temp-skill") {
		t.Fatalf("expected raw_content from GET /api/skills/temp-skill, got %+v", skOut)
	}
	code, _ = postJSON("/api/skills/temp-skill/delete", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("POST /api/skills/temp-skill/delete failed: code=%d", code)
	}
	afterDel, err := ts.Client().Get(ts.URL + "/api/skills/temp-skill")
	if err != nil {
		t.Fatal(err)
	}
	_ = afterDel.Body.Close()
	if afterDel.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after deleting skill, got %d", afterDel.StatusCode)
	}
}
