package registry

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// plantLeftoverLock writes the sibling lock file as if a previous (now dead)
// writer left it behind. With a kernel-managed advisory lock the file is
// unlocked once its holder's process exited, so a leftover file must never
// wedge a new writer — withLock reuses it rather than treating it as held.
func plantLeftoverLock(t *testing.T, regPath string) {
	t.Helper()
	if err := os.WriteFile(regPath+".lock", nil, 0o644); err != nil {
		t.Fatalf("plant lock: %v", err)
	}
}

// TestWithLockIsMutuallyExclusive races many writers against a leftover lock
// file and asserts the lock never lets two of them into the critical section at
// once — the invariant the racy mtime-based reclaim used to violate (two writers
// could both act on the lock name and both believe they held it).
func TestWithLockIsMutuallyExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "envs.json")
	plantLeftoverLock(t, path)
	r := New(path)

	const writers = 12
	var (
		mu        sync.Mutex
		active    int
		maxActive int
		wg        sync.WaitGroup
	)
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.withLock(func() error {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()
				time.Sleep(time.Millisecond) // widen the window for overlap to show
				mu.Lock()
				active--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("withLock returned error: %v", err)
		}
	}
	if maxActive != 1 {
		t.Fatalf("lock allowed %d concurrent holders; want exactly 1", maxActive)
	}
}

// TestConcurrentAddNoLostEntry is the outcome view: N concurrent Adds against a
// leftover lock file must all persist — none clobbered by a co-writer's
// load-modify-save.
func TestConcurrentAddNoLostEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "envs.json")
	plantLeftoverLock(t, path)
	r := New(path)

	const n = 16
	var wg sync.WaitGroup
	var added atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Add(Entry{Shape: "docker", Provider: "docker"}); err != nil {
				t.Errorf("Add: %v", err)
				return
			}
			added.Add(1)
		}()
	}
	wg.Wait()

	if got := added.Load(); got != n {
		t.Fatalf("only %d of %d Adds succeeded", got, n)
	}
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("registry lost entries under concurrent writes: have %d, want %d", len(entries), n)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.ID] {
			t.Fatalf("duplicate entry id %s — a clobbering write", e.ID)
		}
		seen[e.ID] = true
	}
}
