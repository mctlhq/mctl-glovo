//go:build unix

package glovologic

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByMe reports whether st belongs to the effective user.
func ownedByMe(st fs.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Geteuid()
}
