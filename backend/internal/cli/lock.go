package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/supervisor"
)

// lockedError means another server holds the data folder's lock.
type lockedError struct {
	root string
	pid  int
}

func (e *lockedError) Error() string {
	if e.pid > 0 {
		return fmt.Sprintf("Meko is already running for %s (pid %d)", e.root, e.pid)
	}
	return fmt.Sprintf("Meko is already running for %s", e.root)
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

// lockWait is how long serve retries a held lock. The CLI's own checks (running) take the lock for
// an instant, so a serve starting during a status or stop must not mistake that for a server.
var lockWait = 2 * time.Second

// acquireServeLock is acquireLock for serve: it retries a held lock for lockWait.
func acquireServeLock(path string) (*os.File, error) {
	deadline := time.Now().Add(lockWait)
	for {
		f, err := acquireLock(path)
		if !errors.Is(err, errLocked) || time.Now().After(deadline) {
			return f, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cleanRunFiles kills the children a killed server left (their process groups, which the server's
// own kill didn't reach) and removes its run files. It holds the lock while it does, so a server
// starting at the same moment can't lose the files it just wrote or the engine it just started. It
// does nothing if a server holds the lock.
func cleanRunFiles(l paths.Layout) {
	if f, err := acquireLock(l.Lock); err == nil {
		_, _ = supervisor.KillRecorded(l.ChildrenDir)
		removeRunFiles(l)
		releaseLock(f)
	}
}

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
