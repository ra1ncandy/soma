//go:build !windows

package dag

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts cmd in its own process group so killProcTree can reach its children.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcTree SIGKILLs cmd's entire process group.
func killProcTree(cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
