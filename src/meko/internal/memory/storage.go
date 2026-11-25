package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// The storage limit ([storage] max_content_mb, 200 MB by default): the most content Meko keeps, so
// its searches stay as fast as they were measured at that size. Content is what callers send
// (store.ContentUsage); indexes come on top. Every write that adds content reserves its bytes first
// (reserve), and holds them until it's done, so writes in flight at once count against the limit
// together; an upload link holds its file's size until the file arrives or the link expires. Nothing
// is ever deleted to get under the limit.

// ErrStorageFull is a write that would take Meko past its storage limit. The error returned is a
// *StorageFullError, which says the limit.
var ErrStorageFull = errors.New("meko's storage is full")

// StorageFullError is ErrStorageFull with the limit, in words a caller can pass on.
type StorageFullError struct {
	Limit int64
	// Need is the bytes the refused write needed.
	Need int64
	// Linked says Meko Cloud is linked, so new work can go there now.
	Linked bool
}

func (e *StorageFullError) Error() string {
	next := "link Meko Cloud for larger datasets"
	if e.Linked {
		next = "keep new work in a Meko Cloud datapack"
	}
	return fmt.Sprintf("Meko is full: its storage limit is %s. Delete memories, conversations or files you "+
		"don't need, or %s.", sizeText(e.Limit), next)
}

// Is makes errors.Is(err, ErrStorageFull) true.
func (e *StorageFullError) Is(target error) bool { return target == ErrStorageFull }

// sizeText is a limit in the unit it was set in: whole MB from config.toml, finer only in tests.
func sizeText(n int64) string {
	switch {
	case n > 0 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n > 0 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// StorageStatus is how full Meko is: the content it holds, by kind, and its limit.
type StorageStatus struct {
	UsedBytes  int64 `json:"used_bytes"`
	LimitBytes int64 `json:"limit_bytes"`
	store.ContentUsage
}

// Storage says how full Meko is. Bytes that writes in flight and upload links hold aren't used yet,
// so they're left out.
func (s *Service) Storage(ctx context.Context) (StorageStatus, error) {
	u, err := s.st.ContentUsage(ctx)
	if err != nil {
		return StorageStatus{}, err
	}
	return StorageStatus{UsedBytes: u.Total(), LimitBytes: s.maxContent, ContentUsage: u}, nil
}

// reserve holds n bytes of the limit for a write about to be made, or returns a *StorageFullError
// when they'd take Meko past it. The caller calls release once its write is done or failed: till
// then the bytes count against the limit for every other write, so writes in flight at once can't
// all fit in the same room.
func (s *Service) reserve(ctx context.Context, n int64) (release func(), err error) {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	if err = s.roomLocked(ctx, n); err != nil {
		return func() {}, err
	}
	s.reserved += n
	return func() {
		s.roomMu.Lock()
		defer s.roomMu.Unlock()
		s.reserved -= n
	}, nil
}

// room returns a *StorageFullError when n more bytes wouldn't fit, without holding them. With n 0 it
// asks whether Meko is already at its limit.
func (s *Service) room(ctx context.Context, n int64) error {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	return s.roomLocked(ctx, n)
}

// roomLocked is room, under roomMu: what's stored, what writes in flight reserve and what upload links
// hold, against the limit.
func (s *Service) roomLocked(ctx context.Context, n int64) error {
	u, err := s.st.ContentUsage(ctx)
	if err != nil {
		return err
	}
	used := u.Total() + s.reserved + s.tickets.held()
	if used+n > s.maxContent || n == 0 && used >= s.maxContent {
		return &StorageFullError{Limit: s.maxContent, Need: n, Linked: s.cloudLinked != nil && s.cloudLinked()}
	}
	return nil
}

// jsonSize is how many bytes v takes as the JSON the store keeps it as (0 for nothing).
func jsonSize(v any) int64 {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}
