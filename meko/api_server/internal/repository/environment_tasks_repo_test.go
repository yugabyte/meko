package repository

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

// envTaskColumns enumerates the columns the environment_tasks SELECTs return, in
// order. scanEnvironmentTask reads them positionally, so a column added to the query
// without a matching field here fails these tests rather than shifting every value
// silently one place along.
var envTaskColumns = []string{
	"task_id", "kind", "status", "cluster_id", "target_ref", "error",
	"result", "ycp_generation", "task_created_at", "task_updated_at", "reported_at",
}

func newEnvTaskDB(t *testing.T) (*EnvironmentTaskRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewEnvironmentTaskRepo(db), mock, func() { db.Close() }
}

// TestUpsertTasksGuardsAgainstAStaleReport pins the WHERE clause on the upsert. The
// agent re-reads the boundary of its change feed on every sweep and re-sends
// anything it could not confirm delivered, so the same task arrives repeatedly and
// can arrive out of order across a retry. Without the guard a late "running" report
// walks a terminal task back and the console shows the operation in flight forever.
func TestUpsertTasksGuardsAgainstAStaleReport(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	now := time.Now().UTC()
	gen := int64(4)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO meko_system\.environment_tasks`).
		WithArgs("env-1", "task-1", "resize_endpoint", "failed",
			nil, "endpoint/abc", "deploy timed out", []byte(`{"in_place_resize":"infeasible"}`),
			&gen, now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := r.UpsertTasks("env-1", []models.EnvironmentTask{{
		TaskID: "task-1", Kind: "resize_endpoint", Status: "failed",
		TargetRef: "endpoint/abc", Error: "deploy timed out",
		Result: json.RawMessage(`{"in_place_resize":"infeasible"}`), YCPGeneration: &gen,
		CreatedAt: now, UpdatedAt: now,
	}})
	if err != nil {
		t.Fatalf("UpsertTasks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	// The guard itself is in the SQL, so assert it is there: a rewrite that drops it
	// still passes every behavioural expectation above against a mock.
	stmt := upsertStatementFor(t)
	if !regexp.MustCompile(`WHERE\s+EXCLUDED\.task_updated_at\s*>=\s*meko_system\.environment_tasks\.task_updated_at`).
		MatchString(stmt) {
		t.Error("the upsert has no task_updated_at guard; a stale report can regress a terminal task")
	}
	if !regexp.MustCompile(`ON CONFLICT \(env_uuid, task_id\) DO UPDATE`).MatchString(stmt) {
		t.Error("the upsert does not conflict on (env_uuid, task_id)")
	}
}

// TestUpsertTasksSkipsAnEmptyDelta: a quiet tick is the common case, and it must not
// open a transaction to write nothing.
func TestUpsertTasksSkipsAnEmptyDelta(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	if err := r.UpsertTasks("env-1", nil); err != nil {
		t.Fatalf("UpsertTasks(nil): %v", err)
	}
	// No ExpectBegin was registered, so any statement at all fails here.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestUpsertTasksStoresAbsentOptionalsAsNull: "" would come back as an empty string
// the console renders as a present-but-blank value, and is not castable to uuid at
// all for cluster_id.
func TestUpsertTasksStoresAbsentOptionalsAsNull(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	now := time.Now().UTC()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO meko_system\.environment_tasks`).
		WithArgs("env-1", "task-1", "create_branch", "succeeded",
			nil, nil, nil, nil, (*int64)(nil), now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := r.UpsertTasks("env-1", []models.EnvironmentTask{{
		TaskID: "task-1", Kind: "create_branch", Status: "succeeded",
		CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("UpsertTasks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTasksScopesAndOrders covers the read: scoped to the env_uuid it was given,
// ordered with a total order, and paged.
func TestListTasksScopesAndOrders(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT count\(\*\) FROM meko_system\.environment_tasks WHERE env_uuid = \$1`).
		WithArgs("env-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery(`ORDER BY task_created_at DESC, task_id DESC\s+LIMIT \$2 OFFSET \$3`).
		WithArgs("env-1", 2, 0).
		WillReturnRows(sqlmock.NewRows(envTaskColumns).
			AddRow("task-2", "create_branch", "succeeded", nil, nil, nil, "", nil, now, now, now).
			AddRow("task-1", "resize_endpoint", "running", nil, "endpoint/a", nil, "", int64(9), now, now, now))

	got, err := r.ListTasks("env-1", EnvironmentTaskListOpts{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if got.Total != 3 || len(got.Items) != 2 {
		t.Fatalf("result = {total:%d items:%d}, want 3/2", got.Total, len(got.Items))
	}
	if !got.HasMore {
		t.Error("has_more = false with 2 of 3 rows returned")
	}
	if got.Items[1].YCPGeneration == nil || *got.Items[1].YCPGeneration != 9 {
		t.Errorf("ycp_generation = %v, want 9", got.Items[1].YCPGeneration)
	}
	if got.Items[0].YCPGeneration != nil {
		t.Errorf("a NULL ycp_generation read back as %v, want nil — that is what marks a task YCP did not plan",
			*got.Items[0].YCPGeneration)
	}
	// An empty result JSON must stay nil, not become the invalid JSON literal "".
	if got.Items[0].Result != nil {
		t.Errorf("absent result read back as %q, want nil", got.Items[0].Result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestListTasksEmptyPageIsNotNil: the handler serializes Items directly, and a nil
// slice would render as JSON null, which a console iterating the field breaks on.
func TestListTasksEmptyPageIsNotNil(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	mock.ExpectQuery(`SELECT count\(\*\)`).WithArgs("env-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs("env-1", 25, 0).
		WillReturnRows(sqlmock.NewRows(envTaskColumns))

	got, err := r.ListTasks("env-1", EnvironmentTaskListOpts{Limit: 25})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if got.Items == nil {
		t.Error("Items is nil; it must be an empty slice so it serializes as []")
	}
	if got.HasMore {
		t.Error("has_more = true on an empty page")
	}
}

// TestListTasksFiltersBind covers the optional predicates: they must be positionally
// numbered ahead of the page args, and the status set must go as one bound array so
// the statement's shape does not change with the number of statuses.
func TestListTasksFiltersBind(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	clusterID := "22222222-2222-2222-2222-222222222222"
	mock.ExpectQuery(`WHERE env_uuid = \$1 AND status = ANY\(\$2\) AND kind = \$3 AND cluster_id = \$4::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`LIMIT \$5 OFFSET \$6`).
		WillReturnRows(sqlmock.NewRows(envTaskColumns))

	if _, err := r.ListTasks("env-1", EnvironmentTaskListOpts{
		Limit: 10, Statuses: []string{"pending", "running"},
		Kind: "resize_endpoint", ClusterID: clusterID,
	}); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestGetTaskUnknownIsNilNotError, so the handler can turn "no such task" into a 404
// instead of a 500.
func TestGetTaskUnknownIsNilNotError(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	mock.ExpectQuery(`FROM meko_system\.environment_tasks\s+WHERE env_uuid = \$1 AND task_id = \$2`).
		WithArgs("env-1", "nope").
		WillReturnRows(sqlmock.NewRows(envTaskColumns))

	got, err := r.GetTask("env-1", "nope")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got != nil {
		t.Errorf("GetTask(unknown) = %+v, want nil", got)
	}
}

// TestPruneTerminalSparesLiveTasks: a pending or running task is live state however
// old its timestamp looks, and deleting one makes an operation the console is
// watching vanish mid-flight rather than resolve.
func TestPruneTerminalSparesLiveTasks(t *testing.T) {
	r, mock, done := newEnvTaskDB(t)
	defer done()

	mock.ExpectExec(`DELETE FROM meko_system\.environment_tasks\s+WHERE status = ANY\(\$1\) AND task_updated_at < \$2`).
		WillReturnResult(sqlmock.NewResult(0, 12))

	n, err := r.PruneTerminal(TaskRetention)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if n != 12 {
		t.Errorf("pruned = %d, want 12", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
	// The status set is the terminal pair and nothing else.
	for _, s := range models.TerminalTaskStatuses {
		if s == "pending" || s == "running" {
			t.Fatalf("TerminalTaskStatuses contains %q; retention would delete live tasks", s)
		}
	}
	if len(models.TerminalTaskStatuses) != 2 {
		t.Errorf("TerminalTaskStatuses = %v, want exactly succeeded and failed", models.TerminalTaskStatuses)
	}
}

// upsertStatementFor drives one upsert through a mock whose query matcher records
// the statement, so a test can assert the SQL itself rather than only its effects
// against a mock that would accept anything.
func upsertStatementFor(t *testing.T) string {
	t.Helper()
	var captured string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
		func(_, actual string) error {
			captured = actual
			return nil
		})))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(".*").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	now := time.Now().UTC()
	if err := NewEnvironmentTaskRepo(db).UpsertTasks("env-1", []models.EnvironmentTask{{
		TaskID: "t", Kind: "k", Status: "succeeded", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("UpsertTasks: %v", err)
	}
	if captured == "" {
		t.Fatal("the query matcher recorded no statement")
	}
	return captured
}
