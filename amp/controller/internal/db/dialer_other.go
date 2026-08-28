//go:build !linux

package db

import "syscall"

// setSocketTimeouts is a no-op off Linux: TCP_USER_TIMEOUT has no portable
// equivalent. See the Linux implementation for what production actually gets.
func setSocketTimeouts(_, _ string, _ syscall.RawConn) error { return nil }
