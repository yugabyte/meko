package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestFleetSnapshotReadsNullsAsEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hb := time.Unix(1_800_000_000, 0).UTC()
	mock.ExpectQuery(`FROM meko_system\.environments e`).WillReturnRows(
		sqlmock.NewRows([]string{"uuid", "kind", "type", "state", "agent_version", "last_heartbeat_at"}).
			AddRow("u1", "amp", "BYOC", "READY", "1.2", hb).
			AddRow("u2", "meko", "BYOC", "PENDING", nil, nil))

	rows, err := NewEnvironmentRepo(db).FleetSnapshot(context.Background())
	if err != nil {
		t.Fatalf("FleetSnapshot: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].AgentVersion != "1.2" || rows[0].LastHeartbeatAt == nil || !rows[0].LastHeartbeatAt.Equal(hb) {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].AgentVersion != "" || rows[1].LastHeartbeatAt != nil {
		t.Errorf("row 1 = %+v, want empty version and no heartbeat", rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskCountByStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT status, COUNT\(\*\) FROM meko_system\.environment_tasks GROUP BY status`).
		WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).AddRow("running", 2).AddRow("failed", 5))

	got, err := NewEnvironmentTaskRepo(db).CountByStatus(context.Background())
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if got["running"] != 2 || got["failed"] != 5 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}
