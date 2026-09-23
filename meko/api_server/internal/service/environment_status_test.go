package service

import (
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// A report that names a state but omits the agent version must not erase the stored
// one.
//
// CLOUDGA-36984 closed the body-less case: normalizeState refuses a report carrying
// no state before the row is even read. AgentVersion outlives that check, because a
// report can be well-formed and still omit it — and nullStr turns the empty string
// into a NULL, so the column loses the last version the agent reported. Every other
// merged field here (mergeFacts, Components, Resources) already held to that rule.
//
// This builds its own row rather than using envRow, which stores "" for the version:
// a fixture storing "" cannot tell a preserved value from an erased one.
func TestApplyStatus_ReportWithoutVersionKeepsTheStoredOne(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewEnvironmentService(repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))

	stored := sqlmock.NewRows(envRowCols).AddRow(
		envUUID, "amp-acme-prod", acctA, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "v1.4.2", nil, "", "", "[]", true, keyA, time.Now(), KindAmp, "[]", nil, "BYOC",
	)
	mock.ExpectQuery(`SELECT .* FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(stored)

	// $10 is agent_version, and it must still be the stored one; $4 is the state the
	// report did carry. Everything else is free.
	args := make([]driver.Value, 18)
	for i := range args {
		args[i] = sqlmock.AnyArg()
	}
	args[3] = "DEGRADED"
	args[9] = "v1.4.2"
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.ApplyStatus(envUUID, StatusReport{EnvironmentState: "DEGRADED"})
	if err != nil {
		t.Fatalf("ApplyStatus: %v", err)
	}
	if !res.Found {
		t.Fatal("ApplyStatus returned Found=false for a known environment")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a report with no agent version erased the stored one: %v", err)
	}
}
