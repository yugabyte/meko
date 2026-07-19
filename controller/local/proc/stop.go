package proc

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// StopMode selects the signal used to stop a process.
type StopMode int

const (
	// StopFast sends SIGTERM for a graceful shutdown.
	StopFast StopMode = iota
	// StopImmediate sends SIGQUIT for an immediate shutdown.
	StopImmediate
)

// stop-loop parameters, matching background_process.rs: wait up to 10s, polling
// every retryInterval.
const (
	stopRetryTimeout = 10 * time.Second
	stopRetries      = int(stopRetryTimeout / retryInterval)
)

// StopProcess reads the pid from "<dataDir>/<name>.pid" and signals the process
// to stop (SIGTERM for StopFast, SIGQUIT for StopImmediate), then waits until it
// is gone. It returns nil if the process is already stopped. Mirrors
// stop_process in background_process.rs.
//
// The pidfile is never unlinked: a live process is detected via its flock, and
// removing the file would open a race where two "singleton" processes run at
// once (see the pid_file module comment in the Rust tree).
func StopProcess(name string, dataDir string, mode StopMode) error {
	pidFilePath := filepath.Join(dataDir, name+".pid")

	kind, pid, err := readPidFile(pidFilePath)
	if err != nil {
		return fmt.Errorf("read pid_file %q: %w", pidFilePath, err)
	}
	switch kind {
	case pidNotExist:
		fmt.Printf("%s is already stopped: no pid file present at %q\n", name, pidFilePath)
		return nil
	case pidNotHeld:
		// The pid might have been reused by another process, so don't kill by
		// file contents, and don't delete the file (racy). See module comment.
		fmt.Printf("No process is holding the pidfile. The process must have already exited. "+
			"Leave in place to avoid race conditions: %q\n", pidFilePath)
		return nil
	}
	// kind == pidLocked; pid is valid.
	// XXX the pid could become invalid (and recycled) at any time before the kill below.

	sig := unix.SIGTERM
	if mode == StopImmediate {
		fmt.Printf("Stopping %s with pid %d immediately..", name, pid)
		sig = unix.SIGQUIT
	} else {
		fmt.Printf("Stopping %s with pid %d gracefully..", name, pid)
	}

	if err := unix.Kill(pid, sig); err != nil {
		if errors.Is(err, unix.ESRCH) {
			// The pid got recycled; nothing of ours to kill. Don't delete the
			// pidfile (racy).
			fmt.Printf("\n%s with pid %d does not exist, but a pid file %q was found. "+
				"Likely the pid got recycled.\n", name, pid, pidFilePath)
			return nil
		}
		return fmt.Errorf("failed to send signal to %s with pid %d: %w", name, pid, err)
	}

	return waitUntilStopped(name, pid)
}

// waitUntilStopped polls until the process with the given pid is gone.
func waitUntilStopped(name string, pid int) error {
	for i := 0; i < stopRetries; i++ {
		stopped, err := processHasStopped(pid)
		if err != nil {
			return fmt.Errorf("%s with pid %d failed to stop: %w", name, pid, err)
		}
		if stopped {
			fmt.Printf("\n%s stopped\n", name)
			return nil
		}
		if i == noticeAfterRetries {
			fmt.Printf("\n%s has not stopped yet, continuing to wait", name)
		}
		if i%dotEveryRetries == 0 {
			fmt.Print(".")
		}
		time.Sleep(retryInterval)
	}
	fmt.Println()
	return fmt.Errorf("%s with pid %d did not stop in %s", name, pid, stopRetryTimeout)
}

// processHasStopped reports whether the process with the given pid is gone,
// probing it with signal 0.
func processHasStopped(pid int) (bool, error) {
	err := unix.Kill(pid, 0)
	switch {
	case err == nil:
		return false, nil // still alive
	case errors.Is(err, unix.ESRCH):
		return true, nil // gone
	default:
		return false, fmt.Errorf("failed to signal pid %d: %w", pid, err)
	}
}
