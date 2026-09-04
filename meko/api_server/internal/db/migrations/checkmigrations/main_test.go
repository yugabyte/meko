package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"testing"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// TestNoDuplicateVersionsInTree loads each migration directory through
// golang-migrate's own iofs source -- the same loader connect.go uses in
// production, just pointed at the files on disk (os.DirFS) instead of the
// compiled-in embed.FS, since a //go:embed pattern can't reach a sibling
// package directory via "..". It fails if two files share a
// (version, direction) pair, which is what actually aborts a pod at
// startup. checkAgainstRef's git-diff-based checks only compare against
// what git says this branch changed, so this is the check that still
// catches a collision sitting in one already-committed tree regardless of
// git history -- including right after a push to main, where
// checkAgainstRef is a no-op because HEAD already equals baseRef.
func TestNoDuplicateVersionsInTree(t *testing.T) {
	for _, dir := range []string{"../system", "../data", "../controlplane"} {
		t.Run(dir, func(t *testing.T) {
			fsys := os.DirFS(dir)

			// Report every colliding version at once, with every filename
			// that claims it, using golang-migrate's own filename parser
			// (source.Parse) so "what counts as the same version" can never
			// drift from what iofs.New itself enforces below.
			entries, err := fs.ReadDir(fsys, ".")
			if err != nil {
				t.Fatalf("read %s: %v", dir, err)
			}
			type key struct {
				version   uint
				direction source.Direction
			}
			byKey := map[key][]string{}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				m, err := source.Parse(entry.Name())
				if err != nil {
					continue // not a migration file
				}
				byKey[key{m.Version, m.Direction}] = append(byKey[key{m.Version, m.Direction}], entry.Name())
			}
			var dupes []key
			for k, names := range byKey {
				if len(names) > 1 {
					dupes = append(dupes, k)
				}
			}
			if len(dupes) > 0 {
				sort.Slice(dupes, func(i, j int) bool { return dupes[i].version < dupes[j].version })
				var msg string
				for _, k := range dupes {
					names := byKey[k]
					sort.Strings(names)
					msg += fmt.Sprintf("\n  version %d (%s): %v", k.version, k.direction, names)
				}
				t.Fatalf("%s: duplicate migration version(s):%s", dir, msg)
			}

			// Belt and suspenders: also run the real loader used in
			// production, in case a collision the scan above doesn't model
			// still exists.
			src, err := iofs.New(fsys, ".")
			if err != nil {
				t.Fatalf("iofs.New(%s): %v", dir, err)
			}
			defer src.Close()

			version, err := src.First()
			if err != nil {
				t.Fatalf("%s: First: %v", dir, err)
			}
			for {
				next, err := src.Next(version)
				if errors.Is(err, os.ErrNotExist) {
					break
				}
				if err != nil {
					t.Fatalf("%s: Next(%d): %v", dir, version, err)
				}
				version = next
			}
		})
	}
}

func TestParseMigrationKey(t *testing.T) {
	cases := []struct {
		filename string
		want     migrationKey
		ok       bool
	}{
		{"000014_pending_uploads.up.sql", migrationKey{14, "000014", "up"}, true},
		{"000014_pending_uploads.down.sql", migrationKey{14, "000014", "down"}, true},
		{"1_seed.up.sql", migrationKey{1, "1", "up"}, true},
		{"README.md", migrationKey{}, false},
		{"", migrationKey{}, false},
	}
	for _, tc := range cases {
		got, ok := parseMigrationKey(tc.filename)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseMigrationKey(%q) = (%+v, %v), want (%+v, %v)",
				tc.filename, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFindCollisions(t *testing.T) {
	dir := "internal/db/migrations/system"
	f := func(name string) string { return dir + "/" + name }

	t.Run("no files at ref: no collision", func(t *testing.T) {
		got := findCollisions(f("000014_foo.up.sql"), nil, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("this file's own up/down siblings at ref: not a collision with itself", func(t *testing.T) {
		// A rename (renumbering) diffs against a tree that still has the SAME
		// file's other direction under the SAME version -- that must not
		// flag, since it's the file's own pair, not a different migration.
		existing := []string{f("000014_foo.down.sql")}
		got := findCollisions(f("000014_foo.up.sql"), existing, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none (own up/down pair is not a collision)", got)
		}
	})

	t.Run("unrelated file, same version, same direction: collision", func(t *testing.T) {
		existing := []string{f("000014_bar.up.sql")}
		got := findCollisions(f("000014_foo.up.sql"), existing, "origin/main")
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one collision", got)
		}
	})

	t.Run("unrelated file, same version, opposite direction: not a collision", func(t *testing.T) {
		// This is the bug this function exists to avoid: every migration
		// legitimately has an .up.sql and a .down.sql sharing one version,
		// so version alone is not enough -- direction has to match too.
		existing := []string{f("000014_bar.down.sql")}
		got := findCollisions(f("000014_foo.up.sql"), existing, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none (different direction, not a collision)", got)
		}
	})

	t.Run("different version entirely: not a collision", func(t *testing.T) {
		existing := []string{f("000015_bar.up.sql")}
		got := findCollisions(f("000014_foo.up.sql"), existing, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("non-migration filename in existing: ignored, not a false match", func(t *testing.T) {
		existing := []string{f("README.md")}
		got := findCollisions(f("000014_foo.up.sql"), existing, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})
}

func TestCheckDir(t *testing.T) {
	dir := "internal/db/migrations/system"
	f := func(name string) string { return dir + "/" + name }

	t.Run("two files added in the same PR at the same version: collision, not silently deduped", func(t *testing.T) {
		// This is the shape of the bug checkmigrations exists to catch: two
		// migrations landing together in one commit/PR that duplicate each
		// other. Neither collides with baseRef's tree (both are new), and
		// findGaps alone would dedup them to a single, legitimate-looking
		// step from 000013 to 000014 -- only comparing the added files
		// against each other catches it. headTree is fully paired here so
		// the count below is only the two collisions, not also unpaired
		// findings for a half-added migration.
		existing := []string{f("000013_x.up.sql")}
		added := []string{f("000014_a.up.sql"), f("000014_b.up.sql")}
		headTree := []string{
			f("000013_x.up.sql"), f("000013_x.down.sql"),
			f("000014_a.up.sql"), f("000014_a.down.sql"),
			f("000014_b.up.sql"), f("000014_b.down.sql"),
		}
		got := checkDir(dir, added, existing, headTree, "origin/main")
		if len(got) != 2 { // each of the two files reports the other
			t.Fatalf("got %v, want exactly two collisions", got)
		}
	})

	t.Run("normal up/down pair added together: no false collision", func(t *testing.T) {
		existing := []string{f("000013_x.up.sql")}
		added := []string{f("000014_a.up.sql"), f("000014_a.down.sql")}
		headTree := []string{f("000013_x.up.sql"), f("000013_x.down.sql"), f("000014_a.up.sql"), f("000014_a.down.sql")}
		got := checkDir(dir, added, existing, headTree, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("added file is missing its down.sql: reported even though nothing collides or skips", func(t *testing.T) {
		existing := []string{f("000013_x.up.sql"), f("000013_x.down.sql")}
		added := []string{f("000014_a.up.sql")} // forgot the down.sql
		headTree := []string{f("000013_x.up.sql"), f("000013_x.down.sql"), f("000014_a.up.sql")}
		got := checkDir(dir, added, existing, headTree, "origin/main")
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one unpaired finding", got)
		}
	})
}

func TestFindUnpaired(t *testing.T) {
	dir := "internal/db/migrations/system"
	f := func(name string) string { return dir + "/" + name }

	t.Run("every version has both up and down: no problem", func(t *testing.T) {
		files := []string{f("000013_x.up.sql"), f("000013_x.down.sql"), f("000014_y.up.sql"), f("000014_y.down.sql")}
		got := findUnpaired(dir, files)
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("up with no down: reported", func(t *testing.T) {
		got := findUnpaired(dir, []string{f("000013_x.up.sql")})
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one", got)
		}
	})

	t.Run("down with no up: reported", func(t *testing.T) {
		got := findUnpaired(dir, []string{f("000013_x.down.sql")})
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one", got)
		}
	})

	t.Run("non-migration filename: ignored", func(t *testing.T) {
		got := findUnpaired(dir, []string{f("README.md")})
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("no files: no problem", func(t *testing.T) {
		got := findUnpaired(dir, nil)
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})
}

func TestFindGaps(t *testing.T) {
	dir := "internal/db/migrations/system"
	f := func(name string) string { return dir + "/" + name }

	t.Run("no new files: no problem", func(t *testing.T) {
		got := findGaps(dir, nil, []string{f("000013_x.up.sql")}, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("one new migration, exactly next: no problem", func(t *testing.T) {
		existing := []string{f("000013_x.up.sql"), f("000013_x.down.sql")}
		added := []string{f("000014_foo.up.sql"), f("000014_foo.down.sql")}
		got := findGaps(dir, added, existing, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("several new migrations added together, sequential: no problem", func(t *testing.T) {
		existing := []string{f("000013_x.up.sql")}
		added := []string{
			f("000014_a.up.sql"), f("000014_a.down.sql"),
			f("000015_b.up.sql"), f("000015_b.down.sql"),
			f("000016_c.up.sql"), f("000016_c.down.sql"),
		}
		got := findGaps(dir, added, existing, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("new migration skips ahead: reported", func(t *testing.T) {
		existing := []string{f("000013_x.up.sql")}
		added := []string{f("000016_c.up.sql")} // should have been 000014
		got := findGaps(dir, added, existing, "origin/main")
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one gap", got)
		}
	})

	t.Run("no existing files: first migration must start at 1, not skip ahead", func(t *testing.T) {
		added := []string{f("000003_c.up.sql")}
		got := findGaps(dir, added, nil, "origin/main")
		if len(got) != 1 {
			t.Fatalf("got %v, want exactly one gap", got)
		}
	})

	t.Run("empty root dir with no new files: no problem", func(t *testing.T) {
		got := findGaps(dir, nil, nil, "origin/main")
		if len(got) != 0 {
			t.Fatalf("got %v, want none", got)
		}
	})
}
