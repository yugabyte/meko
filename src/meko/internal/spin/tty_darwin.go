package spin

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
	// ioctlFlushTermios sets them after discarding input the terminal sent that wasn't read.
	ioctlFlushTermios = unix.TIOCSETAF
)
