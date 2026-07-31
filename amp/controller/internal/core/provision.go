package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file adds the *idempotent, id-keyed* provisioning operations the
// write-ahead task framework's step catalog drives (controller/internal/task).
// They differ from the Backend create methods above only in that the caller
// supplies the row ids (assigned write-ahead when the task is planned) instead
// of the backend generating them — which is what lets a step be re-run after a
// crash and converge (INSERT ... ON CONFLICT (pk) DO NOTHING) rather than create
// a duplicate. They are deliberately *not* on the Backend interface: the
// synchronous read / pre-check path uses the id-generating Get methods and never
// these; only the worker's steps drive these id-keyed operations. *PgBackend
// structurally satisfies task.StateStore.
//
// SetEndpointAddress and DeleteEndpoint (already on Backend) round out the
// task.StateStore create/set/delete-endpoint surface; the Ensure* methods plus
// the cascade-delete primitives below (EndpointIDsForBranch / EndpointIDsFor
// Project / DeleteBranch / DeleteProject) are the task-only additions. The
// cascade primitives are id-keyed and idempotent: an absent parent enumerates to
// an empty slice and an absent row deletes to a no-op, so a delete task re-run
// after a crash converges.

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
		projectID.String(), branchID.String(), DefaultBranchName, pgVersion); err != nil {
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

// EndpointIDsForBranch returns the ids of every endpoint on the
// (projectID, branchID) branch (the delete_branch step's Execute-time
// enumeration). An absent branch yields an empty slice — no ErrNotFound — so a
// cascade re-run after a crash converges.
func (b *PgBackend) EndpointIDsForBranch(ctx context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT id::text FROM endpoint WHERE project_id = $1::uuid AND branch_id = $2::uuid`,
		projectID.String(), branchID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoint ids for branch: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// EndpointIDsForProject returns the ids of every endpoint in the project (the
// delete_project step's Execute-time enumeration). An absent project yields an
// empty slice — no ErrNotFound.
func (b *PgBackend) EndpointIDsForProject(ctx context.Context, projectID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT id::text FROM endpoint WHERE project_id = $1::uuid`,
		projectID.String())
	if err != nil {
		return nil, fmt.Errorf("list endpoint ids for project: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// ChildBranchIDs returns the ids of every branch in projectID forked directly
// from branchID (the delete_branch step's pre-cascade check). An absent branch
// yields an empty slice — no ErrNotFound.
func (b *PgBackend) ChildBranchIDs(ctx context.Context, projectID, branchID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := b.pool.Query(ctx,
		`SELECT branch_id::text FROM branch WHERE project_id = $1::uuid AND parent_branch_id = $2::uuid`,
		projectID.String(), branchID.String())
	if err != nil {
		return nil, fmt.Errorf("list child branch ids: %w", err)
	}
	defer rows.Close()
	return scanUUIDs(rows)
}

// DeleteBranch removes the (projectID, branchID) branch (idempotent; an absent
// row is a no-op). The caller retires the branch's endpoints first; the parent
// FK is RESTRICT, so a still-referencing child fork surfaces as an error, which
// is correct (the cascade must drop forks before their in-project parents).
func (b *PgBackend) DeleteBranch(ctx context.Context, projectID, branchID uuid.UUID) error {
	_, err := b.pool.Exec(ctx,
		`DELETE FROM branch WHERE project_id = $1::uuid AND branch_id = $2::uuid`,
		projectID.String(), branchID.String())
	if err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	return nil
}

// DeleteProject removes the project and all its branches and grants (idempotent;
// absent rows are a no-op). The caller retires the project's endpoints first;
// this transaction does NOT defensively drop endpoints — leaving the endpoint FK
// RESTRICT in force means an endpoint concurrently added between the step's
// enumeration and this transaction fails the project delete, so the task replays
// and retires that endpoint's instance before dropping its record (preserving the
// invariant: never drop an endpoint record without retiring its instance). The
// branch parent FK ((project_id, parent_branch_id) -> branch) is RESTRICT, so the
// branches are nulled-then-deleted in a single transaction: nulling the in-project
// parent pointers sidesteps the self-FK ordering, after which the bulk branch
// delete succeeds regardless of fork lineage. The user_project_grant FK is also
// RESTRICT, so its rows are deleted before the project row to avoid a wedge once
// grants exist.
func (b *PgBackend) DeleteProject(ctx context.Context, projectID uuid.UUID) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	id := projectID.String()
	// Drop the self-FK references first so the bulk branch delete is order-free.
	if _, err := tx.Exec(ctx,
		`UPDATE branch SET parent_branch_id = NULL WHERE project_id = $1::uuid`, id); err != nil {
		return fmt.Errorf("null branch parents: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM branch WHERE project_id = $1::uuid`, id); err != nil {
		return fmt.Errorf("delete project branches: %w", err)
	}
	// The grant FK is RESTRICT, so clear grants before the project row.
	if _, err := tx.Exec(ctx,
		`DELETE FROM user_project_grant WHERE project_id = $1::uuid`, id); err != nil {
		return fmt.Errorf("delete project grants: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM project WHERE id = $1::uuid`, id); err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete project: %w", err)
	}
	return nil
}

// scanUUIDs scans a single-column (id::text) result set into a slice of UUIDs.
func scanUUIDs(rows pgx.Rows) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("parse id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ids: %w", err)
	}
	return ids, nil
}
