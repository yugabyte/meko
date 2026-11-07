package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/store"
)

// storageService is a service over a fresh store, with no context engine, and a limit of limit bytes.
func storageService(t *testing.T, limit int64, linked bool) (*Service, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dp, err := st.EnsureDefaultDatapack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return New(st, nil, Options{FilesDir: t.TempDir(), MaxContentBytes: limit,
		CloudLinked: func() bool { return linked }}), dp
}

// Writes in flight reserve their bytes: two that each fit alone don't both get through, and a
// release gives the room back.
func TestReserveCountsWritesInFlight(t *testing.T) {
	ctx := context.Background()
	s, dp := storageService(t, 100, false)
	release, err := s.reserve(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.reserve(ctx, 60)
	var full *StorageFullError
	if !errors.As(err, &full) || full.Need != 60 || full.Linked ||
		!strings.Contains(err.Error(), "link Meko Cloud for larger datasets") {
		t.Fatalf("a second write in flight: %v", err)
	}
	// An artifact counts what's stored plus what's in flight.
	if _, err = s.PutArtifact(ctx, dp, "a", "f.txt", "text/plain", make([]byte, 50)); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("an artifact past what's reserved: %v", err)
	}
	release()
	if _, err = s.PutArtifact(ctx, dp, "a", "f.txt", "text/plain", make([]byte, 50)); err != nil {
		t.Fatalf("after the release: %v", err)
	}
	if s.reserved != 0 {
		t.Errorf("left reserved: %d", s.reserved)
	}
}

// While Meko Cloud is linked, the error says to keep new work there.
func TestStorageFullWhileLinked(t *testing.T) {
	s, _ := storageService(t, 100, true)
	err := s.room(context.Background(), 200)
	if !errors.Is(err, ErrStorageFull) || !strings.Contains(err.Error(), "keep new work in a Meko Cloud datapack") {
		t.Fatalf("linked: %v", err)
	}
}

// A refused save sets the room the next extraction run needs; a run that finishes resets it.
func TestNoteFull(t *testing.T) {
	s, _ := storageService(t, 100, false)
	if s.extractNeed != extractRoom {
		t.Fatalf("starts at %d", s.extractNeed)
	}
	s.noteFull(&StorageFullError{Limit: 100, Need: 5000})
	if s.extractNeed != 5000 {
		t.Errorf("after a refusal of 5000 bytes: %d", s.extractNeed)
	}
	s.noteFull(&StorageFullError{Limit: 100, Need: 10})
	if s.extractNeed != extractRoom {
		t.Errorf("a small refusal still needs extractRoom: %d", s.extractNeed)
	}
	s.noteFull(errors.New("other"))
	if s.extractNeed != extractRoom {
		t.Errorf("another error: %d", s.extractNeed)
	}
}
