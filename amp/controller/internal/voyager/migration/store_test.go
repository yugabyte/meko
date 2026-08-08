package migration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
	"github.com/yugabyte/amp/controller/internal/voyager/migrations"
)

const testVoyagerSchema = "amp_voyager_control"

// newTestStore migrates the amp_voyager_control schema and returns a YbStore over a
// pool pinned to it. It skips unless AMP_TEST_DATABASE_URL points at a
// throwaway database (the same convention db.TestMigrateRoundTrip uses). Each
// test truncates the control tables on cleanup so cases don't share rows.
func newTestStore(t *testing.T) *YbStore {
	t.Helper()
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AMP_TEST_DATABASE_URL not set; the store tests need a database")
	}
	ctx := context.Background()
	if err := db.MigrateFS(ctx, url, testVoyagerSchema, migrations.FS); err != nil {
		t.Fatalf("migrate amp_voyager_control: %v", err)
	}
	pool, err := db.Connect(ctx, url, testVoyagerSchema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered before Close so it runs first (t.Cleanup is LIFO), leaving a
	// clean schema for the next test.
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`TRUNCATE migration_run, migration CASCADE`); err != nil {
			t.Logf("cleanup truncate: %v", err)
		}
	})
	return NewYbStore(pool)
}

func sampleSource() SourceConfig {
	return SourceConfig{
		DBType:   "postgresql",
		Host:     "db.internal",
		Port:     5432,
		Username: "app_user",
		Password: "secret",
		Database: "prod",
		Schemas:  []string{"public"},
	}
}

// mustCreate inserts a migration with a fresh id and fails the test on error.
func mustCreate(t *testing.T, store *YbStore, name string) Migration {
	t.Helper()
	m, err := store.CreateMigration(context.Background(), uuid.New(), name, sampleSource())
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return m
}

func TestYbStoreCreateAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id := uuid.New()
	created, err := store.CreateMigration(ctx, id, "Migrate prod Postgres", sampleSource())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID != id {
		t.Fatalf("id = %s, want %s", created.ID, id)
	}
	if created.State != StateCreated {
		t.Fatalf("state = %q, want %q", created.State, StateCreated)
	}
	if created.Detail != "awaiting next step" {
		t.Fatalf("detail = %q", created.Detail)
	}
	if created.Runs == nil {
		t.Fatalf("runs should be a non-nil empty slice, got nil")
	}

	got, err := store.GetMigration(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Source.Password != "secret" {
		t.Fatalf("source password not round-tripped internally")
	}
	if got.Target != nil {
		t.Fatalf("target should be nil on a new migration")
	}
}

// CreateMigration is idempotent: replaying the same id converges on one row.
func TestYbStoreCreateIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id := uuid.New()
	if _, err := store.CreateMigration(ctx, id, "dup", sampleSource()); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := store.CreateMigration(ctx, id, "dup", sampleSource()); err != nil {
		t.Fatalf("replay create: %v", err)
	}
	_, total, err := store.ListMigrations(ctx, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1 (create should not duplicate)", total)
	}
}

func TestYbStoreGetMissing(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.GetMigration(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestYbStoreGetRunMissing(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.GetRun(context.Background(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestYbStoreList(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"a", "b", "c"} {
		mustCreate(t, store, name)
	}

	// A page smaller than the total returns just the page but the full count.
	page, total, err := store.ListMigrations(ctx, 2, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(page) != 2 {
		t.Fatalf("page = %d, want 2", len(page))
	}

	// A non-positive limit is clamped to the default page rather than leaking a
	// driver error or returning an empty page.
	all, total, err := store.ListMigrations(ctx, -1, -5)
	if err != nil {
		t.Fatalf("list clamped: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("clamped list = %d rows, total %d; want 3, 3", len(all), total)
	}
}

func TestYbStoreRunLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, store, "assess flow")

	attempt, err := store.NextAttempt(ctx, m.ID, OperationAssess)
	if err != nil {
		t.Fatalf("next attempt: %v", err)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}

	runID := uuid.New()
	taskID := uuid.New()
	spec := RunSpec{ID: runID, MigrationID: m.ID, Operation: OperationAssess, Attempt: attempt, TaskID: &taskID}
	if _, err := store.EnsureRun(ctx, spec); err != nil {
		t.Fatalf("ensure run: %v", err)
	}
	// Replaying the same spec converges on the same row.
	if _, err := store.EnsureRun(ctx, spec); err != nil {
		t.Fatalf("ensure run replay: %v", err)
	}

	active, err := store.HasActiveRun(ctx, m.ID)
	if err != nil || !active {
		t.Fatalf("HasActiveRun = %v, %v; want true", active, err)
	}

	meta := json.RawMessage(`{"namespace":"amp","job_name":"voyager-assess"}`)
	if err := store.MarkRunRunning(ctx, runID, taskID, "kubernetes", meta); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := store.MarkRunTerminal(ctx, runID, taskID, RunSucceeded, ""); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}
	// Marking the same terminal status again is a no-op success.
	if err := store.MarkRunTerminal(ctx, runID, taskID, RunSucceeded, ""); err != nil {
		t.Fatalf("mark terminal replay: %v", err)
	}

	active, err = store.HasActiveRun(ctx, m.ID)
	if err != nil || active {
		t.Fatalf("HasActiveRun = %v, %v; want false", active, err)
	}

	got, err := store.GetMigration(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateAssessed {
		t.Fatalf("state = %q, want %q", got.State, StateAssessed)
	}
}

// A worker that does not own the run (wrong task_id) cannot transition it.
func TestYbStoreRunOwnerFencing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, store, "fencing")
	runID := uuid.New()
	owner := uuid.New()
	if _, err := store.EnsureRun(ctx, RunSpec{
		ID: runID, MigrationID: m.ID, Operation: OperationAssess, Attempt: 1, TaskID: &owner,
	}); err != nil {
		t.Fatalf("ensure run: %v", err)
	}

	intruder := uuid.New()
	if err := store.MarkRunRunning(ctx, runID, intruder, "kubernetes", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("intruder MarkRunRunning = %v, want ErrConflict", err)
	}
	if err := store.MarkRunTerminal(ctx, runID, intruder, RunFailed, "boom"); !errors.Is(err, ErrConflict) {
		t.Fatalf("intruder MarkRunTerminal = %v, want ErrConflict", err)
	}
	// The real owner still succeeds.
	if err := store.MarkRunRunning(ctx, runID, owner, "kubernetes", nil); err != nil {
		t.Fatalf("owner MarkRunRunning: %v", err)
	}
}

func TestYbStoreEnsureRunValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m := mustCreate(t, store, "validation")

	cases := map[string]RunSpec{
		"nil run id":        {ID: uuid.Nil, MigrationID: m.ID, Operation: OperationAssess, Attempt: 1},
		"nil migration id":  {ID: uuid.New(), MigrationID: uuid.Nil, Operation: OperationAssess, Attempt: 1},
		"invalid operation": {ID: uuid.New(), MigrationID: m.ID, Operation: Operation("nope"), Attempt: 1},
		"zero attempt":      {ID: uuid.New(), MigrationID: m.ID, Operation: OperationAssess, Attempt: 0},
	}
	for name, spec := range cases {
		if _, err := store.EnsureRun(ctx, spec); err == nil {
			t.Fatalf("%s: expected a validation error, got nil", name)
		}
	}
}

// A run against a migration that does not exist trips the FK and maps to
// ErrNotFound.
func TestYbStoreEnsureRunUnknownMigration(t *testing.T) {
	store := newTestStore(t)
	_, err := store.EnsureRun(context.Background(), RunSpec{
		ID: uuid.New(), MigrationID: uuid.New(), Operation: OperationAssess, Attempt: 1,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestYbStoreSingleActiveRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, store, "single active")
	if _, err := store.EnsureRun(ctx, RunSpec{
		ID: uuid.New(), MigrationID: m.ID, Operation: OperationAssess, Attempt: 1,
	}); err != nil {
		t.Fatalf("ensure first run: %v", err)
	}
	// A second in-flight run for the same migration violates the partial unique
	// index and must surface as a conflict.
	_, err := store.EnsureRun(ctx, RunSpec{
		ID: uuid.New(), MigrationID: m.ID, Operation: OperationMigrate, Attempt: 1,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestYbStoreClearTargetTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, store, "clear target")
	taskID := uuid.New()

	// Clearing when no claim is held is a no-op success.
	if err := store.ClearTargetTask(ctx, m.ID, taskID); err != nil {
		t.Fatalf("clear with no claim: %v", err)
	}

	if err := store.SetTargetTask(ctx, m.ID, taskID); err != nil {
		t.Fatalf("set target task: %v", err)
	}
	if err := store.ClearTargetTask(ctx, m.ID, taskID); err != nil {
		t.Fatalf("clear target task: %v", err)
	}
	got, err := store.GetMigration(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TargetTaskID != nil {
		t.Fatalf("target task id should be cleared")
	}
	if got.State != StateCreated {
		t.Fatalf("state = %q, want %q after clearing the claim", got.State, StateCreated)
	}

	// Clearing on a migration that does not exist surfaces ErrNotFound.
	if err := store.ClearTargetTask(ctx, uuid.New(), taskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clear on missing migration = %v, want ErrNotFound", err)
	}
}

func TestYbStoreTargetProvisioning(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	m := mustCreate(t, store, "target flow")

	taskID := uuid.New()
	if err := store.SetTargetTask(ctx, m.ID, taskID); err != nil {
		t.Fatalf("set target task: %v", err)
	}
	// Replay by the same task is allowed.
	if err := store.SetTargetTask(ctx, m.ID, taskID); err != nil {
		t.Fatalf("set target task replay: %v", err)
	}
	// A competing task is a conflict.
	if err := store.SetTargetTask(ctx, m.ID, uuid.New()); !errors.Is(err, ErrConflict) {
		t.Fatalf("competing claim err = %v, want ErrConflict", err)
	}

	provisioning, err := store.GetMigration(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if provisioning.State != StateProvisioningTarget {
		t.Fatalf("state = %q, want %q", provisioning.State, StateProvisioningTarget)
	}

	target := TargetConfig{
		PoolID:     uuid.New(),
		ClusterID:  uuid.New(),
		BranchID:   uuid.New(),
		EndpointID: uuid.New(),
		Host:       "amp-prod.yugabyte.cloud",
		Port:       5433,
		Username:   "cloud_admin",
		Password:   "cloud_admin",
		Database:   "prod",
	}

	// A task that does not hold the claim cannot attach a target.
	if err := store.AttachTarget(ctx, m.ID, uuid.New(), target); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-holder attach = %v, want ErrConflict", err)
	}

	if err := store.AttachTarget(ctx, m.ID, taskID, target); err != nil {
		t.Fatalf("attach target: %v", err)
	}
	// Absolute idempotent update: same target converges even after the claim
	// was cleared by the first attach.
	if err := store.AttachTarget(ctx, m.ID, taskID, target); err != nil {
		t.Fatalf("attach target replay: %v", err)
	}

	ready, err := store.GetMigration(ctx, m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ready.State != StateTargetReady {
		t.Fatalf("state = %q, want %q", ready.State, StateTargetReady)
	}
	if ready.Target == nil || ready.Target.Host != target.Host {
		t.Fatalf("target not attached correctly: %+v", ready.Target)
	}
	if ready.TargetTaskID != nil {
		t.Fatalf("target task id should be cleared after attach")
	}
}
