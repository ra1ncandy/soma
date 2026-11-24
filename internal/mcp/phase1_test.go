package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFilterToolsAndNaming(t *testing.T) {
	tools := []sdkmcp.Tool{
		{Name: "read_file"},
		{Name: "write_file"},
		{Name: "delete_file"},
		{Name: "list_issues"},
	}

	filtered := FilterTools(tools, []string{"*_file", "list_*"}, []string{"delete_*"})
	if len(filtered) != 3 {
		t.Fatalf("expected 3 filtered tools, got %d: %+v", len(filtered), filtered)
	}
	if IsToolAllowed("delete_file", []string{"*_file"}, []string{"delete_*"}) {
		t.Fatal("expected exclude to take precedence over include")
	}

	hermesName := HermesToolName("github", "create_issue")
	if hermesName != "mcp__github__create_issue" {
		t.Fatalf("unexpected HermesToolName: %q", hermesName)
	}
	srv, tool, ok := ParseToolName(hermesName)
	if !ok || srv != "github" || tool != "create_issue" {
		t.Fatalf("ParseToolName(%q) = (%q, %q, %v)", hermesName, srv, tool, ok)
	}
	srv2, tool2, ok2 := ParseToolName("desktop.click")
	if !ok2 || srv2 != "desktop" || tool2 != "click" {
		t.Fatalf("ParseToolName(desktop.click) = (%q, %q, %v)", srv2, tool2, ok2)
	}
}

func TestManager_StdioFilterResourcesAndPrompts(t *testing.T) {
	pyScript := `import json, sys
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    req = json.loads(raw)
    m = req.get("method", "")
    if m == "notifications/initialized":
        continue
    if m == "initialize":
        res = {"protocolVersion": "2024-11-05", "capabilities": {"tools": {}, "resources": {}, "prompts": {}}, "serverInfo": {"name": "mock", "version": "1.0"}}
    elif m == "tools/list":
        res = {"tools": [
            {"name": "safe_read", "description": "Read", "inputSchema": {"type": "object"}},
            {"name": "danger_drop", "description": "Drop", "inputSchema": {"type": "object"}}
        ]}
    elif m == "tools/call":
        res = {"content": [{"type": "text", "text": "ok"}]}
    elif m == "resources/list":
        res = {"resources": [{"uri": "memo://notes", "name": "notes", "mimeType": "text/plain"}]}
    elif m == "resources/read":
        res = {"contents": [{"uri": "memo://notes", "mimeType": "text/plain", "text": "hello resource"}]}
    elif m == "prompts/list":
        res = {"prompts": [{"name": "summarize", "description": "Summarize input"}]}
    elif m == "prompts/get":
        res = {"description": "Summarize", "messages": [{"role": "user", "content": {"type": "text", "text": "Summarize this"}}]}
    else:
        res = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": req.get("id"), "result": res}) + "\n")
    sys.stdout.flush()
`
	scriptPath := filepath.Join(t.TempDir(), "mock_full.py")
	if err := os.WriteFile(scriptPath, []byte(pyScript), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	mgr := NewManager()
	defer func() { _ = mgr.Close() }()

	err := mgr.Connect(ctx, ServerEndpoint{
		Name:         "notes",
		Transport:    "stdio",
		Command:      "python3",
		Args:         []string{scriptPath},
		IncludeTools: []string{"safe_*"},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	tools := mgr.ListTools("notes")["notes"]
	if len(tools) != 1 || tools[0].Name != "safe_read" {
		t.Fatalf("expected only safe_read, got %+v", tools)
	}
	if _, err := mgr.CallTool(ctx, "notes", "danger_drop", nil); err == nil {
		t.Fatal("expected blocked tool call to fail")
	}
	if err := mgr.Ping(ctx, "notes"); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	resMap, err := mgr.ListResources(ctx, "notes")
	if err != nil || len(resMap["notes"]) != 1 {
		t.Fatalf("ListResources: err=%v map=%+v", err, resMap)
	}
	readOut, err := mgr.ReadResource(ctx, "notes", "memo://notes")
	if err != nil || readOut.Text != "hello resource" || readOut.TrustTier != "untrusted_external" {
		t.Fatalf("ReadResource: err=%v out=%+v", err, readOut)
	}

	promptMap, err := mgr.ListPrompts(ctx, "notes")
	if err != nil || len(promptMap["notes"]) != 1 {
		t.Fatalf("ListPrompts: err=%v map=%+v", err, promptMap)
	}
	promptOut, err := mgr.GetPrompt(ctx, "notes", "summarize", nil)
	if err != nil || len(promptOut.Messages) != 1 || promptOut.TrustTier != "untrusted_external" {
		t.Fatalf("GetPrompt: err=%v out=%+v", err, promptOut)
	}
}

func TestManager_StreamableHTTPAndSSEAndOAuth(t *testing.T) {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test-remote", Version: "1.0.0"}, nil)
	type echoArgs struct {
		Msg string `json:"msg"`
	}
	sdkmcp.AddTool(srv, &sdkmcp.Tool{Name: "echo", Description: "Echo message"}, func(_ context.Context, _ *sdkmcp.CallToolRequest, args echoArgs) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo:" + args.Msg}},
		}, nil, nil
	})

	// Streamable HTTP endpoint protected by Bearer token.
	streamHandler := sdkmcp.NewStreamableHTTPHandler(func(_ *http.Request) *sdkmcp.Server {
		return srv
	}, nil)
	authMux := http.NewServeMux()
	authMux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		streamHandler.ServeHTTP(w, r)
	})
	authMux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		_ = json.NewEncoder(w).Encode(AuthMetadata{
			Issuer:                base,
			AuthorizationEndpoint: base + "/authorize",
			TokenEndpoint:         base + "/token",
		})
	})
	authMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("grant_type") == "refresh_token" && r.FormValue("refresh_token") == "rt-1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "fresh-access-token",
				"refresh_token": "rt-2",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
			return
		}
		http.Error(w, "bad token request", http.StatusBadRequest)
	})
	ts := httptest.NewServer(authMux)
	defer ts.Close()

	tokenStore := &TokenStore{Dir: filepath.Join(t.TempDir(), "tokens")}
	// Save an expired token so StoredOAuthHandler automatically refreshes it via /token.
	err := tokenStore.Save("remote_stream", OAuthToken{
		AccessToken:  "stale-token",
		RefreshToken: "rt-1",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	mgr := NewManager()
	mgr.SetTokenStore(tokenStore)
	defer func() { _ = mgr.Close() }()

	err = mgr.Connect(ctx, ServerEndpoint{
		Name:      "remote_stream",
		Transport: "streamable_http",
		URL:       ts.URL + "/mcp",
		OAuth: &OAuthConfig{
			Enabled:       true,
			ClientID:      "soma-cli",
			AuthServerURL: ts.URL,
		},
	})
	if err != nil {
		t.Fatalf("Connect streamable_http with OAuth refresh: %v", err)
	}
	out, err := mgr.CallTool(ctx, "remote_stream", "echo", map[string]any{"msg": "hi"})
	if err != nil || out.Content != "echo:hi" || out.TrustTier != "untrusted_external" {
		t.Fatalf("CallTool streamable_http: err=%v out=%+v", err, out)
	}
	persisted, err := tokenStore.Load("remote_stream")
	if err != nil || persisted.AccessToken != "fresh-access-token" || persisted.RefreshToken != "rt-2" {
		t.Fatalf("expected refreshed token persisted, got err=%v tok=%+v", err, persisted)
	}

	// Verify SSE transport as well.
	sseHandler := sdkmcp.NewSSEHandler(func(_ *http.Request) *sdkmcp.Server {
		return srv
	}, nil)
	sseSrv := httptest.NewServer(sseHandler)
	defer sseSrv.Close()

	if err := mgr.Connect(ctx, ServerEndpoint{
		Name:      "remote_sse",
		Transport: "sse",
		URL:       sseSrv.URL,
	}); err != nil {
		t.Fatalf("Connect sse: %v", err)
	}
	sseOut, err := mgr.CallTool(ctx, "remote_sse", "echo", map[string]any{"msg": "sse"})
	if err != nil || sseOut.Content != "echo:sse" {
		t.Fatalf("CallTool sse: err=%v out=%+v", err, sseOut)
	}
}

func TestGeneratePKCEAndBuildAuthorizeURL(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("PKCE challenge mismatch: got %q want %q", challenge, want)
	}
	u, err := BuildAuthorizeURL("https://auth.example.com/authorize", OAuthConfig{
		ClientID: "cid-1",
		Scopes:   []string{"read", "write"},
	}, "http://127.0.0.1:9999/callback", "state-xyz", challenge)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "code_challenge_method=S256") || !strings.Contains(u, "client_id=cid-1") {
		t.Fatalf("unexpected authorize URL: %s", u)
	}
}
