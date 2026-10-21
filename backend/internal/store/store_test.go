package store

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrateFreshAndAgain(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i := 0; i < 2; i++ { // the second run is a no-op
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != len(ms) {
		t.Fatalf("version %d, %v; want %d", v, err, len(ms))
	}
	if _, err := os.Stat(s.BackupPath(0)); !os.IsNotExist(err) {
		t.Errorf("a fresh database shouldn't be backed up: %v", err)
	}
	if fi, err := os.Stat(s.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("database mode %v, %v", fi.Mode().Perm(), err)
	}
}

func TestMigrateBacksUpBeforeUpgrading(t *testing.T) {
	ctx := context.Background()
	old := migrationSource
	t.Cleanup(func() { migrationSource = old })
	migrationSource = fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a(x);")}}
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	migrationSource = fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a(x);")},
		"0002_b.sql": {Data: []byte("CREATE TABLE b(y);")},
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 2 {
		t.Fatalf("version %d", v)
	}
	bak, err := Open(s.BackupPath(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bak.Close() }()
	if v, _ := bak.SchemaVersion(ctx); v != 1 {
		t.Errorf("backup is at version %d, want 1", v)
	}
}

func TestMigrateRefuses(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.w.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer than this meko") {
		t.Fatalf("newer database: got %v", err)
	}

	old := migrationSource
	t.Cleanup(func() { migrationSource = old })
	migrationSource = fstest.MapFS{"0002_gap.sql": {Data: []byte("SELECT 1;")}}
	if _, err := loadMigrations(); err == nil || !strings.Contains(err.Error(), "out of sequence") {
		t.Fatalf("gap: got %v", err)
	}
}

func TestDefaultDatapackAndInstallID(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := s.EnsureDefaultDatapack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := uuid.Parse(id); perr != nil {
		t.Errorf("datapack id %q isn't a UUID", id)
	}
	if again, _ := s.EnsureDefaultDatapack(ctx); again != id {
		t.Errorf("second call made %q", again)
	}
	dps, err := s.Datapacks(ctx)
	if err != nil || len(dps) != 1 || dps[0].Name != DefaultDatapackName ||
		strings.HasPrefix(dps[0].Name, "meko_default_datapack") {
		t.Fatalf("datapacks %+v, %v", dps, err)
	}
	// Times are stored fixed-width, so they sort as text.
	var created string
	if err = s.r.QueryRow("SELECT created_at FROM datapacks").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{9}Z$`).MatchString(created) {
		t.Errorf("created_at %q isn't fixed-width", created)
	}
	// Deleted by the user, it isn't made again.
	if _, err = s.w.Exec("DELETE FROM datapacks"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnsureDefaultDatapack(ctx); err != nil {
		t.Fatal(err)
	}
	if dps, _ := s.Datapacks(ctx); len(dps) != 0 {
		t.Errorf("default datapack came back: %+v", dps)
	}

	a, err := s.EnsureInstallID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := s.EnsureInstallID(ctx); a != b {
		t.Errorf("install id changed: %q then %q", a, b)
	}
}

func blob(v ...float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// sqlite-vec and FTS5 are compiled in, and vec0's partition key filters by datapack.
func TestVectorAndFullTextSearch(t *testing.T) {
	s := open(t)
	for _, q := range []string{
		"CREATE VIRTUAL TABLE v USING vec0(dp TEXT partition key, embedding float[2] distance_metric=cosine)",
		"CREATE VIRTUAL TABLE f USING fts5(dp, text)",
	} {
		if _, err := s.w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		id  int
		dp  string
		vec []byte
	}{{1, "a", blob(1, 0)}, {2, "a", blob(0, 1)}, {3, "b", blob(1, 0.01)}}
	for _, r := range rows {
		if _, err := s.w.Exec("INSERT INTO v(rowid, dp, embedding) VALUES (?, ?, ?)", r.id, r.dp, r.vec); err != nil {
			t.Fatal(err)
		}
	}
	var id int
	err := s.r.QueryRow("SELECT rowid FROM v WHERE embedding MATCH ? AND k = 1 AND dp = 'a'", blob(1, 0.02)).Scan(&id)
	if err != nil || id != 1 {
		t.Fatalf("knn in datapack a: %d, %v", id, err)
	}
	if _, err := s.w.Exec("INSERT INTO f(rowid, dp, text) VALUES " +
		"(1, 'a', 'sessions survive restarts'), (2, 'b', 'sessions survive restarts')"); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRow("SELECT rowid FROM f WHERE f MATCH 'dp:b AND restarts'").Scan(&id); err != nil || id != 2 {
		t.Fatalf("fts in datapack b: %d, %v", id, err)
	}
}

func TestLocalUserIDAndDatapackExists(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u1, err := s.EnsureLocalUserID(ctx)
	if err != nil || uuid.Validate(u1) != nil {
		t.Fatalf("local user id %q, %v", u1, err)
	}
	install, _ := s.EnsureInstallID(ctx)
	if u2, _ := s.EnsureLocalUserID(ctx); u2 != u1 || u1 == install {
		t.Errorf("local user id %q then %q (install id %q)", u1, u2, install)
	}
	dp, err := s.EnsureDefaultDatapack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DatapackExists(ctx, dp); !ok || err != nil {
		t.Errorf("the default datapack: %v %v", ok, err)
	}
	if ok, err := s.DatapackExists(ctx, "nope"); ok || err != nil {
		t.Errorf("a missing datapack: %v %v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DatapackExists(ctx, dp); err == nil {
		t.Error("an error from the database (closed) wasn't passed on")
	}
}
