package cli

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
	// ioctlFlushTermios sets them after discarding input the terminal sent that wasn't read.
	ioctlFlushTermios = unix.TCSETSF
)
