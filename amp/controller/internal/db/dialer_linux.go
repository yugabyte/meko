//go:build linux

package db

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setSocketTimeouts applies TCP_USER_TIMEOUT to a freshly created socket. The
// controller runs on Linux in every deployment; the fallback exists so the
// package still builds (and its tests still run) on a developer's macOS box,
// where the recovery bound is the pool-level one alone.
func setSocketTimeouts(_, _ string, c syscall.RawConn) error {
	var setErr error
	if err := c.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT,
			int(tcpUserTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return setErr
}
