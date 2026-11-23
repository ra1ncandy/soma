package mcp

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManager_StdioTransportMultimodalImageAndCatalog(t *testing.T) {
	// 1x1 valid PNG bytes.
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg=="
	pyScript := `import json, sys
PNG = "` + pngB64 + `"
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    req = json.loads(raw)
    m = req.get("method", "")
    if m == "notifications/initialized":
        continue
    if m == "initialize":
        res = {"protocolVersion": "2024-11-05", "capabilities": {"tools": {}}, "serverInfo": {"name": "mock-desktop", "version": "1.0"}}
    elif m == "tools/list":
        res = {"tools": [{"name": "desktop_screenshot", "description": "Capture screen", "inputSchema": {"type": "object", "properties": {}}}]}
    elif m == "tools/call":
        res = {"content": [{"type": "text", "text": "captured 1x1"}, {"type": "image", "data": PNG, "mimeType": "image/png"}]}
    else:
        res = {}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": req.get("id"), "result": res}) + "\n")
    sys.stdout.flush()
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "mock_mcp.py")
	if err := os.WriteFile(scriptPath, []byte(pyScript), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	mgr := NewManager()
	defer func() { _ = mgr.Close() }()

	err := mgr.Connect(ctx, ServerEndpoint{
		Name:            "desktop",
		Transport:       "stdio",
		Command:         "python3",
		Args:            []string{scriptPath},
		IsolatedDesktop: true,
	})
	if err != nil {
		t.Fatalf("Connect stdio MCP: %v", err)
	}
	if !mgr.IsIsolatedDesktop("desktop") {
		t.Fatal("expected desktop server to report IsolatedDesktop=true")
	}

	cat := NewEpochCatalog()
	mgr.PopulateCatalog(cat)
	hits := cat.Search("screenshot")
	if len(hits) != 1 || hits[0].Name != "desktop.desktop_screenshot" {
		t.Fatalf("unexpected catalog search hits: %+v", hits)
	}

	out, err := mgr.CallTool(ctx, "desktop", "desktop_screenshot", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out.TrustTier != "untrusted_external" {
		t.Fatalf("expected untrusted_external trust tier, got %q", out.TrustTier)
	}
	if !strings.Contains(out.Content, "captured 1x1") {
		t.Fatalf("unexpected text content: %q", out.Content)
	}
	if len(out.Images) != 1 || out.Images[0].MIMEType != "image/png" {
		t.Fatalf("expected 1 PNG image attachment, got %+v", out.Images)
	}
	wantPNG, _ := base64.StdEncoding.DecodeString(pngB64)
	if string(out.Images[0].Data) != string(wantPNG) {
		t.Fatalf("decoded PNG bytes mismatch: got %d bytes want %d", len(out.Images[0].Data), len(wantPNG))
	}
}
