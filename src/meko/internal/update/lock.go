package update

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// Lock serializes the updates of one data folder, Meko's own and its extensions': an in-process
// mutex for serve's scheduled checks and the web UI's installs, and a flock on run/update.lock that
// meko update, meko extension update and delete, and meko extension install while it downloads a
// release, take too, in processes of their own. A check or an install holds it from the release list
// to the switch, and a switch to a new Meko keeps it until serve restarts.
type Lock struct {
	file string

	mu   sync.Mutex
	f    *os.File
	held sync.Mutex
}

// NewLock is the update lock of the data folder at dataDir.
func NewLock(dataDir string) *Lock { return &Lock{file: filepath.Join(dataDir, "run", "update.lock")} }

// TryLock takes the lock unless something holds it: this process (a check or an install running)
// or another meko process.
func (l *Lock) TryLock() bool {
	if !l.held.TryLock() {
		return false
	}
	if err := l.lockFile(); err != nil {
		l.held.Unlock()
		return false
	}
	return true
}

// Unlock lets the lock go.
func (l *Lock) Unlock() {
	l.mu.Lock()
	if l.f != nil {
		_ = l.f.Close() // closing the file lets the flock go
		l.f = nil
	}
	l.mu.Unlock()
	l.held.Unlock()
}

func (l *Lock) lockFile() error {
	if err := os.MkdirAll(filepath.Dir(l.file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return err
	}
	l.mu.Lock()
	l.f = f
	l.mu.Unlock()
	return nil
}

// ErrLocked is what an update refuses with while another holds the lock.
var ErrLocked = errors.New("another update is running (the server's own, or a meko update, or a meko " +
	"extension update, install or delete): try again when it's done")
