package repository

import (
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

const (
	// The claim that skips locked rows, and the one that waits for them. The
	// waiting pattern only matches a bare FOR UPDATE, so NOWAIT would not pass.
	claimSkipLocked = `FOR UPDATE SKIP LOCKED\s*\)`
	claimWait       = `FOR UPDATE\s*\)`
	roomCheck       = `SELECT EXISTS`
)

func universeRow() *sqlmock.Rows {
	return sqlmock.NewRows(
		[]string{"universe_id", "ysql_hostname", "ysql_port", "ysql_admin_connection_string"},
	).AddRow("u-1", "yb", 5433, "postgresql://yb")
}

func noRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"universe_id"})
}

func room(has bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"exists"}).AddRow(has)
}

var serializationFailure = &pq.Error{Code: "40001", Message: "could not serialize access due to concurrent update"}

func TestYBARepo_AssignUniverse(t *testing.T) {
	tests := []struct {
		name    string
		expect  func(sqlmock.Sqlmock)
		wantErr error
	}{
		{
			name: "takes an unlocked universe without waiting",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(claimSkipLocked).WillReturnRows(universeRow())
			},
		},
		{
			name: "waits for a locked universe when every universe with room is locked",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(claimSkipLocked).WillReturnRows(noRows())
				m.ExpectQuery(claimWait).WillReturnRows(universeRow())
			},
		},
		{
			name: "retries when the waited-on universe filled up and another has room",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(claimSkipLocked).WillReturnRows(noRows())
				m.ExpectQuery(claimWait).WillReturnRows(noRows())
				m.ExpectQuery(roomCheck).WillReturnRows(room(true))
				m.ExpectQuery(claimSkipLocked).WillReturnRows(universeRow())
			},
		},
		{
			name: "retries a serialization failure while a universe has room",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(claimSkipLocked).WillReturnRows(noRows())
				m.ExpectQuery(claimWait).WillReturnError(serializationFailure)
				m.ExpectQuery(roomCheck).WillReturnRows(room(true))
				m.ExpectQuery(claimSkipLocked).WillReturnRows(universeRow())
			},
		},
		{
			name: "reports contention, not a full pool, when every attempt loses while a universe has room",
			expect: func(m sqlmock.Sqlmock) {
				for i := 0; i < assignAttempts; i++ {
					m.ExpectQuery(claimSkipLocked).WillReturnRows(noRows())
					m.ExpectQuery(claimWait).WillReturnError(serializationFailure)
					m.ExpectQuery(roomCheck).WillReturnRows(room(true))
				}
			},
			wantErr: ErrUniverseContention,
		},
		{
			name: "returns any other database error at once",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(claimSkipLocked).WillReturnError(errors.New("connection reset"))
			},
			wantErr: errors.New("connection reset"),
		},
		{
			name: "reports the pool full when no universe has room",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(claimSkipLocked).WillReturnRows(noRows())
				m.ExpectQuery(claimWait).WillReturnRows(noRows())
				m.ExpectQuery(roomCheck).WillReturnRows(room(false))
			},
			wantErr: ErrNoUniverseCapacity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			tt.expect(mock)

			a, err := NewYBARepo(db).AssignUniverse()
			if tt.wantErr != nil {
				// A sentinel must match with errors.Is; a driver error only by its text.
				if err == nil || !(errors.Is(err, tt.wantErr) || strings.Contains(err.Error(), tt.wantErr.Error())) {
					t.Fatalf("AssignUniverse error = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil || a.UniverseID != "u-1" {
				t.Fatalf("AssignUniverse = %+v, %v; want u-1", a, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}
