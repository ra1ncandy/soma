package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseToolMounts(t *testing.T) {
	home := t.TempDir()
	tools := filepath.Join(home, "tools", "pylib")
	for _, d := range []string{tools, filepath.Join(home, ".ssh"), filepath.Join(home, "proj", ".soma")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(home, "innocent")
	if err := os.Symlink(filepath.Join(home, ".ssh"), link); err != nil {
		t.Fatal(err)
	}

	// A destination defaults to the host path; /etc is used here because test directories live
	// under /tmp, which is reserved inside the sandbox.
	got, err := ParseToolMounts(" "+tools+":/opt/pylib , /etc ,", home)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolMount{{Host: tools, Dest: "/opt/pylib"}, {Host: "/etc", Dest: "/etc"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if m, err := ParseToolMounts("", home); err != nil || len(m) != 0 {
		t.Fatalf("empty value: %v %v", m, err)
	}

	for name, value := range map[string]string{
		"relative host":            "tools:/opt/x",
		"relative destination":     tools + ":opt/x",
		"missing host":             filepath.Join(home, "nope"),
		"root":                     "/",
		"the home directory":       home,
		"an ancestor of home":      filepath.Dir(home),
		"credentials":              filepath.Join(home, ".ssh"),
		"credentials by symlink":   link,
		"soma state":               filepath.Join(home, "proj", ".soma"),
		"destination /":            tools + ":/",
		"destination in /usr":      tools + ":/usr/lib/x",
		"destination /workspace":   tools + ":/workspace/tools",
		"destination /tmp":         tools + ":/tmp/x",
		"one bad entry among good": tools + "," + filepath.Join(home, ".ssh"),
	} {
		if m, err := ParseToolMounts(value, home); err == nil {
			t.Errorf("%s: %q must be rejected, got %+v", name, value, m)
		}
	}
}

func TestParseToolEnv(t *testing.T) {
	got, err := ParseToolEnv("PYTHONPATH=/opt/a:/opt/b; FOO=x=y ;")
	if err != nil || got["PYTHONPATH"] != "/opt/a:/opt/b" || got["FOO"] != "x=y" || len(got) != 2 {
		t.Fatalf("got %v err %v", got, err)
	}
	for _, bad := range []string{"NOVALUE", "=x", "API_KEY=abc", "MY_TOKEN=1"} {
		if _, err := ParseToolEnv(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestBwrapArgsIncludeToolMountsReadOnly(t *testing.T) {
	ws := t.TempDir()
	tools := t.TempDir()
	args, err := BuildBwrapArgs(ExecSpec{WorkspaceDir: ws, Command: "true", ToolMounts: []ToolMount{{Host: tools, Dest: "/opt/tools"}}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--ro-bind "+tools+" /opt/tools") {
		t.Fatalf("tool mount missing or not read-only: %s", joined)
	}
	if strings.Contains(joined, "--bind "+tools) {
		t.Fatalf("tool mount must never be writable: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind-try /etc/fonts /etc/fonts") {
		t.Fatalf("font configuration missing: %s", joined)
	}
	// The host's account list is never exposed: /etc/passwd comes from soma's own file.
	if strings.Contains(joined, "--ro-bind /etc/passwd") || strings.Contains(joined, "--ro-bind-try /etc/passwd") {
		t.Fatalf("host passwd must not be bound: %s", joined)
	}
}

// In the real sandbox: the current user has a name, tool mounts are readable and not writable,
// and the host's passwd is not what the command sees.
func TestSandboxIdentityAndToolMountLive(t *testing.T) {
	b := newBwrapBackend()
	if !b.Available() {
		t.Skip("bwrap not available")
	}
	ws := t.TempDir()
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "hello.txt"), []byte("from-tools"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `id -un; cat /opt/tools/hello.txt; echo; touch /opt/tools/x 2>/dev/null && echo WRITABLE || echo readonly; wc -l < /etc/passwd`
	res, err := b.Exec(context.Background(), ExecSpec{
		WorkspaceDir: ws, Command: "sh", Args: []string{"-c", script}, Timeout: 20 * time.Second,
		ToolMounts: []ToolMount{{Host: tools, Dest: "/opt/tools"}},
	})
	if err != nil {
		t.Fatalf("exec: %v (%s)", err, res.Combined)
	}
	out := strings.Fields(string(res.Stdout))
	if len(out) != 4 || out[0] != "soma" || out[1] != "from-tools" || out[2] != "readonly" || out[3] != "1" {
		t.Fatalf("unexpected sandbox view: %q (stderr %q)", res.Stdout, res.Stderr)
	}
}
