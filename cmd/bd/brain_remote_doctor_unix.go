//go:build !windows

package main

import (
	"os"
	"syscall"
)

// fileOwnerUID returns the uid that owns fi, or -1 when the platform does not
// expose it. The client doctor uses it to notice that it is reading another
// user's config file (the "client read /Users/someone-else/.config" bug class).
func fileOwnerUID(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
