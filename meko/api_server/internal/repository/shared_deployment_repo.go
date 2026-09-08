package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SharedDeploymentRepo tracks the farm of pre-deployed AgentCore Runtimes
// any signed-in user can invoke without deploying their own -- one ARN per
// deployable agent_slug. Backing table: meko_system.shared_deployments.
// Unlike DeploymentRepo, these rows have no owning user_id and no
// lifecycle this repo drives: the runtimes are created and operated
// entirely outside Meko, and rows are written by an operator, not a deploy
// flow.
type SharedDeploymentRepo struct {
	db *sql.DB
}

// NewSharedDeploymentRepo creates a new SharedDeploymentRepo.
func NewSharedDeploymentRepo(db *sql.DB) *SharedDeploymentRepo {
	return &SharedDeploymentRepo{db: db}
}

// GetARN returns the shared ARN configured for agentSlug, or ("", nil) if
// none is set yet.
func (r *SharedDeploymentRepo) GetARN(ctx context.Context, agentSlug string) (string, error) {
	var arn string
	err := r.db.QueryRowContext(ctx,
		`SELECT agent_runtime_arn FROM meko_system.shared_deployments WHERE agent_slug = $1`,
		agentSlug,
	).Scan(&arn)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("shared deployment: get arn: %w", err)
	}
	return arn, nil
}

// Set upserts the shared ARN for agentSlug -- the write path an operator
// uses to add or update a farm entry.
func (r *SharedDeploymentRepo) Set(ctx context.Context, agentSlug, arn string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO meko_system.shared_deployments (agent_slug, agent_runtime_arn)
		 VALUES ($1, $2)
		 ON CONFLICT (agent_slug) DO UPDATE SET agent_runtime_arn = $2, updated_at = NOW()`,
		agentSlug, arn,
	)
	if err != nil {
		return fmt.Errorf("shared deployment: set arn: %w", err)
	}
	return nil
}

// List returns every configured (agent_slug -> arn) pair in the farm.
func (r *SharedDeploymentRepo) List(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT agent_slug, agent_runtime_arn FROM meko_system.shared_deployments`)
	if err != nil {
		return nil, fmt.Errorf("shared deployment: list: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var slug, arn string
		if err := rows.Scan(&slug, &arn); err != nil {
			return nil, fmt.Errorf("shared deployment: scan: %w", err)
		}
		out[slug] = arn
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shared deployment: list rows: %w", err)
	}
	return out, nil
}
