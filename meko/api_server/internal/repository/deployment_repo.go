package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"meko/api_server/internal/models"
)

// DeploymentRepo encapsulates database operations for agent deployments.
//
// Backing table: meko_system.deployments. Identified by (user_id, id) --
// every lookup binds both, so a caller can never address another user's
// deployment by id guess (same defense-in-depth as MCPPATRepo).
type DeploymentRepo struct {
	db *sql.DB
}

// NewDeploymentRepo creates a new DeploymentRepo.
func NewDeploymentRepo(db *sql.DB) *DeploymentRepo {
	return &DeploymentRepo{db: db}
}

const deploymentColumns = `id, user_id, agent_slug, runtime, account_mode, region,
	role_arn, external_id, agent_runtime_arn, agent_runtime_id, container_uri,
	status, status_message, created_at, updated_at`

func scanDeployment(row interface{ Scan(...any) error }) (*models.Deployment, error) {
	var d models.Deployment
	err := row.Scan(
		&d.ID, &d.UserID, &d.AgentSlug, &d.Runtime, &d.AccountMode, &d.Region,
		&d.RoleARN, &d.ExternalID, &d.AgentRuntimeARN, &d.AgentRuntimeID, &d.ContainerURI,
		&d.Status, &d.StatusMessage, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// CreateDeploymentParams holds the inputs for inserting a new deployment row.
// RoleARN / ExternalID are set by the BYOC init/confirm flow, not at
// managed-mode create time.
type CreateDeploymentParams struct {
	UserID      string
	AgentSlug   string
	Runtime     string
	AccountMode string
	Region      string
	ExternalID  *string
}

// Create inserts a new deployment row with status='pending' and returns the
// persisted record.
func (r *DeploymentRepo) Create(ctx context.Context, p CreateDeploymentParams) (*models.Deployment, error) {
	row := r.db.QueryRowContext(ctx,
		`INSERT INTO meko_system.deployments
			(user_id, agent_slug, runtime, account_mode, region, external_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+deploymentColumns,
		p.UserID, p.AgentSlug, p.Runtime, p.AccountMode, p.Region, p.ExternalID,
	)
	d, err := scanDeployment(row)
	if err != nil {
		slog.Error("failed to insert deployment",
			"user_id", p.UserID, "agent_slug", p.AgentSlug, "runtime", p.Runtime, "error", err)
		return nil, fmt.Errorf("deployment: insert: %w", err)
	}
	slog.Info("deployment created",
		"deployment_id", d.ID, "user_id", p.UserID, "agent_slug", p.AgentSlug,
		"runtime", p.Runtime, "account_mode", p.AccountMode)
	return d, nil
}

// ListByUser returns every deployment owned by userID, newest first.
func (r *DeploymentRepo) ListByUser(ctx context.Context, userID string) ([]models.Deployment, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+deploymentColumns+`
		 FROM meko_system.deployments
		 WHERE user_id = $1
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("deployment: list: %w", err)
	}
	defer func() {
		if cErr := rows.Close(); cErr != nil {
			slog.Warn("deployment list: rows.Close", "user_id", userID, "error", cErr)
		}
	}()

	var out []models.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("deployment: scan: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deployment: list rows: %w", err)
	}
	return out, nil
}

// GetByID returns the deployment identified by (userID, id), or (nil, nil)
// when no row matches.
func (r *DeploymentRepo) GetByID(ctx context.Context, userID, id string) (*models.Deployment, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+deploymentColumns+`
		 FROM meko_system.deployments
		 WHERE user_id = $1 AND id = $2`,
		userID, id,
	)
	d, err := scanDeployment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("deployment: get by id: %w", err)
	}
	return d, nil
}

// ListDeploying returns every deployment currently in status='deploying',
// across all users -- the status poller's input set. Cheap via
// deployments_status_idx.
func (r *DeploymentRepo) ListDeploying(ctx context.Context) ([]models.Deployment, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+deploymentColumns+`
		 FROM meko_system.deployments
		 WHERE status = $1`,
		models.DeploymentStatusDeploying,
	)
	if err != nil {
		return nil, fmt.Errorf("deployment: list deploying: %w", err)
	}
	defer func() {
		if cErr := rows.Close(); cErr != nil {
			slog.Warn("deployment list deploying: rows.Close", "error", cErr)
		}
	}()

	var out []models.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("deployment: scan: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deployment: list deploying rows: %w", err)
	}
	return out, nil
}

// SetRoleARN stores the BYOC role ARN confirmed by the caller (onboarding
// step 2). Scoped to (id, userID) so a caller cannot set another user's
// deployment's role.
func (r *DeploymentRepo) SetRoleARN(ctx context.Context, userID, id, roleARN string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.deployments
		 SET role_arn = $1, updated_at = NOW()
		 WHERE user_id = $2 AND id = $3`,
		roleARN, userID, id,
	)
	if err != nil {
		return fmt.Errorf("deployment: set role arn: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("deployment: set role arn rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("deployment %q not found", id)
	}
	return nil
}

// UpdateStatus writes the deployment's lifecycle status + optional message,
// and (when non-empty) the AWS identifiers CreateAgentRuntime/GetAgentRuntime
// return. Scoped by id alone -- the status poller runs across all users, not
// on behalf of a single caller.
func (r *DeploymentRepo) UpdateStatus(
	ctx context.Context, id, status, statusMessage string,
	agentRuntimeARN, agentRuntimeID, containerURI string,
) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE meko_system.deployments
		 SET status = $1,
		     status_message = NULLIF($2, ''),
		     agent_runtime_arn = COALESCE(NULLIF($3, ''), agent_runtime_arn),
		     agent_runtime_id = COALESCE(NULLIF($4, ''), agent_runtime_id),
		     container_uri = COALESCE(NULLIF($5, ''), container_uri),
		     updated_at = NOW()
		 WHERE id = $6`,
		status, statusMessage, agentRuntimeARN, agentRuntimeID, containerURI, id,
	)
	if err != nil {
		return fmt.Errorf("deployment: update status: %w", err)
	}
	return nil
}

// DeleteByID removes the deployment row. Scoped to (id, userID).
func (r *DeploymentRepo) DeleteByID(ctx context.Context, userID, id string) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM meko_system.deployments WHERE user_id = $1 AND id = $2`,
		userID, id,
	)
	if err != nil {
		return fmt.Errorf("deployment: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("deployment: delete rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("deployment %q not found", id)
	}
	return nil
}
