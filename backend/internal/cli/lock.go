package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// lockedError means another server holds the data folder's lock.
type lockedError struct {
	root string
	pid  int
}

func (e *lockedError) Error() string {
	if e.pid > 0 {
		return fmt.Sprintf("Meko Local is already running for %s (pid %d)", e.root, e.pid)
	}
	return fmt.Sprintf("Meko Local is already running for %s", e.root)
}

// acquireLock takes the exclusive lock serve holds until it exits. The lock, not the pid file,
// says whether a server runs: the kernel drops it when the process dies, however it dies.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}

var errLocked = errors.New("locked")

func releaseLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close() // the lock file holds nothing
}

// running reports whether a server holds the lock at path.
func running(path string) (bool, error) {
	f, err := acquireLock(path)
	switch {
	case errors.Is(err, errLocked):
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	releaseLock(f)
	return false, nil
}

func readInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("%s doesn't hold a number", path)
	}
	return n, nil
}
