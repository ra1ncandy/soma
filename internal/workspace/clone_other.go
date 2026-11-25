//go:build !linux

package workspace

import "os"

func tryReflink(_, _ *os.File) bool { return false }
