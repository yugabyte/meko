package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationSource holds the NNNN_name.sql files; tests replace it.
var migrationSource fs.FS = mustSub(migrationFiles, "migrations")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the NNNN_name.sql files, which must be numbered 1, 2, 3... with no gaps.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationSource, ".")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("migration %s isn't named NNNN_name.sql", e.Name())
		}
		b, err := fs.ReadFile(migrationSource, e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s is out of sequence: expected version %d", m.name, i+1)
		}
	}
	return ms, nil
}

// SchemaVersion is the database's PRAGMA user_version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// BackupPath is where Migrate copies a database at version v before upgrading it.
func (s *Store) BackupPath(v int) string { return fmt.Sprintf("%s.bak-%d", s.path, v) }

// Migrate brings the schema up to date. Each migration runs in its own transaction with the new
// version. An existing database is first copied with VACUUM INTO, so a failed upgrade can be
// rolled back; a database newer than this binary is refused.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	latest := ms[len(ms)-1].version
	if cur > latest {
		return fmt.Errorf("%s has schema version %d, newer than this meko-local knows (%d). Use a newer meko-local",
			s.path, cur, latest)
	}
	if cur == latest {
		return nil
	}
	if cur > 0 {
		bak := s.BackupPath(cur)
		if err := os.Remove(bak); err != nil && !os.IsNotExist(err) {
			return err
		}
		if _, err := s.w.ExecContext(ctx, "VACUUM INTO ?", bak); err != nil {
			return fmt.Errorf("can't back up %s before upgrading it: %w", s.path, err)
		}
		if err := os.Chmod(bak, 0o600); err != nil {
			return err
		}
	}
	for _, m := range ms[cur:] {
		if err := s.apply(ctx, m); err != nil {
			return fmt.Errorf("migration %s failed: %w", m.name, err)
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, m migration) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}
