package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file adds the *idempotent, id-keyed* provisioning operations the
// write-ahead task framework's step catalog drives (controller/internal/task).
// They differ from the Backend create methods above only in that the caller
// supplies the row ids (assigned write-ahead when the task is planned) instead
// of the backend generating them — which is what lets a step be re-run after a
// crash and converge (INSERT ... ON CONFLICT (pk) DO NOTHING) rather than create
// a duplicate. They are deliberately *not* on the Backend interface: the
// synchronous API path keeps using the id-generating methods, and only the task
// steps need these. *PgBackend structurally satisfies task.StateStore.
//
// SetEndpointAddress and DeleteEndpoint (already on Backend) round out the
// task.StateStore method set; only the Ensure* methods are new.

// EnsureProject idempotently creates a project and its default branch with the
// given ids (the create_project step). Re-running with the same ids is a no-op.
// A project-name collision within the pool surfaces as ErrConflict (the UNIQUE
// (storage_pool_id, name) constraint, which is not the ON CONFLICT target); a
// missing pool surfaces as ErrNotFound (the FK on project.storage_pool_id).
func (b *PgBackend) EnsureProject(ctx context.Context, poolID, projectID, branchID uuid.UUID, name string, pgVersion int) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO project (id, storage_pool_id, name) VALUES ($1::uuid, $2::uuid, $3)
		 ON CONFLICT (id) DO NOTHING`,
		projectID.String(), poolID.String(), name); err != nil {
		return mapProjectErr("insert project", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO branch (project_id, branch_id, name, pg_version, is_default)
		 VALUES ($1::uuid, $2::uuid, $3, $4, true)
		 ON CONFLICT (project_id, branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), name, pgVersion); err != nil {
		return mapBranchErr("insert default branch", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ensure project+default branch: %w", err)
	}
	return nil
}

// EnsureBranch idempotently creates a branch (a non-default branch) with the
// given id, forked from parentBranchID at forkLSN, under projectID (the
// create_branch step). Re-running with the same id is a no-op. A name collision
// within the project surfaces as ErrConflict; a missing parent (the in-project
// fork FK) as ErrNotFound. forkLSN must be a well-formed "X/X" LSN (the caller
// validates before it reaches the pg_lsn cast). No pool id is needed: the branch
// is scoped to its project, and the in-project parent FK enforces lineage.
func (b *PgBackend) EnsureBranch(ctx context.Context, parentBranchID, projectID, branchID uuid.UUID, name, forkLSN string, pgVersion int) error {
	// is_default = false; record the fork ancestry. The explicit pg_version is the
	// parent's, resolved write-ahead at plan time (a branch inherits its
	// ancestor's version), so it never drifts from the parent.
	if _, err := b.pool.Exec(ctx,
		`INSERT INTO branch (project_id, branch_id, name, pg_version, is_default, parent_branch_id, fork_lsn)
		 VALUES ($1::uuid, $2::uuid, $3, $4, false, $5::uuid, $6::pg_lsn)
		 ON CONFLICT (project_id, branch_id) DO NOTHING`,
		projectID.String(), branchID.String(), name, pgVersion, parentBranchID.String(), forkLSN); err != nil {
		return mapBranchErr("insert branch", err)
	}
	return nil
}

// EnsureEndpointRecord idempotently records an endpoint with the given id on the
// (projectID, branchID) branch (the create_endpoint step's first effect).
// Re-running with the same id is a no-op (ON CONFLICT (id) DO NOTHING), so the
// single-writer partial unique index is not violated on replay. A *second*
// read_write endpoint (a different id) surfaces as ErrConflict; a missing branch
// as ErrNotFound.
func (b *PgBackend) EnsureEndpointRecord(ctx context.Context, endpointID, projectID, branchID uuid.UUID, role string, config []byte) error {
	var cfg any
	if len(config) > 0 {
		cfg = config
	}
	_, err := b.pool.Exec(ctx,
		`INSERT INTO endpoint (id, project_id, branch_id, role, config)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4::endpoint_role, $5::jsonb)
		 ON CONFLICT (id) DO NOTHING`,
		endpointID.String(), projectID.String(), branchID.String(), role, cfg)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation: a read_write endpoint already exists (one_writer_per_branch)
				return ErrConflict
			case "23503": // foreign_key_violation: the (project_id, branch_id) branch does not exist
				return ErrNotFound
			}
		}
		return fmt.Errorf("insert endpoint: %w", err)
	}
	return nil
}
