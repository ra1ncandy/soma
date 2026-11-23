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
		"quiet_hours":             "23:00-07:00",
		"max_pending_inbox":       5,
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
}
