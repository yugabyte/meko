package repository

import (
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// A duplicate per-user datapack name must be classified as a unique violation so
// Create/UpdateByID return a clean "already exists" (→ 409), not a generic 500
// loop — and it must work on YugabyteDB, where pq.Error.Constraint is empty and
// only the message carries the index name (AGENTS.md; CLOUDGA-36978). This is why
// the repo keys on SQLSTATE 23505 via isUniqueViolation23505, not on Constraint.
func TestDatapackNameConflictClassification(t *testing.T) {
	const index = "uq_datapacks_user_name"
	msg := fmt.Sprintf(`duplicate key value violates unique constraint "%s"`, index)

	for _, tc := range []struct {
		err  *pq.Error
		name string
	}{
		{name: "postgres", err: &pq.Error{Code: "23505", Constraint: index, Message: msg}},
		{name: "ysql", err: &pq.Error{Code: "23505", Message: msg}}, // YSQL leaves Constraint empty
	} {
		if !isUniqueViolation23505(tc.err) {
			t.Errorf("%s: a duplicate datapack name should be classified as a unique violation", tc.name)
		}
	}

	if isUniqueViolation23505(&pq.Error{Code: "23503"}) {
		t.Error("a non-unique-violation SQLSTATE must not be classified as a name conflict")
	}
}

// ysqlNameViolation is how YugabyteDB reports a uq_datapacks_user_name conflict:
// SQLSTATE 23505 with Constraint EMPTY (only the message carries the index name).
func ysqlNameViolation() *pq.Error {
	return &pq.Error{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint "uq_datapacks_user_name"`,
	}
}

// Create must turn that INSERT violation into the clean "already exists" the handler
// maps to 409 — not the generic "failed to insert" that falls through to a 500 retry
// loop. Exercises the branch at datapack_repo.go:181; deleting it fails here.
func TestDatapackRepo_Create_DuplicateNameIsConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT EXISTS \(\s*SELECT 1 FROM meko_system\.agents`).
		WithArgs("user-1", "meko_agent").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`INSERT INTO meko_system\.datapacks`).
		WillReturnError(ysqlNameViolation())
	mock.ExpectRollback()

	repo := NewDatapackRepo(db, "", DeployEnvProd)
	_, err = repo.Create(CreateDatapackParams{
		Name: "dp-1", UserID: "user-1", AgentID: "meko_agent", UniverseID: "u-1",
		ConnStr: "postgresql://...", MCPUserName: "mcp_user_1", MCPPassword: "secret",
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Create on a duplicate name = %v, want an \"already exists\" conflict", err)
	}
}

// UpdateByID must do the same for the rename path (datapack_repo.go:600).
func TestDatapackRepo_UpdateByID_DuplicateNameIsConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE meko_system\.datapacks`).
		WillReturnError(ysqlNameViolation())

	repo := NewDatapackRepo(db, "", DeployEnvProd)
	err = repo.UpdateByID("user-1", "dp-uuid-1", UpdateDatapackParams{Name: "taken"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("UpdateByID on a duplicate name = %v, want an \"already exists\" conflict", err)
	}
}
