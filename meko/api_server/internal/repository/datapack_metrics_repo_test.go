package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

func newMetricsRepo(t *testing.T) (*DatapackMetricsRepo, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &DatapackMetricsRepo{systemDB: db}, mock
}

func TestDatapackMetricsRepo_FetchAccess_OwnerImplicitSeatExcluded(t *testing.T) {
	// Owner's default meko_agent row is excluded; unshared datapack → 0, 0.
	repo, mock := newMetricsRepo(t)

	mock.ExpectQuery(`SELECT`).
		WithArgs("dp-1", "owner-1").
		WillReturnRows(
			sqlmock.NewRows([]string{"agent_count", "human_count", "last_accessed_at"}).
				AddRow(0, 0, nil),
		)

	m := &models.DatapackMetrics{}
	if err := repo.fetchAccess(context.Background(), "dp-1", "owner-1", m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.AgentCount != 0 {
		t.Errorf("AgentCount: got %d, want 0", m.AgentCount)
	}
	if m.HumanCount != 0 {
		t.Errorf("HumanCount: got %d, want 0", m.HumanCount)
	}
	if m.LastAccessedAt != nil {
		t.Errorf("LastAccessedAt: want nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackMetricsRepo_FetchAccess_SharedUserAndPendingInvite(t *testing.T) {
	// One accepted share + one pending invite → agent 2, human 2.
	repo, mock := newMetricsRepo(t)

	ts := time.Now().Truncate(time.Second)
	mock.ExpectQuery(`SELECT`).
		WithArgs("dp-1", "owner-1").
		WillReturnRows(
			sqlmock.NewRows([]string{"agent_count", "human_count", "last_accessed_at"}).
				AddRow(2, 2, ts),
		)

	m := &models.DatapackMetrics{}
	if err := repo.fetchAccess(context.Background(), "dp-1", "owner-1", m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.AgentCount != 2 {
		t.Errorf("AgentCount: got %d, want 2", m.AgentCount)
	}
	if m.HumanCount != 2 {
		t.Errorf("HumanCount: got %d, want 2", m.HumanCount)
	}
	if m.LastAccessedAt == nil {
		t.Error("LastAccessedAt: want non-nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDatapackMetricsRepo_FetchAccess_OwnerExtraAgentCounted(t *testing.T) {
	// Owner has linked a second agent (Claude Code) beyond the default meko_agent.
	// That extra row must remain — only (owner, meko_agent) is excluded.
	repo, mock := newMetricsRepo(t)

	mock.ExpectQuery(`SELECT`).
		WithArgs("dp-1", "owner-1").
		WillReturnRows(
			sqlmock.NewRows([]string{"agent_count", "human_count", "last_accessed_at"}).
				AddRow(1, 1, nil),
		)

	m := &models.DatapackMetrics{}
	if err := repo.fetchAccess(context.Background(), "dp-1", "owner-1", m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.AgentCount != 1 {
		t.Errorf("AgentCount: got %d, want 1", m.AgentCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
