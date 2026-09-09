package cgroups_test

import (
	"io/fs"
	"syscall"
)

// ownedBy reports whether a file belongs to one uid. It is its own file
// because the answer comes from a system dependent stat structure and the test
// itself should read like the sentence it checks.
func ownedBy(info fs.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == uid
}
