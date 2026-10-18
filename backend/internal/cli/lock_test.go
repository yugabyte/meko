package cli

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/yugabyte/meko-local/backend/internal/paths"
)

// serve waits out the instant lock the CLI's checks take, but not a lock a server keeps.
func TestServeLockRetries(t *testing.T) {
	l := paths.New(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	shorten(t, &lockWait, 2*time.Second)
	probe, err := acquireLock(l.Lock)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, func() { releaseLock(probe) })
	lock, err := acquireServeLock(l.Lock)
	if err != nil {
		t.Fatalf("serve didn't get the lock once the probe let go: %v", err)
	}
	defer releaseLock(lock)

	shorten(t, &lockWait, 200*time.Millisecond)
	if _, err := acquireServeLock(l.Lock); !errors.Is(err, errLocked) {
		t.Fatalf("a lock a server keeps: got %v, want errLocked", err)
	}
}

// cleanRunFiles leaves a running server's files alone, and removes a killed one's.
func TestCleanRunFiles(t *testing.T) {
	l := paths.New(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := paths.WriteFile(l.Pid, []byte("1\n")); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(l.Lock)
	if err != nil {
		t.Fatal(err)
	}
	cleanRunFiles(l)
	if _, err := os.Stat(l.Pid); err != nil {
		t.Fatalf("removed a running server's pid file: %v", err)
	}
	releaseLock(lock)
	cleanRunFiles(l)
	if _, err := os.Stat(l.Pid); !os.IsNotExist(err) {
		t.Fatalf("left a killed server's pid file: %v", err)
	}
}
