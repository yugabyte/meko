package db

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/migrations"
)

// migrationVersion finds a migration by filename suffix, so a renumber (these have
// already moved twice, dodging numbers claimed by other branches) cannot silently
// turn this into a test of some other migration.
func migrationVersion(t *testing.T, suffix string) uint {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		v, err := strconv.ParseUint(strings.SplitN(name, "_", 2)[0], 10, 64)
		if err != nil {
			t.Fatalf("parse version from %q: %v", name, err)
		}
		return uint(v)
	}
	t.Fatalf("no *%s in the embedded migrations", suffix)
	return 0
}

// TestMigrateBackfillsBranchExtensionTaskTargets exercises the one statement in
// the ycp_generation migration that TestMigrateRoundTrip cannot: the backfill.
// That walk runs against an EMPTY database, so `UPDATE tasks SET target_ref = …`
// matches zero rows every time and a broken cast or a wrong json key would ship
// unnoticed — and this statement runs at controller startup, so getting it wrong
// is a boot failure on a populated deployment.
//
// What it has to get right: add_branch_extensions only started stamping
// target_ref with this change, so a task already in flight across the deploy
// carries NULL there, and a partial unique index stores no entry for NULL. Without
// the backfill the first add after the deploy is admitted alongside one still
// redeploying the branch's endpoints — the concurrent upgrade of a single release
// the index exists to prevent.
//
// The migration is applied for real, in order, against pre-migration rows: go all
// the way up, step back over it, seed the old shape (which is only expressible
// once its columns are gone), then re-apply.
func TestMigrateBackfillsBranchExtensionTaskTargets(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; the backfill needs a YugabyteDB")
	}
	// Bounded, so a future deadlock here fails with a stack pointing at the call
	// that hung instead of eating the package's whole 10m test budget — which is
	// exactly what the first version of this test did.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	scratchURL, drop, err := ScratchDatabase(ctx, url, "amp_test_ycp_generation_backfill")
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	unlock, err := lockScratchProvisioning(ctx)
	if err != nil {
		t.Fatalf("take DDL lock: %v", err)
	}
	t.Cleanup(unlock)

	const schema = "controller" // the --db-schema default

	// The migrator and the seeding/reading MUST NOT share a connection pool.
	// newMigrator pins itself to a single connection (a YB catalog-caching
	// workaround), and it holds that connection for the migration session — so a
	// query issued on the same *sql.DB blocks forever waiting for it. Each phase
	// below opens its own pool and closes it before the next migration step, which
	// also keeps a DDL change from landing while an idle backend caches the old
	// catalog version.
	withPool := func(what string, fn func(pool *Pool)) {
		t.Helper()
		pool, err := Connect(ctx, scratchURL, schema)
		if err != nil {
			t.Fatalf("connect (%s): %v", what, err)
		}
		defer pool.Close()
		fn(pool)
	}

	m, sqlDB, err := newMigrator(ctx, scratchURL, schema, migrations.FS)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	defer sqlDB.Close()

	// Two migrations, deliberately: the columns and the target_ref repair, then the
	// index in a transaction of its own (see 0014's header). Both are under test,
	// because the repair only means anything if the index that follows it actually
	// covers the repaired rows.
	colsVersion := migrationVersion(t, "_ycp_generation.up.sql")
	lockVersion := migrationVersion(t, "_ycp_task_lock.up.sql")
	if lockVersion <= colsVersion {
		t.Fatalf("the lock migration (%d) must follow the columns one (%d)", lockVersion, colsVersion)
	}

	if err := m.Migrate(lockVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", lockVersion, err)
	}
	// Back over both, so the seeded rows are in the shape a real deployment has
	// when this pair is about to run.
	if err := m.Steps(-2); err != nil {
		t.Fatalf("step back over %d and %d: %v", lockVersion, colsVersion, err)
	}
	if v, _, err := m.Version(); err != nil || v >= colsVersion {
		t.Fatalf("after stepping back: version=%d err=%v, want below %d", v, err, colsVersion)
	}

	branch, cluster := uuid.New(), uuid.New()
	inFlight, terminal := uuid.New(), uuid.New()
	// The old planner handed the branch id to cluster_id and left target_ref unset.
	withPool("seed", func(pool *Pool) {
		for _, row := range []struct {
			id     uuid.UUID
			status string
		}{{inFlight, "running"}, {terminal, "succeeded"}} {
			params := fmt.Sprintf(`{"pool_id":%q,"cluster_id":%q,"branch_id":%q,"extensions":["pgvector"]}`,
				uuid.New(), cluster, branch)
			if _, err := pool.Exec(ctx,
				`INSERT INTO tasks (id, kind, status, cluster_id, target_ref, params)
				 VALUES ($1::uuid, 'add_branch_extensions', $2::task_status, $3::uuid, NULL, $4::jsonb)`,
				row.id.String(), row.status, branch.String(), params); err != nil {
				t.Fatalf("seed %s task: %v", row.status, err)
			}
		}
	})

	if err := m.Steps(2); err != nil {
		t.Fatalf("re-apply %d and %d: %v", colsVersion, lockVersion, err)
	}

	withPool("assert", func(pool *Pool) {
		read := func(id uuid.UUID) (target *string, clusterID *string) {
			t.Helper()
			if err := pool.QueryRow(ctx,
				`SELECT target_ref, cluster_id::text FROM tasks WHERE id = $1::uuid`, id.String()).
				Scan(&target, &clusterID); err != nil {
				t.Fatalf("read task %s: %v", id, err)
			}
			return
		}

		// The in-flight row is repaired, so the new index covers it.
		target, clusterID := read(inFlight)
		if target == nil || *target != branch.String() {
			t.Errorf("in-flight target_ref = %v, want the branch id %s", target, branch)
		}
		if clusterID == nil || *clusterID != cluster.String() {
			t.Errorf("in-flight cluster_id = %v, want the cluster id %s (it held the branch id)", clusterID, cluster)
		}

		// The terminal row is left alone: it sits outside the index predicate, and
		// its columns are a historical record nothing reconciles against.
		target, clusterID = read(terminal)
		if target != nil {
			t.Errorf("terminal target_ref = %q, want it untouched (NULL)", *target)
		}
		if clusterID == nil || *clusterID != branch.String() {
			t.Errorf("terminal cluster_id = %v, want it untouched (%s)", clusterID, branch)
		}

		// The point of the whole pair: the index must SEE the repaired row. It only
		// does because 0014 builds it in a transaction of its own — on YugabyteDB a
		// CREATE INDEX shares a transaction with 0013's UPDATE would be downgraded to
		// a non-concurrent build that skips rows written in that transaction, and the
		// DDL succeeds either way, so nothing but this assertion would notice.
		_, err := pool.Exec(ctx,
			`INSERT INTO tasks (id, kind, status, cluster_id, target_ref, params)
			 VALUES ($1::uuid, 'add_branch_extensions', 'pending', $2::uuid, $3, '{}'::jsonb)`,
			uuid.New().String(), cluster.String(), branch.String())
		if err == nil {
			t.Fatal("a second add for the backfilled branch was admitted; the index does not cover it")
		}
		if !strings.Contains(err.Error(), "23505") && !strings.Contains(err.Error(), "duplicate key") {
			t.Errorf("second add failed with %v, want a unique violation", err)
		}
	})
}
