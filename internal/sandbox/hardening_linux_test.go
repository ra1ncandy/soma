//go:build linux

package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBwrapArgsCarryIsolationFlags(t *testing.T) {
	args, err := BuildBwrapArgs(ExecSpec{WorkspaceDir: t.TempDir(), Command: "/bin/true"})
	if err != nil {
		t.Fatalf("BuildBwrapArgs: %v", err)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, flag := range []string{
		"--unshare-net", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--new-session", "--cap-drop ALL", "--die-with-parent", "--clearenv",
	} {
		if !strings.Contains(joined, " "+flag+" ") {
			t.Errorf("bwrap args missing %q: %s", flag, joined)
		}
	}

	withNet, err := BuildBwrapArgs(ExecSpec{WorkspaceDir: t.TempDir(), Command: "/bin/true", AllowNetwork: true})
	if err != nil {
		t.Fatalf("BuildBwrapArgs: %v", err)
	}
	if strings.Contains(strings.Join(withNet, " "), "--unshare-net") {
		t.Error("AllowNetwork=true must not unshare the network namespace")
	}
}

func TestManagerCommandFailsClosed(t *testing.T) {
	// The Landlock backend is probe-only and must never be selected to run a command.
	mgr := &Manager{FailIfUnavailable: true, Backends: []Backend{newLandlockBackend()}}
	if _, _, err := mgr.Command(context.Background(), ExecSpec{WorkspaceDir: t.TempDir(), Command: "/bin/true"}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("want ErrSandboxUnavailable, got %v", err)
	}
}
