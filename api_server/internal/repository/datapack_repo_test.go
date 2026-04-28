package repository

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDatapackRepo_Create_RequiresAgentID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	repo := NewDatapackRepo(db)
	_, err = repo.Create(CreateDatapackParams{
		Name:       "dp-1",
		UserID:     "user-1",
		UniverseID: "u-1",
	})
	if err == nil {
		t.Fatal("expected error when AgentID is empty")
	}
	if !strings.Contains(err.Error(), "agent_id is required") {
		t.Errorf("got %v", err)
	}
}

func TestDatapackRepo_Create_AgentNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT EXISTS \(\s*SELECT 1 FROM meko_system\.agents`).
		WithArgs("user-1", "ghost").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectRollback()

	repo := NewDatapackRepo(db)
	_, err = repo.Create(CreateDatapackParams{
		Name:        "dp-1",
		UserID:      "user-1",
		AgentID:     "ghost",
		UniverseID:  "u-1",
		ConnStr:     "postgresql://...",
		MCPUserName: "mcp_user_1",
		MCPPassword: "secret",
	})
	if err == nil {
		t.Fatal("expected ErrAgentNotFound, got nil")
	}
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("expected ErrAgentNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Create_HappyPath_StampsAgentAndLinks(t *testing.T) {
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
		WithArgs(
			"user-1", "meko_agent", "dp-1", "u-1",
			"postgresql://...", "mcp_user_1", "secret",
			nil,
		).
		WillReturnRows(
			sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-uuid-1"),
		)
	mock.ExpectExec(`INSERT INTO meko_system\.agent_access`).
		WithArgs("user-1", "meko_agent", "dp-uuid-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewDatapackRepo(db)
	res, err := repo.Create(CreateDatapackParams{
		Name:        "dp-1",
		UserID:      "user-1",
		AgentID:     "meko_agent",
		UniverseID:  "u-1",
		ConnStr:     "postgresql://...",
		MCPUserName: "mcp_user_1",
		MCPPassword: "secret",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil ||
		res.DatapackID != "dp-uuid-1" ||
		res.AgentID != "meko_agent" ||
		res.UserID != "user-1" {
		t.Errorf("got result %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Create_WithDescription(t *testing.T) {
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
		WithArgs(
			"user-1", "meko_agent", "dp-desc", "u-1",
			"postgresql://...", "mcp_user_1", "secret",
			`{"description":"My awesome datapack"}`,
		).
		WillReturnRows(
			sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-uuid-2"),
		)
	mock.ExpectExec(`INSERT INTO meko_system\.agent_access`).
		WithArgs("user-1", "meko_agent", "dp-uuid-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewDatapackRepo(db)
	res, err := repo.Create(CreateDatapackParams{
		Name:        "dp-desc",
		UserID:      "user-1",
		AgentID:     "meko_agent",
		UniverseID:  "u-1",
		ConnStr:     "postgresql://...",
		MCPUserName: "mcp_user_1",
		MCPPassword: "secret",
		Description: "My awesome datapack",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.DatapackID != "dp-uuid-2" {
		t.Errorf("got result %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Create_RollsBackWhenInsertDatapackFails(t *testing.T) {
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
		WithArgs(
			"user-1", "meko_agent", "dp-1", "u-1",
			"postgresql://...", "mcp_user_1", "secret",
			nil,
		).
		WillReturnError(errors.New("unique violation"))
	mock.ExpectRollback()

	repo := NewDatapackRepo(db)
	_, err = repo.Create(CreateDatapackParams{
		Name:        "dp-1",
		UserID:      "user-1",
		AgentID:     "meko_agent",
		UniverseID:  "u-1",
		ConnStr:     "postgresql://...",
		MCPUserName: "mcp_user_1",
		MCPPassword: "secret",
	})
	if err == nil {
		t.Fatal("expected error from datapack insert, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Create_RollsBackWhenLinkInsertFails(t *testing.T) {
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
		WithArgs(
			"user-1", "meko_agent", "dp-1", "u-1",
			"postgresql://...", "mcp_user_1", "secret",
			nil,
		).
		WillReturnRows(
			sqlmock.NewRows([]string{"datapack_id"}).AddRow("dp-uuid-2"),
		)
	mock.ExpectExec(`INSERT INTO meko_system\.agent_access`).
		WillReturnError(errors.New("link insert failed"))
	mock.ExpectRollback()

	repo := NewDatapackRepo(db)
	_, err = repo.Create(CreateDatapackParams{
		Name:        "dp-1",
		UserID:      "user-1",
		AgentID:     "meko_agent",
		UniverseID:  "u-1",
		ConnStr:     "postgresql://...",
		MCPUserName: "mcp_user_1",
		MCPPassword: "secret",
	})
	if err == nil {
		t.Fatal("expected error from link insert, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_GetByName_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.datapacks d\s+WHERE d\.user_id`).
		WithArgs("user-1", "missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name",
			"created_by_agent_id",
			"universe_id", "datapack_ysql_connection_string",
			"datapack_ysql_username", "created_at", "updated_at",
			"description",
		}))

	repo := NewDatapackRepo(db)
	dp, err := repo.GetByName("user-1", "missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dp != nil {
		t.Errorf("expected nil, got %+v", dp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_GetByName_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now()
	mock.ExpectQuery(`FROM meko_system\.datapacks d\s+WHERE d\.user_id`).
		WithArgs("user-1", "dp-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "user_id", "datapack_name",
			"created_by_agent_id",
			"universe_id", "datapack_ysql_connection_string",
			"datapack_ysql_username", "created_at", "updated_at",
			"description",
		}).AddRow(
			"dp-uuid-1", "user-1", "dp-1",
			"meko_agent",
			"u-1", "postgresql://...",
			"mcp_user_1", now, now,
			"test description",
		))

	repo := NewDatapackRepo(db)
	dp, err := repo.GetByName("user-1", "dp-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dp == nil ||
		dp.ID != "dp-uuid-1" ||
		dp.Name != "dp-1" ||
		dp.UserID != "user-1" ||
		dp.CreatedByAgentID != "meko_agent" ||
		dp.MCPUserName != "mcp_user_1" ||
		dp.Description != "test description" {
		t.Errorf("got %+v", dp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Delete_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system\.datapacks`).
		WithArgs("user-1", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewDatapackRepo(db)
	err = repo.Delete("user-1", "ghost")
	if err == nil {
		t.Fatal("expected error for missing datapack, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Delete_OK(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`DELETE FROM meko_system\.datapacks`).
		WithArgs("user-1", "dp-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewDatapackRepo(db)
	if err := repo.Delete("user-1", "dp-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Update_NoChange(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	repo := NewDatapackRepo(db)
	if err := repo.Update("user-1", "dp-1", UpdateDatapackParams{}); err != nil {
		t.Errorf("unexpected error for empty params: %v", err)
	}
}

func TestDatapackRepo_Update_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE meko_system\.datapacks`).
		WithArgs("postgresql://new", "user-1", "ghost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewDatapackRepo(db)
	err = repo.Update("user-1", "ghost", UpdateDatapackParams{
		YBConnStr: "postgresql://new",
	})
	if err == nil {
		t.Fatal("expected error for missing datapack, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_Update_NameAndDescription(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE meko_system\.datapacks`).
		WithArgs("new-name", `{"description":"new desc"}`, "user-1", "dp-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewDatapackRepo(db)
	err = repo.Update("user-1", "dp-1", UpdateDatapackParams{
		Name:        "new-name",
		Description: "new desc",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackRepo_GetStatus_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM meko_system\.datapacks d\s+WHERE d\.user_id`).
		WithArgs("user-1", "ghost").
		WillReturnRows(sqlmock.NewRows([]string{
			"datapack_id", "datapack_name",
			"created_by_agent_id",
			"universe_id",
			"datapack_ysql_connection_string",
			"datapack_ysql_username", "created_at",
			"agent_count", "kb_count",
		}))

	repo := NewDatapackRepo(db)
	st, err := repo.GetStatus("user-1", "ghost")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st != nil {
		t.Errorf("expected nil status, got %+v", st)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"foo", `"foo"`},
		{"FOO", `"FOO"`},
		{`weird"name`, `"weird""name"`},
	}
	for _, c := range cases {
		if got := QuoteIdent(c.in); got != c.want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
