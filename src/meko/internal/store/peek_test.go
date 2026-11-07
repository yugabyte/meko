package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Peek reads the schema version and settings without writing: a missing database is version 0,
// and the database it read is left as it was, with the server's own store still open on it.
func TestPeek(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "meko.db")
	if v, s, err := Peek(ctx, missing, KeyInstallID); err != nil || v != 0 || len(s) != 0 {
		t.Errorf("missing: %d %v %v", v, s, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("Peek made the database: %v", err)
	}

	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := s.EnsureInstallID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, got, err := Peek(ctx, s.Path(), KeyInstallID, "not_set")
	if err != nil || v != want || got[KeyInstallID] != id || len(got) != 1 {
		t.Errorf("Peek = %d %v %v, want %d and the install id %s", v, got, err, want, id)
	}
	// The server's store still writes.
	if err := s.SetSetting(ctx, "after_peek", "1"); err != nil {
		t.Errorf("the store can't write after a peek: %v", err)
	}
}
