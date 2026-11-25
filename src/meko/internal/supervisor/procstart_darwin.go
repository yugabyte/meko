package supervisor

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStart identifies when pid started: its start time to the microsecond, from the kernel's
// process table, so a reused pid doesn't match it.
func processStart(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	t := kp.Proc.P_starttime
	if t.Sec == 0 {
		return "", fmt.Errorf("no process %d", pid)
	}
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), nil
}
