//go:build linux

package cli

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

func probeSandboxEnvironment() (bwrapStatus string, landlockABI int, ready bool) {
	if path, err := exec.LookPath("bwrap"); err == nil {
		bwrapStatus = fmt.Sprintf("available (%s)", path)
		ready = true
	} else {
		bwrapStatus = "not found"
	}

	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno == 0 {
		landlockABI = int(abi)
		if landlockABI >= 4 {
			ready = true
		}
	}
	return bwrapStatus, landlockABI, ready
}
