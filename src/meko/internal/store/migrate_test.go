package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestRestoreBackupUndoesAnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meko.db")
	latest, err := LatestSchema()
	if err != nil || latest < 2 {
		t.Fatalf("latest %d, %v", latest, err)
	}
	// A database at version latest-1, as an older meko left it.
	old := migrationSource
	migrationSource = fstest.MapFS{}
	ms, _ := fs.ReadDir(old, ".")
	for _, e := range ms[:latest-1] {
		b, _ := fs.ReadFile(old, e.Name())
		migrationSource.(fstest.MapFS)[e.Name()] = &fstest.MapFile{Data: b} //nolint:errcheck // set just above
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnsureInstallID(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	migrationSource = old
	t.Cleanup(func() { migrationSource = old })

	if done, rerr := RestoreBackup(path, latest-1); rerr != nil || done {
		t.Fatalf("nothing to restore yet: %v %v", done, rerr)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil { // the upgrade: writes meko.db.bak-<latest-1>
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	done, err := RestoreBackup(path, latest-1)
	if err != nil || !done {
		t.Fatalf("restore: %v %v", done, err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, _ := s.SchemaVersion(ctx); v != latest-1 {
		t.Errorf("schema %d after restore, want %d", v, latest-1)
	}
	if _, err := os.Stat(fmt.Sprintf("%s.failed-%d", path, latest)); err != nil {
		t.Errorf("the upgraded file wasn't kept aside: %v", err)
	}
}
