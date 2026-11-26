//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the in-sandbox forwarder, exactly like cmd/soma.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ForwarderCommand {
		os.Exit(RunNetForwarder(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// With network enabled, sandboxed code must still be unable to reach services on the host's
// loopback interface (such as the dashboard): not directly, and not through the egress proxy.
func TestNetworkedSandboxCannotReachHostLoopback(t *testing.T) {
	mgr := NewManagerForMode("bwrap")
	if mgr.ActiveBackend() == nil {
		t.Skip("bwrap unavailable")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := ForwarderBinary
	ForwarderBinary = exe
	defer func() { ForwarderBinary = prev }()

	dashboard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("DASHBOARD-SECRET"))
	}))
	defer dashboard.Close()
	port := dashboard.Listener.Addr().(*net.TCPAddr).Port

	script := fmt.Sprintf(`
import os, socket, urllib.request
r = []
try:
    s = socket.create_connection(("127.0.0.1", %d), timeout=3); r.append("direct=OPEN")
except Exception as e:
    r.append("direct=closed")
try:
    body = urllib.request.urlopen("http://127.0.0.1:%d/", timeout=5).read().decode()
    r.append("proxy=" + body)
except Exception as e:
    r.append("proxy=refused:" + type(e).__name__)
r.append("proxyenv=" + os.environ.get("HTTP_PROXY", ""))
print(" ".join(r))
`, port, port)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := mgr.Exec(ctx, ExecSpec{WorkspaceDir: t.TempDir(), Command: "python3", Args: []string{"-c", script}, AllowNetwork: true, Timeout: 50 * time.Second})
	if err != nil {
		t.Fatalf("exec: %v\n%s", err, res.Combined)
	}
	out := string(res.Combined)
	if strings.Contains(out, "DASHBOARD-SECRET") || strings.Contains(out, "direct=OPEN") {
		t.Fatalf("networked sandbox reached the host loopback service:\n%s", out)
	}
	if !strings.Contains(out, "direct=closed") || !strings.Contains(out, "proxy=refused") || !strings.Contains(out, "proxyenv=http://127.0.0.1:3128") {
		t.Fatalf("unexpected sandbox output:\n%s", out)
	}
}

func TestBlockedEgressIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "64:ff9b::a00:1", "2002:a00::1", "198.18.0.1"} {
		if !BlockedEgressIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700::1111"} {
		if BlockedEgressIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}
