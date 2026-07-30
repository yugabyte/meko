package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

func TestMCPPATAuditRepo_Append_DefaultsActorTypeAndMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WithArgs(
			"pat-1", "user-1", string(models.MCPPATAuditCreated),
			"user-1", string(models.MCPPATActorUser),
			nil, nil, sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	repo := NewMCPPATAuditRepo(db)
	id, err := repo.Append(context.Background(), MCPPATAuditAppendParams{
		TokenID: "pat-1",
		UserID:  "user-1",
		Action:  models.MCPPATAuditCreated,
		ActorID: "user-1",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if id != "audit-1" {
		t.Errorf("expected audit-1, got %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATAuditRepo_Append_PassesActorIPAndAppClient(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WithArgs(
			"pat-1", "user-1", string(models.MCPPATAuditRevoked),
			"user-1", string(models.MCPPATActorUser),
			"127.0.0.1", "curl/8", sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-2"))

	repo := NewMCPPATAuditRepo(db)
	_, err = repo.Append(context.Background(), MCPPATAuditAppendParams{
		TokenID:        "pat-1",
		UserID:         "user-1",
		Action:         models.MCPPATAuditRevoked,
		ActorID:        "user-1",
		ActorIP:        "127.0.0.1",
		ActorAppClient: "curl/8",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATAuditRepo_Append_RejectsMissingFields(t *testing.T) {
	repo := NewMCPPATAuditRepo(nil)
	_, err := repo.Append(context.Background(), MCPPATAuditAppendParams{
		UserID:  "u",
		Action:  models.MCPPATAuditCreated,
		ActorID: "u",
	})
	if err == nil {
		t.Fatal("expected error for missing token_id")
	}
}

func TestMCPPATAuditRepo_ListByToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	cols := []string{
		"id", "token_id", "user_id", "action", "actor_id", "actor_type",
		"actor_ip", "actor_app_client", "metadata", "created_at",
	}
	mock.ExpectQuery(`FROM meko_system\.mcp_pat_audit_log\s+WHERE token_id = \$1`).
		WithArgs("pat-1").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("a-2", "pat-1", "user-1", "revoked", "user-1", "user",
				"127.0.0.1", "curl", []byte(`{"reason":"rotation"}`), now).
			AddRow("a-1", "pat-1", "user-1", "created", "user-1", "user",
				nil, nil, []byte(`{}`), now.Add(-time.Hour)),
		)

	repo := NewMCPPATAuditRepo(db)
	rows, err := repo.ListByToken(context.Background(), "pat-1")
	if err != nil {
		t.Fatalf("ListByToken: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Action != models.MCPPATAuditRevoked {
		t.Errorf("expected revoked first, got %s", rows[0].Action)
	}
	if rows[0].ActorIP == nil || *rows[0].ActorIP != "127.0.0.1" {
		t.Errorf("expected ActorIP 127.0.0.1, got %v", rows[0].ActorIP)
	}
	if rows[0].ActorAppClient != "curl" {
		t.Errorf("expected ActorAppClient curl, got %q", rows[0].ActorAppClient)
	}
	if rows[1].ActorIP != nil {
		t.Errorf("expected nil ActorIP for second row, got %v", rows[1].ActorIP)
	}
	if rows[1].ActorAppClient != "" {
		t.Errorf("expected empty ActorAppClient for second row, got %q", rows[1].ActorAppClient)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
