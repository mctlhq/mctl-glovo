//go:build !unix

package glovologic

import "io/fs"

// ownedByMe has no portable owner check off unix; the mode and symlink
// checks still apply.
func ownedByMe(fs.FileInfo) bool { return true }
