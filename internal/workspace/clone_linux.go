//go:build linux

package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

// tryReflink makes dst share src's data blocks (copy-on-write) on filesystems that support it
// (btrfs, XFS, bcachefs). It costs no I/O proportional to file size and no extra disk space until
// one side is modified. Returns false when the filesystem cannot do it, so the caller copies bytes.
func tryReflink(dst, src *os.File) bool {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}
