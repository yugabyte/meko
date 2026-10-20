package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMigrateEndpointCreateStepIndex pins migration 0015. The endpoint API reads
// each endpoint's create task on every list, from a task log that is never
// trimmed, so the lookup has to be served by task_steps_create_endpoint — and an
// index nothing uses fails silently: every list still answers, just by scanning
// every step ever written.
func TestMigrateEndpointCreateStepIndex(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; the index needs a YugabyteDB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	scratchURL, drop, err := ScratchDatabase(ctx, url, "amp_test_endpoint_create_step_index")
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)
	unlock, err := lockScratchProvisioning(ctx)
	if err != nil {
		t.Fatalf("take DDL lock: %v", err)
	}
	t.Cleanup(unlock)

	const schema = "controller"
	if err := Migrate(ctx, scratchURL, schema); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	var def string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'task_steps' AND indexname = 'task_steps_create_endpoint'`,
	).Scan(&def); err != nil {
		t.Fatalf("read the index: %v", err)
	}
	for _, want := range []string{"endpoint_id", "create_endpoint"} {
		if !strings.Contains(def, want) {
			t.Errorf("index %q does not mention %q", def, want)
		}
	}

	// The predicate and the expression have to match the query's exactly, or the
	// planner cannot use the index; this is the inner query of
	// task.PgStore.EndpointCreateTasks.
	rows, err := pool.Query(ctx, `EXPLAIN SELECT st.task_id FROM task_steps st
		WHERE st.kind = 'create_endpoint' AND st.args->>'endpoint_id' = ANY($1::text[])`,
		[]string{"00000000-0000-0000-0000-000000000001"})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, line)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "task_steps_create_endpoint") {
		t.Errorf("the lookup does not use task_steps_create_endpoint:\n%s", strings.Join(plan, "\n"))
	}
}
