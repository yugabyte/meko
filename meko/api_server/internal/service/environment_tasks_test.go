package service

import (
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// anyEnvUpdateArgs matches the 18 columns of the environments UPDATE, none of which
// these tests care about — they are about what happens to the TASK table after it.
func anyEnvUpdateArgs() []driver.Value {
	args := make([]driver.Value, 18)
	for i := range args {
		args[i] = sqlmock.AnyArg()
	}
	return args
}

// expectHeartbeat sets up the environment lookup + update every ApplyStatus does.
func expectHeartbeat(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(envRow("amp-acme-prod", KindAmp))
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(anyEnvUpdateArgs()...).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestApplyStatus_EmptyTaskDeltaWritesNothing is the asymmetry with Resources, and
// the one a reader would most likely get wrong. Resources is a snapshot, so an empty
// set is a real report and replaces what was stored. Tasks is a DELTA, so an empty
// set means "nothing changed this tick" — which is most ticks — and must not open a
// transaction, let alone erase history.
func TestApplyStatus_EmptyTaskDeltaWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks []TaskStatus
	}{
		{"nil (a meko agent, which reports no tasks at all)", nil},
		{"empty (an amp agent on a quiet tick)", []TaskStatus{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			svc := NewEnvironmentService(
				repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

			expectHeartbeat(mock)
			// No task statement is registered, so any Begin/Exec against the task table
			// is an unexpected call and fails.

			res, err := svc.ApplyStatus(envUUID, StatusReport{
				EnvironmentState: "READY", Tasks: tc.tasks,
			})
			if err != nil || !res.Found {
				t.Fatalf("ApplyStatus = %+v, %v", res, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("an empty task delta touched the task table: %v", err)
			}
		})
	}
}

// TestApplyStatus_RecordsTaskDelta: a non-empty delta is upserted, and the retention
// prune runs on the same path that grew the table -- once, not on every heartbeat.
func TestApplyStatus_RecordsTaskDelta(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(
		repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	now := time.Now().UTC()
	expectHeartbeat(mock)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO meko_system\.environment_tasks`).
		WithArgs(envUUID, "task-1", "resize_endpoint", "failed",
			nil, "endpoint/a", "deploy timed out", nil, (*int64)(nil), now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`DELETE FROM meko_system\.environment_tasks`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	report := StatusReport{
		EnvironmentState: "DEGRADED",
		Tasks: []TaskStatus{{
			ID: "task-1", Kind: "resize_endpoint", Status: "failed",
			TargetRef: "endpoint/a", Error: "deploy timed out",
			CreatedAt: now, UpdatedAt: now,
		}},
	}
	res, err := svc.ApplyStatus(envUUID, report)
	if err != nil || !res.Found {
		t.Fatalf("ApplyStatus = %+v, %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the task delta did not reach the table: %v", err)
	}

	// A second heartbeat one tick later still upserts. Whether it prunes again is
	// asserted by TestClaimPruneThrottles, not here: recordTasks logs a prune failure
	// rather than returning it, so an unexpected DELETE is invisible from this level.
	expectHeartbeat(mock)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO meko_system\.environment_tasks`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if res, err := svc.ApplyStatus(envUUID, report); err != nil || !res.Found {
		t.Fatalf("second ApplyStatus = %+v, %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the second heartbeat did not upsert: %v", err)
	}
}

// TestClaimPruneThrottles covers the retention throttle on its own, because the
// caller swallows a prune failure and so cannot observe an extra one.
//
// The cost of getting this wrong is not subtle: every environment's agent heartbeats
// every 10s, so an unthrottled prune is one fleet-wide DELETE per environment per
// tick, essentially always deleting nothing.
func TestClaimPruneThrottles(t *testing.T) {
	svc := &EnvironmentService{}

	// The zero value claims at once, so a control plane that has just started clears
	// whatever aged out while it was down instead of waiting an interval first.
	if !svc.claimPrune() {
		t.Fatal("a never-pruned service refused the first pass")
	}
	if svc.claimPrune() {
		t.Error("two passes inside one interval; the throttle is not holding")
	}

	// Once the interval has elapsed it claims again.
	svc.lastPruned = time.Now().Add(-pruneEvery - time.Second)
	if !svc.claimPrune() {
		t.Error("refused a pass after the interval elapsed; retention would never run again")
	}
}

// TestApplyStatus_TaskWriteFailureIsReportedNotSwallowed covers both halves of the
// split, and the second half is the one with teeth.
//
// The heartbeat must still succeed: state and inventory are what the environment's
// health rides on, and a hiccup on a history table must not take the fleet view down.
//
// But it must ALSO report that the tasks did not land. A task delta is delivered
// once: an agent told the report was accepted retires it, and the feed cannot
// re-offer it. If the lost transition was a task reaching succeeded/failed over a row
// already stored as running, that row stays non-terminal forever -- PruneTerminal
// deliberately spares live tasks -- and the console shows the operation in flight
// permanently. Reporting Found without TasksRecorded is what lets the agent hold it.
func TestApplyStatus_TaskWriteFailureIsReportedNotSwallowed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(
		repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	now := time.Now().UTC()
	expectHeartbeat(mock)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO meko_system\.environment_tasks`).
		WillReturnError(errors.New("relation does not exist"))
	mock.ExpectRollback()

	res, err := svc.ApplyStatus(envUUID, StatusReport{
		EnvironmentState: "READY",
		Tasks: []TaskStatus{{
			ID: "task-1", Kind: "create_branch", Status: "succeeded",
			CreatedAt: now, UpdatedAt: now,
		}},
	})
	if err != nil {
		t.Fatalf("ApplyStatus returned %v; a task-write failure must not fail the heartbeat", err)
	}
	if !res.Found {
		t.Fatal("ApplyStatus returned Found=false for a known environment")
	}
	if res.TasksRecorded {
		t.Error("TasksRecorded is true though the upsert failed; the agent would retire a delta " +
			"nothing stored, and the transition would be unrecoverable")
	}
}

// TestApplyStatus_TasksRecordedTrueWhenThereWereNone: a report carrying no delta has
// nothing to retire, so it must not be told to hold one -- that would stall the
// watermark of every meko agent and every quiet amp tick.
func TestApplyStatus_TasksRecordedTrueWhenThereWereNone(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(
		repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	expectHeartbeat(mock)
	res, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: "READY"})
	if err != nil || !res.Found {
		t.Fatalf("ApplyStatus = %+v, %v", res, err)
	}
	if !res.TasksRecorded {
		t.Error("TasksRecorded is false for a report that carried no tasks")
	}
}

// TestApplyStatus_NoTaskRepoIsSafe: the service is constructed with a nil task repo
// in plenty of tests and could be in a future caller. That must degrade to "no task
// history", not panic on a heartbeat.
func TestApplyStatus_NoTaskRepoIsSafe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), nil)

	expectHeartbeat(mock)
	now := time.Now().UTC()
	res, err := svc.ApplyStatus(envUUID, StatusReport{
		EnvironmentState: "READY",
		Tasks: []TaskStatus{{
			ID: "t", Kind: "create_branch", Status: "succeeded", CreatedAt: now, UpdatedAt: now,
		}},
	})
	if err != nil || !res.Found {
		t.Fatalf("ApplyStatus = %+v, %v", res, err)
	}

	page, err := svc.Tasks(envUUID, repository.EnvironmentTaskListOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if page.Items == nil {
		t.Error("Tasks returned a nil slice with no repo; it must serialize as []")
	}
	if task, err := svc.Task(envUUID, "t"); err != nil || task != nil {
		t.Errorf("Task = %v, %v; want nil, nil with no repo", task, err)
	}
}
