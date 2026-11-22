//go:build windows

package dag

import "os/exec"

func setProcGroup(_ *exec.Cmd) {}

func killProcTree(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
