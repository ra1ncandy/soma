//go:build !linux

package cli

import (
	"fmt"
	"os/exec"
	"runtime"
)

func probeSandboxEnvironment() (bwrapStatus string, landlockABI int, ready bool) {
	if runtime.GOOS == "darwin" {
		if path, err := exec.LookPath("sandbox-exec"); err == nil {
			return fmt.Sprintf("seatbelt (%s)", path), 0, true
		}
		return "seatbelt not found", 0, false
	}
	return "unsupported host OS (use WSL2)", 0, false
}
