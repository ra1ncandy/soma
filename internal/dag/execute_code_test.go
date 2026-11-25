package dag

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestExecuteCodeCallsToolsFromTheSandbox(t *testing.T) {
	mgr := sandbox.NewManager()
	if _, _, err := mgr.Command(context.Background(), sandbox.ExecSpec{WorkspaceDir: t.TempDir(), Command: "/bin/true"}); err != nil {
		t.Skipf("no usable OS sandbox: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	ctx := context.Background()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), ".soma"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	r := NewSandboxedToolRunner(vmem.NewManager(st), mgr)
	code := `import soma_tools as t
lines = t.read_file(path="data.txt")
n = sum(1 for l in lines.splitlines() if "a" in l)
t.write_file(path="count.txt", content=str(n))
print("matches", n)
try:
    t.call("mcp_call", server="x", tool="y")
except RuntimeError as e:
    print("blocked:", e)
`
	out, err := r.Execute(ctx, ToolCall{RunID: "run_code", BranchID: "b", Name: "execute_code", ArgsJSON: mustJSON(map[string]any{"code": code}), WorkspaceDir: ws})
	if err != nil {
		t.Fatalf("execute_code: %v", err)
	}
	text := string(out.Content)
	if !strings.Contains(text, "exit_code=0") || !strings.Contains(text, "tool_calls=2") {
		t.Fatalf("unexpected result:\n%s", text)
	}
	if !strings.Contains(text, "matches 3") {
		t.Fatalf("script output missing:\n%s", text)
	}
	if !strings.Contains(text, "blocked: mcp_call is not callable") {
		t.Fatalf("external tool was not refused:\n%s", text)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "count.txt")); string(b) != "3" {
		t.Fatalf("write through RPC failed: %q", b)
	}
}
