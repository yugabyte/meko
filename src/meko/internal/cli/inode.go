package cli

import (
	"os"
	"syscall"
)

// inode is a file's inode number, which tells a rotated log from the one that replaced it.
func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}
