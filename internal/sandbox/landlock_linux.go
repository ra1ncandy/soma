//go:build linux

package sandbox

import (
	"context"

	"golang.org/x/sys/unix"
)

// ProbeLandlockABI queries the running Linux kernel for its supported Landlock LSM ABI version.
func ProbeLandlockABI() int {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(abi)
}

type landlockBackend struct {
	abi int
}

func newLandlockBackend() Backend {
	return &landlockBackend{abi: ProbeLandlockABI()}
}

func (l *landlockBackend) Name() string { return "landlock" }

// Available returns false when used without a child-exec hook, ensuring fail-closed fallback
// rather than executing unprotected processes when bwrap is absent.
func (l *landlockBackend) Available() bool { return false }

// SupportsNetworkIsolation requires Linux Landlock ABI 4+ (TCP bind/connect scoping).
func (l *landlockBackend) SupportsNetworkIsolation() bool { return l.abi >= 4 }

// SupportsIPCScoping requires Linux Landlock ABI 6+ (abstract unix socket & signal scoping).
func (l *landlockBackend) SupportsIPCScoping() bool { return l.abi >= 6 }

func (l *landlockBackend) Exec(_ context.Context, spec ExecSpec) (ExecResult, error) {
	if !spec.AllowNetwork && !l.SupportsNetworkIsolation() {
		return ExecResult{}, ErrSandboxUnavailable
	}
	return ExecResult{}, ErrSandboxUnavailable
}
