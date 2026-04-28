package repository

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAgentRepo_CreateAgent_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-1", "agent-a",
			sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewAgentRepo(db)
	if err := repo.CreateAgent(
		"user-1", "agent-a", json.RawMessage(`{"role":"assistant"}`),
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_CreateAgent_DefaultDetails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-d", "agent-d", []byte(`{}`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewAgentRepo(db)
	if err := repo.CreateAgent("user-d", "agent-d", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_CreateAgent_ConflictReturnsErrAgentExists(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-1", "agent-dup", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewAgentRepo(db)
	err = repo.CreateAgent(
		"user-1", "agent-dup", json.RawMessage(`{}`),
	)
	if err == nil {
		t.Fatal("expected ErrAgentExists, got nil")
	}
	if !errors.Is(err, ErrAgentExists) {
		t.Errorf("expected ErrAgentExists, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_CreateAgent_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.agents`).
		WithArgs("user-e", "agent-e", sqlmock.AnyArg()).
		WillReturnError(errors.New("connection lost"))

	repo := NewAgentRepo(db)
	err = repo.CreateAgent("user-e", "agent-e", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrAgentExists) {
		t.Error("did not expect ErrAgentExists for DB error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_GetAgent_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM meko_system\.agents\s+WHERE user_id`).
		WithArgs("user-1", "agent-a").
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id", "agent_id", "agent_details",
			"created_at", "updated_at",
		}).AddRow(
			"user-1", "agent-a", `{"role":"assistant"}`,
			now, now,
		))

	repo := NewAgentRepo(db)
	a, err := repo.GetAgent("user-1", "agent-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == nil || a.AgentID != "agent-a" {
		t.Fatalf("got %+v", a)
	}
	if !strings.Contains(string(a.Details), "assistant") {
		t.Errorf("expected details to contain assistant, got %s", a.Details)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_GetAgent_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.agents\s+WHERE user_id`).
		WithArgs("user-1", "ghost").
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id", "agent_id", "agent_details",
			"created_at", "updated_at",
		}))

	repo := NewAgentRepo(db)
	a, err := repo.GetAgent("user-1", "ghost")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a != nil {
		t.Errorf("expected nil, got %+v", a)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_LinkAgentToDatapack_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.agent_access`).
		WithArgs("user-1", "agent-a", "dp-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewAgentRepo(db)
	linked, err := repo.LinkAgentToDatapack(
		"user-1", "agent-a", "dp-1",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !linked {
		t.Error("expected linked=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_LinkAgentToDatapack_AgentMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.agent_access`).
		WithArgs("user-1", "ghost", "dp-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(
		`SELECT 1 FROM meko_system\.agent_access`).
		WithArgs("user-1", "ghost", "dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))

	repo := NewAgentRepo(db)
	linked, err := repo.LinkAgentToDatapack(
		"user-1", "ghost", "dp-1",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if linked {
		t.Error("expected linked=false when agent does not exist")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_LinkAgentToDatapack_AlreadyLinked(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO meko_system\.agent_access`).
		WithArgs("user-1", "agent-a", "dp-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(
		`SELECT 1 FROM meko_system\.agent_access`).
		WithArgs("user-1", "agent-a", "dp-1").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))

	repo := NewAgentRepo(db)
	linked, err := repo.LinkAgentToDatapack(
		"user-1", "agent-a", "dp-1",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !linked {
		t.Error("expected linked=true when row already exists")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_UnlinkAgentFromDatapack_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system\.agent_access`).
		WithArgs("user-1", "agent-a", "dp-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewAgentRepo(db)
	if err := repo.UnlinkAgentFromDatapack(
		"user-1", "agent-a", "dp-1",
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_UnlinkAgentFromDatapack_NotLinked(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system\.agent_access`).
		WithArgs("user-1", "agent-a", "dp-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewAgentRepo(db)
	err = repo.UnlinkAgentFromDatapack("user-1", "agent-a", "dp-1")
	if err == nil {
		t.Fatal("expected error for not-linked, got nil")
	}
	if !errors.Is(err, ErrAgentLinkNotFound) {
		t.Errorf("expected ErrAgentLinkNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "not linked") {
		t.Errorf("expected 'not linked' message, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_ListAgentsForUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM meko_system\.agents\s+WHERE user_id`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id", "agent_id", "agent_details",
			"created_at", "updated_at",
		}).
			AddRow("user-1", "meko_agent", `{}`, now, now).
			AddRow("user-1", "agent-a", `{"role":"assistant"}`, now, now))

	repo := NewAgentRepo(db)
	agents, err := repo.ListAgentsForUser("user-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_ListAgentsForDatapack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM meko_system\.agents a\s+JOIN meko_system\.agent_access ad`).
		WithArgs("dp-1", "user-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id", "agent_id", "agent_details",
			"created_at", "updated_at",
		}).
			AddRow("user-1", "meko_agent", `{}`, now, now))

	repo := NewAgentRepo(db)
	agents, err := repo.ListAgentsForDatapack("user-1", "dp-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_DeleteAgent_RemovesLinksAndAgent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM meko_system\.agent_access\s+WHERE user_id`).
		WithArgs("user-1", "agent-a").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM meko_system\.agents\s+WHERE user_id`).
		WithArgs("user-1", "agent-a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewAgentRepo(db)
	if err := repo.DeleteAgent("user-1", "agent-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAgentRepo_DeleteAgent_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM meko_system\.agent_access\s+WHERE user_id`).
		WithArgs("user-1", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM meko_system\.agents\s+WHERE user_id`).
		WithArgs("user-1", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	repo := NewAgentRepo(db)
	err = repo.DeleteAgent("user-1", "ghost")
	if err == nil {
		t.Fatal("expected error for missing agent, got nil")
	}
	if !errors.Is(err, ErrAgentNotFoundForDelete) {
		t.Errorf("expected ErrAgentNotFoundForDelete, got %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' message, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
