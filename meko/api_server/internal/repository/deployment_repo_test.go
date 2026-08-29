package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var deploymentColumnsForTest = []string{
	"id", "user_id", "agent_slug", "runtime", "account_mode", "region",
	"role_arn", "external_id", "agent_runtime_arn", "agent_runtime_id", "container_uri",
	"status", "status_message", "created_at", "updated_at",
}

func newDeploymentRow(
	id, userID, agentSlug, runtime, accountMode, region string,
	roleARN, externalID, agentRuntimeARN, agentRuntimeID, containerURI any,
	status string, statusMessage any,
	created, updated time.Time,
) *sqlmock.Rows {
	return sqlmock.NewRows(deploymentColumnsForTest).AddRow(
		id, userID, agentSlug, runtime, accountMode, region,
		roleARN, externalID, agentRuntimeARN, agentRuntimeID, containerURI,
		status, statusMessage, created, updated,
	)
}

func TestDeploymentRepo_Create_ReturnsRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	extID := "ext-123"
	mock.ExpectQuery(`INSERT INTO meko_system\.deployments`).
		WithArgs("user-1", "adlc-review-agent", "agentcore", "byoc", "us-east-1", &extID).
		WillReturnRows(newDeploymentRow(
			"dep-1", "user-1", "adlc-review-agent", "agentcore", "byoc", "us-east-1",
			nil, extID, nil, nil, nil,
			"pending", nil, now, now,
		))

	repo := NewDeploymentRepo(db)
	dep, err := repo.Create(context.Background(), CreateDeploymentParams{
		UserID:      "user-1",
		AgentSlug:   "adlc-review-agent",
		Runtime:     "agentcore",
		AccountMode: "byoc",
		Region:      "us-east-1",
		ExternalID:  &extID,
	})
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if dep.ID != "dep-1" || dep.Status != "pending" {
		t.Errorf("Create: got %+v", dep)
	}
	if dep.ExternalID == nil || *dep.ExternalID != extID {
		t.Errorf("expected ExternalID=%q, got %v", extID, dep.ExternalID)
	}
	if dep.RoleARN != nil {
		t.Errorf("expected RoleARN nil, got %v", dep.RoleARN)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDeploymentRepo_GetByID_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT .* FROM meko_system\.deployments WHERE user_id = \$1 AND id = \$2`).
		WithArgs("user-1", "missing").
		WillReturnRows(sqlmock.NewRows(deploymentColumnsForTest))

	repo := NewDeploymentRepo(db)
	dep, err := repo.GetByID(context.Background(), "user-1", "missing")
	if err != nil {
		t.Fatalf("GetByID: unexpected error: %v", err)
	}
	if dep != nil {
		t.Errorf("expected nil deployment, got %+v", dep)
	}
}

func TestDeploymentRepo_SetRoleARN_NotFoundReturnsError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE meko_system\.deployments`).
		WithArgs("arn:aws:iam::123456789012:role/meko-deploy", "user-1", "dep-missing").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewDeploymentRepo(db)
	err = repo.SetRoleARN(context.Background(), "user-1", "dep-missing", "arn:aws:iam::123456789012:role/meko-deploy")
	if err == nil {
		t.Fatal("expected error for missing row, got nil")
	}
}

func TestDeploymentRepo_ListDeploying_ScansRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT .* FROM meko_system\.deployments WHERE status = \$1`).
		WithArgs("deploying").
		WillReturnRows(newDeploymentRow(
			"dep-1", "user-1", "adlc-review-agent", "agentcore", "managed", "us-east-1",
			nil, nil, "arn:aws:bedrock-agentcore:...", "runtime-1", "1234.dkr.ecr.us-east-1.amazonaws.com/adlc:latest",
			"deploying", nil, now, now,
		))

	repo := NewDeploymentRepo(db)
	deps, err := repo.ListDeploying(context.Background())
	if err != nil {
		t.Fatalf("ListDeploying: unexpected error: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != "dep-1" {
		t.Errorf("ListDeploying: got %+v", deps)
	}
	if deps[0].AgentRuntimeID == nil || *deps[0].AgentRuntimeID != "runtime-1" {
		t.Errorf("expected AgentRuntimeID=runtime-1, got %v", deps[0].AgentRuntimeID)
	}
}
