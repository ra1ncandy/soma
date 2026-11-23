package desktop

import (
	"strings"
	"testing"
)

func TestBuildRunArgs_HardenedLoopbackAndMounts(t *testing.T) {
	ws := t.TempDir()
	name := ContainerNameForWorkspace(ws)
	if !strings.HasPrefix(name, "soma-desktop-") {
		t.Fatalf("unexpected container name: %q", name)
	}
	args, err := BuildRunArgs(Config{
		WorkspaceDir: ws,
		NoVNCPort:    6085,
		Resolution:   "1440x900",
		NetworkMode:  "none",
	}, name, "secretpass")
	if err != nil {
		t.Fatalf("BuildRunArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--shm-size=2g",
		"--network none",
		"-p 127.0.0.1:6085:6080",
		":/workspace",
		"SCREEN_WIDTH=1440",
		"SCREEN_HEIGHT=900",
		"SOMA_VNC_PASSWORD=secretpass",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in docker args: %s", want, joined)
		}
	}

	st := State{ContainerName: name, NoVNCPort: 6085, VNCPassword: "secretpass"}
	if !strings.HasPrefix(st.NoVNCURL(), "http://127.0.0.1:6085/vnc.html?") {
		t.Fatalf("unexpected NoVNCURL: %q", st.NoVNCURL())
	}

	mgr := &Manager{}
	eps := mgr.MCPEndpoints(st, true)
	if len(eps) != 2 || !eps[0].IsolatedDesktop || !eps[1].IsolatedDesktop {
		t.Fatalf("expected 2 isolated desktop MCP endpoints, got %+v", eps)
	}
}
