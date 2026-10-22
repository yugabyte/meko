// Package store is Meko's only door to SQLite: every query lives here behind typed
// methods, and no other package imports the driver.
//
// One write connection takes every write (SQLite allows one writer at a time, and a single
// connection keeps transactions from waiting on locks); a pool of readers serves reads. The
// file is in WAL mode, and sqlite-vec and FTS5 are compiled in: the driver is the
// yugabyte/go-sqlite3 fork (go.mod's replace), built with the sqlite_fts5 and sqlite_vec tags.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver
)

// Readers is the size of the read pool. Searches (vector and keyword scans, which can take
// hundreds of milliseconds in a large datapack) may hold at most SearchSlots of them at once, so
// the rest always serve plain reads: a slow search never makes a list or a get wait.
const (
	Readers     = 8
	SearchSlots = 4
)

// errNoVec is what a binary built without the sqlite_vec tag gets: SQLite without sqlite-vec.
var errNoVec = errors.New(`SQLite has no sqlite-vec: build with -tags sqlite_fts5,sqlite_vec (see backend/Makefile)`)

// vecErr names the missing build tag when a query failed because sqlite-vec isn't compiled in.
func vecErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "no such function: vec_version") {
		return errNoVec
	}
	return err
}

// Store is an open database.
type Store struct {
	path string
	w    *sql.DB
	r    *sql.DB
	dims atomic.Int64 // the vector tables' width once known (vectors.go)
	// search holds a token per search running (searchSlot).
	search chan struct{}
}

// Open opens (creating it if needed, readable only by this user) the database at path.
// Call Migrate before using it.
func Open(path string) (*Store, error) {
	// Create the file first so SQLite (and its -wal and -shm files, which copy its mode) is 0600.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite3", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err = w.Ping(); err != nil {
		w.Close()
		return nil, fmt.Errorf("can't open %s: %w", path, err)
	}
	if err = vecErr(w.QueryRow("SELECT vec_version()").Scan(new(string))); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite3", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(Readers)
	return &Store{path: path, w: w, r: r, search: make(chan struct{}, SearchSlots)}, nil
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	if readOnly {
		q.Set("_query_only", "true")
	} else {
		q.Set("_txlock", "immediate")
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

// searchSlot waits for one of the SearchSlots search slots, or for ctx to end, and returns the
// function that frees it.
func (s *Store) searchSlot(ctx context.Context) (func(), error) {
	select {
	case s.search <- struct{}{}:
		return func() { <-s.search }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// Checkpoint copies the WAL into the database file and truncates it.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Close closes every connection.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// Versions are the SQLite and sqlite-vec versions compiled into the binary.
type Versions struct {
	SQLite    string `json:"sqlite_version"`
	SQLiteVec string `json:"sqlite_vec_version"`
}

// LibVersions reads the versions from an in-memory database, so it works without a data folder.
func LibVersions() (Versions, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return Versions{}, err
	}
	defer db.Close()
	var v Versions
	err = db.QueryRow("SELECT sqlite_version(), vec_version()").Scan(&v.SQLite, &v.SQLiteVec)
	return v, vecErr(err)
}
