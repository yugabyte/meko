// Package logging sets up slog: JSON lines in a rotated file, and text on stderr while serve
// runs in a terminal. Logs carry ids and counts, never keys, tokens or memory text.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// Rotation defaults for logs/meko.log.
const (
	MaxBytes = 10 << 20
	Keep     = 5
)

// New returns a logger writing JSON to file and, if stderr isn't nil, text to stderr.
func New(file, stderr io.Writer, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	hs := []slog.Handler{slog.NewJSONHandler(file, opts)}
	if stderr != nil {
		hs = append(hs, slog.NewTextHandler(stderr, opts))
	}
	if len(hs) == 1 {
		return slog.New(hs[0])
	}
	return slog.New(fanout(hs))
}

// fanout sends each record to every handler.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// RotatingFile appends to path and, when a write would take it past maxBytes, renames it to
// path.1 (path.1 to path.2, and so on), keeping keep old files.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// OpenRotating opens path for appending, creating it readable only by this user.
func OpenRotating(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	r := &RotatingFile{path: path, maxBytes: maxBytes, keep: keep}
	return r, r.open()
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

// Write appends p, rotating first if it wouldn't fit.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil && r.f == nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the files along and opens a new one. If a rename fails it reopens the current
// file, so logging carries on in it rather than stopping for the rest of the run.
func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		r.f = nil
		return errors.Join(err, r.open())
	}
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	var err error
	if r.keep > 0 {
		err = os.Rename(r.path, r.path+".1")
	} else {
		err = os.Remove(r.path)
	}
	if oerr := r.open(); oerr != nil {
		r.f = nil
		return errors.Join(err, oerr)
	}
	return err
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
