// Package registry is the standing-environment registry (implementation §2
// M2/§3, architecture §5.5): a file-backed record of environments that
// outlive a single ampqa invocation — provisioned once, attached to later by
// `check`/`teardown`/manual use. File store now; a portal-backed
// implementation lands later behind the same shape (Add/Get/List/Remove).
package registry

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Entry is one registered standing environment.
type Entry struct {
	ID        string
	Name      string
	Shape     string
	CreatedAt time.Time
	TTL       time.Duration
	Provider  string
	Data      map[string]string // connection info: host, ssh_user, ssh_key, containers, ...
}

// Expired reports whether e's TTL has elapsed as of now. A zero or negative
// TTL never expires.
func (e Entry) Expired(now time.Time) bool {
	if e.TTL <= 0 {
		return false
	}
	return now.After(e.CreatedAt.Add(e.TTL))
}

// entryJSON is Entry's on-disk shape: TTL as a human-readable duration string
// instead of a bare nanosecond count.
type entryJSON struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Shape     string            `json:"shape"`
	CreatedAt time.Time         `json:"created_at"`
	TTL       string            `json:"ttl"`
	Provider  string            `json:"provider"`
	Data      map[string]string `json:"data,omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (e Entry) MarshalJSON() ([]byte, error) {
	return json.Marshal(entryJSON{
		ID: e.ID, Name: e.Name, Shape: e.Shape, CreatedAt: e.CreatedAt,
		TTL: e.TTL.String(), Provider: e.Provider, Data: e.Data,
	})
}

// UnmarshalJSON implements json.Unmarshaler.
func (e *Entry) UnmarshalJSON(b []byte) error {
	var raw entryJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var ttl time.Duration
	if raw.TTL != "" {
		d, err := time.ParseDuration(raw.TTL)
		if err != nil {
			return fmt.Errorf("entry %s: parse ttl %q: %w", raw.ID, raw.TTL, err)
		}
		ttl = d
	}
	*e = Entry{
		ID: raw.ID, Name: raw.Name, Shape: raw.Shape, CreatedAt: raw.CreatedAt,
		TTL: ttl, Provider: raw.Provider, Data: raw.Data,
	}
	return nil
}

// fileFormat is the whole registry file.
type fileFormat struct {
	Entries []Entry `json:"entries"`
}

// Lock-file tuning: writers serialize on a kernel-managed advisory lock over
// <path>.lock. lockTimeout bounds how long a writer waits for the lock;
// lockPollInterval is the non-blocking retry cadence used to give flock (whose
// LOCK_EX blocks without a timeout) a bounded wait.
const (
	lockPollInterval = 20 * time.Millisecond
	lockTimeout      = 10 * time.Second
)

// Registry is a file-backed CRUD store over Entry.
type Registry struct {
	path string
}

// New returns a Registry backed by the file at path (not required to exist yet).
func New(path string) *Registry { return &Registry{path: path} }

// DefaultPath is ~/.ampqa/envs.json.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".ampqa", "envs.json"), nil
}

func (r *Registry) load() ([]Entry, error) {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry %s: %w", r.path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", r.path, err)
	}
	return ff.Entries, nil
}

// withLock serializes a read-modify-write against a sibling lock file
// (<path>.lock) using a kernel-managed advisory lock (flock), so two concurrent
// writers (a `setup`, an `env rm`, a crashing run's crash-recovery record) can't
// each load-mutate-save over the other and silently drop an entry — which would
// orphan real infrastructure with no record to reap it by.
//
// flock is the right primitive precisely because non-clean exits must not wedge
// or leak: the kernel releases a holder's lock the instant its process dies
// (crash, SIGKILL, OOM), so the registry never gets stuck behind a dead writer
// and needs no racy mtime-based stale reclaim (an earlier remove/rename-by-name
// reclaim had a stat→act TOCTOU: two writers could both act on the lock NAME
// after it had come to point at a live holder's fresh lock, and both believe
// they held it). The lock file is persistent and never removed — removing it
// would break flock's inode-based exclusion. Read-only List/Get need no lock:
// save's atomic temp+rename means a reader always sees a complete old-or-new
// file.
func (r *Registry) withLock(fn func() error) (err error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(r.path), err)
	}
	lockPath := r.path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open registry lock %s: %w", lockPath, err)
	}
	defer func() { _ = f.Close() }()
	// LOCK_EX blocks without a timeout, so poll with LOCK_NB to bound the wait.
	deadline := time.Now().Add(lockTimeout)
	for {
		lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lerr == nil {
			break
		}
		if !errors.Is(lerr, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock registry %s: %w", lockPath, lerr)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registry lock held too long (%s): %s", lockTimeout, lockPath)
		}
		time.Sleep(lockPollInterval)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// save writes entries via a temp-file-then-rename (atomic on the same
// filesystem), so a reader never observes a partially-written file. Callers
// mutating the file (Add/Remove) hold withLock across their load+save so
// concurrent writers serialize instead of clobbering each other.
func (r *Registry) save(entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(r.path), err)
	}
	data, err := json.MarshalIndent(fileFormat{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%s", r.path, os.Getpid(), randSuffix())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// Add appends e, generating a short id when e.ID is empty and stamping
// CreatedAt when zero, and returns the stored entry. The load-append-save runs
// under the file lock so a concurrent writer can't drop the new entry.
func (r *Registry) Add(e Entry) (Entry, error) {
	if e.ID == "" {
		e.ID = randSuffix()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	err := r.withLock(func() error {
		entries, err := r.load()
		if err != nil {
			return err
		}
		return r.save(append(entries, e))
	})
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Get returns the entry with id, or (Entry{}, false, nil) if not found.
func (r *Registry) Get(id string) (Entry, bool, error) {
	entries, err := r.load()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range entries {
		if e.ID == id {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// List returns every entry, oldest first.
func (r *Registry) List() ([]Entry, error) {
	entries, err := r.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt.Before(entries[j].CreatedAt) })
	return entries, nil
}

// Remove deletes the entry with id, reporting whether one was found. The
// load-filter-save runs under the file lock so a concurrent writer's entry is
// not dropped by this Remove's save.
func (r *Registry) Remove(id string) (bool, error) {
	removed := false
	err := r.withLock(func() error {
		entries, err := r.load()
		if err != nil {
			return err
		}
		out := entries[:0]
		for _, e := range entries {
			if e.ID == id {
				removed = true
				continue
			}
			out = append(out, e)
		}
		if !removed {
			return nil
		}
		return r.save(out)
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

func randSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
