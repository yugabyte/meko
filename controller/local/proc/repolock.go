package proc

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// RepoLock is an flock-based guard over the yb-amp repository directory (the
// ".yb_amp/" dir). yb-amp stores its state in a collection of plain files, so
// it is not safe to run multiple commands concurrently; callers take this lock
// for the duration of a command. Mirrors the RepoLock in
// control_plane/src/bin/yb_amp_local.rs.
//
// The lock is an advisory exclusive flock held for as long as the directory
// descriptor is open; Close releases it.
type RepoLock struct {
	dir *os.File
}

// LockRepo opens repoDir and takes a blocking exclusive flock on it. It blocks
// until any other holder releases the lock.
func LockRepo(repoDir string) (*RepoLock, error) {
	dir, err := os.Open(repoDir)
	if err != nil {
		return nil, fmt.Errorf("no yb-amp repo at %q — run `init` first: %w", repoDir, err)
	}
	if err := unix.Flock(int(dir.Fd()), unix.LOCK_EX); err != nil {
		dir.Close()
		return nil, fmt.Errorf("flock repo dir %q: %w", repoDir, err)
	}
	return &RepoLock{dir: dir}, nil
}

// Close releases the repo lock.
func (l *RepoLock) Close() error {
	if l.dir == nil {
		return nil
	}
	err := l.dir.Close() // closing the fd releases the flock
	l.dir = nil
	return err
}
