package db

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// golang-migrate builds its version table while constructing the driver, so by the
// time getMigrator returns, meko_system must already exist and the table must have
// been looked for there rather than in whatever the search path resolves to. The
// mock is ordered, so it also pins CREATE SCHEMA ahead of the driver.
func TestControlPlaneMigratorTracksInMekoSystem(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = conn.Close() }()

	mock.ExpectExec("CREATE SCHEMA IF NOT EXISTS meko_system").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT CURRENT_DATABASE\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"current_database"}).AddRow("meko"))
	mock.ExpectExec("SELECT pg_advisory_lock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("FROM information_schema.tables").
		WithArgs("meko_system", "schema_migrations_controlplane").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec("SELECT pg_advisory_unlock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectClose()

	m, err := getMigrator(conn, DBPurposeControlPlane, controlPlaneMigrationsFS, "migrations/controlplane")
	if err != nil {
		t.Fatalf("getMigrator: %v", err)
	}
	if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
		t.Errorf("close: source=%v database=%v", srcErr, dbErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
