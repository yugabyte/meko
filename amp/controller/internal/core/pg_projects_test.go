package core

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestListProjectsIncludeFailed covers the failed-creation filter: a project
// whose create_project task failed (its row written mid-provisioning before a
// later step failed) is an orphan. ListProjects excludes it by default (usable
// clusters only), and includes it only when the caller passes includeFailed=true
// (an operator's fleet view, to see and delete it). Succeeded and
// still-in-progress projects are always listed.
//
// Exercises the real PgBackend SQL against a Postgres/YSQL-compatible database.
// It provisions an isolated schema per run and drops it afterwards, so it is
// safe against any reachable DB (e.g. a meko-dev YB universe). Set
// AMP_TEST_DATABASE_URL to run; skipped otherwise so `go test ./...` stays green
// without a database.
func TestListProjectsIncludeFailed(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Isolated schema per run so we never touch a live controller schema.
	schema := fmt.Sprintf("amptest_%s", uuid.New().String()[:8])
	if err := db.Migrate(ctx, url, schema); err != nil {
		t.Fatalf("migrate schema %s: %v", schema, err)
	}
	pool, err := db.Connect(ctx, url, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), fmt.Sprintf("DROP SCHEMA %s CASCADE", schema)); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
		pool.Close()
	})

	// --- seed: one pool, three projects each with a default branch, each with a
	// create_project task in a different terminal/in-flight state. ---
	owner := uuid.New()
	poolID := uuid.New()
	mustExec(t, ctx, pool, `INSERT INTO app_user (id) VALUES ($1)`, owner)
	mustExec(t, ctx, pool, `INSERT INTO storage_pool (id, name, owner_user_id) VALUES ($1,$2,$3)`,
		poolID, "pool-"+schema, owner)

	type seed struct {
		name       string
		taskStatus string
		id         uuid.UUID
	}
	seeds := []seed{
		{name: "ok", taskStatus: "succeeded", id: uuid.New()},
		{name: "failed", taskStatus: "failed", id: uuid.New()},
		{name: "inflight", taskStatus: "running", id: uuid.New()},
	}
	for _, s := range seeds {
		mustExec(t, ctx, pool, `INSERT INTO project (id, storage_pool_id, name) VALUES ($1,$2,$3)`,
			s.id, poolID, s.name)
		mustExec(t, ctx, pool, `INSERT INTO branch (project_id, branch_id, name, pg_version, is_default) VALUES ($1,$2,'main',18,true)`,
			s.id, uuid.New())
		mustExec(t, ctx, pool, `INSERT INTO task (kind, status, project_id) VALUES ('create_project',$1::task_status,$2)`,
			s.taskStatus, s.id)
	}

	b := NewPgBackend(pool)
	names := func(ps []Project) map[string]bool {
		m := map[string]bool{}
		for _, p := range ps {
			m[p.Name] = true
		}
		return m
	}

	// --- includeFailed=false (the default): the failed project is hidden. ---
	t.Run("excludes failed by default", func(t *testing.T) {
		projects, total, err := b.ListProjects(ctx, poolID, 100, 0, false)
		if err != nil {
			t.Fatalf("ListProjects: %v", err)
		}
		got := names(projects)
		if got["failed"] {
			t.Errorf("failed-creation project should be excluded with includeFailed=false, but it was returned")
		}
		if !got["ok"] || !got["inflight"] {
			t.Errorf("succeeded + in-progress projects should be listed, got %v", got)
		}
		if total != 2 || len(projects) != 2 {
			t.Errorf("total=%d len=%d, want 2/2 (failed excluded from the count too)", total, len(projects))
		}
	})

	// --- includeFailed=true (opt-in): all three, including the failed one. ---
	t.Run("includes failed when opted in", func(t *testing.T) {
		projects, total, err := b.ListProjects(ctx, poolID, 100, 0, true)
		if err != nil {
			t.Fatalf("ListProjects: %v", err)
		}
		got := names(projects)
		if !got["failed"] || !got["ok"] || !got["inflight"] {
			t.Errorf("all three projects should be listed with includeFailed=true, got %v", got)
		}
		if total != 3 || len(projects) != 3 {
			t.Errorf("total=%d len=%d, want 3/3 (failed included)", total, len(projects))
		}
	})
}

func mustExec(t *testing.T, ctx context.Context, pool *db.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
