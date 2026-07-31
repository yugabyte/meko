package proc

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// pidReadKind describes the result of inspecting a pidfile. It mirrors the
// PidFileRead / LockFileRead enums in compute/libs/utils/src/{pid_file,lock_file}.rs.
type pidReadKind int

const (
	// pidNotExist: no pidfile at the given path.
	pidNotExist pidReadKind = iota
	// pidNotHeld: the pidfile exists but no process holds an flock on it, so
	// the process that wrote it has exited.
	pidNotHeld
	// pidLocked: the pidfile is flocked by a live process; the returned pid is
	// its content.
	pidLocked
)

// readPidFile opens the pidfile at path and probes it with a non-blocking
// exclusive flock to decide whether a process currently holds it.
//
// A successful flock means nobody holds the file (the writer exited), so we
// release it immediately and report pidNotHeld. EWOULDBLOCK means a live
// process holds the lock, in which case we read and parse its pid.
//
// Mirrors lock_file::read_and_hold_lock_file + pid_file::read. Unlike the Rust
// version we do not retain the guard in the not-held case, since the launcher
// only needs the classification.
func readPidFile(path string) (pidReadKind, int, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pidNotExist, 0, nil
		}
		return pidNotExist, 0, fmt.Errorf("open pid file %q: %w", path, err)
	}
	defer f.Close()

	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		// We got the lock, so no live process holds it. Release and report.
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return pidNotHeld, 0, nil
	}
	if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		return pidNotExist, 0, fmt.Errorf("flock pid file %q: %w", path, err)
	}

	// Locked by another process; read its pid.
	content, err := os.ReadFile(path)
	if err != nil {
		return pidNotExist, 0, fmt.Errorf("read pid file %q: %w", path, err)
	}
	pid, err := parsePidContent(string(content))
	if err != nil {
		return pidNotExist, 0, fmt.Errorf("pid file %q: %w", path, err)
	}
	return pidLocked, pid, nil
}

func parsePidContent(content string) (int, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(content))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile content to PID: %w", err)
	}
	if pid < 1 {
		return 0, fmt.Errorf("bad value in pidfile %q", content)
	}
	return pid, nil
}

// createLockedPidFile creates (or truncates) the pidfile at path and takes a
// non-blocking exclusive flock on it. The returned file's descriptor is passed
// to the child via exec.Cmd.ExtraFiles so the child inherits the flock (a
// shared open file description keeps the lock alive after the launcher closes
// its own copy). Mirrors pre_exec_create_pidfile / claim_for_current_process,
// except the launcher (not the child) performs the claim.
//
// FD_CLOEXEC is cleared so the descriptor survives the child's exec; Go's
// os/exec already inherits ExtraFiles without CLOEXEC, but we clear it
// explicitly to match the Rust behaviour and to be robust.
func createLockedPidFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open pid file %q: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("pid file %q is already locked", path)
		}
		return nil, fmt.Errorf("flock pid file %q: %w", path, err)
	}
	// Remove FD_CLOEXEC so the lock survives the child's exec().
	if _, err := unix.FcntlInt(f.Fd(), unix.F_SETFD, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("clear FD_CLOEXEC on %q: %w", path, err)
	}
	return f, nil
}

// writePid replaces the pidfile content with pid and fsyncs it.
func writePid(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate pid file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(pid)), 0); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync pid file: %w", err)
	}
	return nil
}
