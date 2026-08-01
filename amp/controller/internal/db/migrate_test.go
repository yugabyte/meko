package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/yugabyte/amp/controller/migrations"
)

// TestMigrateRoundTrip walks each migration up, down, then up again, in order:
// 1↑ 1↓ 1↑, 2↑ 2↓ 2↑, 3↑ 3↓ 3↑, … So every individual `.down.sql` is exercised
// against the state its `.up.sql` produced (with the earlier migrations applied),
// and the re-up catches a down that doesn't fully revert its up. This is stricter
// than an all-up / all-down / all-up round-trip, which would only test the down
// migrations as a whole batch.
//
// It needs YugabyteDB: migration 0002 uses YB-specific `HASH` / `SPLIT INTO
// TABLETS` DDL that plain Postgres rejects. So it skips unless
// AMP_TEST_DATABASE_URL points at one — CI's "Migrations" job provides a
// throwaway database.
func TestMigrateRoundTrip(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; the up/down round-trip needs a YugabyteDB")
	}
	ctx := context.Background()

	// A throwaway database, not a throwaway schema: the migrations create two
	// fixed schemas (the resource model under --db-schema plus the always-
	// `controller` task log), so relocating one of them no longer isolates a run.
	// This also means the walk exercises the production schema names.
	scratchURL, drop, err := ScratchDatabase(ctx, url, "amp_test_migrate_roundtrip")
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	// Registered BEFORE the unlock below so it runs after it: cleanups are LIFO,
	// and drop takes the same lock, which would deadlock if it ran while held.
	t.Cleanup(drop)

	// Hold the machine-local DDL lock for the WHOLE walk, not just one statement:
	// this test applies and reverts every migration in turn, and YugabyteDB's
	// catalog is cluster-wide, so dropping `amp_storage` here serialization-aborts
	// against another package creating its own copy. Taken after ScratchDatabase
	// (which locks internally) because flock is not reentrant within a process.
	unlock, err := lockScratchProvisioning(ctx)
	if err != nil {
		t.Fatalf("take DDL lock: %v", err)
	}
	t.Cleanup(unlock)

	const schema = "controller" // the --db-schema default
	m, sqlDB, err := newMigrator(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	defer sqlDB.Close()

	// Count the migrations so we can step through them one at a time.
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			n++
		}
	}
	if n == 0 {
		t.Fatal("no .up.sql migrations found")
	}

	step := func(label string, fn func() error) {
		t.Helper()
		// ErrNoChange only happens at the boundaries (e.g. the reset below on a
		// fresh DB); a stepwise up/down should otherwise always move a version.
		if err := fn(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Fatalf("%s: %v", label, err)
		}
	}

	// Start from a clean slate so the walk is deterministic even on a re-run
	// against a non-fresh scratch DB.
	step("reset to version 0", m.Down)

	// migration i: apply it, revert just it (1..i-1 stay applied), re-apply it.
	for i := 1; i <= n; i++ {
		step(fmt.Sprintf("migration %d up", i), func() error { return m.Steps(1) })
		step(fmt.Sprintf("migration %d down", i), func() error { return m.Steps(-1) })
		step(fmt.Sprintf("migration %d up again", i), func() error { return m.Steps(1) })
	}
}
